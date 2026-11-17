package main

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/render"
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
		render.WatchFrame(&b, st, peaks, now, w.interval)
		if _, err := io.WriteString(w.out, b.String()); err != nil {
			return err
		}
		if w.maxFrames > 0 && frame >= w.maxFrames {
			break
		}
		select {
		case <-ctx.Done():
			return render.WatchSummary(w.out, peaks, w.now())
		case <-ticker.C:
		}
	}
	return render.WatchSummary(w.out, peaks, w.now())
}
