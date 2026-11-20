package main

import (
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
	if len(runs) != 6 {
		t.Fatalf("runs = %d", len(runs))
	}
	for _, r := range runs {
		if r.Config == "swap" && r.Run == 3 && r.Latency != nil {
			t.Error("empty latency.json (process killed) must be ignored")
		}
	}
	var csvOut bytes.Buffer
	if err := WriteCSV(&csvOut, runs); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csvOut.String()), "\n")
	if len(lines) != 7 || !strings.Contains(lines[0], "zswap_pool_limit_hit,zswap_written_back_pages") {
		t.Errorf("csv:\n%s", csvOut.String())
	}
	if !strings.Contains(csvOut.String(), "zswap-lz4,cold,random,1,ok,0,30.20,,,,,,,,,,,,20,12,4096,8,0,") {
		t.Errorf("zswap deltas missing:\n%s", csvOut.String())
	}

	var env Env
	if _, err := readJSON(filepath.Join(dir, "env.json"), &env); err != nil {
		t.Fatal(err)
	}
	groups := GroupRuns(runs, env)
	var swapCold Group
	for _, g := range groups {
		if g.Config == "swap" && g.Workload == "cold" {
			swapCold = g
		}
	}
	if swapCold.Total != 3 || swapCold.OK != 2 || swapCold.Duration != 35.5 || swapCold.PeakSwap != 2200 ||
		swapCold.SomePeak != 75 || swapCold.LatP99 != 90 || swapCold.LatMax != 500 {
		t.Errorf("swap/cold group = %+v", swapCold)
	}

	var md bytes.Buffer
	if err := WriteMarkdown(&md, env, runs); err != nil {
		t.Fatal(err)
	}
	out := md.String()
	for _, want := range []string{
		"Run in a VM: performance numbers are not representative",
		"## cold / random",
		"| swap | 2/3 (oom ×1) | 35.5 | 2200 | 0 | — | 75.0 / 32.0 | 45.0 / 13.0 | 1.3 / 90.0 | 2.50 |",
		"| zswap-lz4 | 1/1 | 30.2 |",
		"| yes (12) |",
		"| zswap-zstd | 0/1 (skipped ×1) | — |",
		"| no-swap | 0/1 (failed ×1) | — |",
		"## Observations",
		"The zswap pool limit was hit (debugfs): zswap-lz4 — cold / random (pool_limit_hit +12)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	env.Virt = "none"
	md.Reset()
	WriteMarkdown(&md, env, runs)
	if strings.Contains(md.String(), "not representative") {
		t.Error("no VM warning on bare metal")
	}
}

func TestAllocate(t *testing.T) {
	enomem := func(int) ([]byte, error) { return nil, unix.ENOMEM }
	_, err := allocate(5<<30, enomem)
	var nm *NoMemoryError
	if !errors.As(err, &nm) || !strings.Contains(err.Error(), "could not allocate 5120 MiB: not enough memory") {
		t.Fatalf("err = %v", err)
	}
	if exitCode(err) != ExitRefused || exitCode(nil) != 0 || exitCode(errors.New("x")) != 1 {
		t.Error("exit codes")
	}
	other := func(int) ([]byte, error) { return nil, unix.EINVAL }
	if _, err := allocate(1<<20, other); err == nil || errors.As(err, &nm) {
		t.Errorf("EINVAL is a tool error, not refusal: %v", err)
	}
	// Настоящий mmap размером 1 PiB: больше адресного пространства — ядро
	// отказывает при любом режиме overcommit.
	if _, err := allocate(1<<50, mmapAnon); !errors.As(err, &nm) {
		t.Errorf("1 PiB: want NoMemoryError, got %v", err)
	}
	b, err := allocate(1<<20, mmapAnon)
	if err != nil || len(b) != 1<<20 {
		t.Fatalf("1 MiB: %v", err)
	}
	unix.Munmap(b)
}

// Раньше здесь была фатальная ошибка рантайма Go (код 2, трейс): make()
// на отказ ядра не возвращает ошибку. Теперь — штатный отказ.
func TestWorkloadRefusedWithoutPanic(t *testing.T) {
	err := runWorkload(context.Background(), []string{"--size", "1048576G", "--duration", "1s"})
	var nm *NoMemoryError
	if !errors.As(err, &nm) || exitCode(err) != ExitRefused {
		t.Fatalf("err = %v", err)
	}
	if err := runLatency(context.Background(), []string{"--size", "1048576G", "--duration", "1s"}); exitCode(err) != ExitRefused {
		t.Fatalf("latency: %v", err)
	}
}

