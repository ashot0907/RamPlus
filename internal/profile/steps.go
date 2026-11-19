package profile

import (
	"context"
	"fmt"
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
	plan.RegisterAction(ActionProfileApply, plan.ActionInfo{Domain: plan.DomainProfile, Title: "применение профиля"})
	plan.RegisterAction(ActionProfileReset, plan.ActionInfo{Domain: plan.DomainProfile, Title: "сброс профиля"})
	journal.Register(KindSetValue, journal.Handler{
		Describe: func(s plan.Step) string {
			if k := KeyForPath(s.Path); k != "" {
				return fmt.Sprintf("%s = %s (запись в %s)", k, s.Content, s.Path)
			}
			return fmt.Sprintf("записать %q в %s", s.Content, s.Path)
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
			return nil
		},
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, string) {
			s := a.Step
			b, err := e.Sys.FS.ReadFile(s.Path)
			prev, _ := prevValue(a)
			cur := strings.TrimSpace(string(b))
			switch {
			case err != nil:
				return journal.VerdictMismatch, s.Path + " недоступен"
			case cur == s.Content:
				return journal.VerdictPresent, fmt.Sprintf("%s = %s, как в журнале", s.Path, s.Content)
			case cur == prev:
				return journal.VerdictAbsent, s.Path + " не изменён"
			default:
				return journal.VerdictMismatch, fmt.Sprintf("%s = %s: ни прежнее (%s), ни записанное (%s)", s.Path, cur, prev, s.Content)
			}
		},
		Expect: func(s plan.Step) []journal.Expectation {
			path, want := s.Path, s.Content
			return []journal.Expectation{{Key: "value:" + path, Path: path, Check: func(sys system.System) string {
				st, err := sys.FS.Stat(path)
				if err != nil || !st.Mode().IsRegular() || st.Size() > journal.ConfigMax {
					return ""
				}
				b, err := sys.FS.ReadFile(path)
				if err != nil {
					return ""
				}
				if cur := strings.TrimSpace(string(b)); cur != want {
					name := path
					if k := KeyForPath(path); k != "" {
						name = k
					}
					return fmt.Sprintf("%s = %s, по журналу %s", name, cur, want)
				}
				return ""
			}}}
		},
	})
}
