package swap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/profile"
)

// legacyConfigMax — remove_file из журнала v1: файлы до этого размера
// считаются конфигами (юнит), больше — swapfile.
const legacyConfigMax = 1 << 20

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

// Runner выполняет шаги по одному с write-ahead журналом:
//
//	prepare (вычислить отмену, сохранить бэкап) → журнал pending →
//	выполнить → журнал done.
//
// Если шаг падает, уже выполненные шаги этого запуска откатываются в
// обратном порядке.
type Runner struct {
	Sys  System
	OpID string
	// Journal вызывается с полным списком записей (done + последняя
	// pending). Ошибка журнала до шага — шаг не выполняется; после шага —
	// шаг откатывается (без журнала нельзя гарантировать rollback).
	Journal  func(entries []Applied) error
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

func (r *Runner) journal(entries []Applied) error {
	if r.Journal == nil {
		return nil
	}
	if err := r.Journal(entries); err != nil {
		return fmt.Errorf("запись журнала %s: %w", StatePath, err)
	}
	return nil
}

// Run выполняет steps. При ошибке возвращает *StepError и выполненные
// (уже откаченные) шаги.
func (r *Runner) Run(ctx context.Context, steps []Step) ([]Applied, error) {
	var done []Applied
	for i, s := range steps {
		fail := func(err error, undoAlso *Applied) ([]Applied, error) {
			r.progress(Event{Index: i + 1, Total: len(steps), Step: s, Err: err})
			toUndo := append([]Applied(nil), done...)
			if undoAlso != nil {
				toUndo = append(toUndo, *undoAlso)
			}
			se := &StepError{Index: i + 1, Total: len(steps), Step: s, Err: err}
			se.RollbackErrors = r.undo(ctx, toUndo)
			return toUndo, se
		}

		a, err := r.prepare(s)
		if err != nil {
			return fail(err, nil)
		}
		a.Status = StepPending
		if err := r.journal(append(append([]Applied(nil), done...), a)); err != nil {
			return fail(err, nil)
		}
		if err := r.do(ctx, s); err != nil {
			return fail(err, nil)
		}
		a.Status = StepDone
		if err := r.journal(append(append([]Applied(nil), done...), a)); err != nil {
			return fail(err, &a)
		}
		done = append(done, a)
		r.progress(Event{Index: i + 1, Total: len(steps), Step: s})
	}
	return done, nil
}

// undo откатывает записи в обратном порядке, продолжая после ошибок.
// После отката каждой записи журнал укорачивается, чтобы state.json не
// содержал уже отменённых шагов.
func (r *Runner) undo(ctx context.Context, entries []Applied) []error {
	var errs []error
	total := len(UndoSteps(entries))
	n := 0
	for i := len(entries) - 1; i >= 0; i-- {
		ok := true
		for _, s := range entries[i].Undo {
			n++
			err := r.exec(ctx, s)
			r.progress(Event{Undo: true, Index: n, Total: total, Step: s, Err: err})
			if err != nil {
				ok = false
				errs = append(errs, fmt.Errorf("%s: %w", s.Describe(), err))
			}
		}
		if ok && r.Journal != nil {
			// Ошибка журнала здесь не делает откат неполным: система уже
			// восстановлена, итоговый статус запишет вызывающий.
			_ = r.Journal(entries[:i])
		}
	}
	return errs
}

// exec — prepare + do без журнала (для шагов отката).
func (r *Runner) exec(ctx context.Context, s Step) error {
	if _, err := r.prepare(s); err != nil {
		return err
	}
	return r.do(ctx, s)
}

func (r *Runner) run(ctx context.Context, name string, args ...string) error {
	_, err := r.Sys.Exec.Run(ctx, name, args...)
	return err
}

func (r *Runner) perm(path string, def fs.FileMode) fs.FileMode {
	if st, err := r.Sys.FS.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return def
}

// prepare читает состояние до шага и вычисляет его отмену. Систему не
// меняет (кроме копии файла в BackupDir).
func (r *Runner) prepare(s Step) (Applied, error) {
	fsys := r.Sys.FS
	a := Applied{Step: s}
	withReload := func(undo ...Step) []Step {
		if s.Reload {
			undo = append(undo, daemonReload())
		}
		return undo
	}
	switch s.Kind {
	case KindAllocate, KindBtrfsMkswapfile:
		if _, err := fsys.Stat(s.Path); err == nil {
			return a, &fs.PathError{Op: "create", Path: s.Path, Err: fs.ErrExist} // чужой файл не трогаем
		}
		a.Undo = []Step{{Kind: KindDeleteSwapfile, Path: s.Path, Size: s.Size, NoCOW: s.NoCOW}}
	case KindSwapon:
		a.Undo = []Step{{Kind: KindSwapoff, Path: s.Path, Priority: s.Priority, Size: s.Size}}
	case KindSwapoff:
		a.Undo = []Step{{Kind: KindSwapon, Path: s.Path, Priority: s.Priority, Size: s.Size}}
	case KindDeleteSwapfile:
		st, err := r.checkSwapfile(s)
		if err != nil {
			return a, err
		}
		// Содержимое swap после swapoff не нужно — пустой swapfile того же
		// размера эквивалентен исходному.
		size := uint64(st.Size())
		a.Undo = []Step{
			{Kind: KindAllocate, Path: s.Path, Size: size, NoCOW: s.NoCOW},
			{Kind: KindChmod, Path: s.Path, Mode: 0o600},
			{Kind: KindMkswap, Path: s.Path},
		}
	case KindWriteFile:
		prev, err := fsys.ReadFile(s.Path)
		switch {
		case err == nil:
			if a.Backup, err = r.backup(s.Path, prev); err != nil {
				return a, err
			}
			a.Undo = withReload(Step{Kind: KindWriteFile, Path: s.Path, Content: string(prev), Mode: uint32(r.perm(s.Path, 0o644)), Reload: s.Reload})
		case errors.Is(err, fs.ErrNotExist):
			// Content — что именно мы записали: удалим, только если файл не изменился.
			a.Undo = withReload(Step{Kind: KindDeleteConfig, Path: s.Path, Content: s.Content, Reload: s.Reload})
		default:
			return a, err
		}
	case KindDeleteConfig:
		prev, err := fsys.ReadFile(s.Path)
		if err != nil {
			return a, err
		}
		if err := checkConfig(s, string(prev)); err != nil {
			return a, err
		}
		if a.Backup, err = r.backup(s.Path, prev); err != nil {
			return a, err
		}
		a.Undo = withReload(Step{Kind: KindWriteFile, Path: s.Path, Content: string(prev), Mode: uint32(r.perm(s.Path, 0o644)), Reload: s.Reload})
	case KindFstabDisable, KindFstabEnable:
		content, err := fsys.ReadFile(s.Path)
		if err != nil {
			return a, err
		}
		if _, err := fstabEdit(s)(string(content), s.Line); err != nil {
			return a, err
		}
		if a.Backup, err = r.backup(s.Path, content); err != nil {
			return a, err
		}
		inverse := KindFstabEnable
		if s.Kind == KindFstabEnable {
			inverse = KindFstabDisable
		}
		a.Undo = withReload(Step{Kind: inverse, Path: s.Path, Line: s.Line, Reload: s.Reload})
	case KindSystemctl:
		if len(s.Args) >= 2 {
			switch s.Args[0] {
			case "enable":
				a.Undo = []Step{{Kind: KindSystemctl, Args: append([]string{"disable"}, s.Args[1:]...)}}
			case "disable":
				a.Undo = []Step{{Kind: KindSystemctl, Args: append([]string{"enable"}, s.Args[1:]...)}}
			}
		}
	case KindMkdir:
		a.Undo = []Step{{Kind: KindRmdir, Path: s.Path}}
	case KindRmdir:
		a.Undo = []Step{{Kind: KindMkdir, Path: s.Path}}
	case KindBtrfsSubvolCreate:
		a.Undo = []Step{{Kind: KindBtrfsSubvolDelete, Path: s.Path}}
	case KindBtrfsSubvolDelete:
		// btrfs subvolume delete удаляет вместе с содержимым — только пустой.
		if rest, err := fsys.Glob(filepath.Join(s.Path, "*")); err != nil {
			return a, err
		} else if len(rest) > 0 {
			return a, fmt.Errorf("%s не пуст (%s) — подтом не удаляется", s.Path, strings.Join(rest, ", "))
		}
		a.Undo = []Step{{Kind: KindBtrfsSubvolCreate, Path: s.Path}, {Kind: KindChattrNoCOW, Path: s.Path}}
	case KindRemoveFile: // журнал v1
		st, err := fsys.Stat(s.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// уже удалён — шаг ничего не сделает
		case err != nil:
			return a, err
		case st.Size() > legacyConfigMax:
			if _, err := r.checkSwapfile(s); err != nil {
				return a, err
			}
			size := uint64(st.Size())
			a.Undo = []Step{
				{Kind: KindAllocate, Path: s.Path, Size: size},
				{Kind: KindChmod, Path: s.Path, Mode: 0o600},
				{Kind: KindMkswap, Path: s.Path},
			}
		default:
			prev, err := fsys.ReadFile(s.Path)
			if err != nil {
				return a, err
			}
			if err := checkConfig(s, string(prev)); err != nil {
				return a, err
			}
			if a.Backup, err = r.backup(s.Path, prev); err != nil {
				return a, err
			}
			a.Undo = []Step{{Kind: KindWriteFile, Path: s.Path, Content: string(prev), Mode: uint32(st.Mode().Perm())}}
		}
	case KindSetValue:
		prev, err := fsys.ReadFile(s.Path)
		if err != nil {
			return a, err
		}
		a.Undo = []Step{{Kind: KindSetValue, Path: s.Path, Content: strings.TrimSpace(string(prev))}}
	case KindChmod, KindMkswap, KindChattrNoCOW:
		// отменять нечего
	default:
		return a, fmt.Errorf("неизвестный шаг %q", s.Kind)
	}
	return a, nil
}

// ErrNotOurs — файл не совпадает с записью журнала: удалять его нельзя.
var ErrNotOurs = errors.New("файл не совпадает с журналом RamPlus — удаление отменено")

// checkSwapfile — перед удалением swapfile: обычный файл, размер как в
// журнале, в заголовке сигнатура swap или нули (файл ещё до mkswap).
func (r *Runner) checkSwapfile(s Step) (fs.FileInfo, error) {
	st, err := r.Sys.FS.Stat(s.Path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: не обычный файл: %w", s.Path, ErrNotOurs)
	}
	size := uint64(st.Size())
	switch {
	case s.Size == 0: // размер не записан (журнал v1, старый swap без наблюдения)
	case s.SizeAtMost && size <= s.Size:
	case size != s.Size:
		return nil, fmt.Errorf("%s: размер %s, по журналу %s: %w", s.Path, FormatSize(size), FormatSize(s.Size), ErrNotOurs)
	}
	head, err := r.Sys.FS.ReadPrefix(s.Path, os.Getpagesize())
	if err != nil {
		return nil, err
	}
	if !IsSwapHeader(head) && !allZero(head) {
		return nil, fmt.Errorf("%s: нет сигнатуры swap (SWAPSPACE2) — это не swapfile: %w", s.Path, ErrNotOurs)
	}
	return st, nil
}

// IsSwapHeader — первая страница файла содержит сигнатуру mkswap.
func IsSwapHeader(page []byte) bool {
	if len(page) < 10 {
		return false
	}
	sig := string(page[len(page)-10:])
	return sig == "SWAPSPACE2" || sig == "SWAP-SPACE"
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// checkConfig — перед удалением конфига: содержимое как записано RamPlus
// (если известно), иначе в файле должен быть маркер RamPlus.
func checkConfig(s Step, current string) error {
	switch {
	case s.Content != "" && current == s.Content:
		return nil
	case s.Content != "":
		return fmt.Errorf("%s: изменён после записи RamPlus: %w", s.Path, ErrNotOurs)
	case strings.Contains(current, profile.Marker):
		return nil
	default:
		return fmt.Errorf("%s: нет маркера RamPlus: %w", s.Path, ErrNotOurs)
	}
}

func fstabEdit(s Step) func(content, line string) (string, error) {
	if s.Kind == KindFstabEnable {
		return EnableFstabLine
	}
	return DisableFstabLine
}

// do выполняет шаг.
func (r *Runner) do(ctx context.Context, s Step) error {
	fsys := r.Sys.FS
	switch s.Kind {
	case KindAllocate:
		if err := fsys.Create(s.Path, 0o600); err != nil {
			return err // в т.ч. fs.ErrExist: чужой файл не трогаем
		}
		err := func() error {
			if s.NoCOW {
				// +C можно поставить только пустому файлу.
				if err := r.run(ctx, "chattr", "+C", s.Path); err != nil {
					return err
				}
			}
			err := fsys.Fallocate(s.Path, s.Size)
			if errors.Is(err, ErrFallocateUnsupported) {
				err = fsys.WriteZeros(s.Path, s.Size)
			}
			return err
		}()
		if err != nil {
			_ = fsys.Remove(s.Path) // файл создан нами — убираем недописанный
		}
		return err

	case KindBtrfsMkswapfile:
		err := r.run(ctx, "btrfs", "filesystem", "mkswapfile", "--size", strconv.FormatUint(s.Size, 10), s.Path)
		if err != nil {
			if _, serr := fsys.Stat(s.Path); serr == nil {
				_ = fsys.Remove(s.Path)
			}
		}
		return err

	case KindChmod:
		return fsys.Chmod(s.Path, fs.FileMode(s.Mode))
	case KindSetValue:
		if err := fsys.WriteValue(s.Path, s.Content); err != nil {
			return fmt.Errorf("запись %s в %s: %w", s.Content, s.Path, err)
		}
		// Ядро может принять запись, но оставить другое значение.
		b, err := fsys.ReadFile(s.Path)
		if err != nil {
			return err
		}
		if got := strings.TrimSpace(string(b)); got != s.Content {
			return fmt.Errorf("%s: записано %q, а ядро показывает %q", s.Path, s.Content, got)
		}
	case KindMkswap:
		return r.run(ctx, "mkswap", s.Path)
	case KindSwapon:
		if err := r.Sys.Kernel.Swapon(s.Path, s.Priority); err != nil {
			return fmt.Errorf("swapon %s: %w", s.Path, err)
		}
	case KindSwapoff:
		if err := r.Sys.Kernel.Swapoff(s.Path); err != nil {
			return fmt.Errorf("swapoff %s: %w", s.Path, err)
		}
	case KindDeleteSwapfile, KindDeleteConfig:
		return fsys.Remove(s.Path)
	case KindRemoveFile:
		if err := fsys.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	case KindWriteFile:
		mode := fs.FileMode(s.Mode)
		if mode == 0 {
			mode = 0o644
		}
		return fsys.WriteFileAtomic(s.Path, []byte(s.Content), mode)
	case KindFstabDisable, KindFstabEnable:
		content, err := fsys.ReadFile(s.Path)
		if err != nil {
			return err
		}
		updated, err := fstabEdit(s)(string(content), s.Line)
		if err != nil {
			return err
		}
		return fsys.WriteFileAtomic(s.Path, []byte(updated), r.perm(s.Path, 0o644))
	case KindSystemctl:
		return r.run(ctx, "systemctl", s.Args...)
	case KindMkdir:
		return fsys.Mkdir(s.Path, 0o700)
	case KindRmdir:
		if err := fsys.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	case KindChattrNoCOW:
		return r.run(ctx, "chattr", "+C", s.Path)
	case KindBtrfsSubvolCreate:
		return r.run(ctx, "btrfs", "subvolume", "create", s.Path)
	case KindBtrfsSubvolDelete:
		return r.run(ctx, "btrfs", "subvolume", "delete", s.Path)
	default:
		return fmt.Errorf("неизвестный шаг %q", s.Kind)
	}
	return nil
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

// Verdict — что осталось в системе от шага, прерванного на середине.
type Verdict string

const (
	VerdictAbsent   Verdict = "absent"   // шаг не успел изменить систему — откатывать нечего
	VerdictPresent  Verdict = "present"  // артефакт есть и совпадает с записью — откатить
	VerdictMismatch Verdict = "mismatch" // что-то есть, но не то, что записано — нужна ручная проверка
)

// VerifyPending проверяет артефакт записи со статусом pending (процесс
// упал между записью pending и done). Только чтение.
func VerifyPending(sys System, a Applied) (Verdict, string) {
	s := a.Step
	fsys := sys.FS
	stat := func() (fs.FileInfo, bool) {
		st, err := fsys.Stat(s.Path)
		return st, err == nil
	}
	activeSwap := func() bool {
		b, err := fsys.ReadFile(SwapsPath)
		if err != nil {
			return false
		}
		devs, _ := monitor.ParseSwaps(bytes.NewReader(b))
		for _, d := range devs {
			if d.Path == s.Path {
				return true
			}
		}
		return false
	}
	readFile := func() (string, bool) {
		b, err := fsys.ReadFile(s.Path)
		return string(b), err == nil
	}
	prevContent := func() (string, bool) { // содержимое до шага, из записи отмены
		for _, u := range a.Undo {
			if u.Kind == KindWriteFile {
				return u.Content, true
			}
		}
		return "", false
	}

	switch s.Kind {
	case KindSetValue:
		cur, ok := readFile()
		prev, _ := prevValue(a)
		switch {
		case !ok:
			return VerdictMismatch, s.Path + " недоступен"
		case strings.TrimSpace(cur) == s.Content:
			return VerdictPresent, fmt.Sprintf("%s = %s, как в журнале", s.Path, s.Content)
		case strings.TrimSpace(cur) == prev:
			return VerdictAbsent, s.Path + " не изменён"
		default:
			return VerdictMismatch, fmt.Sprintf("%s = %s: ни прежнее (%s), ни записанное (%s)", s.Path, strings.TrimSpace(cur), prev, s.Content)
		}
	case KindAllocate, KindBtrfsMkswapfile:
		st, ok := stat()
		switch {
		case !ok:
			return VerdictAbsent, s.Path + " не создан"
		case st.Mode().IsRegular() && st.Mode().Perm() == 0o600 && uint64(st.Size()) <= s.Size:
			return VerdictPresent, fmt.Sprintf("%s: путь, права 600 и размер (%s ≤ %s) совпадают с записью",
				s.Path, FormatSize(uint64(st.Size())), FormatSize(s.Size))
		default:
			return VerdictMismatch, fmt.Sprintf("%s существует, но не похож на файл RamPlus (права %o, размер %s, ожидалось ≤ %s)",
				s.Path, st.Mode().Perm(), FormatSize(uint64(st.Size())), FormatSize(s.Size))
		}
	case KindSwapon:
		if activeSwap() {
			return VerdictPresent, s.Path + " активен"
		}
		return VerdictAbsent, s.Path + " не активен"
	case KindSwapoff:
		if activeSwap() {
			return VerdictAbsent, s.Path + " всё ещё активен"
		}
		return VerdictPresent, s.Path + " отключён"
	case KindDeleteSwapfile:
		st, ok := stat()
		switch {
		case !ok:
			return VerdictPresent, s.Path + " удалён"
		case s.Size == 0 || uint64(st.Size()) == s.Size:
			return VerdictAbsent, s.Path + " на месте"
		default:
			return VerdictMismatch, fmt.Sprintf("%s: размер %s, в записи %s", s.Path, FormatSize(uint64(st.Size())), FormatSize(s.Size))
		}
	case KindWriteFile:
		cur, ok := readFile()
		prev, hadPrev := prevContent()
		switch {
		case ok && cur == s.Content:
			return VerdictPresent, s.Path + " совпадает с содержимым из журнала RamPlus"
		case ok && hadPrev && cur == prev, !ok && !hadPrev:
			return VerdictAbsent, s.Path + " не изменён"
		default:
			return VerdictMismatch, s.Path + " не совпадает ни с прежним, ни с записанным содержимым"
		}
	case KindDeleteConfig:
		cur, ok := readFile()
		prev, _ := prevContent()
		switch {
		case !ok:
			return VerdictPresent, s.Path + " удалён"
		case cur == prev:
			return VerdictAbsent, s.Path + " на месте"
		default:
			return VerdictMismatch, s.Path + " изменён после записи в журнал"
		}
	case KindFstabDisable, KindFstabEnable:
		cur, ok := readFile()
		if !ok {
			return VerdictMismatch, s.Path + " недоступен"
		}
		marked, plain := hasLine(cur, fstabDisabledBy+s.Line), hasLine(cur, s.Line)
		disabled, enabled := marked && !plain, plain && !marked
		switch {
		case (s.Kind == KindFstabDisable && disabled) || (s.Kind == KindFstabEnable && enabled):
			return VerdictPresent, "строка в " + s.Path + " изменена"
		case (s.Kind == KindFstabDisable && enabled) || (s.Kind == KindFstabEnable && disabled):
			return VerdictAbsent, "строка в " + s.Path + " не изменена"
		default:
			return VerdictMismatch, "строка «" + strings.TrimSpace(s.Line) + "» в " + s.Path + " не в ожидаемом виде"
		}
	case KindSystemctl:
		if len(a.Undo) > 0 {
			return VerdictPresent, "systemctl " + strings.Join(s.Args, " ") + " — отмена безопасна при любом исходе"
		}
		return VerdictAbsent, "отменять нечего"
	case KindMkdir, KindBtrfsSubvolCreate:
		if _, ok := stat(); ok {
			return VerdictPresent, s.Path + " существует"
		}
		return VerdictAbsent, s.Path + " не создан"
	case KindRmdir, KindBtrfsSubvolDelete, KindRemoveFile:
		if _, ok := stat(); ok {
			return VerdictAbsent, s.Path + " на месте"
		}
		return VerdictPresent, s.Path + " удалён"
	default: // chmod, mkswap, chattr — отменять нечего
		return VerdictAbsent, "отменять нечего"
	}
}

func prevValue(a Applied) (string, bool) {
	for _, u := range a.Undo {
		if u.Kind == KindSetValue {
			return u.Content, true
		}
	}
	return "", false
}

func hasLine(content, line string) bool {
	for _, l := range strings.Split(content, "\n") {
		if l == line {
			return true
		}
	}
	return false
}
