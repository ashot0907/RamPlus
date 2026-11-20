package render

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/profile"
)

// ProfileCurrent — ramplus profile current.
func ProfileCurrent(l i18n.Lang, w io.Writer, pf profile.Facts) error {
	ap, base := pf.Profile, pf.Baseline
	if ap == nil {
		fmt.Fprintln(w, t(l, "render.profile.none"))
	} else {
		fmt.Fprintf(w, t(l, "render.profile.applied"), ap.Name, profile.StatusText(l, ap.Status), ap.AppliedAt.Local().Format("2006-01-02 15:04:05"))
		for k, v := range ap.Overrides {
			fmt.Fprintf(w, t(l, "render.profile.override"), k, v)
		}
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, t(l, "render.profile.columns"))
	var fromBase []string
	for _, pr := range profile.Params {
		cur, present := pf.Values[pr.Key]
		if !present {
			cur = t(l, "render.not_in_kernel")
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
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", pr.Key, cur, want, orig, pf.SourceOf(pr.Key).Text(l))
	}
	tw.Flush()
	if base == nil {
		fmt.Fprintln(w, t(l, "render.profile.no_baseline"))
	} else if len(fromBase) == 0 {
		fmt.Fprintln(w, t(l, "render.profile.no_diff"))
	} else {
		fmt.Fprintf(w, t(l, "render.profile.diff"), base.CapturedAt.Local().Format("2006-01-02 15:04:05"), strings.Join(fromBase, "; "))
	}
	if d := profile.ProfileDrift(ap, pf.Values); len(d) > 0 {
		var parts []string
		for _, r := range d {
			parts = append(parts, fmt.Sprintf(t(l, "render.profile.drift_item"), r.Key, dash(r.Current), r.New))
		}
		fmt.Fprintf(w, t(l, "render.profile.drift"), strings.Join(parts, "; "))
	}
	return nil
}
