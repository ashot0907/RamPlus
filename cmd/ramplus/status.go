package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/render"
)

func newStatusCmd() *cobra.Command {
	var (
		asJSON   bool
		watch    bool
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Показать состояние памяти, swap, zswap, zram и корневого диска",
		Long: "Только чтение: команда ничего не меняет в системе и работает без root.\n" +
			"Недоступные источники показываются как n/a с причиной.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if watch {
				if asJSON {
					return errors.New("--watch несовместим с --json")
				}
				if interval < minWatchInterval {
					return fmt.Errorf("--interval: минимум %s", minWatchInterval)
				}
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				out := cmd.OutOrStdout()
				f, isFile := out.(*os.File)
				w := &watcher{
					out: out, interval: interval, tty: isFile && isTerminal(f),
					collect: func() monitor.Status { return monitor.CollectFrom(monitor.DefaultPaths) },
					now:     time.Now,
				}
				return w.run(ctx)
			}
			st := newCore().Status()
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			return render.Status(out, st)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "вывод в JSON (та же схема, что у веб-API)")
	cmd.Flags().BoolVar(&watch, "watch", false, "обновлять на месте: RAM, swap, zswap, PSI и пики за время наблюдения (Ctrl+C — итог)")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "период обновления для --watch")
	return cmd
}