func TestClassify(t *testing.T) {
	stressNoRes := "stress-ng: info:  [16892] setting to a 10 secs run per stressor\n" +
		"stress-ng: info:  [16892] skipped: 1: vm (1)\n" +
		"stress-ng: info:  [16892] successful run completed in 10.01 secs"
	tests := []struct {
		name   string
		in     ClassifyInput
		status string
		note   string
	}{
		{"ok", ClassifyInput{Workload: "cold", ExitCode: 0}, StatusOK, ""},
		{"probe refused", ClassifyInput{Workload: "cold", ExitCode: ExitRefused,
			Log: "ramplus-probe: не удалось выделить 5082 MiB: недостаточно памяти (cannot allocate memory)"}, StatusRefused, "5082 MiB"},
		{"stress-ng no resource", ClassifyInput{Workload: "hot", ExitCode: 3, Log: stressNoRes}, StatusRefused, "skipped"},
		{"stress-ng no resource, old log", ClassifyInput{Workload: "hot", ExitCode: 3,
			Log: "stress-ng: info:  [16892] successful run completed in 10.01 secs"}, StatusRefused, "code 3"},
		{"stress-ng no available memory", ClassifyInput{Workload: "hot", ExitCode: 2,
			Log: "stress-ng: error: [1] vm: no available memory"}, StatusRefused, "no available memory"},
		{"go fatal error", ClassifyInput{Workload: "cold", ExitCode: 2,
			Log: "runtime: out of memory\nfatal error: out of memory\n\ngoroutine 1 [running]:\n\t/x/signal.go:303 +0x165"}, StatusFailed, "fatal error: out of memory"},
		{"oom killer", ClassifyInput{Workload: "cold", ExitCode: 137, DurationS: 40, LimitS: 210}, StatusOOM, "SIGKILL"},
		{"timeout", ClassifyInput{Workload: "hot", ExitCode: 137, DurationS: 210, LimitS: 210}, StatusTimeout, "210"},
		{"tool error", ClassifyInput{Workload: "cold", ExitCode: 1, Log: "ramplus-probe: --size обязателен"}, StatusFailed, "--size обязателен"},
	}
	for _, tt := range tests {
		status, note := Classify(tt.in)
		if status != tt.status || !strings.Contains(note, tt.note) {
			t.Errorf("%s: got %s %q, want %s ~%q", tt.name, status, note, tt.status, tt.note)
		}
	}
}

func TestZswapCounters(t *testing.T) {
	z := ReadZswapCounters(filepath.Join("testdata", "zswap-debugfs"))
	if !z.Available || z.Counters["pool_limit_hit"] != 12 || len(z.Counters) != 5 {
		t.Errorf("counters = %+v", z)
	}
	if z := ReadZswapCounters(filepath.Join(t.TempDir(), "absent")); z.Available || z.Error == "" {
		t.Errorf("absent dir: %+v", z)
	}
	before := ZswapCounters{Available: true, Counters: map[string]uint64{"pool_limit_hit": 5, "written_back_pages": 100}}
	after := ZswapCounters{Available: true, Counters: map[string]uint64{"pool_limit_hit": 9, "written_back_pages": 100, "reject_reclaim_fail": 3}}
	d := ZswapDelta(before, after)
	if d["pool_limit_hit"] != 4 || d["written_back_pages"] != 0 || len(d) != 2 {
		t.Errorf("delta = %v", d)
	}
	if ZswapDelta(ZswapCounters{}, after) != nil {
		t.Error("no debugfs → nil")
	}
}

func TestPoolPercent(t *testing.T) {
	for cfg, want := range map[string]int{"swap": 0, "no-swap": 0, "zswap-lz4": 20, "zswap-lz4-p35": 35, "zswap-lz4-p50": 50, "zswap-lzo-zbud": 20} {
		if got := PoolPercent(Run{Meta: Meta{Config: cfg}}); got != want {
			t.Errorf("PoolPercent(%s) = %d, want %d", cfg, got, want)
		}
	}
	if PoolPercent(Run{Meta: Meta{Config: "zswap-lz4", MaxPoolPercent: 33}}) != 33 {
		t.Error("meta wins over name")
	}
}

// Наблюдения на результатах --quick из VM (results.csv прогона).
func TestObservationsVMQuick(t *testing.T) {
	dir := filepath.Join("testdata", "vm-quick")
	f, err := os.Open(filepath.Join(dir, "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runs, err := LoadRunsCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	var env Env
	if _, err := readJSON(filepath.Join(dir, "env.json"), &env); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, r := range runs {
		statuses[r.Config+"/"+r.Workload+"/"+r.Data] = r.Status
	}
	// stress-ng с кодом 3 — отказ системы; паника старого probe — ошибка инструмента.
	if statuses["no-swap/hot/stress-ng"] != StatusRefused || statuses["no-swap/cold/random"] != StatusFailed {
		t.Errorf("reclassification: %v", statuses)
	}
	obs := strings.Join(Observations(env, runs, GroupRuns(runs, env)), "\n")
	for _, want := range []string{
		"single runs, conclusions are unreliable",
		"hot / stress-ng — swap, max 66942 ms",
		"hot / stress-ng: p99 66942.2 ms (swap), max 66942.2 ms (swap) — **freeze**",
		"cold / compressible: p99 5.6 ms (zswap-lzo-zbud), max 194.4 ms (zswap-zstd)",
		"limit 20% RAM = 782 MiB",
		"zswap-zstd — hot-probe / compressible (peak 782 MiB)",
		"zswap-lzo-zbud — cold / compressible (peak 765 MiB)",
		"Unusually long: hot-probe / compressible — zswap-zstd 106.1 s with a median of 24.6 s across configurations (the zswap pool hit the limit)",
		"Unusually long: hot / stress-ng — zswap-lzo-zbud 128.7 s with a median of 43.0 s across configurations (the zswap pool hit the limit)",
		"refused — expected without swap",
		"no-swap: hot / stress-ng",
		"**Tool errors (failed)**",
	} {
		if !strings.Contains(obs, want) {
			t.Errorf("observations lack %q:\n%s", want, obs)
		}
	}
	if strings.Contains(obs, "zswap-lz4 — cold / random") {
		t.Error("cold/random zswap peak 65 MiB is far from the limit")
	}
}
