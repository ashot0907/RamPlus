package swap

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

var actionTitle = map[Action]string{
	ActionApply:        "создание swapfile",
	ActionResize:       "изменение размера swapfile",
	ActionRemove:       "удаление swapfile",
	ActionRollback:     "откат последней операции",
	ActionProfileApply: "применение профиля",
	ActionProfileReset: "сброс профиля",
}

var levelTag = map[Level]string{
	LevelOK: "[ok]  ", LevelInfo: "[info]", LevelWarn: "[warn]", LevelFail: "[FAIL]",
}

// RenderOptions — настройки вывода плана.
type RenderOptions struct {
	Verbose bool // полное содержимое записываемых файлов (systemd-юнит)
}

// Render печатает план для человека.
func (p Plan) Render(w io.Writer) error { return p.RenderWith(w, RenderOptions{}) }

// RenderWith печатает план с настройками.
func (p Plan) RenderWith(w io.Writer, opts RenderOptions) error {
	if p.Profile != nil {
		return p.renderProfile(w, opts)
	}
	f := p.facts
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }

	pr("План: %s\n", actionTitle[p.Action])
	pr("Команда: %s\n\n", p.Command())

	pr("Сейчас\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(f.Swaps) == 0 {
		fmt.Fprintf(tw, "  swap\tнет\n")
	}
	for _, s := range f.Swaps {
		tags := []string{}
		switch {
		case isZram(s.Path):
			tags = append(tags, "zram — не затрагивается")
		case f.State.Managed != nil && s.Path == f.State.Managed.Path:
			tags = append(tags, "RamPlus")
		case len(f.fstabEntriesFor(s.Path)) > 0:
			tags = append(tags, "в /etc/fstab")
		}
		fmt.Fprintf(tw, "  swap\t%s\t%s\t%s\tзанято %s\tприоритет %d\t%s\n",
			s.Path, s.Type, FormatSize(s.SizeBytes), FormatSize(s.UsedBytes), s.Priority, strings.Join(tags, ", "))
	}
	fi := f.Target.FS
	if fi.FSType != "" {
		disk := "диск n/a"
		if len(fi.Disks) > 0 {
			d := fi.Disks[0]
			disk = fmt.Sprintf("%s, %s, %s", d.Name, kindText(fi.Kind), d.Transport)
		}
		fmt.Fprintf(tw, "  ФС цели\t%s на %s, точка %s (%s)\n", fi.FSType, fi.Source, fi.MountPoint, disk)
	}
	if fi.TotalBytes != nil && fi.AvailableBytes != nil {
		fmt.Fprintf(tw, "  место на %s\tдоступно %s из %s\n", fi.MountPoint, FormatSize(*fi.AvailableBytes), FormatSize(*fi.TotalBytes))
	}
	if v, ok := f.Mem["MemAvailable"]; ok {
		fmt.Fprintf(tw, "  RAM\tдоступно %s из %s\n", FormatSize(v), FormatSize(f.Mem["MemTotal"]))
	}
	if m := f.State.Managed; m != nil {
		fmt.Fprintf(tw, "  RamPlus\t%s, %s, приоритет %d, юнит %s\n", m.Path, FormatSize(m.SizeBytes), m.Priority, m.Unit)
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

	pr("\nИтог\n")
	pr("  swap: %s → %s\n", FormatSize(p.SwapBefore), FormatSize(p.SwapAfter))
	list := func(title string, items []string) {
		v := "—"
		if len(items) > 0 {
			v = strings.Join(items, ", ")
		}
		pr("  %s: %s\n", title, v)
	}
	list("создаётся", p.Created)
	list("отключается", p.Disabled)
	list("удаляется", p.Deleted)

	pr("\n")
	if fails := p.Failures(); len(fails) > 0 {
		pr("Результат: ПЛАН ЗАБЛОКИРОВАН — проверок не пройдено: %d. Ничего не изменено.\n", len(fails))
		return nil
	}
	_, err := fmt.Fprintf(w, "Результат: план выполним. Применить: sudo %s\n", p.Options.Command())
	return err
}

func hasKind(steps []Step, k StepKind) bool {
	for _, s := range steps {
		if s.Kind == k {
			return true
		}
	}
	return false
}
