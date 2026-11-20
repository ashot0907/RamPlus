package main

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/render"
)

// ErrExperimentalNotConfirmed — экспериментальный профиль без подтверждения.
var ErrExperimentalNotConfirmed = core.ErrExperimentalNotConfirmed

type profileFlags struct {
	set          []string
	force        bool
	experimental bool
	yes          bool
	dryRun       bool
	verbose      bool
	json         bool
}

func (f *profileFlags) request(name string) (profile.ProfileRequest, error) {
	set, err := profile.ParseSet(f.set)
	if err != nil {
		return profile.ProfileRequest{}, err
	}
	return profile.ProfileRequest{Name: name, Set: set, Force: f.force, Experimental: f.experimental, Lang: lang}, nil
}

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: T("cli.profile.short"),
	}
	cmd.AddCommand(newProfileListCmd(), newProfileShowCmd(), newProfilePlanCmd(), newProfileApplyCmd(),
		newProfileCurrentCmd(), newProfileResetCmd())
	return cmd
}

func activeProfile() *profile.AppliedProfile {
	return newCore().ProfileFacts(context.Background()).Profile
}

func newProfileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: T("cli.profile.list_short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			active := activeProfile()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, T("cli.profile.list_columns"))
			for _, p := range profile.All() {
				mark := " "
				if active != nil && active.Name == p.Name {
					mark = "*"
				}
				fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\n", mark, p.Name, profile.StatusText(lang, p.Status), dashStr(p.Evidence), p.Title(lang))
			}
			tw.Flush()
			if active != nil {
				fmt.Fprintf(out, T("cli.profile.list_applied"), active.Name, profile.StatusText(lang, active.Status))
			} else {
				fmt.Fprintln(out, T("cli.profile.list_none"))
			}
			fmt.Fprintln(out, T("cli.profile.list_note"))
			return nil
		},
	}
}

func dashStr(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func newProfileShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   T("cli.profile.show_use"),
		Short: T("cli.profile.show_short"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, ok := profile.Get(args[0])
			if !ok {
				return fmt.Errorf(T("cli.profile.unknown"), args[0], strings.Join(profile.Names(), ", "))
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, T("cli.profile.show_header"), p.Name, p.Title(lang), p.Description(lang))
			fmt.Fprintf(out, T("cli.profile.show_status"), profile.StatusText(lang, p.Status), dashStr(p.Evidence), T("profile."+p.Name+".comment"))
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, T("cli.profile.show_columns"))
			for _, pr := range profile.Params {
				v, ok := p.Params[pr.Key]
				if !ok {
					v = T("cli.profile.untouched")
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", pr.Key, v, profile.ParamTitle(lang, pr.Key))
			}
			return tw.Flush()
		},
	}
}

func addProfileFlags(cmd *cobra.Command, f *profileFlags, exec bool) {
	cmd.Flags().StringArrayVar(&f.set, "set", nil, T("cli.flag.set_repeat"))
	cmd.Flags().BoolVar(&f.force, "force", false, T("cli.flag.profile_force"))
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, T("cli.flag.verbose_files"))
	addJSONFlag(cmd, &f.json)
	if exec {
		cmd.Flags().BoolVar(&f.experimental, "experimental", false, T("cli.flag.experimental"))
		cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, T("cli.flag.yes"))
		cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, T("cli.flag.dry_run_short"))
	}
}

func newProfilePlanCmd() *cobra.Command {
	var f profileFlags
	cmd := &cobra.Command{
		Use:   T("cli.profile.plan_use"),
		Short: T("cli.profile.plan_short"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := f.request(args[0])
			if err != nil {
				return err
			}
			return showPlan(cmd, newCore().ProfilePlan(cmd.Context(), r), f.verbose, f.json)
		},
	}
	addProfileFlags(cmd, &f, false)
	return cmd
}

func newProfileApplyCmd() *cobra.Command {
	var f profileFlags
	cmd := &cobra.Command{
		Use:   T("cli.profile.apply_use"),
		Short: T("cli.profile.apply_short"),
		Long: T("cli.profile.apply_long1") +
			T("cli.profile.apply_long2") +
			T("cli.profile.apply_long3"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := f.request(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.dryRun {
				return showPlan(cmd, newCore().ProfilePlan(cmd.Context(), r), f.verbose, f.json)
			}
			if f.json {
				if !f.yes {
					return errJSONNeedsYes
				}
				return executeJSON(cmd, core.Request{Profile: &r, Lang: lang}, profileConfirmer(true, f.experimental, nil, nil))
			}
			err = execute(cmd, core.Request{Profile: &r, Lang: lang}, f.verbose, profileConfirmer(f.yes, f.experimental, cmd.InOrStdin(), out))
			if errors.Is(err, journal.ErrNeedRoot) {
				return i18n.Errorf("cli.profile.apply_need_root", err, args[0], args[0])
			}
			return err
		},
	}
	addProfileFlags(cmd, &f, true)
	return cmd
}

func newProfileResetCmd() *cobra.Command {
	var f profileFlags
	cmd := &cobra.Command{
		Use:   "reset",
		Short: T("cli.profile.reset_short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if f.dryRun {
				return showPlan(cmd, newCore().ProfileResetPlan(cmd.Context(), lang), f.verbose, f.json)
			}
			if f.json {
				if !f.yes {
					return errJSONNeedsYes
				}
				return executeJSON(cmd, core.Request{ProfileReset: true, Lang: lang}, confirmer(true, nil, nil))
			}
			err := execute(cmd, core.Request{ProfileReset: true, Lang: lang}, f.verbose, confirmer(f.yes, cmd.InOrStdin(), out))
			if errors.Is(err, journal.ErrNeedRoot) {
				return i18n.Errorf("cli.profile.reset_need_root", err)
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, T("cli.flag.yes"))
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, T("cli.flag.dry_run_short"))
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, T("cli.flag.verbose"))
	addJSONFlag(cmd, &f.json)
	return cmd
}

// profileConfirmer — обычное подтверждение плюс отдельное для
// экспериментального профиля.
func profileConfirmer(yes, experimental bool, in io.Reader, out io.Writer) func(*plan.Plan) (bool, error) {
	base := confirmer(yes, in, out)
	return func(p *plan.Plan) (bool, error) {
		if p.Profile != nil && p.Profile.Experimental && !experimental {
			if yes {
				return false, ErrExperimentalNotConfirmed
			}
			file, ok := in.(*os.File)
			if !ok || !isTerminal(file) {
				return false, ErrExperimentalNotConfirmed
			}
			fmt.Fprint(out, T("cli.profile.confirm_experimental"))
			line, _ := bufio.NewReader(in).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "yes" && a != "да" {
				return false, nil
			}
			return true, nil
		}
		return base(p)
	}
}

func newProfileCurrentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: T("cli.profile.current_short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pf := newCore().ProfileFacts(cmd.Context())
			return render.ProfileCurrent(lang, cmd.OutOrStdout(), pf)
		},
	}
}
