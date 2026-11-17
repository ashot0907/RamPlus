// Package render — текстовый вывод для CLI: план, ход выполнения, статус.
// Строится только из данных (plan.Plan, monitor.Status), без логики.
package render

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
)

// Options — настройки вывода плана.
type Options struct {
	Verbose bool // полное содержимое записываемых файлов (systemd-юнит)
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
	if p.Profile != nil {
		return profilePlan(w, p, opts)
	}
	return swapPlan(w, p, opts)
}

func steps(w io.Writer, p *plan.Plan, opts Options) {
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	pr("\nШаги\n")
	if len(p.Steps) == 0 {
		pr("  (нет)\n")
	}
	for i, s := range p.Steps {
		pr("  %2d. %s\n", i+1, s.Describe())
		if opts.Verbose && s.Kind == plan.KindWriteFile {
			pr("      ┌── %s\n", s.Path)
			for _, line := range strings.Split(strings.TrimSuffix(s.Content, "\n"), "\n") {
				pr("      │ %s\n", line)
			}
			pr("      └──\n")
		}
	}
	if !opts.Verbose && p.HasKind(plan.KindWriteFile) {
		pr("  (содержимое записываемых файлов: --verbose)\n")
	}
}

func checks(w io.Writer, p *plan.Plan) {
	fmt.Fprintf(w, "\nПроверки\n")
	for _, c := range p.Checks {
		fmt.Fprintf(w, "  %s %s: %s\n", levelTag[c.Level], c.Title, c.Message)
	}
}

