package journal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/ashot0907/ramplus/internal/plan"
)

// Marker — маркер файлов, созданных RamPlus.
const Marker = "# Managed by RamPlus"

// ErrNotOurs — файл не совпадает с записью журнала: удалять его нельзя.
var ErrNotOurs = errors.New("файл не совпадает с журналом RamPlus — удаление отменено")

// CheckConfig — перед удалением конфига: содержимое как записано RamPlus
// (если известно), иначе в файле должен быть маркер RamPlus.
func CheckConfig(s plan.Step, current string) error {
	switch {
	case s.Content != "" && current == s.Content:
		return nil
	case s.Content != "":
		return fmt.Errorf("%s: изменён после записи RamPlus: %w", s.Path, ErrNotOurs)
	case strings.Contains(current, Marker):
		return nil
	default:
		return fmt.Errorf("%s: нет маркера RamPlus: %w", s.Path, ErrNotOurs)
	}
}

// ReadState — содержимое файла для проверок прерванного шага.
func readFile(e *Env, path string) (string, bool) {
	b, err := e.Sys.FS.ReadFile(path)
	return string(b), err == nil
}

// PrevContent — содержимое файла до шага (из записи отмены).
func PrevContent(a Applied) (string, bool) {
	for _, u := range a.Undo {
		if u.Kind == plan.KindWriteFile {
			return u.Content, true
		}
	}
	return "", false
}

