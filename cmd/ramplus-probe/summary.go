package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Пороги для наблюдений.
const (
	FreezeMs          = 1000.0 // latency max выше — «фриз»
	OutlierFactor     = 2.0    // длительность выше медианы по конфигурациям во столько раз — выброс
	PoolNearLimitFrac = 0.97   // пик пула ≥ 97% лимита — упор в max_pool_percent (без debugfs)
	MinReliableRuns   = 3
)

// Group — медианы по конфигурации для одной нагрузки.
type Group struct {
	Workload, Data, Config string
	Total, OK              int
	Statuses               map[string]int
	Notes                  []string
	Duration, PeakSwap     float64
	PeakZswap              float64
	SomePeak, SomeAvg      float64
	FullPeak, FullAvg      float64
	LatP50, LatP99, LatMax float64 // LatMax — максимум по прогонам
	GiBps                  float64
	PoolPercent            int  // 0 — без zswap
	PoolHits               int  // макс. прирост pool_limit_hit; -1 — нет данных debugfs
	WrittenBack            int  // макс. прирост written_back_pages; -1 — нет данных
	PoolNearLimit          bool // пик пула ≈ лимит (оценка без debugfs)
}

// GroupRuns считает медианы по успешным прогонам.
func GroupRuns(runs []Run, env Env) []Group {
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
			groups = append(groups, Group{Workload: r.Workload, Data: r.Data, Config: r.Config, Statuses: map[string]int{},
				PoolPercent: PoolPercent(r), PoolHits: -1, WrittenBack: -1, LatMax: math.NaN()})
			vals[k] = map[string][]float64{}
		}
		g := &groups[i]
		g.Total++
		g.Statuses[r.Status]++
		if r.Status != StatusOK {
			if r.Note != "" {
				g.Notes = append(g.Notes, r.Note)
			}
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
			if limit := poolLimitMiB(env, g.PoolPercent); limit > 0 && s.PeakZswapPoolMiB >= PoolNearLimitFrac*limit {
				g.PoolNearLimit = true
			}
		}
		if l := r.Latency; l != nil && l.Samples > 0 {
			v["p50"] = append(v["p50"], l.P50Ms)
			v["p99"] = append(v["p99"], l.P99Ms)
			if math.IsNaN(g.LatMax) || l.MaxMs > g.LatMax {
				g.LatMax = l.MaxMs
			}
		}
		if w := r.Result; w != nil {
			v["gibps"] = append(v["gibps"], w.GiBps)
		}
		if d := r.ZswapDelta; d != nil {
			if h, ok := d["pool_limit_hit"]; ok {
				g.PoolHits = max(g.PoolHits, int(h))
			}
			if wb, ok := d["written_back_pages"]; ok {
				g.WrittenBack = max(g.WrittenBack, int(wb))
			}
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

func poolLimitMiB(env Env, pct int) float64 {
	if pct <= 0 || env.MemTotalMiB <= 0 {
		return 0
	}
	return float64(env.MemTotalMiB) * float64(pct) / 100
}

// PoolCell — колонка «лимит пула».
func PoolCell(g Group) string {
	switch {
	case g.PoolPercent == 0:
		return "—"
	case g.PoolHits > 0:
		return fmt.Sprintf("yes (%d)", g.PoolHits)
	case g.PoolHits == 0:
		return "no"
	case g.OK == 0:
		return "—"
	case g.PoolNearLimit:
		return fmt.Sprintf("likely: peak ≈ %d%% RAM", g.PoolPercent)
	default:
		return "no (by peak)"
	}
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

func successCell(g Group) string {
	s := fmt.Sprintf("%d/%d", g.OK, g.Total)
	var other []string
	for st, n := range g.Statuses {
		if st != StatusOK {
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

// WriteMarkdown пишет summary.md.
func WriteMarkdown(w io.Writer, env Env, runs []Run) error {
	groups := GroupRuns(runs, env)
	var b strings.Builder
	b.WriteString("# RamPlus bench — summary\n\n")
	fmt.Fprintf(&b, "- date: %s\n- kernel: %s\n- virtualization: %s\n- RAM: %d MiB, swapfile: %s, load: %s, %d s, runs: %d\n- root disk: %s\n\n",
		env.Date, env.Kernel, orDash(env.Virt), env.MemTotalMiB, env.SwapSize, env.WorkSize, env.DurationS, env.Runs, orDash(env.RootDisk))
	if env.IsVM() {
		b.WriteString("> **Run in a VM: performance numbers are not representative.** The VM disk is cached in the host RAM, " +
			"so swap looks faster than it really is. Default values must be chosen from runs on a real SSD.\n\n")
	}
	b.WriteString("Medians over successful runs. PSI — % of time tasks waited for memory (peak avg10 / average over the run). " +
		"Latency — the delay of a small process walking 50 MiB of memory every 100 ms. " +
		"Statuses: refused — the system denied memory (expected without swap), failed — a tool error. " +
		"Pool limit — pool_limit_hit events (debugfs) or, without debugfs, a pool peak ≈ max_pool_percent × RAM.\n")

	cur := ""
	for _, g := range groups {
		title := g.Workload + " / " + g.Data
		if title != cur {
			cur = title
			fmt.Fprintf(&b, "\n## %s\n\n", title)
			b.WriteString("| configuration | success | duration, s | peak swap, MiB | peak zswap, MiB | pool limit | PSI some peak/avg, % | PSI full peak/avg, % | latency p50/p99, ms | hot-page walk, GiB/s |\n")
			b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			g.Config, successCell(g), cell(g.Duration, 1), cell(g.PeakSwap, 0), cell(g.PeakZswap, 0), PoolCell(g),
			pair(g.SomePeak, g.SomeAvg, 1), pair(g.FullPeak, g.FullAvg, 1), pair(g.LatP50, g.LatP99, 1), cell(g.GiBps, 2))
	}

	b.WriteString("\n## Observations\n\n")
	for _, o := range Observations(env, runs, groups) {
		b.WriteString("- " + o + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Observations — автоматические выводы по сводке.
func Observations(env Env, runs []Run, groups []Group) []string {
	var out []string

	n := env.Runs
	if n == 0 {
		for _, r := range runs {
			n = max(n, r.Run)
		}
	}
	switch {
	case n == 1:
		out = append(out, "Runs per configuration: 1 — **single runs, conclusions are unreliable**.")
	case n < MinReliableRuns:
		out = append(out, fmt.Sprintf("Runs per configuration: %d (< %d) — conclusions are unreliable.", n, MinReliableRuns))
	}
	if env.IsVM() {
		out = append(out, "Run in a VM: absolute numbers and swap/zswap ratios are not representative.")
	}

	type wd struct{ w, d string }
	var order []wd
	byLoad := map[wd][]Group{}
	for _, g := range groups {
		k := wd{g.Workload, g.Data}
		if _, ok := byLoad[k]; !ok {
			order = append(order, k)
		}
		byLoad[k] = append(byLoad[k], g)
	}

	// Худшая latency и фризы.
	var latLines, freezes []string
	for _, k := range order {
		var p99g, maxg *Group
		for i := range byLoad[k] {
			g := &byLoad[k][i]
			if g.OK == 0 {
				continue
			}
			if !math.IsNaN(g.LatP99) && (p99g == nil || g.LatP99 > p99g.LatP99) {
				p99g = g
			}
			if !math.IsNaN(g.LatMax) && (maxg == nil || g.LatMax > maxg.LatMax) {
				maxg = g
			}
		}
		if p99g == nil || maxg == nil {
			continue
		}
		line := fmt.Sprintf("%s / %s: p99 %s ms (%s), max %s ms (%s)", k.w, k.d, cell(p99g.LatP99, 1), p99g.Config, cell(maxg.LatMax, 1), maxg.Config)
		if maxg.LatMax > FreezeMs {
			line += " — **freeze**"
			freezes = append(freezes, fmt.Sprintf("%s / %s — %s, max %s ms", k.w, k.d, maxg.Config, cell(maxg.LatMax, 0)))
		}
		latLines = append(latLines, line)
	}
	if len(freezes) > 0 {
		out = append(out, fmt.Sprintf("**Freezes** (latency max > %.0f ms): %s.", FreezeMs, strings.Join(freezes, "; ")))
	}
	if len(latLines) > 0 {
		out = append(out, "Worst latency per workload: "+strings.Join(latLines, "; ")+".")
	}

	// Лимит пула zswap.
	var hit, near []string
	for _, g := range groups {
		switch {
		case g.PoolHits > 0:
			hit = append(hit, fmt.Sprintf("%s — %s / %s (pool_limit_hit +%d)", g.Config, g.Workload, g.Data, g.PoolHits))
		case g.PoolHits < 0 && g.PoolNearLimit:
			near = append(near, fmt.Sprintf("%s — %s / %s (peak %s MiB)", g.Config, g.Workload, g.Data, cell(g.PeakZswap, 0)))
		}
	}
	if len(hit) > 0 {
		out = append(out, "The zswap pool limit was hit (debugfs): "+strings.Join(hit, "; ")+".")
	}
	if len(near) > 0 {
		out = append(out, fmt.Sprintf("The zswap pool reached max_pool_percent (estimated from the peak, no debugfs; limit 20%% RAM = %.0f MiB): %s.",
			poolLimitMiB(env, 20), strings.Join(near, "; ")))
	}

	// Выбросы по длительности.
	for _, k := range order {
		var durs []float64
		for _, g := range byLoad[k] {
			if g.OK > 0 && !math.IsNaN(g.Duration) {
				durs = append(durs, g.Duration)
			}
		}
		if len(durs) < 3 {
			continue
		}
		med := Median(durs)
		for _, g := range byLoad[k] {
			if g.OK > 0 && g.Duration > OutlierFactor*med {
				line := fmt.Sprintf("Unusually long: %s / %s — %s %s s with a median of %s s across configurations", k.w, k.d, g.Config, cell(g.Duration, 1), cell(med, 1))
				if g.PoolHits > 0 || (g.PoolHits < 0 && g.PoolNearLimit) {
					line += " (the zswap pool hit the limit)"
				}
				out = append(out, line+".")
			}
		}
	}

	// Неуспешные прогоны.
	statusLine := func(status, title string) {
		byConfig := map[string][]string{}
		var configs []string
		for _, g := range groups {
			if g.Statuses[status] == 0 {
				continue
			}
			if _, ok := byConfig[g.Config]; !ok {
				configs = append(configs, g.Config)
			}
			byConfig[g.Config] = append(byConfig[g.Config], g.Workload+" / "+g.Data)
		}
		if len(configs) == 0 {
			return
		}
		var parts []string
		for _, c := range configs {
			parts = append(parts, c+": "+strings.Join(byConfig[c], ", "))
		}
		out = append(out, title+": "+strings.Join(parts, "; ")+".")
	}
	statusLine(StatusRefused, "The system denied memory (refused — expected without swap when the load exceeds RAM)")
	statusLine(StatusOOM, "Killed by the OOM killer (oom)")
	statusLine(StatusTimeout, "Killed by the timeout (timeout)")
	statusLine(StatusFailed, "**Tool errors (failed)** — these rows are unreliable")
	statusLine(StatusSkipped, "Skipped (skipped — the configuration is not available on this kernel)")

	if len(out) == 0 {
		out = append(out, "No remarks.")
	}
	return out
}
