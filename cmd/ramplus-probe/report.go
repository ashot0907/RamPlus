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
	"sort"
	"strconv"
	"strings"
)

// Meta — описание прогона, пишет bench.sh (runs/<имя>/meta.json).
type Meta struct {
	Config    string  `json:"config"`
	Workload  string  `json:"workload"`
	Data      string  `json:"data"`
	Run       int     `json:"run"`
	Status    string  `json:"status"` // ok | failed | oom | timeout | skipped
	ExitCode  int     `json:"exit_code"`
	DurationS float64 `json:"duration_s"`
	Note      string  `json:"note"`
}

// Env — окружение бенчмарка (env.json).
type Env struct {
	Date        string `json:"date"`
	Kernel      string `json:"kernel"`
	Virt        string `json:"virt"`
	MemTotalMiB int    `json:"mem_total_mib"`
	SwapSize    string `json:"swap_size"`
	WorkSize    string `json:"work_size"`
	DurationS   int    `json:"duration_s"`
	Runs        int    `json:"runs"`
	RootDisk    string `json:"root_disk"`
}

// Run — прогон со всеми результатами.
type Run struct {
	Meta
	Sample  *SampleSummary
	Latency *LatencySummary
	Result  *WorkloadResult // workload.json (нагрузка ramplus-probe)
}

// configOrder — порядок конфигураций в отчёте.
var configOrder = map[string]int{"no-swap": 0, "swap": 1, "zswap-lz4": 2, "zswap-zstd": 3, "zswap-lzo-zbud": 4}

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
		runs = append(runs, r)
	}
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		if a.Data != b.Data {
			return a.Data < b.Data
		}
		if configOrder[a.Config] != configOrder[b.Config] {
			return configOrder[a.Config] < configOrder[b.Config]
		}
		return a.Run < b.Run
	})
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
	_ = cw.Write([]string{"config", "workload", "data", "run", "status", "exit_code", "duration_s",
		"peak_swap_used_mib", "peak_zswap_pool_mib", "min_mem_available_mib",
		"psi_some_peak", "psi_some_avg", "psi_full_peak", "psi_full_avg",
		"lat_p50_ms", "lat_p99_ms", "lat_max_ms", "hot_touch_gib_per_s", "note"})
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
		_ = cw.Write([]string{r.Config, r.Workload, r.Data, strconv.Itoa(r.Run), r.Status, strconv.Itoa(r.ExitCode), num(r.DurationS),
			num(s.PeakSwapUsedMiB), num(s.PeakZswapPoolMiB), num(s.MinMemAvailMiB),
			num(s.PSISomePeak), num(s.PSISomeAvg), num(s.PSIFullPeak), num(s.PSIFullAvg),
			num(l.P50Ms), num(l.P99Ms), num(l.MaxMs), num(gibps), r.Note})
	}
	cw.Flush()
	return cw.Error()
}

// Group — медианы по конфигурации для одной нагрузки.
type Group struct {
	Workload, Data, Config string
	Total, OK              int
	Statuses               map[string]int
	Duration, PeakSwap     float64
	PeakZswap              float64
	SomePeak, SomeAvg      float64
	FullPeak, FullAvg      float64
	LatP50, LatP99         float64
	GiBps                  float64
}

// GroupRuns считает медианы по успешным прогонам.
func GroupRuns(runs []Run) []Group {
	type key struct{ w, d, c string }
	idx := map[key]int{}
	var groups []Group
	vals := map[key]map[string][]float64{}
	for _, r := range runs {
		k := key{r.Workload, r.Data, r.Config}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, Group{Workload: r.Workload, Data: r.Data, Config: r.Config, Statuses: map[string]int{}})
			vals[k] = map[string][]float64{}
		}
		g := &groups[i]
		g.Total++
		g.Statuses[r.Status]++
		if r.Status != "ok" {
			continue
		}
		g.OK++
		v := vals[k]
		v["dur"] = append(v["dur"], r.DurationS)
		if s := r.Sample; s != nil {
			v["swap"] = append(v["swap"], s.PeakSwapUsedMiB)
			v["zswap"] = append(v["zswap"], s.PeakZswapPoolMiB)
			v["sp"] = append(v["sp"], s.PSISomePeak)
			v["sa"] = append(v["sa"], s.PSISomeAvg)
			v["fp"] = append(v["fp"], s.PSIFullPeak)
			v["fa"] = append(v["fa"], s.PSIFullAvg)
		}
		if l := r.Latency; l != nil && l.Samples > 0 {
			v["p50"] = append(v["p50"], l.P50Ms)
			v["p99"] = append(v["p99"], l.P99Ms)
		}
		if w := r.Result; w != nil {
			v["gibps"] = append(v["gibps"], w.GiBps)
		}
	}
	for k, i := range idx {
		v, g := vals[k], &groups[i]
		g.Duration, g.PeakSwap, g.PeakZswap = Median(v["dur"]), Median(v["swap"]), Median(v["zswap"])
		g.SomePeak, g.SomeAvg, g.FullPeak, g.FullAvg = Median(v["sp"]), Median(v["sa"]), Median(v["fp"]), Median(v["fa"])
		g.LatP50, g.LatP99, g.GiBps = Median(v["p50"]), Median(v["p99"]), Median(v["gibps"])
	}
	return groups
}

