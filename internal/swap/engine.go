package swap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrFallocateUnsupported — ФС не поддерживает fallocate.
var ErrFallocateUnsupported = errors.New("fallocate not supported")

// Event — прогресс выполнения для вывода пользователю.
type Event struct {
	Undo  bool // шаг отката
	Index int  // 1-based
	Total int
	Step  Step
	Err   error
}

// Runner выполняет шаги по одному. Если шаг падает, уже выполненные шаги
// этого запуска откатываются в обратном порядке.
type Runner struct {
	Sys  System
	OpID string
	// Journal вызывается после каждого успешного шага; ошибка журнала
	// считается ошибкой шага (без журнала нельзя гарантировать rollback).
	Journal  func(applied []Applied) error
	Progress func(Event)
}

// StepError — шаг упал; RollbackErrors — что не удалось откатить.
type StepError struct {
	Index, Total   int
	Step           Step
	Err            error
	RollbackErrors []error
}

func (e *StepError) Error() string {
	msg := fmt.Sprintf("шаг %d/%d (%s): %v", e.Index, e.Total, e.Step.Describe(), e.Err)
	if len(e.RollbackErrors) == 0 {
		return msg + "; выполненные шаги откачены, система в исходном состоянии"
	}
	var b strings.Builder
	b.WriteString(msg + "; ОТКАТ ВЫПОЛНЕН НЕ ПОЛНОСТЬЮ, нужна ручная проверка:")
	for _, re := range e.RollbackErrors {
		b.WriteString("\n  - " + re.Error())
	}
	return b.String()
}

func (e *StepError) Unwrap() error { return e.Err }

func (r *Runner) progress(ev Event) {
	if r.Progress != nil {
		r.Progress(ev)
	}
}

// Run выполняет steps. При ошибке возвращает *StepError и выполненные
// (уже откаченные) шаги.
func (r *Runner) Run(ctx context.Context, steps []Step) ([]Applied, error) {
	var applied []Applied
	for i, s := range steps {
		ev := Event{Index: i + 1, Total: len(steps), Step: s}
		a, err := r.exec(ctx, s)
		if err == nil {
			applied = append(applied, a)
			if r.Journal != nil {
				if jerr := r.Journal(applied); jerr != nil {
					err = fmt.Errorf("запись журнала %s: %w", StatePath, jerr)
				}
			}
		}
		ev.Err = err
		r.progress(ev)
		if err != nil {
			se := &StepError{Index: i + 1, Total: len(steps), Step: s, Err: err}
			se.RollbackErrors = r.undo(ctx, applied)
			return applied, se
		}
	}
	return applied, nil
}

// undo откатывает applied в обратном порядке, продолжая после ошибок.
// После отката каждого шага журнал укорачивается, чтобы state.json не
// содержал уже отменённых шагов (иначе rollback отменил бы их повторно).
func (r *Runner) undo(ctx context.Context, applied []Applied) []error {
	var errs []error
	total := len(UndoSteps(applied))
	n := 0
	for i := len(applied) - 1; i >= 0; i-- {
		ok := true
		for _, s := range applied[i].Undo {
			n++
			_, err := r.exec(ctx, s)
			r.progress(Event{Undo: true, Index: n, Total: total, Step: s, Err: err})
			if err != nil {
				ok = false
				errs = append(errs, fmt.Errorf("%s: %w", s.Describe(), err))
			}
		}
		if ok && r.Journal != nil {
			// Ошибка журнала здесь не делает откат неполным: система уже
			// восстановлена, итоговый статус запишет вызывающий.
			_ = r.Journal(applied[:i])
		}
	}
	return errs
}

func (r *Runner) run(ctx context.Context, name string, args ...string) error {
	_, err := r.Sys.Exec.Run(ctx, name, args...)
	return err
}

