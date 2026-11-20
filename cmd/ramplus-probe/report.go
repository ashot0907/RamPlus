package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Meta — описание прогона, пишет bench.sh (runs/<имя>/meta.json).
type Meta struct {
	Config         string  `json:"config"`
	Workload       string  `json:"workload"`
	Data           string  `json:"data"`
	Run            int     `json:"run"`
	Status         string  `json:"status"` // ok | refused | oom | timeout | failed | skipped
	ExitCode       int     `json:"exit_code"`
	DurationS      float64 `json:"duration_s"`
	Note           string  `json:"note"`
	MaxPoolPercent int     `json:"max_pool_percent,omitempty"` // zswap: max_pool_percent этого прогона
}

// Env — окружение бенчмарка (env.json).
type Env struct {
	Date         string `json:"date"`
	Kernel       string `json:"kernel"`
	Virt         string `json:"virt"`
	MemTotalMiB  int    `json:"mem_total_mib"`
	SwapSize     string `json:"swap_size"`
	WorkSize     string `json:"work_size"`
	DurationS    int    `json:"duration_s"`
	Runs         int    `json:"runs"`
	RootDisk     string `json:"root_disk"`
	ZswapDebugfs bool   `json:"zswap_debugfs"`
}

// zswapDeltaKeys — счётчики debugfs, дельта которых пишется в отчёт.
var zswapDeltaKeys = []string{"pool_limit_hit", "written_back_pages", "reject_compress_poor", "reject_reclaim_fail"}

// Run — прогон со всеми результатами.
type Run struct {
	Meta
	Sample  *SampleSummary
	Latency *LatencySummary
	Result  *WorkloadResult // workload.json (нагрузка ramplus-probe)
	// ZswapDelta — прирост счётчиков debugfs за прогон; nil — нет данных.
	ZswapDelta map[string]uint64
}

// configOrder — порядок конфигураций в отчёте.
var configOrder = map[string]int{"no-swap": 0, "swap": 1, "zswap-lz4": 2, "zswap-lz4-p35": 3, "zswap-lz4-p50": 4, "zswap-zstd": 5, "zswap-lzo-zbud": 6}

var poolSuffix = regexp.MustCompile(`-p(\d+)$`)

// PoolPercent — max_pool_percent прогона: из meta, иначе по имени
// конфигурации (суффикс -pNN, по умолчанию 20). 0 — zswap не включён.
func PoolPercent(r Run) int {
	if !strings.HasPrefix(r.Config, "zswap") {
		return 0
	}
	if r.MaxPoolPercent > 0 {
		return r.MaxPoolPercent
	}
	if m := poolSuffix.FindStringSubmatch(r.Config); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v
	}
	return 20
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return false, nil // процесс убит до записи
	}
	return true, json.Unmarshal(b, v)
}

// ZswapDelta — прирост счётчиков между двумя снимками; nil, если данных нет.
func ZswapDelta(before, after ZswapCounters) map[string]uint64 {
	if !before.Available || !after.Available {
		return nil
	}
	d := map[string]uint64{}
	for _, k := range zswapDeltaKeys {
		b, ok1 := before.Counters[k]
		a, ok2 := after.Counters[k]
		if ok1 && ok2 && a >= b {
			d[k] = a - b
		}
	}
	return d
}

// LoadRuns читает dir/runs/*/.
func LoadRuns(dir string) ([]Run, error) {
	metas, err := filepath.Glob(filepath.Join(dir, "runs", "*", "meta.json"))
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, m := range metas {
		d := filepath.Dir(m)
		var r Run
		if _, err := readJSON(m, &r.Meta); err != nil {
			return nil, fmt.Errorf("%s: %w", m, err)
		}
		var s SampleSummary
		if ok, err := readJSON(filepath.Join(d, "sample.json"), &s); err == nil && ok {
			r.Sample = &s
		}
		var l LatencySummary
		if ok, err := readJSON(filepath.Join(d, "latency.json"), &l); err == nil && ok {
			r.Latency = &l
		}
		var w WorkloadResult
		if ok, err := readJSON(filepath.Join(d, "workload.json"), &w); err == nil && ok {
			r.Result = &w
		}
		var zb, za ZswapCounters
		okB, _ := readJSON(filepath.Join(d, "zswap-before.json"), &zb)
		okA, _ := readJSON(filepath.Join(d, "zswap-after.json"), &za)
		if okB && okA {
			r.ZswapDelta = ZswapDelta(zb, za)
		}
		runs = append(runs, r)
	}
	sortRuns(runs)
	return runs, nil
}

func sortRuns(runs []Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		if a.Data != b.Data {
			return a.Data < b.Data
		}
		if oa, ob := order(a.Config), order(b.Config); oa != ob {
			return oa < ob
		}
		return a.Run < b.Run
	})
}

func order(config string) int {
	if v, ok := configOrder[config]; ok {
		return v
	}
	return len(configOrder)
}

