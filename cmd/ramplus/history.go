package main

import (
	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/render"
)

func newHistoryCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Операции RamPlus из журнала: ID, время, команда, статус, можно ли откатить",
		Long:  "Только чтение: команда ничего не меняет и работает без root.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := newCore().History()
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), h)
			}
			return render.History(cmd.OutOrStdout(), h)
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}
