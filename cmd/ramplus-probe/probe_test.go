package main

import (
	"bytes"
	"compress/flate"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPercentileMedian(t *testing.T) {
	v := []float64{5, 1, 4, 2, 3, 100, 6, 7, 8, 9}
	if got := Percentile(v, 50); got != 5 {
		t.Errorf("p50 = %v", got)
	}
	if got := Percentile(v, 99); got != 100 {
		t.Errorf("p99 = %v", got)
	}
	if got := Percentile(v, 0); got != 1 {
		t.Errorf("p0 = %v", got)
	}
	if got := Median([]float64{3, 1, 2}); got != 2 {
		t.Errorf("median odd = %v", got)
	}
	if got := Median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("median even = %v", got)
	}
	if !math.IsNaN(Median(nil)) || !math.IsNaN(Percentile(nil, 50)) {
		t.Error("empty → NaN")
	}
	if v[0] != 5 {
		t.Error("input must not be sorted in place")
	}
	s := Summarize([]float64{1, 2, 3, 4, 100})
	if s.Samples != 5 || s.P50Ms != 3 || s.MaxMs != 100 || s.MeanMs != 22 {
		t.Errorf("summary = %+v", s)
	}
}

func ratio(t *testing.T, data string) float64 {
	t.Helper()
	buf := make([]byte, 64*pageSize)
	if err := FillPages(buf, data, 7); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw, _ := flate.NewWriter(&out, flate.BestSpeed)
	zw.Write(buf)
	zw.Close()
	return float64(len(buf)) / float64(out.Len())
}

func TestFillPagesCompressibility(t *testing.T) {
	if r := ratio(t, DataRandom); r > 1.05 {
		t.Errorf("random data compresses %.2fx — must be incompressible", r)
	}
	if r := ratio(t, DataCompressible); r < 1.4 || r > 6 {
		t.Errorf("compressible data ratio %.2fx, want realistic ~1.5–4x", r)
	}
	// Страницы не одинаково заполнены (zswap хранит такие без сжатия).
	buf := make([]byte, 4*pageSize)
	FillPages(buf, DataCompressible, 1)
	page := buf[:pageSize]
	same := true
	for _, b := range page {
		if b != page[0] {
			same = false
			break
		}
	}
	if same || bytes.Equal(buf[:pageSize], buf[pageSize:2*pageSize]) {
		t.Error("pages must differ and not be same-filled")
	}
	if FillPages(buf, "zeros", 1) == nil {
		t.Error("unknown data type must fail")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"50M": 50 << 20, "5G": 5 << 30, "512K": 512 << 10, "1.5G": 3 << 29, "4096": 4096} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := ParseSize("x"); err == nil {
		t.Error("want error")
	}
}

const meminfo = "MemTotal: 4000000 kB\nMemAvailable: 1000000 kB\nSwapTotal: 4194304 kB\nSwapFree: 3145728 kB\nZswap: 102400 kB\nZswapped: 409600 kB\n"

func psi(some, full float64, someTotal, fullTotal int) []byte {
	return []byte(strings.Join([]string{
		"some avg10=" + num(some) + " avg60=0.00 avg300=0.00 total=" + itoa(someTotal),
		"full avg10=" + num(full) + " avg60=0.00 avg300=0.00 total=" + itoa(fullTotal),
	}, "\n"))
}

func itoa(v int) string { return strings.TrimSpace(strings.Replace(num(float64(v)), ".00", "", 1)) }

func TestSamples(t *testing.T) {
	s0 := MakeSample(0, []byte(meminfo), psi(10, 2, 1_000_000, 200_000))
	if s0.SwapUsed != 1<<30 || s0.ZswapPool != 100<<20 || s0.SomeAvg10 != 10 || !s0.HavePSI {
		t.Errorf("sample = %+v", s0)
	}
	if !strings.HasPrefix(s0.CSV(), "0,976.6,1024.0,100.0,400.0,10.00,2.00,1000000,200000") {
		t.Errorf("csv = %s", s0.CSV())
	}
	if strings.Count(CSVHeader, ",") != strings.Count(s0.CSV(), ",") {
		t.Error("header/row column mismatch")
	}
	// За 10 с счётчик some вырос на 3 с → в среднем 30% времени простоя.
	s1 := MakeSample(10*time.Second, []byte(meminfo), psi(70, 40, 4_000_000, 1_200_000))
	sum := SummarizeSamples([]Sample{s0, s1})
	if sum.PSISomePeak != 70 || sum.PSIFullPeak != 40 || math.Abs(sum.PSISomeAvg-30) > 1e-9 || math.Abs(sum.PSIFullAvg-10) > 1e-9 {
		t.Errorf("summary = %+v", sum)
	}
	// Ядро без PSI: замеры есть, PSI-поля нулевые, без паники.
	noPSI := SummarizeSamples([]Sample{MakeSample(0, []byte(meminfo), nil), MakeSample(time.Second, []byte(meminfo), nil)})
	if noPSI.Samples != 2 || noPSI.PSISomeAvg != 0 {
		t.Errorf("no psi = %+v", noPSI)
	}
}

func TestReport(t *testing.T) {
	dir := filepath.Join("testdata", "bench")
	runs, err := LoadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 5 || runs[3].Latency != nil {
		t.Fatalf("runs = %d; empty latency.json (process killed) must be ignored", len(runs))
	}
	var csvOut bytes.Buffer
	if err := WriteCSV(&csvOut, runs); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csvOut.String()), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "config,workload,data,run,status") {
		t.Errorf("csv:\n%s", csvOut.String())
	}

	groups := GroupRuns(runs)
	var swapCold Group
	for _, g := range groups {
		if g.Config == "swap" && g.Workload == "cold" {
			swapCold = g
		}
	}
	if swapCold.Total != 3 || swapCold.OK != 2 || swapCold.Duration != 35.5 || swapCold.PeakSwap != 2200 ||
		swapCold.SomePeak != 75 || swapCold.LatP99 != 90 {
		t.Errorf("swap/cold group = %+v", swapCold)
	}

	var env Env
	if _, err := readJSON(filepath.Join(dir, "env.json"), &env); err != nil {
		t.Fatal(err)
	}
	var md bytes.Buffer
	if err := WriteMarkdown(&md, env, groups); err != nil {
		t.Fatal(err)
	}
	out := md.String()
	for _, want := range []string{
		"Запуск в VM: цифры производительности нерепрезентативны",
		"## cold / random",
		"| swap | 2/3 (oom ×1) | 35.5 | 2200 | 0 | 75.0 / 32.0 | 45.0 / 13.0 | 1.3 / 90.0 | 2.50 |",
		"| zswap-zstd | 0/1 (skipped ×1) | — |",
		"| no-swap | 0/1 (failed ×1) | — |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	env.Virt = "none"
	md.Reset()
	WriteMarkdown(&md, env, groups)
	if strings.Contains(md.String(), "нерепрезентативны") {
		t.Error("no VM warning on bare metal")
	}
}