func swapPlan(w io.Writer, p *plan.Plan, opts Options) error {
	v := p.Swap
	if v == nil {
		v = &plan.SwapView{}
	}
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }

	pr("План: %s\n", v.Title)
	pr("Команда: %s\n\n", p.Command)

	pr("Сейчас\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(v.Swaps) == 0 {
		fmt.Fprintf(tw, "  swap\tнет\n")
	}
	for _, s := range v.Swaps {
		fmt.Fprintf(tw, "  swap\t%s\t%s\t%s\tзанято %s\tприоритет %d\t%s\n",
			s.Path, s.Type, plan.FormatSize(s.Size), plan.FormatSize(s.Used), s.Priority, s.Tag)
	}
	if fi := v.Target; fi != nil {
		if fi.FSType != "" {
			fmt.Fprintf(tw, "  ФС цели\t%s на %s, точка %s (%s)\n", fi.FSType, fi.Source, fi.MountPoint, fi.Disk)
		}
		if fi.Total != nil && fi.Avail != nil {
			fmt.Fprintf(tw, "  место на %s\tдоступно %s из %s\n", fi.MountPoint, plan.FormatSize(*fi.Avail), plan.FormatSize(*fi.Total))
		}
	}
	if v.MemAvailable != nil {
		fmt.Fprintf(tw, "  RAM\tдоступно %s из %s\n", plan.FormatSize(*v.MemAvailable), plan.FormatSize(v.MemTotal))
	}
	if m := v.Managed; m != nil {
		fmt.Fprintf(tw, "  RamPlus\t%s, %s, приоритет %d, юнит %s\n", m.Path, plan.FormatSize(m.Size), m.Priority, m.Unit)
	}
	tw.Flush()

	if len(p.Choices) > 0 {
		pr("\nВыбор\n")
		for _, c := range p.Choices {
			how := "указано"
			if c.Defaulted {
				how = "по умолчанию — флаг не указан"
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

	checks(w, p)
	steps(w, p, opts)

	pr("\nИтог\n")
	pr("  swap: %s → %s\n", plan.FormatSize(p.SwapBefore), plan.FormatSize(p.SwapAfter))
	list := func(title string, items []string) {
		pr("  %s: %s\n", title, dash(strings.Join(items, ", ")))
	}
	list("создаётся", p.Created)
	list("отключается", p.Disabled)
	list("удаляется", p.Deleted)

	pr("\n")
	if fails := p.Failures(); len(fails) > 0 {
		pr("Результат: ПЛАН ЗАБЛОКИРОВАН — проверок не пройдено: %d. Ничего не изменено.\n", len(fails))
		return nil
	}
	_, err := fmt.Fprintf(w, "Результат: план выполним. Применить: sudo %s\n", v.ApplyCommand)
	return err
}

func profilePlan(w io.Writer, p *plan.Plan, opts Options) error {
	info := p.Profile
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	if info.Reset {
		pr("План: сброс профиля RamPlus — вернуть значения до первого apply\n")
	} else {
		pr("План: профиль %s — %s\n", info.Name, info.StatusText)
	}
	pr("Команда: %s\n", p.Command)
	if !info.Reset {
		if info.Title != "" {
			pr("Профиль: %s. %s\n", info.Title, info.Description)
		}
		if info.Evidence != "" {
			pr("Evidence: %s\n", info.Evidence)
		} else if info.BaseStatus != "standard" {
			pr("Evidence: нет (результата scripts/bench.sh на реальном SSD нет)\n")
		}
	}
	if info.Current != "" {
		pr("Сейчас применён: %s (%s)\n", info.Current, info.CurrentStatusText)
	}

	if len(info.Rows) > 0 {
		pr("\nПараметры\n")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  ПАРАМЕТР\tСЕЙЧАС\t\tНОВОЕ\tОТКУДА СЕЙЧАС\n")
		var same []string
		for _, r := range info.Rows {
			switch {
			case r.Skip != "":
				fmt.Fprintf(tw, "  %s\t%s\t\t%s\t%s — пропуск\n", r.Key, dash(r.Current), r.New, r.Skip)
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
			pr("  без изменений: %s\n", strings.Join(same, ", "))
		}
	}
	if info.SysctlOrder != "" {
		pr("\nПорядок sysctl при загрузке: %s\n", info.SysctlOrder)
	}

	checks(w, p)
	steps(w, p, opts)

	runtime := 0
	for _, s := range p.Steps {
		if s.Kind == "set_value" {
			runtime++
		}
	}
	pr("\nИтог\n")
	pr("  сразу (runtime): изменится параметров: %d\n", runtime)
	pr("  создаётся: %s\n", dash(strings.Join(p.Created, ", ")))
	pr("  удаляется: %s\n", dash(strings.Join(p.Deleted, ", ")))
	pr("  kernel cmdline и GRUB: не меняются\n\n")
	switch {
	case p.Blocked():
		pr("Результат: ПЛАН ЗАБЛОКИРОВАН — проверок не пройдено: %d. Ничего не изменено.\n", len(p.Failures()))
	case len(p.Steps) == 0:
		pr("Результат: менять нечего.\n")
	default:
		cmd := p.Command
		if info.Experimental && !strings.Contains(cmd, "--experimental") {
			cmd += "   (экспериментальный: потребуется подтверждение или --experimental)"
		}
		pr("Результат: план выполним. Применить: sudo %s\n", cmd)
	}
	return nil
}

// Exec — вывод хода выполнения операции (как в CLI).
func Exec(w io.Writer) journal.Hooks {
	return journal.Hooks{
		Nothing: func() { fmt.Fprintln(w, "Нечего делать.") },
		Started: func(opID string) { fmt.Fprintf(w, "\nВыполнение (операция %s)\n", opID) },
		Progress: func(ev journal.Event) {
			prefix, status := "", "ok"
			if ev.Undo {
				prefix = "откат "
			}
			if ev.Err != nil {
				status = "ОШИБКА: " + ev.Err.Error()
			}
			fmt.Fprintf(w, "  %s[%d/%d] %s ... %s\n", prefix, ev.Index, ev.Total, ev.Step.Describe(), status)
		},
		Finished: func() { fmt.Fprintln(w, "Готово.") },
	}
}
