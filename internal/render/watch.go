package render

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ashot0907/ramplus/internal/monitor"
)

func pct(part, total *uint64) string {
	if part == nil || total == nil || *total == 0 {
		return ""
	}
	return fmt.Sprintf(" (%.0f%%)", float64(*part)*100/float64(*total))
}

func avg10(l *monitor.PSILine) string {
	if l == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", l.Avg10)
}

// WatchFrame — кадр ramplus status --watch.
func WatchFrame(l i18n.Lang, w io.Writer, st monitor.Status, p *monitor.Peaks, now time.Time, interval time.Duration) {
	fmt.Fprintf(w, t(l, "render.watch.header"), interval, now.Local().Format("15:04:05"))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	m := st.Memory
	if m.Error != nil {
		fmt.Fprintf(tw, "RAM\t%s\n", reasonText(l, m.Error, nil))
	} else {
		fmt.Fprintf(tw, t(l, "render.watch.ram"),
			bytesOrNA(m.UsedBytes), bytesOrNA(m.TotalBytes), pct(m.UsedBytes, m.TotalBytes), bytesOrNA(m.AvailableBytes))
		if m.SwapTotalBytes != nil && *m.SwapTotalBytes == 0 {
			fmt.Fprint(tw, t(l, "render.watch.no_swap"))
		} else {
			fmt.Fprintf(tw, t(l, "render.watch.swap"), bytesOrNA(m.SwapUsedBytes), bytesOrNA(m.SwapTotalBytes), pct(m.SwapUsedBytes, m.SwapTotalBytes))
		}
	}
	z := st.Zswap
	switch {
	case !z.Present:
		fmt.Fprint(tw, t(l, "render.watch.no_zswap"))
	default:
		state := t(l, "render.watch.off")
		if z.Params.Enabled != nil && *z.Params.Enabled {
			state = t(l, "render.watch.on")
		}
		fmt.Fprintf(tw, t(l, "render.watch.zswap"), bytesOrNA(z.PoolBytes), bytesOrNA(z.StoredBytes), state)
	}
	if st.PSI.Error != nil {
		fmt.Fprintf(tw, "PSI\t%s\n", reasonText(l, st.PSI.Error, map[string]string{"not_present": t(l, "render.status.no_psi")}))
	} else {
		fmt.Fprintf(tw, "PSI avg10\tsome %s  full %s — %s\n", avg10(st.PSI.Some), avg10(st.PSI.Full), pressureText(l, st.PSI.PressureLevel))
	}
	tw.Flush()

	fmt.Fprintf(w, t(l, "render.watch.peaks"), now.Sub(p.Since).Round(time.Second), p.Samples)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, t(l, "render.watch.peak_swap"), humanBytes(p.SwapUsedBytes), at(l, p.SwapUsedAt))
	if !p.PSISomeAt.IsZero() {
		fmt.Fprintf(tw, "  PSI some avg10\t%.2f%s — %s\n", p.PSISomeAvg10, at(l, p.PSISomeAt), pressureText(l, p.PressureLevel))
		fmt.Fprintf(tw, "  PSI full avg10\t%.2f%s\n", p.PSIFullAvg10, at(l, p.PSIFullAt))
	}
	tw.Flush()
}

func at(l i18n.Lang, when time.Time) string {
	if when.IsZero() {
		return ""
	}
	return t(l, "render.watch.at") + when.Local().Format("15:04:05")
}

// WatchSummary — итог наблюдения (по Ctrl+C).
func WatchSummary(l i18n.Lang, w io.Writer, p *monitor.Peaks, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, t(l, "render.watch.summary"), now.Sub(p.Since).Round(time.Second), p.Samples)
	fmt.Fprintf(&b, t(l, "render.watch.max_swap"), humanBytes(p.SwapUsedBytes), at(l, p.SwapUsedAt))
	fmt.Fprintf(&b, t(l, "render.watch.max_ram"), humanBytes(p.MemUsedBytes))
	if p.ZswapPoolBytes > 0 {
		fmt.Fprintf(&b, t(l, "render.watch.max_pool"), humanBytes(p.ZswapPoolBytes))
	}
	if !p.PSISomeAt.IsZero() {
		fmt.Fprintf(&b, t(l, "render.watch.max_psi_some"), p.PSISomeAvg10, at(l, p.PSISomeAt), pressureText(l, p.PressureLevel))
		fmt.Fprintf(&b, t(l, "render.watch.max_psi_full"), p.PSIFullAvg10, at(l, p.PSIFullAt))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
