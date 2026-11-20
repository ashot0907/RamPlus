package main

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"errors"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/swap"
)

type setupFlags struct {
	swapFlags
	profile      string
	set          []string
	experimental bool
}

func (f *setupFlags) request() (core.SetupRequest, error) {
	var r core.SetupRequest
	if f.size != "" || f.path != "" || f.existing != "" || f.deleteOld {
		o, err := f.options(swap.ActionApply)
		if err != nil {
			return r, err
		}
		r.Swap = &o
	}
	if f.profile != "" {
		set, err := profile.ParseSet(f.set)
		if err != nil {
			return r, err
		}
		r.Profile = &profile.ProfileRequest{Name: f.profile, Set: set, Force: f.force, Experimental: f.experimental, Lang: lang}
	} else if len(f.set) > 0 {
		return r, errors.New(T("cli.setup.set_needs_profile"))
	}
	return r, nil
}

func newSetupCmd() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: T("cli.setup.short"),
		Long: T("cli.setup.long1") +
			T("cli.setup.long2") +
			T("cli.setup.long3"),
		Example: "  ramplus setup --size 16G --existing=replace --profile zswap-lz4 --dry-run\n" +
			"  sudo ramplus setup --size 16G --existing=replace --profile zswap-lz4 --experimental",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := f.request()
			if err != nil {
				return err
			}
			req := core.Request{Setup: &r, Lang: lang}
			if f.dryRun {
				return showPlan(cmd, newCore().SetupPlan(cmd.Context(), r, lang), f.verbose, f.json)
			}
			if f.json {
				if !f.yes {
					return errJSONNeedsYes
				}
				return executeJSON(cmd, req, profileConfirmer(true, f.experimental, nil, nil))
			}
			err = execute(cmd, req, f.verbose, profileConfirmer(f.yes, f.experimental, cmd.InOrStdin(), cmd.OutOrStdout()))
			if errors.Is(err, journal.ErrNeedRoot) {
				return i18n.Errorf("cli.setup.need_root", err)
			}
			return err
		},
	}
	addPlanFlags(cmd, &f.swapFlags, true, true)
	addVerboseFlag(cmd, &f.swapFlags)
	addExecFlags(cmd, &f.swapFlags)
	addJSONFlag(cmd, &f.json)
	cmd.Flags().StringVar(&f.profile, "profile", "", T("cli.flag.profile")+joinNames())
	cmd.Flags().StringArrayVar(&f.set, "set", nil, T("cli.flag.set"))
	cmd.Flags().BoolVar(&f.experimental, "experimental", false, T("cli.flag.experimental_setup"))
	return cmd
}

func joinNames() string {
	s := ""
	for i, n := range profile.Names() {
		if i > 0 {
			s += ", "
		}
		s += n
	}
	return s
}
