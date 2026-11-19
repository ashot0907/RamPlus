package main

import (
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
	o := swap.Options{Action: action, DeleteOld: f.deleteOld, Force: f.force, Path: f.path, Orphan: f.orphan}
	if f.size != "" {
		size, err := swap.ParseSize(f.size)
		if err != nil {
			return o, fmt.Errorf("--size: %w", err)
		}
		o.Size = size
	}
	switch swap.ExistingMode(f.existing) {
	case "", swap.ExistingKeep, swap.ExistingReplace:
		o.Existing = swap.ExistingMode(f.existing)
	default:
		return o, fmt.Errorf("--existing: ожидается keep или replace, получено %q", f.existing)
	}
	return o, nil
}

func addPlanFlags(cmd *cobra.Command, f *swapFlags, withSize, withExisting bool) {
	if withSize {
		cmd.Flags().StringVar(&f.size, "size", "", "размер swapfile: 4G, 512M, 1.5G (двоичные единицы)")
	}
	if withExisting {
		cmd.Flags().StringVar(&f.path, "path", "", "путь swapfile (по умолчанию /"+swap.DefaultFileName+" на корневой ФС), например /mnt/nvme/"+swap.DefaultFileName)
		cmd.Flags().StringVar(&f.existing, "existing", "", "существующий swap: keep (по умолчанию) или replace")
		cmd.Flags().BoolVar(&f.deleteOld, "delete-old", false, "при replace удалить старые swap-файлы")
	}
	cmd.Flags().BoolVar(&f.force, "force", false, "продолжить несмотря на предупреждения (swap на USB-диске; нехватка места при rollback)")
}

func addVerboseFlag(cmd *cobra.Command, f *swapFlags) {
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "показать полное содержимое записываемых файлов (systemd-юнит)")
}

func addExecFlags(cmd *cobra.Command, f *swapFlags) {
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "не спрашивать подтверждение (для скриптов)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "только показать план, ничего не менять")
}

func newSwapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "swap",
		Short: "Управление swapfile RamPlus (план → apply → rollback)",
	}
	cmd.AddCommand(
		newPlanCmd(),
		newExecCmd(swap.ActionApply, "apply", "Создать swapfile (нужен root)", true, true),
		newExecCmd(swap.ActionResize, "resize", "Изменить размер swapfile RamPlus (нужен root)", true, false),
		newExecCmd(swap.ActionRemove, "remove", "Удалить swapfile RamPlus (нужен root)", false, false),
		newCheckCleanCmd(),
	)
	return cmd
}

func newRollbackCmd() *cobra.Command {
	return newExecCmd(swap.ActionRollback, "rollback", "Отменить последнюю операцию RamPlus (повторный вызов идёт дальше назад)", false, false)
}

func newPlanCmd() *cobra.Command {
	var f swapFlags
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Показать план создания swapfile. Ничего не меняет, root не нужен",
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
		Long: short + ".\n\nСначала печатается план и запрашивается подтверждение (--yes — без вопроса).\n" +
			"Если шаг падает, выполненные шаги откатываются. Журнал: " + journal.StatePath,
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
				return executeJSON(cmd, core.Request{Swap: &o}, confirmer(true, nil, nil))
			}
			err = execute(cmd, core.Request{Swap: &o}, f.verbose, confirmer(f.yes, cmd.InOrStdin(), out))
			if errors.Is(err, journal.ErrNeedRoot) {
				return fmt.Errorf("%w: sudo %s (посмотреть план без root: --dry-run)", err, o.Command())
			}
			return err
		},
	}
	if withSize || withExisting {
		addPlanFlags(cmd, &f, withSize, withExisting)
	} else {
		cmd.Flags().BoolVar(&f.force, "force", false, "продолжить несмотря на предупреждения")
	}
	if action == swap.ActionRemove {
		cmd.Flags().BoolVar(&f.orphan, "orphan", false, "убрать следы RamPlus без записи в журнале (swapfile, юнит) — с проверкой маркеров")
	}
	addExecFlags(cmd, &f)
	addVerboseFlag(cmd, &f)
	addJSONFlag(cmd, &f.json)
	return cmd
}

// execute — построить план под блокировкой, показать, подтвердить, выполнить.
func execute(cmd *cobra.Command, r core.Request, verbose bool, confirm func(*plan.Plan) (bool, error)) error {
	out := cmd.OutOrStdout()
	h := render.Exec(out)
	h.Render = func(p *plan.Plan) error { return render.Plan(out, p, render.Options{Verbose: verbose}) }
	h.Confirm = confirm
	_, err := newCore().Run(cmd.Context(), r, h)
	return err
}

func confirmer(yes bool, in io.Reader, out io.Writer) func(*plan.Plan) (bool, error) {
	return func(p *plan.Plan) (bool, error) {
		if yes {
			return true, nil
		}
		if file, ok := in.(*os.File); !ok || !isTerminal(file) {
			return false, errors.New("нет терминала для подтверждения: добавьте --yes")
		}
		fmt.Fprintf(out, "\nПрименить план (%d шагов)? [y/N]: ", len(p.Steps))
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
		Short: "Проверить, что в системе нет следов RamPlus (управляемого swap, юнитов, незавершённых операций)",
		Long: "Смотрит на фактическое состояние: managed в журнале, активный swap RamPlus,\n" +
			"юниты *ramplus*.swap, незавершённые операции, файлы по путям RamPlus.\n" +
			"Журнал с историей откаченных операций следом не считается. Код выхода 1 — не чисто.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			problems := newCore().CleanProblems(cmd.Context())
			out := cmd.OutOrStdout()
			if len(problems) == 0 {
				fmt.Fprintln(out, "чисто: следов RamPlus нет")
				return nil
			}
			for _, p := range problems {
				fmt.Fprintln(out, "- "+p)
			}
			return fmt.Errorf("найдены следы RamPlus: %d", len(problems))
		},
	}
}
