package main

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/render"
	"github.com/ashot0907/ramplus/internal/swap"
)

// swapFlags — флаги, общие для команд swap.
type swapFlags struct {
	size      string
	path      string
	verbose   bool
	existing  string
	deleteOld bool
	force     bool
	yes       bool
	dryRun    bool
	json      bool
	orphan    bool
}

func (f *swapFlags) options(action swap.Action) (swap.Options, error) {
	o := swap.Options{Action: action, DeleteOld: f.deleteOld, Force: f.force, Path: f.path, Orphan: f.orphan, Lang: lang}
	if f.size != "" {
		size, err := swap.ParseSize(f.size)
		if err != nil {
			return o, i18n.Errorf("cli.flag_error", "--size", err)
		}
		o.Size = size
	}
	switch swap.ExistingMode(f.existing) {
	case "", swap.ExistingKeep, swap.ExistingReplace:
		o.Existing = swap.ExistingMode(f.existing)
	default:
		return o, fmt.Errorf(T("cli.swap.existing_invalid"), f.existing)
	}
	return o, nil
}

func addPlanFlags(cmd *cobra.Command, f *swapFlags, withSize, withExisting bool) {
	if withSize {
		cmd.Flags().StringVar(&f.size, "size", "", T("cli.flag.size"))
	}
	if withExisting {
		cmd.Flags().StringVar(&f.path, "path", "", T("cli.flag.path1")+swap.DefaultFileName+T("cli.flag.path2")+swap.DefaultFileName)
		cmd.Flags().StringVar(&f.existing, "existing", "", T("cli.flag.existing"))
		cmd.Flags().BoolVar(&f.deleteOld, "delete-old", false, T("cli.flag.delete_old"))
	}
	cmd.Flags().BoolVar(&f.force, "force", false, T("cli.flag.swap_force"))
}

func addVerboseFlag(cmd *cobra.Command, f *swapFlags) {
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, T("cli.flag.verbose_unit"))
}

func addExecFlags(cmd *cobra.Command, f *swapFlags) {
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, T("cli.flag.yes_scripts"))
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, T("cli.flag.dry_run"))
}

func newSwapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "swap",
		Short: T("cli.swap.short"),
	}
	cmd.AddCommand(
		newPlanCmd(),
		newExecCmd(swap.ActionApply, "apply", T("cli.swap.apply_short"), true, true),
		newExecCmd(swap.ActionResize, "resize", T("cli.swap.resize_short"), true, false),
		newExecCmd(swap.ActionRemove, "remove", T("cli.swap.remove_short"), false, false),
		newCheckCleanCmd(),
	)
	return cmd
}

func newRollbackCmd() *cobra.Command {
	return newExecCmd(swap.ActionRollback, "rollback", T("cli.rollback.short"), false, false)
}

func newPlanCmd() *cobra.Command {
	var f swapFlags
	cmd := &cobra.Command{
		Use:   "plan",
		Short: T("cli.swap.plan_short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := f.options(swap.ActionApply)
			if err != nil {
				return err
			}
			return showPlan(cmd, newCore().SwapPlan(cmd.Context(), o), f.verbose, f.json)
		},
	}
	addPlanFlags(cmd, &f, true, true)
	addVerboseFlag(cmd, &f)
	addJSONFlag(cmd, &f.json)
	return cmd
}

func newExecCmd(action swap.Action, use, short string, withSize, withExisting bool) *cobra.Command {
	var f swapFlags
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + T("cli.swap.exec_long1") +
			T("cli.swap.exec_long2") + journal.StatePath,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := f.options(action)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.dryRun {
				return showPlan(cmd, newCore().SwapPlan(cmd.Context(), o), f.verbose, f.json)
			}
			if f.json {
				if !f.yes {
					return errJSONNeedsYes
				}
				return executeJSON(cmd, core.Request{Swap: &o, Lang: lang}, confirmer(true, nil, nil))
			}
			err = execute(cmd, core.Request{Swap: &o, Lang: lang}, f.verbose, confirmer(f.yes, cmd.InOrStdin(), out))
			if errors.Is(err, journal.ErrNeedRoot) {
				return i18n.Errorf("cli.swap.need_root", err, o.Command())
			}
			return err
		},
	}
	if withSize || withExisting {
		addPlanFlags(cmd, &f, withSize, withExisting)
	} else {
		cmd.Flags().BoolVar(&f.force, "force", false, T("cli.flag.force"))
	}
	if action == swap.ActionRemove {
		cmd.Flags().BoolVar(&f.orphan, "orphan", false, T("cli.flag.orphan"))
	}
	addExecFlags(cmd, &f)
	addVerboseFlag(cmd, &f)
	addJSONFlag(cmd, &f.json)
	return cmd
}

// execute — построить план под блокировкой, показать, подтвердить, выполнить.
func execute(cmd *cobra.Command, r core.Request, verbose bool, confirm func(*plan.Plan) (bool, error)) error {
	out := cmd.OutOrStdout()
	h := render.Exec(lang, out)
	h.Render = func(p *plan.Plan) error { return render.Plan(out, p, render.Options{Verbose: verbose, Lang: lang}) }
	h.Confirm = confirm
	h.Origin = cliOrigin()
	h.Lang = lang
	_, err := newCore().Run(cmd.Context(), r, h)
	return err
}

func confirmer(yes bool, in io.Reader, out io.Writer) func(*plan.Plan) (bool, error) {
	return func(p *plan.Plan) (bool, error) {
		if yes {
			return true, nil
		}
		if file, ok := in.(*os.File); !ok || !isTerminal(file) {
			return false, errors.New(T("cli.confirm.no_tty"))
		}
		fmt.Fprintf(out, T("cli.confirm.apply"), len(p.Steps))
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && line == "" {
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes", "д", "да":
			return true, nil
		}
		return false, nil
	}
}

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

func newCheckCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-clean",
		Short: T("cli.clean.short"),
		Long: T("cli.clean.long1") +
			T("cli.clean.long2") +
			T("cli.clean.long3"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			problems := newCore().CleanProblems(cmd.Context(), lang)
			out := cmd.OutOrStdout()
			if len(problems) == 0 {
				fmt.Fprintln(out, T("cli.clean.ok"))
				return nil
			}
			for _, p := range problems {
				fmt.Fprintln(out, "- "+p)
			}
			return fmt.Errorf(T("cli.clean.found"), len(problems))
		},
	}
}
