// Команда ramplus — CLI RamPlus.
package main

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

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

// lang — язык вывода CLI: --lang, иначе LC_ALL/LC_MESSAGES/LANG (ru…),
// по умолчанию английский. Определяется в main до построения команд
// (тексты справки тоже переводятся); в тестах задаётся напрямую.
var lang = i18n.EN

// T — сообщение каталога на языке CLI.
func T(key string, a ...any) string { return i18n.T(lang, key, a...) }

// detectLang — язык из аргументов (--lang en|ru, флаг важнее переменных)
// или окружения.
func detectLang(args []string, getenv func(string) string) (i18n.Lang, error) {
	for i, a := range args {
		if a == "--" {
			break
		}
		v, ok := "", false
		switch {
		case a == "--lang" && i+1 < len(args):
			v, ok = args[i+1], true
		case strings.HasPrefix(a, "--lang="):
			v, ok = strings.TrimPrefix(a, "--lang="), true
		}
		if !ok {
			continue
		}
		switch v {
		case "en", "ru":
			return i18n.Lang(v), nil
		}
		return i18n.EN, fmt.Errorf("--lang: expected en or ru, got %q", v)
	}
	return i18n.FromEnv(getenv), nil
}

func newRootCmd() *cobra.Command {
	var langFlag string
	root := &cobra.Command{
		Use:           "ramplus",
		Short:         T("cli.root.short"),
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			switch langFlag {
			case "en", "ru":
				lang = i18n.Lang(langFlag)
				return nil
			}
			return fmt.Errorf("--lang: expected en or ru, got %q", langFlag)
		},
	}
	root.PersistentFlags().StringVar(&langFlag, "lang", string(lang), T("cli.flag.lang"))
	root.AddCommand(newStatusCmd(), newSwapCmd(), newProfileCmd(), newRollbackCmd(), newHistoryCmd(), newDaemonCmd(),
		newSetupCmd(), newRecommendCmd(), newInitCmd(), newPasswdCmd())
	return root
}

func main() {
	l, err := detectLang(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ramplus:", err)
		os.Exit(1)
	}
	lang = l
	if err := newRootCmd().Execute(); err != nil {
		if !errors.Is(err, errSilent) {
			fmt.Fprintln(os.Stderr, "ramplus:", i18n.ErrText(lang, err))
		}
		os.Exit(1)
	}
}
