package main

import (
	"context"
	"flag"
	"fmt"
	"time"
)

// touch пишет по байту в каждую страницу buf (страница становится
// активной; если она была в swap — её приходится вернуть в RAM).
func touch(buf []byte, v byte) {
	for i := 0; i < len(buf); i += pageSize {
		buf[i] ^= v
	}
}

// runLatency — latency-probe: маленький "интерактивный" процесс.
func runLatency(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("latency", flag.ContinueOnError)
	sizeS := fs.String("size", "50M", "сколько своей памяти трогать")
	interval := fs.Duration("interval", 100*time.Millisecond, "период")
	duration := fs.Duration("duration", 0, "длительность (0 — до SIGTERM)")
	out := fs.String("out", "", "куда писать JSON-сводку (по умолчанию stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	size, err := ParseSize(*sizeS)
	if err != nil {
		return err
	}
	buf := make([]byte, size)
	if err := FillPages(buf, DataCompressible, 1); err != nil {
		return err
	}
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	var samples []float64
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return writeJSON(*out, Summarize(samples))
		case <-ticker.C:
		}
		t0 := time.Now()
		touch(buf, byte(i))
		samples = append(samples, float64(time.Since(t0).Microseconds())/1000)
	}
}

// WorkloadResult — итог нагрузки.
type WorkloadResult struct {
	OK        bool    `json:"ok"`
	Bytes     uint64  `json:"bytes"`
	HotBytes  uint64  `json:"hot_bytes"`
	Data      string  `json:"data"`
	FillSec   float64 `json:"fill_s"`
	Passes    int     `json:"passes"`
	PassP50Ms float64 `json:"pass_p50_ms"`
	PassP99Ms float64 `json:"pass_p99_ms"`
	// GiBps — объём горячих страниц, обойдённых за секунду (по байту на
	// страницу). Абсолютное значение условно; сравнивать между конфигурациями:
	// падение означает, что страницы приходится возвращать из swap/zswap.
	GiBps float64 `json:"hot_touch_gib_per_s"`
}

// runWorkload — выделить size, заполнить, затем duration работать только
// с долей hot (1.0 — все страницы активны, как stress-ng --vm-keep).
func runWorkload(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("workload", flag.ContinueOnError)
	sizeS := fs.String("size", "", "сколько памяти выделить, например 5G")
	hot := fs.Float64("hot", 0.15, "доля активно используемой памяти (0..1]")
	data := fs.String("data", DataCompressible, "compressible|random")
	duration := fs.Duration("duration", 30*time.Second, "сколько работать после заполнения")
	out := fs.String("out", "", "куда писать JSON (по умолчанию stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sizeS == "" {
		return fmt.Errorf("--size обязателен")
	}
	if *hot <= 0 || *hot > 1 {
		return fmt.Errorf("--hot должен быть в (0, 1]")
	}
	size, err := ParseSize(*sizeS)
	if err != nil {
		return err
	}
	res := WorkloadResult{Bytes: size, Data: *data}

	t0 := time.Now()
	buf := make([]byte, size)
	if err := FillPages(buf, *data, 42); err != nil {
		return err
	}
	res.FillSec = time.Since(t0).Seconds()

	// Горячая часть — в начале буфера; остальное после заполнения не
	// трогается и становится кандидатом на вытеснение.
	hotLen := uint64(float64(size)**hot) / pageSize * pageSize
	res.HotBytes = hotLen
	hotBuf := buf[:hotLen]

	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	start := time.Now()
	var passes []float64
	for i := 0; ctx.Err() == nil; i++ {
		p0 := time.Now()
		touch(hotBuf, byte(i))
		passes = append(passes, float64(time.Since(p0).Microseconds())/1000)
	}
	elapsed := time.Since(start).Seconds()
	res.Passes = len(passes)
	res.PassP50Ms, res.PassP99Ms = Percentile(passes, 50), Percentile(passes, 99)
	if elapsed > 0 {
		res.GiBps = float64(hotLen) * float64(len(passes)) / elapsed / (1 << 30)
	}
	res.OK = true
	return writeJSON(*out, res)
}
