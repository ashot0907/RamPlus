// Команда ramplus — CLI RamPlus.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version подставляется при сборке: -ldflags "-X main.version=..."
var version = "dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "ramplus",
		Short:         "Управление виртуальной памятью Linux: swap, zswap, zram, мониторинг",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newStatusCmd(), newSwapCmd(), newRollbackCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ramplus:", err)
		os.Exit(1)
	}
}
