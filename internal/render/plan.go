// Package render — текстовый вывод для CLI: план, ход выполнения, статус.
// Строится только из данных (plan.Plan, monitor.Status), без логики.
package render

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
)

// Options — настройки вывода плана.
type Options struct {
	Lang    i18n.Lang // язык вывода
	Verbose bool      // полное содержимое записываемых файлов (systemd-юнит)
}

var levelTag = map[plan.Level]string{
	plan.LevelOK: "[ok]  ", plan.LevelInfo: "[info]", plan.LevelWarn: "[warn]", plan.LevelFail: "[FAIL]",
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// Plan печатает план для человека.
func Plan(w io.Writer, p *plan.Plan, opts Options) error {
	l := opts.Lang
	if p.Action == "setup" {
		return setupPlan(l, w, p, opts)
	}
	if p.Profile != nil {
		return profilePlan(l, w, p, opts)
	}
	return swapPlan(l, w, p, opts)
}

func steps(l i18n.Lang, w io.Writer, p *plan.Plan, opts Options) {
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	fmt.Fprint(w, t(l, "render.plan.steps"))
	if len(p.Steps) == 0 {
		fmt.Fprint(w, t(l, "render.plan.no_steps"))
	}
	for i, s := range p.Steps {
		pr("  %2d. %s\n", i+1, s.Describe(l))
		if opts.Verbose && s.Kind == plan.KindWriteFile {
			pr("      ┌── %s\n", s.Path)
			for _, line := range strings.Split(strings.TrimSuffix(s.Content, "\n"), "\n") {
				pr("      │ %s\n", line)
			}
			pr("      └──\n")
		}
	}
	if !opts.Verbose && p.HasKind(plan.KindWriteFile) {
		fmt.Fprint(w, t(l, "render.plan.verbose_hint"))
	}
}

func checks(l i18n.Lang, w io.Writer, p *plan.Plan) {
	fmt.Fprint(w, t(l, "render.plan.checks"))
	for _, c := range p.Checks {
		fmt.Fprintf(w, "  %s %s: %s\n", levelTag[c.Level], c.Title, c.Message)
	}
}

func swapPlan(l i18n.Lang, w io.Writer, p *plan.Plan, opts Options) error {
	v := p.Swap
	if v == nil {
		v = &plan.SwapView{}
	}
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }

	pr(t(l, "render.plan.title"), v.Title)
	pr(t(l, "render.plan.command_nl"), p.Command)

	swapNow(l, w, v)
	choices(l, w, p)

	checks(l, w, p)
	steps(l, w, p, opts)

	fmt.Fprint(w, t(l, "render.plan.summary"))
	pr("  swap: %s → %s\n", plan.FormatSize(p.SwapBefore), plan.FormatSize(p.SwapAfter))
	list := func(title string, items []string) {
		pr("  %s: %s\n", title, dash(strings.Join(items, ", ")))
	}
	list(t(l, "render.plan.created"), p.Created)
	list(t(l, "render.plan.disabled"), p.Disabled)
	list(t(l, "render.plan.deleted"), p.Deleted)

	pr("\n")
	if fails := p.Failures(); len(fails) > 0 {
		pr(t(l, "render.plan.result_blocked"), len(fails))
		return nil
	}
	_, err := fmt.Fprintf(w, t(l, "render.plan.result_ok"), v.ApplyCommand)
	return err
}

