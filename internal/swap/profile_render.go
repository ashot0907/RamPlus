package swap

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/profile"
)

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func orDash(s string) string { return dash(s) }

// renderProfile — план профиля для человека.
func (p Plan) renderProfile(w io.Writer, opts RenderOptions) error {
	info := p.Profile
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	if info.Reset {
		pr("План: сброс профиля RamPlus — вернуть значения до первого apply\n")
	} else {
		pr("План: профиль %s — %s\n", info.Name, profile.StatusText(info.Status))
	}
	pr("Команда: %s\n", p.Command())
	if !info.Reset {
		if info.Title != "" {
			pr("Профиль: %s. %s\n", info.Title, info.Description)
		}
		if info.Evidence != "" {
			pr("Evidence: %s\n", info.Evidence)
		} else if info.BaseStatus != profile.StatusStandard {
			pr("Evidence: нет (результата scripts/bench.sh на реальном SSD нет)\n")
		}
	}
	if cur := p.facts.State.Profile; cur != nil {
		pr("Сейчас применён: %s (%s)\n", cur.Name, profile.StatusText(cur.Status))
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

	pr("\nПроверки\n")
	for _, c := range p.Checks {
		pr("  %s %s: %s\n", levelTag[c.Level], c.Title, c.Message)
	}
	pr("\nШаги\n")
	if len(p.Steps) == 0 {
		pr("  (нет)\n")
	}
	for i, s := range p.Steps {
		pr("  %2d. %s\n", i+1, s.Describe())
		if opts.Verbose && s.Kind == KindWriteFile {
			pr("      ┌── %s\n", s.Path)
			for _, line := range strings.Split(strings.TrimSuffix(s.Content, "\n"), "\n") {
				pr("      │ %s\n", line)
			}
			pr("      └──\n")
		}
	}
	if !opts.Verbose && hasKind(p.Steps, KindWriteFile) {
		pr("  (содержимое записываемых файлов: --verbose)\n")
	}
	runtime := 0
	for _, s := range p.Steps {
		if s.Kind == KindSetValue {
			runtime++
		}
	}
	pr("\nИтог\n")
	pr("  сразу (runtime): изменится параметров: %d\n", runtime)
	list := func(title string, items []string) {
		pr("  %s: %s\n", title, dash(strings.Join(items, ", ")))
	}
	list("создаётся", p.Created)
	list("удаляется", p.Deleted)
	pr("  kernel cmdline и GRUB: не меняются\n\n")
	switch {
	case p.Blocked():
		pr("Результат: ПЛАН ЗАБЛОКИРОВАН — проверок не пройдено: %d. Ничего не изменено.\n", len(p.Failures()))
	case len(p.Steps) == 0:
		pr("Результат: менять нечего.\n")
	default:
		cmd := p.Command()
		if info.Experimental && !strings.Contains(cmd, "--experimental") {
			cmd += "   (экспериментальный: потребуется подтверждение или --experimental)"
		}
		pr("Результат: план выполним. Применить: sudo %s\n", cmd)
	}
	return nil
}