// exec выполняет один шаг и вычисляет, как его отменить.
func (r *Runner) exec(ctx context.Context, s Step) (Applied, error) {
	fsys := r.Sys.FS
	a := Applied{Step: s}
	withReload := func(undo ...Step) []Step {
		if s.Reload {
			undo = append(undo, daemonReload())
		}
		return undo
	}

	switch s.Kind {
	case KindAllocate:
		err := fsys.Fallocate(s.Path, s.Size)
		if errors.Is(err, fs.ErrExist) {
			return a, err // чужой файл — не трогаем
		}
		if errors.Is(err, ErrFallocateUnsupported) {
			_ = fsys.Remove(s.Path)
			err = fsys.WriteZeros(s.Path, s.Size)
		}
		if err != nil {
			_ = fsys.Remove(s.Path) // недописанный файл — наш
			return a, err
		}
		// Отмена — delete_swapfile: он сам обратим (пересоздаёт файл), поэтому
		// откат отката тоже работает.
		a.Undo = []Step{{Kind: KindDeleteSwapfile, Path: s.Path, Size: s.Size}}

	case KindBtrfsMkswapfile:
		if _, err := fsys.Stat(s.Path); err == nil {
			return a, &fs.PathError{Op: "create", Path: s.Path, Err: fs.ErrExist}
		}
		if err := r.run(ctx, "btrfs", "filesystem", "mkswapfile", "--size", strconv.FormatUint(s.Size, 10), s.Path); err != nil {
			if _, serr := fsys.Stat(s.Path); serr == nil {
				_ = fsys.Remove(s.Path)
			}
			return a, err
		}
		a.Undo = []Step{{Kind: KindDeleteSwapfile, Path: s.Path, Size: s.Size}}

	case KindChmod:
		if err := fsys.Chmod(s.Path, fs.FileMode(s.Mode)); err != nil {
			return a, err
		}

	case KindMkswap:
		if err := r.run(ctx, "mkswap", s.Path); err != nil {
			return a, err
		}

	case KindSwapon:
		if err := r.Sys.Kernel.Swapon(s.Path, s.Priority); err != nil {
			return a, fmt.Errorf("swapon %s: %w", s.Path, err)
		}
		a.Undo = []Step{{Kind: KindSwapoff, Path: s.Path, Priority: s.Priority, Size: s.Size}}

	case KindSwapoff:
		if err := r.Sys.Kernel.Swapoff(s.Path); err != nil {
			return a, fmt.Errorf("swapoff %s: %w", s.Path, err)
		}
		a.Undo = []Step{{Kind: KindSwapon, Path: s.Path, Priority: s.Priority, Size: s.Size}}

	case KindRemoveFile:
		if err := fsys.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return a, err
		}

	case KindDeleteSwapfile:
		st, err := fsys.Stat(s.Path)
		if err != nil {
			return a, err
		}
		size := uint64(st.Size())
		if err := fsys.Remove(s.Path); err != nil {
			return a, err
		}
		// Содержимое swap после swapoff не нужно — пересоздание пустого
		// swapfile того же размера эквивалентно восстановлению.
		a.Undo = []Step{
			{Kind: KindAllocate, Path: s.Path, Size: size},
			{Kind: KindChmod, Path: s.Path, Mode: 0o600},
			{Kind: KindMkswap, Path: s.Path},
		}

	case KindWriteFile:
		prev, err := fsys.ReadFile(s.Path)
		existed := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return a, err
		}
		mode := fs.FileMode(s.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if existed {
			prevMode := mode
			if st, err := fsys.Stat(s.Path); err == nil {
				prevMode = st.Mode().Perm()
			}
			if a.Backup, err = r.backup(s.Path, prev); err != nil {
				return a, err
			}
			a.Undo = withReload(Step{Kind: KindWriteFile, Path: s.Path, Content: string(prev), Mode: uint32(prevMode)})
		} else {
			a.Undo = withReload(Step{Kind: KindDeleteConfig, Path: s.Path, Reload: s.Reload})
		}
		if err := fsys.WriteFileAtomic(s.Path, []byte(s.Content), mode); err != nil {
			return a, err
		}

	case KindDeleteConfig:
		prev, err := fsys.ReadFile(s.Path)
		if err != nil {
			return a, err
		}
		mode := fs.FileMode(0o644)
		if st, err := fsys.Stat(s.Path); err == nil {
			mode = st.Mode().Perm()
		}
		if a.Backup, err = r.backup(s.Path, prev); err != nil {
			return a, err
		}
		if err := fsys.Remove(s.Path); err != nil {
			return a, err
		}
		a.Undo = withReload(Step{Kind: KindWriteFile, Path: s.Path, Content: string(prev), Mode: uint32(mode), Reload: s.Reload})

	case KindFstabDisable, KindFstabEnable:
		content, err := fsys.ReadFile(s.Path)
		if err != nil {
			return a, err
		}
		edit, inverse := DisableFstabLine, KindFstabEnable
		if s.Kind == KindFstabEnable {
			edit, inverse = EnableFstabLine, KindFstabDisable
		}
		updated, err := edit(string(content), s.Line)
		if err != nil {
			return a, err
		}
		mode := fs.FileMode(0o644)
		if st, err := fsys.Stat(s.Path); err == nil {
			mode = st.Mode().Perm()
		}
		if a.Backup, err = r.backup(s.Path, content); err != nil {
			return a, err
		}
		if err := fsys.WriteFileAtomic(s.Path, []byte(updated), mode); err != nil {
			return a, err
		}
		a.Undo = withReload(Step{Kind: inverse, Path: s.Path, Line: s.Line, Reload: s.Reload})

	case KindSystemctl:
		if err := r.run(ctx, "systemctl", s.Args...); err != nil {
			return a, err
		}
		if len(s.Args) >= 2 {
			switch s.Args[0] {
			case "enable":
				a.Undo = []Step{{Kind: KindSystemctl, Args: append([]string{"disable"}, s.Args[1:]...)}}
			case "disable":
				a.Undo = []Step{{Kind: KindSystemctl, Args: append([]string{"enable"}, s.Args[1:]...)}}
			}
		}

	case KindMkdir:
		if err := fsys.Mkdir(s.Path, 0o700); err != nil {
			return a, err
		}
		a.Undo = []Step{{Kind: KindRmdir, Path: s.Path}}

	case KindRmdir:
		if err := fsys.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return a, err
		}
		a.Undo = []Step{{Kind: KindMkdir, Path: s.Path}}

	case KindChattrNoCOW:
		if err := r.run(ctx, "chattr", "+C", s.Path); err != nil {
			return a, err
		}

	case KindBtrfsSubvolCreate:
		if err := r.run(ctx, "btrfs", "subvolume", "create", s.Path); err != nil {
			return a, err
		}
		a.Undo = []Step{{Kind: KindBtrfsSubvolDelete, Path: s.Path}}

	case KindBtrfsSubvolDelete:
		if err := r.run(ctx, "btrfs", "subvolume", "delete", s.Path); err != nil {
			return a, err
		}
		a.Undo = []Step{{Kind: KindBtrfsSubvolCreate, Path: s.Path}, {Kind: KindChattrNoCOW, Path: s.Path}}

	default:
		return a, fmt.Errorf("неизвестный шаг %q", s.Kind)
	}
	return a, nil
}

// backup сохраняет копию файла перед изменением в BackupDir.
func (r *Runner) backup(path string, content []byte) (string, error) {
	if err := r.Sys.FS.MkdirAll(BackupDir, 0o755); err != nil {
		return "", err
	}
	name := strings.ReplaceAll(strings.Trim(path, "/"), "/", "_")
	dst := filepath.Join(BackupDir, r.OpID+"-"+name)
	if err := r.Sys.FS.WriteFileAtomic(dst, content, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}
