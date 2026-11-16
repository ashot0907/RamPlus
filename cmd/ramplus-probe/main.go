// Команда ramplus-probe — вспомогательные процессы для scripts/bench.sh:
//
//	latency   каждые 100 мс трогает 50 MiB своей памяти и меряет задержку
//	workload  выделяет N байт, заполняет и работает с долей "горячей" памяти
//	sample    сэмплирует /proc/meminfo и /proc/pressure/memory
//	report    собирает results.csv и summary.md из каталога прогонов
//
// Сама по себе систему не меняет (только потребляет память).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `использование: ramplus-probe <команда> [флаги]

  latency   --size 50M --interval 100ms [--duration 0] [--out latency.json]
  workload  --size 5G --hot 0.15 --data compressible|random --duration 30s [--out workload.json]
  sample    --interval 200ms --out samples.csv --summary sample.json
  report    <каталог результатов bench.sh>
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "latency":
		err = runLatency(ctx, os.Args[2:])
	case "workload":
		err = runWorkload(ctx, os.Args[2:])
	case "sample":
		err = runSample(ctx, os.Args[2:])
	case "report":
		err = runReport(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		err = fmt.Errorf("неизвестная команда %q\n%s", os.Args[1], usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ramplus-probe:", err)
		os.Exit(1)
	}
}

// writeJSON пишет v в path ("" или "-" — stdout).
func writeJSON(path string, v any) error {
	var w io.Writer = os.Stdout
	if path != "" && path != "-" {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
