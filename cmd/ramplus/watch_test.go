package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/monitor"
)

func TestWatch(t *testing.T) {
	u := func(v uint64) *uint64 { return &v }
	frames := []monitor.Status{
		{Memory: monitor.Memory{TotalBytes: u(8 << 30), UsedBytes: u(2 << 30), AvailableBytes: u(6 << 30), SwapTotalBytes: u(4 << 30), SwapUsedBytes: u(0)},
			PSI: monitor.PSI{Some: &monitor.PSILine{Avg10: 1}, Full: &monitor.PSILine{Avg10: 0.5}, PressureLevel: monitor.PressureNormal}},
		{Memory: monitor.Memory{TotalBytes: u(8 << 30), UsedBytes: u(7 << 30), AvailableBytes: u(1 << 30), SwapTotalBytes: u(4 << 30), SwapUsedBytes: u(3 << 30)},
			PSI: monitor.PSI{Some: &monitor.PSILine{Avg10: 70}, Full: &monitor.PSILine{Avg10: 30}, PressureLevel: monitor.PressureSevere}},
		{Memory: monitor.Memory{TotalBytes: u(8 << 30), UsedBytes: u(5 << 30), AvailableBytes: u(3 << 30), SwapTotalBytes: u(4 << 30), SwapUsedBytes: u(1 << 30)},
			PSI: monitor.PSI{Some: &monitor.PSILine{Avg10: 12}, Full: &monitor.PSILine{Avg10: 4}, PressureLevel: monitor.PressureElevated}},
	}
	i := 0
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.Local)
	clock := t0
	var out bytes.Buffer
	w := &watcher{
		out: &out, interval: time.Millisecond, maxFrames: len(frames),
		collect: func() monitor.Status { st := frames[i]; i++; return st },
		now:     func() time.Time { clock = clock.Add(time.Second); return clock },
	}
	if err := w.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"занято 3.0 GiB из 4.0 GiB (75%)",
		"some 70.00  full 30.00 — сильные тормоза",
		"some 12.00  full 4.00 — повышенное давление",
		"Итог наблюдения:",
		"макс. swap занято: 3.0 GiB в 12:00:03",
		"макс. PSI some avg10: 70.00 в 12:00:03 — сильные тормоза",
		"макс. PSI full avg10: 30.00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, clearScreen) {
		t.Error("non-tty output must not contain ANSI clear")
	}
}

func TestWatchStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	n := 0
	w := &watcher{
		out: &out, interval: time.Millisecond, now: time.Now,
		collect: func() monitor.Status {
			n++
			if n == 2 {
				cancel()
			}
			return monitor.Status{}
		},
	}
	if err := w.run(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Итог наблюдения") {
		t.Errorf("no summary after Ctrl+C:\n%s", out.String())
	}
}