func profilePlan(l i18n.Lang, w io.Writer, p *plan.Plan, opts Options) error {
	info := p.Profile
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	if info.Reset {
		fmt.Fprint(w, t(l, "render.plan.reset_title"))
	} else {
		pr(t(l, "render.plan.profile_title"), info.Name, info.StatusText)
	}
	pr(t(l, "render.plan.command"), p.Command)
	if !info.Reset {
		if info.Title != "" {
			pr(t(l, "render.plan.profile_desc"), info.Title, info.Description)
		}
		if info.Evidence != "" {
			pr("Evidence: %s\n", info.Evidence)
		} else if info.BaseStatus != "standard" {
			fmt.Fprint(w, t(l, "render.plan.no_evidence"))
		}
	}
	if info.Current != "" {
		pr(t(l, "render.plan.current_profile"), info.Current, info.CurrentStatusText)
	}

	profileRows(l, w, info)

	checks(l, w, p)
	steps(l, w, p, opts)

	runtime := 0
	for _, s := range p.Steps {
		if s.Kind == "set_value" {
			runtime++
		}
	}
	fmt.Fprint(w, t(l, "render.plan.summary"))
	pr(t(l, "render.plan.runtime_changes"), runtime)
	pr(t(l, "render.plan.created_line"), dash(strings.Join(p.Created, ", ")))
	pr(t(l, "render.plan.deleted_line"), dash(strings.Join(p.Deleted, ", ")))
	fmt.Fprint(w, t(l, "render.plan.no_grub"))
	switch {
	case p.Blocked():
		pr(t(l, "render.plan.result_blocked"), len(p.Failures()))
	case len(p.Steps) == 0:
		fmt.Fprint(w, t(l, "render.plan.result_nothing"))
	default:
		cmd := p.Command
		if info.Experimental && !strings.Contains(cmd, "--experimental") {
			cmd += t(l, "render.plan.experimental_hint")
		}
		pr(t(l, "render.plan.result_ok"), cmd)
	}
	return nil
}

// Exec — вывод хода выполнения операции (как в CLI).
func Exec(l i18n.Lang, w io.Writer) journal.Hooks {
	return journal.Hooks{
		Nothing: func() { fmt.Fprintln(w, t(l, "render.exec.nothing")) },
		Started: func(opID string) { fmt.Fprintf(w, t(l, "render.exec.started"), opID) },
		Progress: func(ev journal.Event) {
			prefix, status := "", "ok"
			if ev.Undo {
				prefix = t(l, "render.exec.undo")
			}
			if ev.Err != nil {
				status = t(l, "render.exec.error") + i18n.ErrText(l, ev.Err)
			}
			fmt.Fprintf(w, "  %s[%d/%d] %s ... %s\n", prefix, ev.Index, ev.Total, ev.Step.Describe(l), status)
		},
		Finished: func() { fmt.Fprintln(w, t(l, "render.exec.done")) },
	}
}

// swapNow — блок «Сейчас»: swap, ФС цели, RAM, swap RamPlus.
func swapNow(l i18n.Lang, w io.Writer, v *plan.SwapView) {
	fmt.Fprint(w, t(l, "render.plan.now"))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(v.Swaps) == 0 {
		fmt.Fprint(tw, t(l, "render.plan.now_no_swap"))
	}
	for _, s := range v.Swaps {
		fmt.Fprintf(tw, t(l, "render.plan.now_swap"),
			s.Path, s.Type, plan.FormatSize(s.Size), plan.FormatSize(s.Used), s.Priority, s.Tag)
	}
	if fi := v.Target; fi != nil {
		if fi.FSType != "" {
			fmt.Fprintf(tw, t(l, "render.plan.now_target"), fi.FSType, fi.Source, fi.MountPoint, fi.Disk)
		}
		if fi.Total != nil && fi.Avail != nil {
			fmt.Fprintf(tw, t(l, "render.plan.now_space"), fi.MountPoint, plan.FormatSize(*fi.Avail), plan.FormatSize(*fi.Total))
		}
	}
	if v.MemAvailable != nil {
		fmt.Fprintf(tw, t(l, "render.plan.now_ram"), plan.FormatSize(*v.MemAvailable), plan.FormatSize(v.MemTotal))
	}
	if m := v.Managed; m != nil {
		fmt.Fprintf(tw, t(l, "render.plan.now_managed"), m.Path, plan.FormatSize(m.Size), m.Priority, m.Unit)
	}
	tw.Flush()
}

