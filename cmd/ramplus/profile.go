package main

import (
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
	return profile.ProfileRequest{Name: name, Set: set, Force: f.force, Experimental: f.experimental}, nil
}

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Профили RamPlus: zswap и sysctl vm.* (план → apply → rollback/reset)",
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
		Short: "Профили и их статусы",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			active := activeProfile()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "  ПРОФИЛЬ\tСТАТУС\tEVIDENCE\tОПИСАНИЕ")
			for _, p := range profile.All() {
				mark := " "
				if active != nil && active.Name == p.Name {
					mark = "*"
				}
				fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\n", mark, p.Name, profile.StatusText(p.Status), dashStr(p.Evidence), p.Title)
			}
			tw.Flush()
			if active != nil {
				fmt.Fprintf(out, "\n* применён: %s (%s)\n", active.Name, profile.StatusText(active.Status))
			} else {
				fmt.Fprintln(out, "\nПрофиль RamPlus не применялся.")
			}
			fmt.Fprintln(out, "Статус «проверенный» профиль получает только с evidence — результатом scripts/bench.sh на реальном SSD.")
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
		Use:   "show <имя>",
		Short: "Параметры профиля, статус и evidence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, ok := profile.Get(args[0])
			if !ok {
				return fmt.Errorf("нет профиля %q; доступны: %s", args[0], strings.Join(profile.Names(), ", "))
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Профиль: %s — %s\n%s\n\n", p.Name, p.Title, p.Description)
			fmt.Fprintf(out, "Статус:   %s\nEvidence: %s\nОткуда значения: %s\n\n", profile.StatusText(p.Status), dashStr(p.Evidence), p.Comment)
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "  ПАРАМЕТР\tЗНАЧЕНИЕ\tЧТО ЭТО")
			for _, pr := range profile.Params {
				v, ok := p.Params[pr.Key]
				if !ok {
					v = "не трогать"
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", pr.Key, v, pr.Title)
			}
			return tw.Flush()
		},
	}
}

func addProfileFlags(cmd *cobra.Command, f *profileFlags, exec bool) {
	cmd.Flags().StringArrayVar(&f.set, "set", nil, "точечное изменение поверх профиля: ключ=значение (можно несколько раз)")
	cmd.Flags().BoolVar(&f.force, "force", false, "включить zswap даже при активном/установленном zram")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "показать содержимое записываемых файлов")
	addJSONFlag(cmd, &f.json)
	if exec {
		cmd.Flags().BoolVar(&f.experimental, "experimental", false, "подтвердить применение экспериментального профиля (для скриптов)")
		cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "не спрашивать подтверждение")
		cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "только план")
	}
}

func newProfilePlanCmd() *cobra.Command {
	var f profileFlags
	cmd := &cobra.Command{
		Use:   "plan <имя>",
		Short: "План применения профиля. Ничего не меняет, root не нужен",
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
		Use:   "apply <имя>",
		Short: "Применить профиль (нужен root): сразу и после перезагрузки",
		Long: "Сначала печатается план и запрашивается подтверждение (--yes — без вопроса).\n" +
			"Экспериментальный профиль требует отдельного подтверждения (в скриптах — --experimental).\n" +
			"Отменить: ramplus rollback (общий стек со swap) или ramplus profile reset.",
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
				return executeJSON(cmd, core.Request{Profile: &r}, profileConfirmer(true, f.experimental, nil, nil))
			}
			err = execute(cmd, core.Request{Profile: &r}, f.verbose, profileConfirmer(f.yes, f.experimental, cmd.InOrStdin(), out))
			if errors.Is(err, journal.ErrNeedRoot) {
				return fmt.Errorf("%w: sudo ramplus profile apply %s (посмотреть план без root: ramplus profile plan %s)", err, args[0], args[0])
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
		Short: "Вернуть значения системы до первого apply и удалить файлы RamPlus (нужен root)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if f.dryRun {
				return showPlan(cmd, newCore().ProfileResetPlan(cmd.Context()), f.verbose, f.json)
			}
			if f.json {
				if !f.yes {
					return errJSONNeedsYes
				}
				return executeJSON(cmd, core.Request{ProfileReset: true}, confirmer(true, nil, nil))
			}
			err := execute(cmd, core.Request{ProfileReset: true}, f.verbose, confirmer(f.yes, cmd.InOrStdin(), out))
			if errors.Is(err, journal.ErrNeedRoot) {
				return fmt.Errorf("%w: sudo ramplus profile reset (план без root: ramplus profile reset --dry-run)", err)
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "не спрашивать подтверждение")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "только план")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "подробно")
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
			fmt.Fprint(out, "\nПрофиль экспериментальный: значения не подтверждены scripts/bench.sh на реальном SSD.\nВсё равно применить? Введите yes: ")
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
		Short: "Что применено сейчас и отличия от исходных значений системы",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pf := newCore().ProfileFacts(cmd.Context())
			return render.ProfileCurrent(cmd.OutOrStdout(), pf)
		},
	}
}
