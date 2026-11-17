package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Статусы прогона.
const (
	StatusOK      = "ok"
	StatusRefused = "refused" // система отказала в памяти — ожидаемо, например, для no-swap
	StatusOOM     = "oom"     // убит SIGKILL (OOM killer)
	StatusTimeout = "timeout" // убит по страховочному таймауту
	StatusFailed  = "failed"  // ошибка инструмента
	StatusSkipped = "skipped" // конфигурация недоступна на этом ядре
)

// stress-ng: EXIT_NO_RESOURCE — стрессор не запустился из-за нехватки
// ресурсов (сам stress-ng при этом пишет "successful run completed").
const stressNgNoResource = 3

var refusalMarkers = []string{"no available memory", "out of resources", "not enough memory", "skipped", "недостаточно памяти"}

// ClassifyInput — данные о завершившейся нагрузке.
type ClassifyInput struct {
	Workload  string
	ExitCode  int
	Log       string
	DurationS float64
	LimitS    float64
}

// Classify определяет статус прогона и короткое пояснение.
func Classify(in ClassifyInput) (status, note string) {
	stressNg := in.Workload == "hot"
	logLow := strings.ToLower(in.Log)
	refusal := ""
	for _, m := range refusalMarkers {
		if strings.Contains(logLow, m) {
			refusal = m
			break
		}
	}
	switch {
	case in.ExitCode == 0:
		return StatusOK, ""
	case !stressNg && in.ExitCode == ExitRefused:
		return StatusRefused, "система отказала в выделении памяти: " + logLine(in.Log, "недостаточно памяти")
	case stressNg && (in.ExitCode == stressNgNoResource || refusal != ""):
		return StatusRefused, fmt.Sprintf("stress-ng: нет ресурсов (код %d): %s", in.ExitCode, logLine(in.Log, refusal))
	case in.ExitCode == 137 || in.ExitCode == -9:
		if in.LimitS > 0 && in.DurationS >= in.LimitS-1 {
			return StatusTimeout, fmt.Sprintf("убит по таймауту %.0f с", in.LimitS)
		}
		return StatusOOM, "SIGKILL (вероятно OOM killer)"
	default:
		return StatusFailed, fmt.Sprintf("код выхода %d: %s", in.ExitCode, logLine(in.Log, "fatal error", "panic:", "ramplus-probe:", "error"))
	}
}

// logLine — первая строка лога с одним из маркеров, иначе последняя
// непустая; обрезается до 160 символов.
func logLine(log string, markers ...string) string {
	var last string
	sc := bufio.NewScanner(strings.NewReader(log))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		for _, m := range markers {
			if m != "" && strings.Contains(low, strings.ToLower(m)) {
				return trim(line)
			}
		}
		last = line
	}
	return trim(last)
}

func trim(s string) string {
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}

func runClassify(args []string) error {
	fs := flag.NewFlagSet("classify", flag.ContinueOnError)
	in := ClassifyInput{}
	fs.StringVar(&in.Workload, "workload", "", "hot | hot-probe | cold")
	fs.IntVar(&in.ExitCode, "rc", 0, "код выхода нагрузки")
	logPath := fs.String("log", "", "лог нагрузки")
	fs.Float64Var(&in.DurationS, "duration", 0, "длительность, с")
	fs.Float64Var(&in.LimitS, "limit", 0, "страховочный таймаут, с")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *logPath != "" {
		if b, err := os.ReadFile(*logPath); err == nil {
			in.Log = string(b)
		}
	}
	status, note := Classify(in)
	// TSV: статус и пояснение (без табов и переводов строк).
	fmt.Printf("%s\t%s\n", status, strings.NewReplacer("\t", " ", "\n", " ").Replace(note))
	return nil
}

// ZswapCounters — счётчики /sys/kernel/debug/zswap (нужен root и debugfs).
type ZswapCounters struct {
	Available bool              `json:"available"`
	Error     string            `json:"error,omitempty"`
	Counters  map[string]uint64 `json:"counters"`
}

// ReadZswapCounters читает числовые файлы каталога.
func ReadZswapCounters(dir string) ZswapCounters {
	z := ZswapCounters{Counters: map[string]uint64{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		z.Error = err.Error()
		return z
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
			z.Counters[e.Name()] = v
		}
	}
	z.Available = len(z.Counters) > 0
	if !z.Available && z.Error == "" {
		z.Error = "нет числовых счётчиков"
	}
	return z
}

func runZswapStat(args []string) error {
	fs := flag.NewFlagSet("zswapstat", flag.ContinueOnError)
	dir := fs.String("dir", "/sys/kernel/debug/zswap", "каталог debugfs zswap")
	out := fs.String("out", "", "JSON (по умолчанию stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return writeJSON(*out, ReadZswapCounters(*dir))
}