func cell(v float64, prec int) string {
	if math.IsNaN(v) {
		return "—"
	}
	return strconv.FormatFloat(v, 'f', prec, 64)
}

func pair(a, b float64, prec int) string {
	if math.IsNaN(a) && math.IsNaN(b) {
		return "—"
	}
	return cell(a, prec) + " / " + cell(b, prec)
}

// IsVM — окружение виртуальное (цифры производительности нерепрезентативны).
func (e Env) IsVM() bool { return e.Virt != "" && e.Virt != "none" }

// WriteMarkdown пишет summary.md.
func WriteMarkdown(w io.Writer, env Env, groups []Group) error {
	var b strings.Builder
	b.WriteString("# RamPlus bench — сводка\n\n")
	fmt.Fprintf(&b, "- дата: %s\n- ядро: %s\n- виртуализация: %s\n- RAM: %d MiB, swapfile: %s, нагрузка: %s, %d с, прогонов: %d\n- корневой диск: %s\n\n",
		env.Date, env.Kernel, orDash(env.Virt), env.MemTotalMiB, env.SwapSize, env.WorkSize, env.DurationS, env.Runs, orDash(env.RootDisk))
	if env.IsVM() {
		b.WriteString("> **Запуск в VM: цифры производительности нерепрезентативны.** Диск VM кэшируется в RAM хоста, " +
			"поэтому swap выглядит быстрее реального. Для выбора значений по умолчанию нужен запуск на реальном SSD.\n\n")
	}
	b.WriteString("Медианы по успешным прогонам. PSI — % времени, когда задачи ждали память (пик avg10 / среднее за прогон). " +
		"Latency — задержка обхода 50 MiB памяти маленьким процессом раз в 100 мс.\n")

	cur := ""
	for _, g := range groups {
		title := g.Workload + " / " + g.Data
		if title != cur {
			cur = title
			fmt.Fprintf(&b, "\n## %s\n\n", title)
			b.WriteString("| конфигурация | успех | длительность, с | пик swap, MiB | пик zswap, MiB | PSI some пик/ср, % | PSI full пик/ср, % | latency p50/p99, мс | обход горячих страниц, GiB/с |\n")
			b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			g.Config, successCell(g), cell(g.Duration, 1), cell(g.PeakSwap, 0), cell(g.PeakZswap, 0),
			pair(g.SomePeak, g.SomeAvg, 1), pair(g.FullPeak, g.FullAvg, 1), pair(g.LatP50, g.LatP99, 1), cell(g.GiBps, 2))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func successCell(g Group) string {
	s := fmt.Sprintf("%d/%d", g.OK, g.Total)
	var other []string
	for st, n := range g.Statuses {
		if st != "ok" {
			other = append(other, fmt.Sprintf("%s ×%d", st, n))
		}
	}
	sort.Strings(other)
	if len(other) > 0 {
		s += " (" + strings.Join(other, ", ") + ")"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func runReport(args []string) error {
	if len(args) != 1 {
		return errors.New("использование: ramplus-probe report <каталог>")
	}
	dir := args[0]
	runs, err := LoadRuns(dir)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return fmt.Errorf("в %s/runs нет прогонов", dir)
	}
	var env Env
	if _, err := readJSON(filepath.Join(dir, "env.json"), &env); err != nil {
		return fmt.Errorf("env.json: %w", err)
	}
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
	md, err := os.Create(filepath.Join(dir, "summary.md"))
	if err != nil {
		return err
	}
	defer md.Close()
	return WriteMarkdown(md, env, GroupRuns(runs))
}
