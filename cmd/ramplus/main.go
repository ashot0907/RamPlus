// Команда ramplus — CLI RamPlus.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/swap"
)

// version подставляется при сборке: -ldflags "-X main.version=..."
var version = "dev"

// newSystem — система, с которой работают команды (в тестах — фейковая).
var newSystem = swap.DefaultSystem

// collectStatus — снимок для ramplus status (в тестах — из фикстур).
var collectStatus = func(sys swap.System) monitor.Status {
	st := monitor.Collect()
	swap.AnnotateStatus(&st, sys.FS)
	return st
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "ramplus",
		Short:         "Управление виртуальной памятью Linux: swap, zswap, zram, мониторинг",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newStatusCmd(), newSwapCmd(), newProfileCmd(), newRollbackCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ramplus:", err)
		os.Exit(1)
	}
}
