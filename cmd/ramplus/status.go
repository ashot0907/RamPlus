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
		Short: T("cli.status.short"),
		Long: T("cli.status.long1") +
			T("cli.status.long2"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if watch {
				if asJSON {
					return errors.New(T("cli.status.watch_json"))
				}
				if interval < minWatchInterval {
					return fmt.Errorf(T("cli.status.interval_min"), minWatchInterval)
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
			return render.Status(lang, out, st)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, T("cli.flag.status_json"))
	cmd.Flags().BoolVar(&watch, "watch", false, T("cli.flag.watch"))
	cmd.Flags().DurationVar(&interval, "interval", time.Second, T("cli.flag.interval"))
	return cmd
}
