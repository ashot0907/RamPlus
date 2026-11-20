package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"time"

	"golang.org/x/sys/unix"
)

// ExitRefused — код выхода, когда система отказала в памяти (штатный
// результат, например для конфигурации no-swap).
const ExitRefused = 10

// NoMemoryError — ядро отказало в выделении памяти.
type NoMemoryError struct {
	Size uint64
	Err  error
}

func (e *NoMemoryError) Error() string {
	return fmt.Sprintf("could not allocate %d MiB: not enough memory (%v)", e.Size>>20, e.Err)
}

func (e *NoMemoryError) Unwrap() error { return e.Err }

// mmapAnon выделяет анонимную память. make([]byte, N) здесь не подходит:
// при отказе ядра рантайм Go завершается фатальной ошибкой "out of memory"
// (код 2, трейс всех горутин), которую нельзя перехватить.
var mmapAnon = func(size int) ([]byte, error) {
	return unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
}

// allocate выделяет size байт; отказ ядра — *NoMemoryError.
func allocate(size uint64, mmap func(int) ([]byte, error)) ([]byte, error) {
	if size == 0 {
		return nil, errors.New("zero size")
	}
	if size > math.MaxInt {
		return nil, &NoMemoryError{Size: size, Err: unix.ENOMEM}
	}
	b, err := mmap(int(size))
	if err != nil {
		if errors.Is(err, unix.ENOMEM) || errors.Is(err, unix.EAGAIN) {
			return nil, &NoMemoryError{Size: size, Err: err}
		}
		return nil, fmt.Errorf("mmap %d bytes: %w", size, err)
	}
	return b, nil
}

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
	sizeS := fs.String("size", "50M", "how much of its own memory to touch")
	interval := fs.Duration("interval", 100*time.Millisecond, "period")
	duration := fs.Duration("duration", 0, "duration (0 — until SIGTERM)")
	out := fs.String("out", "", "where to write the JSON summary (default stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	size, err := ParseSize(*sizeS)
	if err != nil {
		return err
	}
	buf, err := allocate(size, mmapAnon)
	if err != nil {
		return err
	}
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
	sizeS := fs.String("size", "", "how much memory to allocate, for example 5G")
	hot := fs.Float64("hot", 0.15, "share of actively used memory (0..1]")
	data := fs.String("data", DataCompressible, "compressible|random")
	duration := fs.Duration("duration", 30*time.Second, "how long to work after filling")
	out := fs.String("out", "", "where to write JSON (default stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sizeS == "" {
		return fmt.Errorf("--size is required")
	}
	if *hot <= 0 || *hot > 1 {
		return fmt.Errorf("--hot must be in (0, 1]")
	}
	size, err := ParseSize(*sizeS)
	if err != nil {
		return err
	}
	res := WorkloadResult{Bytes: size, Data: *data}

	t0 := time.Now()
	buf, err := allocate(size, mmapAnon)
	if err != nil {
		return err
	}
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
