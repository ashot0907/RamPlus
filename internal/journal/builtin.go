package journal

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"github.com/ashot0907/ramplus/internal/i18n"
	"github.com/ashot0907/ramplus/internal/plan"
)

// Marker — маркер файлов, созданных RamPlus.
const Marker = "# Managed by RamPlus"

// ErrNotOurs — файл не совпадает с записью журнала: удалять его нельзя.
var ErrNotOurs = i18n.Errorf("journal.not_ours")

// CheckConfig — перед удалением конфига: содержимое как записано RamPlus
// (если известно), иначе в файле должен быть маркер RamPlus.
func CheckConfig(s plan.Step, current string) error {
	switch {
	case s.Content != "" && current == s.Content:
		return nil
	case s.Content != "":
		return i18n.Errorf("journal.config_changed", s.Path, ErrNotOurs)
	case strings.Contains(current, Marker):
		return nil
	default:
		return i18n.Errorf("journal.config_no_marker", s.Path, ErrNotOurs)
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
		Verify: func(e *Env, a Applied) (Verdict, i18n.Text) {
			s := a.Step
			cur, ok := readFile(e, s.Path)
			prev, hadPrev := PrevContent(a)
			switch {
			case ok && cur == s.Content:
				return VerdictPresent, i18n.Msg("verify.written_matches", s.Path)
			case ok && hadPrev && cur == prev, !ok && !hadPrev:
				return VerdictAbsent, i18n.Msg("verify.unchanged", s.Path)
			default:
				return VerdictMismatch, i18n.Msg("verify.written_mismatch", s.Path)
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
		Verify: func(e *Env, a Applied) (Verdict, i18n.Text) {
			s := a.Step
			cur, ok := readFile(e, s.Path)
			prev, _ := PrevContent(a)
			switch {
			case !ok:
				return VerdictPresent, i18n.Msg("verify.removed", s.Path)
			case cur == prev:
				return VerdictAbsent, i18n.Msg("verify.in_place", s.Path)
			default:
				return VerdictMismatch, i18n.Msg("verify.changed_after", s.Path)
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
		Verify: func(_ *Env, a Applied) (Verdict, i18n.Text) {
			if len(a.Undo) > 0 {
				return VerdictPresent, i18n.Msg("verify.systemctl_safe", "systemctl "+strings.Join(a.Step.Args, " "))
			}
			return VerdictAbsent, i18n.Msg("verify.nothing")
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
func VerifyCreated(e *Env, a Applied) (Verdict, i18n.Text) {
	if _, err := e.Sys.FS.Stat(a.Step.Path); err == nil {
		return VerdictPresent, i18n.Msg("verify.exists", a.Step.Path)
	}
	return VerdictAbsent, i18n.Msg("verify.not_created", a.Step.Path)
}

// VerifyRemoved — прерванный шаг, удаляющий Path.
func VerifyRemoved(e *Env, a Applied) (Verdict, i18n.Text) {
	if _, err := e.Sys.FS.Stat(a.Step.Path); err == nil {
		return VerdictAbsent, i18n.Msg("verify.in_place", a.Step.Path)
	}
	return VerdictPresent, i18n.Msg("verify.removed", a.Step.Path)
}
