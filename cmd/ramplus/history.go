package main

import (
	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/render"
)

func newHistoryCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "history",
		Short: T("cli.history.short"),
		Long:  T("cli.history.long"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := newCore().History(lang)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), h)
			}
			return render.History(lang, cmd.OutOrStdout(), h)
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}
