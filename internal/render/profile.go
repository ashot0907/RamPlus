package render

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/profile"
)

// ProfileCurrent — ramplus profile current.
func ProfileCurrent(w io.Writer, pf profile.Facts) error {
	ap, base := pf.Profile, pf.Baseline
	if ap == nil {
		fmt.Fprintln(w, "Профиль RamPlus не применён.")
	} else {
		fmt.Fprintf(w, "Профиль: %s — %s, применён %s\n", ap.Name, profile.StatusText(ap.Status), ap.AppliedAt.Local().Format("2006-01-02 15:04:05"))
		for k, v := range ap.Overrides {
			fmt.Fprintf(w, "  изменено пользователем: %s=%s\n", k, v)
		}
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ПАРАМЕТР\tСЕЙЧАС\tПРОФИЛЬ\tИСХОДНОЕ\tОТКУДА")
	var fromBase []string
	for _, pr := range profile.Params {
		cur, present := pf.Values[pr.Key]
		if !present {
			cur = "нет в ядре"
		}
		want, orig := "—", "—"
		if ap != nil && ap.Values[pr.Key] != "" {
			want = ap.Values[pr.Key]
		}
		if base != nil && base.Values[pr.Key] != "" {
			orig = base.Values[pr.Key]
			if present && cur != orig {
				fromBase = append(fromBase, fmt.Sprintf("%s: %s → %s", pr.Key, orig, cur))
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", pr.Key, cur, want, orig, pf.SourceOf(pr.Key))
	}
	tw.Flush()
	if base == nil {
		fmt.Fprintln(w, "\nИсходные значения не сохранены (снимаются при первом ramplus profile apply).")
	} else if len(fromBase) == 0 {
		fmt.Fprintln(w, "\nОтличий от исходных значений нет.")
	} else {
		fmt.Fprintf(w, "\nОтличия от исходных значений (сняты %s): %s\n", base.CapturedAt.Local().Format("2006-01-02 15:04:05"), strings.Join(fromBase, "; "))
	}
	if d := profile.ProfileDrift(ap, pf.Values); len(d) > 0 {
		var parts []string
		for _, r := range d {
			parts = append(parts, fmt.Sprintf("%s = %s (профиль: %s)", r.Key, dash(r.Current), r.New))
		}
		fmt.Fprintf(w, "ВНИМАНИЕ: значения изменены вручную после apply: %s\n", strings.Join(parts, "; "))
	}
	return nil
}