func init() {
	Register(plan.KindWriteFile, Handler{
		Prepare: func(e *Env, s plan.Step) ([]plan.Step, string, error) {
			prev, err := e.Sys.FS.ReadFile(s.Path)
			switch {
			case err == nil:
				backup, err := e.Backup(s.Path, prev)
				if err != nil {
					return nil, "", err
				}
				return []plan.Step{{Kind: plan.KindWriteFile, Path: s.Path, Content: string(prev), Mode: uint32(e.Perm(s.Path, 0o644)), Reload: s.Reload}}, backup, nil
			case errors.Is(err, fs.ErrNotExist):
				// Content — что именно мы записали: удалим, только если файл не изменился.
				return []plan.Step{{Kind: plan.KindDeleteConfig, Path: s.Path, Content: s.Content, Reload: s.Reload}}, "", nil
			default:
				return nil, "", err
			}
		},
		Do: func(_ context.Context, e *Env, s plan.Step) error {
			mode := fs.FileMode(s.Mode)
			if mode == 0 {
				mode = 0o644
			}
			return e.Sys.FS.WriteFileAtomic(s.Path, []byte(s.Content), mode)
		},
		Verify: func(e *Env, a Applied) (Verdict, string) {
			s := a.Step
			cur, ok := readFile(e, s.Path)
			prev, hadPrev := PrevContent(a)
			switch {
			case ok && cur == s.Content:
				return VerdictPresent, s.Path + " совпадает с содержимым из журнала RamPlus"
			case ok && hadPrev && cur == prev, !ok && !hadPrev:
				return VerdictAbsent, s.Path + " не изменён"
			default:
				return VerdictMismatch, s.Path + " не совпадает ни с прежним, ни с записанным содержимым"
			}
		},
		Expect: func(s plan.Step) []Expectation {
			c := s.Content
			return []Expectation{FileExpect(s.Path, true, false, 0, &c)}
		},
	})

	Register(plan.KindDeleteConfig, Handler{
		Prepare: func(e *Env, s plan.Step) ([]plan.Step, string, error) {
			prev, err := e.Sys.FS.ReadFile(s.Path)
			if err != nil {
				return nil, "", err
			}
			if err := CheckConfig(s, string(prev)); err != nil {
				return nil, "", err
			}
			backup, err := e.Backup(s.Path, prev)
			if err != nil {
				return nil, "", err
			}
			return []plan.Step{{Kind: plan.KindWriteFile, Path: s.Path, Content: string(prev), Mode: uint32(e.Perm(s.Path, 0o644)), Reload: s.Reload}}, backup, nil
		},
		Do: func(_ context.Context, e *Env, s plan.Step) error { return e.Sys.FS.Remove(s.Path) },
		Verify: func(e *Env, a Applied) (Verdict, string) {
			s := a.Step
			cur, ok := readFile(e, s.Path)
			prev, _ := PrevContent(a)
			switch {
			case !ok:
				return VerdictPresent, s.Path + " удалён"
			case cur == prev:
				return VerdictAbsent, s.Path + " на месте"
			default:
				return VerdictMismatch, s.Path + " изменён после записи в журнал"
			}
		},
		Expect: func(s plan.Step) []Expectation { return []Expectation{FileExpect(s.Path, false, false, 0, nil)} },
	})

	Register(plan.KindSystemctl, Handler{
		Prepare: func(_ *Env, s plan.Step) ([]plan.Step, string, error) {
			if len(s.Args) >= 2 {
				switch s.Args[0] {
				case "enable":
					return []plan.Step{{Kind: plan.KindSystemctl, Args: append([]string{"disable"}, s.Args[1:]...)}}, "", nil
				case "disable":
					return []plan.Step{{Kind: plan.KindSystemctl, Args: append([]string{"enable"}, s.Args[1:]...)}}, "", nil
				}
			}
			return nil, "", nil
		},
		Do: func(ctx context.Context, e *Env, s plan.Step) error { return e.Run(ctx, "systemctl", s.Args...) },
		Verify: func(_ *Env, a Applied) (Verdict, string) {
			if len(a.Undo) > 0 {
				return VerdictPresent, "systemctl " + strings.Join(a.Step.Args, " ") + " — отмена безопасна при любом исходе"
			}
			return VerdictAbsent, "отменять нечего"
		},
	})

	Register(plan.KindMkdir, Handler{
		Prepare: func(_ *Env, s plan.Step) ([]plan.Step, string, error) {
			return []plan.Step{{Kind: plan.KindRmdir, Path: s.Path}}, "", nil
		},
		Do:     func(_ context.Context, e *Env, s plan.Step) error { return e.Sys.FS.Mkdir(s.Path, 0o700) },
		Verify: VerifyCreated,
		Expect: func(s plan.Step) []Expectation { return []Expectation{FileExpect(s.Path, true, true, 0, nil)} },
	})

	Register(plan.KindRmdir, Handler{
		Prepare: func(_ *Env, s plan.Step) ([]plan.Step, string, error) {
			return []plan.Step{{Kind: plan.KindMkdir, Path: s.Path}}, "", nil
		},
		Do: func(_ context.Context, e *Env, s plan.Step) error {
			if err := e.Sys.FS.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		},
		Verify: VerifyRemoved,
		Expect: func(s plan.Step) []Expectation { return []Expectation{FileExpect(s.Path, false, false, 0, nil)} },
	})

	Register(plan.KindChmod, Handler{
		Do: func(_ context.Context, e *Env, s plan.Step) error {
			return e.Sys.FS.Chmod(s.Path, fs.FileMode(s.Mode))
		},
	})
}

// VerifyCreated — прерванный шаг, создающий Path.
func VerifyCreated(e *Env, a Applied) (Verdict, string) {
	if _, err := e.Sys.FS.Stat(a.Step.Path); err == nil {
		return VerdictPresent, a.Step.Path + " существует"
	}
	return VerdictAbsent, a.Step.Path + " не создан"
}

// VerifyRemoved — прерванный шаг, удаляющий Path.
func VerifyRemoved(e *Env, a Applied) (Verdict, string) {
	if _, err := e.Sys.FS.Stat(a.Step.Path); err == nil {
		return VerdictAbsent, a.Step.Path + " на месте"
	}
	return VerdictPresent, a.Step.Path + " удалён"
}