// choices — блок «Выбор» с альтернативами.
func choices(l i18n.Lang, w io.Writer, p *plan.Plan) {
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	if len(p.Choices) > 0 {
		fmt.Fprint(w, t(l, "render.plan.choices"))
		for _, c := range p.Choices {
			how := t(l, "render.plan.choice_given")
			if c.Defaulted {
				how = t(l, "render.plan.choice_default")
			}
			pr("  %s (%s): %s  [%s]\n", c.Title, c.Flag, c.Value, how)
			if c.Note != "" {
				pr("      %s\n", c.Note)
			}
			for _, a := range c.Alternatives {
				mark := " "
				if a.Selected {
					mark = "*"
				}
				pr("    %s %-9s %s\n", mark, a.Value, a.Summary)
			}
		}
	}
}

// profileRows — таблица параметров профиля и порядок sysctl.
func profileRows(l i18n.Lang, w io.Writer, info *plan.ProfileView) {
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	if len(info.Rows) > 0 {
		fmt.Fprint(w, t(l, "render.plan.params"))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprint(tw, t(l, "render.plan.params_columns"))
		var same []string
		for _, r := range info.Rows {
			switch {
			case r.Skip != "":
				fmt.Fprintf(tw, t(l, "render.plan.param_skip"), r.Key, dash(r.Current), r.New, r.Skip)
			case r.Change:
				mark := ""
				if r.FromSet {
					mark = " (--set)"
				}
				fmt.Fprintf(tw, "  %s\t%s\t→\t%s%s\t%s\n", r.Key, dash(r.Current), r.New, mark, r.Source)
			default:
				same = append(same, fmt.Sprintf("%s=%s", r.Key, r.Current))
			}
		}
		tw.Flush()
		if len(same) > 0 {
			pr(t(l, "render.plan.params_same"), strings.Join(same, ", "))
		}
	}
	if info.SysctlOrder != "" {
		pr(t(l, "render.plan.sysctl_order"), info.SysctlOrder)
	}
}

// setupPlan — план setup: swap и профиль одной операцией.
func setupPlan(l i18n.Lang, w io.Writer, p *plan.Plan, opts Options) error {
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	fmt.Fprint(w, t(l, "render.plan.setup_title"))
	pr(t(l, "render.plan.command_nl"), p.Command)
	v := p.Swap
	if v == nil {
		v = &plan.SwapView{}
	}
	swapNow(l, w, v)
	choices(l, w, p)
	if info := p.Profile; info != nil {
		pr(t(l, "render.plan.setup_profile"), info.Name, info.StatusText)
		if info.Current != "" {
			pr(t(l, "render.plan.current_profile"), info.Current, info.CurrentStatusText)
		}
		profileRows(l, w, info)
	}
	checks(l, w, p)
	steps(l, w, p, opts)

	fmt.Fprint(w, t(l, "render.plan.summary"))
	pr("  swap: %s → %s\n", plan.FormatSize(p.SwapBefore), plan.FormatSize(p.SwapAfter))
	if info := p.Profile; info != nil {
		pr(t(l, "render.plan.profile_line"), info.Name)
	}
	pr(t(l, "render.plan.created_line"), dash(strings.Join(p.Created, ", ")))
	pr(t(l, "render.plan.disabled_line"), dash(strings.Join(p.Disabled, ", ")))
	pr(t(l, "render.plan.deleted_line_nl"), dash(strings.Join(p.Deleted, ", ")))
	switch {
	case p.Blocked():
		pr(t(l, "render.plan.result_blocked"), len(p.Failures()))
	case len(p.Steps) == 0:
		fmt.Fprint(w, t(l, "render.plan.result_nothing"))
	default:
		cmd := p.Command
		if p.Profile != nil && p.Profile.Experimental && !strings.Contains(cmd, "--experimental") {
			cmd += t(l, "render.plan.experimental_profile_hint")
		}
		pr(t(l, "render.plan.result_ok"), cmd)
	}
	return nil
}

// t — сообщение каталога на языке вывода.
func t(l i18n.Lang, key string, a ...any) string { return i18n.T(l, key, a...) }
