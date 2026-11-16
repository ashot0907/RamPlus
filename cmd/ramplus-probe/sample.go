package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ashot0907/ramplus/internal/monitor"
)

// Sample — один замер.
type Sample struct {
	T             time.Duration
	MemAvailable  uint64
	SwapUsed      uint64
	ZswapPool     uint64
	ZswapStored   uint64
	SomeAvg10     float64
	FullAvg10     float64
	SomeTotalUs   uint64
	FullTotalUs   uint64
	HavePSI       bool
	HaveSwapStats bool
}

// MakeSample строит замер из содержимого /proc/meminfo и /proc/pressure/memory.
func MakeSample(t time.Duration, meminfo, psi []byte) Sample {
	s := Sample{T: t}
	if mi, err := monitor.ParseMeminfo(bytes.NewReader(meminfo)); err == nil {
		s.MemAvailable = mi["MemAvailable"]
		if tot, ok := mi["SwapTotal"]; ok {
			s.SwapUsed = tot - min(tot, mi["SwapFree"])
			s.HaveSwapStats = true
		}
		s.ZswapPool, s.ZswapStored = mi["Zswap"], mi["Zswapped"]
	}
	if p, err := monitor.ParsePSI(bytes.NewReader(psi)); err == nil && p.Some != nil {
		s.HavePSI = true
		s.SomeAvg10, s.SomeTotalUs = p.Some.Avg10, p.Some.TotalUs
		if p.Full != nil {
			s.FullAvg10, s.FullTotalUs = p.Full.Avg10, p.Full.TotalUs
		}
	}
	return s
}

// CSVHeader и CSV — формат samples.csv.
const CSVHeader = "t_ms,mem_available_mib,swap_used_mib,zswap_pool_mib,zswap_stored_mib,psi_some_avg10,psi_full_avg10,psi_some_total_us,psi_full_total_us"

func (s Sample) CSV() string {
	const mib = 1 << 20
	return fmt.Sprintf("%d,%.1f,%.1f,%.1f,%.1f,%.2f,%.2f,%d,%d",
		s.T.Milliseconds(), float64(s.MemAvailable)/mib, float64(s.SwapUsed)/mib,
		float64(s.ZswapPool)/mib, float64(s.ZswapStored)/mib, s.SomeAvg10, s.FullAvg10, s.SomeTotalUs, s.FullTotalUs)
}

// SampleSummary — итог сэмплирования.
type SampleSummary struct {
	Samples          int     `json:"samples"`
	PeakSwapUsedMiB  float64 `json:"peak_swap_used_mib"`
	PeakZswapPoolMiB float64 `json:"peak_zswap_pool_mib"`
	MinMemAvailMiB   float64 `json:"min_mem_available_mib"`
	PSISomePeak      float64 `json:"psi_some_peak"` // пик avg10, %
	PSIFullPeak      float64 `json:"psi_full_peak"`
	// PSISomeAvg/PSIFullAvg — средняя доля времени простоя за весь замер,
	// по счётчику total (точнее, чем среднее от avg10).
	PSISomeAvg float64 `json:"psi_some_avg"`
	PSIFullAvg float64 `json:"psi_full_avg"`
}

// SummarizeSamples сводит замеры.
func SummarizeSamples(ss []Sample) SampleSummary {
	const mib = 1 << 20
	sum := SampleSummary{Samples: len(ss)}
	if len(ss) == 0 {
		return sum
	}
	sum.MinMemAvailMiB = float64(ss[0].MemAvailable) / mib
	for _, s := range ss {
		sum.PeakSwapUsedMiB = max(sum.PeakSwapUsedMiB, float64(s.SwapUsed)/mib)
		sum.PeakZswapPoolMiB = max(sum.PeakZswapPoolMiB, float64(s.ZswapPool)/mib)
		sum.MinMemAvailMiB = min(sum.MinMemAvailMiB, float64(s.MemAvailable)/mib)
		sum.PSISomePeak = max(sum.PSISomePeak, s.SomeAvg10)
		sum.PSIFullPeak = max(sum.PSIFullPeak, s.FullAvg10)
	}
	first, last := ss[0], ss[len(ss)-1]
	if dt := float64((last.T - first.T).Microseconds()); dt > 0 && first.HavePSI && last.HavePSI {
		sum.PSISomeAvg = float64(last.SomeTotalUs-min(last.SomeTotalUs, first.SomeTotalUs)) / dt * 100
		sum.PSIFullAvg = float64(last.FullTotalUs-min(last.FullTotalUs, first.FullTotalUs)) / dt * 100
	}
	return sum
}

func runSample(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	interval := fs.Duration("interval", 200*time.Millisecond, "период сэмплирования")
	out := fs.String("out", "samples.csv", "CSV с замерами")
	summary := fs.String("summary", "", "JSON-сводка (по умолчанию stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, CSVHeader)

	var samples []Sample
	start := time.Now()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		mi, _ := os.ReadFile("/proc/meminfo")
		psi, _ := os.ReadFile("/proc/pressure/memory")
		s := MakeSample(time.Since(start), mi, psi)
		samples = append(samples, s)
		fmt.Fprintln(w, s.CSV())
		select {
		case <-ctx.Done():
			if err := w.Flush(); err != nil {
				return err
			}
			return writeJSON(*summary, SummarizeSamples(samples))
		case <-ticker.C:
		}
	}
}
