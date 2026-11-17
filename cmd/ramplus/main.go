// Команда ramplus — CLI RamPlus.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/sys"
)

// version подставляется при сборке: -ldflags "-X main.version=..."
var version = "dev"

// newSystem — система, с которой работают команды (в тестах — фейковая).
var newSystem = sys.DefaultSystem

// collect — снимок памяти для ramplus status (в тестах — из фикстур).
var collect = monitor.Collect

// newCore — сервисный слой над системой: вся логика там, CLI — оболочка.
func newCore() *core.Core {
	c := core.New(newSystem())
	c.Collect = collect
	return c
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
