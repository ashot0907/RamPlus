package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/plan"
)

// renderRecommendation — текст ramplus recommend.
func renderRecommendation(w io.Writer, r core.Recommendation) error {
	pr := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	fmt.Fprint(w, T("cli.recommend.header"))
	pr("  RAM: %s (MemTotal %s)\n", plan.FormatSize(r.RAMRoundedBytes), plan.FormatSize(r.RAMBytes))
	if t := r.Target; t != nil {
		avail := "n/a"
		if t.Available != nil {
			avail = plan.FormatSize(*t.Available)
		}
		pr(T("cli.recommend.target"), t.Path, t.FSType, t.MountPoint, t.DiskKind, avail, plan.FormatSize(r.SwapMaxBytes))
	}
	if r.SwapSizeBytes != nil {
		pr(T("cli.recommend.swap"), plan.FormatSize(*r.SwapSizeBytes), r.SwapReason)
	} else {
		pr(T("cli.recommend.no_swap"), r.SwapReason)
	}
	if r.Existing != nil {
		pr(T("cli.recommend.existing"), *r.Existing, r.ExistingReason)
	}
	pr(T("cli.recommend.profile"), r.Profile, r.ProfileStatus, r.ProfileReason)
	for _, n := range r.Notes {
		pr("  [%s] %s\n", n.Code, n.Message)
	}
	cmd := "ramplus setup"
	if r.SwapSizeBytes != nil {
		cmd += " --size " + fmt.Sprintf("%dG", *r.SwapSizeBytes/plan.GiB)
		if r.Existing != nil {
			cmd += " --existing=" + *r.Existing
		}
	}
	cmd += " --profile " + r.Profile
	if r.ProfileStatus == "experimental" {
		cmd += " --experimental"
	}
	_, err := fmt.Fprintf(w, T("cli.recommend.plan"), cmd)
	return err
}

func newRecommendCmd() *cobra.Command {
	var asJSON bool
	var path string
	cmd := &cobra.Command{
		Use:   "recommend",
		Short: T("cli.recommend.short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := newCore().Recommend(cmd.Context(), path, lang)
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), r)
			}
			return renderRecommendation(cmd.OutOrStdout(), r)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", T("cli.flag.recommend_path"))
	addJSONFlag(cmd, &asJSON)
	return cmd
}