// LoadRunsCSV читает готовый results.csv (в том числе старого формата:
// колонки берутся по заголовку). Статусы "failed" пересчитываются текущими
// правилами Classify по коду выхода и пояснению.
func LoadRunsCSV(r io.Reader) ([]Run, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, errors.New("results.csv is empty")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	get := func(row []string, name string) string {
		if i, ok := col[name]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	f := func(row []string, name string) (float64, bool) {
		v, err := strconv.ParseFloat(get(row, name), 64)
		return v, err == nil
	}
	var runs []Run
	for _, row := range rows[1:] {
		var run Run
		run.Config, run.Workload, run.Data = get(row, "config"), get(row, "workload"), get(row, "data")
		run.Run, _ = strconv.Atoi(get(row, "run"))
		run.Status, run.Note = get(row, "status"), get(row, "note")
		run.ExitCode, _ = strconv.Atoi(get(row, "exit_code"))
		run.DurationS, _ = f(row, "duration_s")
		run.MaxPoolPercent, _ = strconv.Atoi(get(row, "max_pool_percent"))
		if run.Status == StatusFailed {
			run.Status, _ = Classify(ClassifyInput{Workload: run.Workload, ExitCode: run.ExitCode, Log: run.Note})
		}
		if v, ok := f(row, "peak_swap_used_mib"); ok {
			s := &SampleSummary{PeakSwapUsedMiB: v}
			s.PeakZswapPoolMiB, _ = f(row, "peak_zswap_pool_mib")
			s.MinMemAvailMiB, _ = f(row, "min_mem_available_mib")
			s.PSISomePeak, _ = f(row, "psi_some_peak")
			s.PSISomeAvg, _ = f(row, "psi_some_avg")
			s.PSIFullPeak, _ = f(row, "psi_full_peak")
			s.PSIFullAvg, _ = f(row, "psi_full_avg")
			run.Sample = s
		}
		if v, ok := f(row, "lat_p50_ms"); ok {
			l := &LatencySummary{Samples: 1, P50Ms: v}
			l.P99Ms, _ = f(row, "lat_p99_ms")
			l.MaxMs, _ = f(row, "lat_max_ms")
			run.Latency = l
		}
		if v, ok := f(row, "hot_touch_gib_per_s"); ok {
			run.Result = &WorkloadResult{OK: true, GiBps: v}
		}
		for _, k := range zswapDeltaKeys {
			if v, err := strconv.ParseUint(get(row, "zswap_"+k), 10, 64); err == nil {
				if run.ZswapDelta == nil {
					run.ZswapDelta = map[string]uint64{}
				}
				run.ZswapDelta[k] = v
			}
		}
		runs = append(runs, run)
	}
	sortRuns(runs)
	return runs, nil
}

func num(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// WriteCSV пишет results.csv: одна строка на прогон.
func WriteCSV(w io.Writer, runs []Run) error {
	cw := csv.NewWriter(w)
	header := []string{"config", "workload", "data", "run", "status", "exit_code", "duration_s",
		"peak_swap_used_mib", "peak_zswap_pool_mib", "min_mem_available_mib",
		"psi_some_peak", "psi_some_avg", "psi_full_peak", "psi_full_avg",
		"lat_p50_ms", "lat_p99_ms", "lat_max_ms", "hot_touch_gib_per_s", "max_pool_percent"}
	for _, k := range zswapDeltaKeys {
		header = append(header, "zswap_"+k)
	}
	_ = cw.Write(append(header, "note"))
	for _, r := range runs {
		nan := math.NaN()
		s := SampleSummary{PeakSwapUsedMiB: nan, PeakZswapPoolMiB: nan, MinMemAvailMiB: nan, PSISomePeak: nan, PSISomeAvg: nan, PSIFullPeak: nan, PSIFullAvg: nan}
		if r.Sample != nil {
			s = *r.Sample
		}
		l := LatencySummary{P50Ms: nan, P99Ms: nan, MaxMs: nan}
		if r.Latency != nil && r.Latency.Samples > 0 {
			l = *r.Latency
		}
		gibps := nan
		if r.Result != nil {
			gibps = r.Result.GiBps
		}
		pool := ""
		if p := PoolPercent(r); p > 0 {
			pool = strconv.Itoa(p)
		}
		row := []string{r.Config, r.Workload, r.Data, strconv.Itoa(r.Run), r.Status, strconv.Itoa(r.ExitCode), num(r.DurationS),
			num(s.PeakSwapUsedMiB), num(s.PeakZswapPoolMiB), num(s.MinMemAvailMiB),
			num(s.PSISomePeak), num(s.PSISomeAvg), num(s.PSIFullPeak), num(s.PSIFullAvg),
			num(l.P50Ms), num(l.P99Ms), num(l.MaxMs), num(gibps), pool}
		for _, k := range zswapDeltaKeys {
			v := ""
			if d, ok := r.ZswapDelta[k]; ok {
				v = strconv.FormatUint(d, 10)
			}
			row = append(row, v)
		}
		_ = cw.Write(append(row, r.Note))
	}
	cw.Flush()
	return cw.Error()
}

func runReport(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ramplus-probe report <dir>")
	}
	dir := args[0]
	runs, err := LoadRuns(dir)
	if err != nil {
		return err
	}
	fromCSV := false
	if len(runs) == 0 {
		// Каталог без runs/ — пересобрать сводку по готовому results.csv.
		f, err := os.Open(filepath.Join(dir, "results.csv"))
		if err != nil {
			return fmt.Errorf("%s has neither runs/ nor results.csv", dir)
		}
		runs, err = LoadRunsCSV(f)
		f.Close()
		if err != nil {
			return err
		}
		fromCSV = true
	}
	var env Env
	if _, err := readJSON(filepath.Join(dir, "env.json"), &env); err != nil {
		return fmt.Errorf("env.json: %w", err)
	}
	if !fromCSV {
		f, err := os.Create(filepath.Join(dir, "results.csv"))
		if err != nil {
			return err
		}
		if err := WriteCSV(f, runs); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	md, err := os.Create(filepath.Join(dir, "summary.md"))
	if err != nil {
		return err
	}
	defer md.Close()
	return WriteMarkdown(md, env, runs)
}
