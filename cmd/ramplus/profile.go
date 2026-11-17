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

	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/swap"
)

// ErrExperimentalNotConfirmed — экспериментальный профиль без подтверждения.
var ErrExperimentalNotConfirmed = errors.New("профиль экспериментальный (не подтверждён бенчмарком на SSD): для скриптов добавьте --experimental")

type profileFlags struct {
	set          []string
	force        bool
	experimental bool
	yes          bool
	dryRun       bool
	verbose      bool
}

func (f *profileFlags) request(name string) (swap.ProfileRequest, error) {
	set, err := profile.ParseSet(f.set)
	if err != nil {
		return swap.ProfileRequest{}, err
	}
	return swap.ProfileRequest{Name: name, Set: set, Force: f.force, Experimental: f.experimental}, nil
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

func activeProfile() *swap.AppliedProfile {
	f := swap.Gather(contextBackground(), swap.DefaultSystem(), "")
	return f.State.Profile
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
			p := swap.PlanProfile(cmd.Context(), swap.DefaultSystem(), r)
			return p.RenderWith(cmd.OutOrStdout(), swap.RenderOptions{Verbose: f.verbose})
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
			sys := swap.DefaultSystem()
			if f.dryRun {
				return swap.PlanProfile(cmd.Context(), sys, r).RenderWith(out, swap.RenderOptions{Verbose: f.verbose})
			}
			m := &swap.Manager{Sys: sys, Out: out, Verbose: f.verbose}
			_, err = m.ExecuteFunc(cmd.Context(), func(ctx contextT) swap.Plan { return swap.PlanProfile(ctx, sys, r) },
				profileConfirmer(f.yes, f.experimental, cmd.InOrStdin(), out))
			if errors.Is(err, swap.ErrNeedRoot) {
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
			sys := swap.DefaultSystem()
			if f.dryRun {
				return swap.PlanProfileReset(cmd.Context(), sys).RenderWith(out, swap.RenderOptions{Verbose: f.verbose})
			}
			m := &swap.Manager{Sys: sys, Out: out, Verbose: f.verbose}
			_, err := m.ExecuteFunc(cmd.Context(), func(ctx contextT) swap.Plan { return swap.PlanProfileReset(ctx, sys) },
				confirmer(f.yes, cmd.InOrStdin(), out))
			if errors.Is(err, swap.ErrNeedRoot) {
				return fmt.Errorf("%w: sudo ramplus profile reset (план без root: ramplus profile reset --dry-run)", err)
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "не спрашивать подтверждение")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "только план")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "подробно")
	return cmd
}

// profileConfirmer — обычное подтверждение плюс отдельное для
// экспериментального профиля.
func profileConfirmer(yes, experimental bool, in io.Reader, out io.Writer) swap.Confirm {
	base := confirmer(yes, in, out)
	return func(p swap.Plan) (bool, error) {
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
			sys := swap.DefaultSystem()
			f := swap.Gather(cmd.Context(), sys, "")
			pf := swap.GatherProfile(cmd.Context(), sys, f, nil)
			return renderCurrent(cmd.OutOrStdout(), pf)
		},
	}
}

func renderCurrent(w io.Writer, pf swap.ProfileFacts) error {
	ap, base := pf.State.Profile, pf.State.Baseline
	if ap == nil {
		fmt.Fprintln(w, "Профиль RamPlus не применён.")
	} else {
		fmt.Fprintf(w, "Профиль: %s — %s, применён %s\n", ap.Name, profile.StatusText(ap.Status), ap.AppliedAt.Local().Format("2006-01-02 15:04:05"))
		for k, v := range ap.Overrides {
			fmt.Fprintf(w, "  изменено пользователем: %s=%s\n", k, v)
		}
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ПАРАМЕТР\tСЕЙЧАС\tПРОФИЛЬ\tИСХОДНОЕ\tОТКУДА")
	var fromBase []string
	for _, pr := range profile.Params {
		cur, present := pf.Values[pr.Key]
		if !present {
			cur = "нет в ядре"
		}
		want, orig := "—", "—"
		if ap != nil && ap.Values[pr.Key] != "" {
			want = ap.Values[pr.Key]
		}
		if base != nil && base.Values[pr.Key] != "" {
			orig = base.Values[pr.Key]
			if present && cur != orig {
				fromBase = append(fromBase, fmt.Sprintf("%s: %s → %s", pr.Key, orig, cur))
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", pr.Key, cur, want, orig, pf.SourceOf(pr.Key))
	}
	tw.Flush()
	if base == nil {
		fmt.Fprintln(w, "\nИсходные значения не сохранены (снимаются при первом ramplus profile apply).")
	} else if len(fromBase) == 0 {
		fmt.Fprintln(w, "\nОтличий от исходных значений нет.")
	} else {
		fmt.Fprintf(w, "\nОтличия от исходных значений (сняты %s): %s\n", base.CapturedAt.Local().Format("2006-01-02 15:04:05"), strings.Join(fromBase, "; "))
	}
	if d := swap.ProfileDrift(ap, pf.Values); len(d) > 0 {
		var parts []string
		for _, r := range d {
			parts = append(parts, fmt.Sprintf("%s = %s (профиль: %s)", r.Key, dashStr(r.Current), r.New))
		}
		fmt.Fprintf(w, "ВНИМАНИЕ: значения изменены вручную после apply: %s\n", strings.Join(parts, "; "))
	}
	return nil
}

type contextT = context.Context

func contextBackground() context.Context { return context.Background() }
