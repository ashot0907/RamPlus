package profile

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"context"
	"strings"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// KindSetValue — записать runtime-значение (Content) в Path
// (/proc/sys/vm/*, /sys/module/zswap/parameters/*); отмена — прежнее.
const KindSetValue plan.StepKind = "set_value"

func prevValue(a journal.Applied) (string, bool) {
	for _, u := range a.Undo {
		if u.Kind == KindSetValue {
			return u.Content, true
		}
	}
	return "", false
}

func init() {
	plan.RegisterAction(ActionProfileApply, plan.ActionInfo{Domain: plan.DomainProfile})
	plan.RegisterAction(ActionProfileReset, plan.ActionInfo{Domain: plan.DomainProfile})
	journal.Register(KindSetValue, journal.Handler{
		Describe: func(s plan.Step) i18n.Text {
			if k := KeyForPath(s.Path); k != "" {
				return i18n.Msg("step.set_value", k, s.Content, s.Path)
			}
			return i18n.Msg("step.set_value_raw", s.Content, s.Path)
		},
		Prepare: func(e *journal.Env, s plan.Step) ([]plan.Step, string, error) {
			prev, err := e.Sys.FS.ReadFile(s.Path)
			if err != nil {
				return nil, "", err
			}
			return []plan.Step{{Kind: KindSetValue, Path: s.Path, Content: strings.TrimSpace(string(prev))}}, "", nil
		},
		Do: func(_ context.Context, e *journal.Env, s plan.Step) error {
			fsys := e.Sys.FS
			if err := fsys.WriteValue(s.Path, s.Content); err != nil {
				return i18n.Errorf("profile.err.write", s.Content, s.Path, err)
			}
			// Ядро может принять запись, но оставить другое значение.
			b, err := fsys.ReadFile(s.Path)
			if err != nil {
				return err
			}
			if got := strings.TrimSpace(string(b)); got != s.Content {
				return i18n.Errorf("profile.err.kernel_shows", s.Path, s.Content, got)
			}
			return nil
		},
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, i18n.Text) {
			s := a.Step
			b, err := e.Sys.FS.ReadFile(s.Path)
			prev, _ := prevValue(a)
			cur := strings.TrimSpace(string(b))
			switch {
			case err != nil:
				return journal.VerdictMismatch, i18n.Msg("verify.unavailable", s.Path)
			case cur == s.Content:
				return journal.VerdictPresent, i18n.Msg("verify.value_matches", s.Path, s.Content)
			case cur == prev:
				return journal.VerdictAbsent, i18n.Msg("verify.unchanged", s.Path)
			default:
				return journal.VerdictMismatch, i18n.Msg("verify.value_mismatch", s.Path, cur, prev, s.Content)
			}
		},
		Expect: func(s plan.Step) []journal.Expectation {
			path, want := s.Path, s.Content
			return []journal.Expectation{{Key: "value:" + path, Path: path, Check: func(sys system.System) i18n.Text {
				st, err := sys.FS.Stat(path)
				if err != nil || !st.Mode().IsRegular() || st.Size() > journal.ConfigMax {
					return nil
				}
				b, err := sys.FS.ReadFile(path)
				if err != nil {
					return nil
				}
				if cur := strings.TrimSpace(string(b)); cur != want {
					name := path
					if k := KeyForPath(path); k != "" {
						name = k
					}
					return i18n.Msg("expect.value", name, cur, want)
				}
				return nil
			}}}
		},
	})
}
