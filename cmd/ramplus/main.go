// Команда ramplus — CLI RamPlus.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/sys"
)

// version подставляется при сборке: -ldflags "-X main.version=..."
var version = "dev"

// newSystem — система, с которой работают команды (в тестах — фейковая).
var newSystem = sys.DefaultSystem

// cliOrigin — кто запустил операцию из терминала (для журнала): uid
// вызвавшего sudo и его имя (в тестах — фиксированное значение).
var cliOrigin = func() journal.Origin {
	return core.CLIOrigin(os.Getuid(), os.Getenv("SUDO_UID"), os.Getenv("SUDO_USER"), func(uid uint32) string {
		u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
		if err != nil {
			return ""
		}
		return u.Username
	})
}

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
	root.AddCommand(newStatusCmd(), newSwapCmd(), newProfileCmd(), newRollbackCmd(), newHistoryCmd(), newDaemonCmd(),
		newSetupCmd(), newRecommendCmd(), newInitCmd(), newPasswdCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		if !errors.Is(err, errSilent) {
			fmt.Fprintln(os.Stderr, "ramplus:", err)
		}
		os.Exit(1)
	}
}
