package main

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/swap"
)

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Показать состояние памяти, swap, zswap, zram и корневого диска",
		Long: "Только чтение: команда ничего не меняет в системе и работает без root.\n" +
			"Недоступные источники показываются как n/a с причиной.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := monitor.Collect()
			swap.AnnotateStatus(&st, swap.OSFS{})
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			return renderStatus(out, st)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "вывод в JSON (та же схема, что у веб-API)")
	return cmd
}
