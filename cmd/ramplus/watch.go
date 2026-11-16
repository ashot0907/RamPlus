package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ashot0907/ramplus/internal/monitor"
)

const (
	clearScreen      = "\033[H\033[2J"
	minWatchInterval = 200 * time.Millisecond
)

// watcher — status --watch: периодический снимок, пики, итог по Ctrl+C.
type watcher struct {
	out      io.Writer
	interval time.Duration
	tty      bool // обновлять на месте (иначе кадры печатаются подряд)
	collect  func() monitor.Status
	now      func() time.Time
	// maxFrames > 0 — остановиться после N кадров (для тестов).
	maxFrames int
}

func (w *watcher) run(ctx context.Context) error {
	peaks := monitor.NewPeaks(w.now())
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for frame := 1; ; frame++ {
		st := w.collect()
		now := w.now()
		peaks.Update(st, now)
		var b strings.Builder
		if w.tty {
			b.WriteString(clearScreen)
		} else if frame > 1 {
			b.WriteString("\n")
		}
		renderWatchFrame(&b, st, peaks, now, w.interval)
		if _, err := io.WriteString(w.out, b.String()); err != nil {
			return err
		}
		if w.maxFrames > 0 && frame >= w.maxFrames {
			break
		}
		select {
		case <-ctx.Done():
			return renderWatchSummary(w.out, peaks, w.now())
		case <-ticker.C:
		}
	}
	return renderWatchSummary(w.out, peaks, w.now())
}

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

func renderWatchFrame(w io.Writer, st monitor.Status, p *monitor.Peaks, now time.Time, interval time.Duration) {
	fmt.Fprintf(w, "RamPlus — наблюдение (каждые %s, Ctrl+C — выход)   %s\n\n", interval, now.Local().Format("15:04:05"))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	m := st.Memory
	if m.Error != nil {
		fmt.Fprintf(tw, "RAM\t%s\n", reasonText(m.Error, nil))
	} else {
		fmt.Fprintf(tw, "RAM\tзанято %s из %s%s, доступно %s\n",
			bytesOrNA(m.UsedBytes), bytesOrNA(m.TotalBytes), pct(m.UsedBytes, m.TotalBytes), bytesOrNA(m.AvailableBytes))
		if m.SwapTotalBytes != nil && *m.SwapTotalBytes == 0 {
			fmt.Fprintf(tw, "swap\tнет\n")
		} else {
			fmt.Fprintf(tw, "swap\tзанято %s из %s%s\n", bytesOrNA(m.SwapUsedBytes), bytesOrNA(m.SwapTotalBytes), pct(m.SwapUsedBytes, m.SwapTotalBytes))
		}
	}
	z := st.Zswap
	switch {
	case !z.Present:
		fmt.Fprintf(tw, "zswap\tнет в ядре\n")
	default:
		state := "выключен"
		if z.Params.Enabled != nil && *z.Params.Enabled {
			state = "включён"
		}
		fmt.Fprintf(tw, "zswap\tпул %s / данные %s (%s)\n", bytesOrNA(z.PoolBytes), bytesOrNA(z.StoredBytes), state)
	}
	if st.PSI.Error != nil {
		fmt.Fprintf(tw, "PSI\t%s\n", reasonText(st.PSI.Error, map[string]string{"not_present": "ядро без PSI"}))
	} else {
		fmt.Fprintf(tw, "PSI avg10\tsome %s  full %s — %s\n", avg10(st.PSI.Some), avg10(st.PSI.Full), pressureText(st.PSI.PressureLevel))
	}
	tw.Flush()

	fmt.Fprintf(w, "\nПики за %s (замеров: %d)\n", now.Sub(p.Since).Round(time.Second), p.Samples)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  swap занято\t%s%s\n", humanBytes(p.SwapUsedBytes), at(p.SwapUsedAt))
	if !p.PSISomeAt.IsZero() {
		fmt.Fprintf(tw, "  PSI some avg10\t%.2f%s — %s\n", p.PSISomeAvg10, at(p.PSISomeAt), pressureText(p.PressureLevel))
		fmt.Fprintf(tw, "  PSI full avg10\t%.2f%s\n", p.PSIFullAvg10, at(p.PSIFullAt))
	}
	tw.Flush()
}

func at(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " в " + t.Local().Format("15:04:05")
}

func renderWatchSummary(w io.Writer, p *monitor.Peaks, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\nИтог наблюдения: %s, замеров: %d\n", now.Sub(p.Since).Round(time.Second), p.Samples)
	fmt.Fprintf(&b, "  макс. swap занято: %s%s\n", humanBytes(p.SwapUsedBytes), at(p.SwapUsedAt))
	fmt.Fprintf(&b, "  макс. RAM занято:  %s\n", humanBytes(p.MemUsedBytes))
	if p.ZswapPoolBytes > 0 {
		fmt.Fprintf(&b, "  макс. пул zswap:   %s\n", humanBytes(p.ZswapPoolBytes))
	}
	if !p.PSISomeAt.IsZero() {
		fmt.Fprintf(&b, "  макс. PSI some avg10: %.2f%s — %s\n", p.PSISomeAvg10, at(p.PSISomeAt), pressureText(p.PressureLevel))
		fmt.Fprintf(&b, "  макс. PSI full avg10: %.2f%s\n", p.PSIFullAvg10, at(p.PSIFullAt))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
