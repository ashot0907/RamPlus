package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/webauth"
)

// lookupWebGroup — gid группы ramplus-web (в тестах подменяется).
var lookupWebGroup = func() (int, error) {
	g, err := user.LookupGroup(webauth.Group)
	if err != nil {
		return 0, fmt.Errorf("нет группы %s — сначала установите RamPlus (sudo make install): %w", webauth.Group, err)
	}
	return strconv.Atoi(g.Gid)
}

type passwordFlags struct {
	stdin   bool
	dryRun  bool
	disable bool
}

func newInitCmd() *cobra.Command {
	var f passwordFlags
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Задать пароль веб-интерфейса (нужен root)",
		Long: "Пароль хранится как хеш argon2id в " + webauth.DefaultPath + " (root:" + webauth.Group + ", 0640).\n" +
			"Пароль спрашивается дважды без эха; для скриптов — --password-stdin (первая строка stdin).\n" +
			"Сменить: sudo ramplus passwd; отключить вход в веб: sudo ramplus passwd --disable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWebPassword(cmd, core.WebPasswordInit, f)
		},
	}
	addPasswordFlags(cmd, &f)
	return cmd
}

func newPasswdCmd() *cobra.Command {
	var f passwordFlags
	cmd := &cobra.Command{
		Use:   "passwd",
		Short: "Сменить пароль веб-интерфейса (нужен root); --disable — удалить пароль",
		Long: "После смены пароля открытые сессии веб-интерфейса завершаются.\n" +
			"--disable удаляет " + webauth.DefaultPath + ": вход в веб невозможен до sudo ramplus init.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode := core.WebPasswordChange
			if f.disable {
				mode = core.WebPasswordDisable
			}
			return runWebPassword(cmd, mode, f)
		},
	}
	addPasswordFlags(cmd, &f)
	cmd.Flags().BoolVar(&f.disable, "disable", false, "удалить пароль (откат init): веб покажет «выполните sudo ramplus init»")
	return cmd
}

func addPasswordFlags(cmd *cobra.Command, f *passwordFlags) {
	cmd.Flags().BoolVar(&f.stdin, "password-stdin", false, "прочитать пароль из первой строки stdin (для скриптов)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "только показать, что будет записано")
}

func runWebPassword(cmd *cobra.Command, mode core.WebPasswordMode, f passwordFlags) error {
	out := cmd.OutOrStdout()
	c := newCore()
	if !f.dryRun && c.Sys.Kernel.Geteuid() != 0 {
		return fmt.Errorf("%w: sudo ramplus %s", journal.ErrNeedRoot, cmd.Name())
	}
	gid, err := lookupWebGroup()
	if err != nil && !f.dryRun {
		return err
	}
	req := core.WebPasswordRequest{Mode: mode, GID: gid, DryRun: f.dryRun}
	if mode != core.WebPasswordDisable {
		if req.Password, err = readPassword(cmd.InOrStdin(), out, f.stdin); err != nil {
			return err
		}
	}
	res, err := c.WebPassword(req)
	if err != nil {
		return err
	}
	switch {
	case res.DryRun && res.Action == "remove":
		fmt.Fprintf(out, "dry-run: будет удалён %s — вход в веб-интерфейс станет невозможен\n", res.Path)
	case res.DryRun:
		fmt.Fprintf(out, "dry-run: будет записан %s (хеш argon2id, владелец %s, права %s); ничего не изменено\n", res.Path, res.Owner, res.Mode)
	case res.Action == "remove":
		fmt.Fprintf(out, "Пароль веб-интерфейса удалён (%s). Задать снова: sudo ramplus init\n", res.Path)
	case mode == core.WebPasswordChange:
		fmt.Fprintf(out, "Пароль веб-интерфейса изменён (%s). Открытые сессии завершены.\n", res.Path)
	default:
		fmt.Fprintf(out, "Пароль веб-интерфейса задан (%s).\nЗапустить: sudo systemctl enable --now ramplusd ramplus-web, затем откройте http://127.0.0.1:7781\n", res.Path)
	}
	return nil
}

// readPassword — из stdin (--password-stdin) или дважды из терминала без эха.
func readPassword(in io.Reader, out io.Writer, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		pw := strings.TrimRight(line, "\r\n")
		if pw == "" {
			return "", errors.New("--password-stdin: пустой пароль")
		}
		return pw, webauth.CheckPassword(pw)
	}
	file, ok := in.(*os.File)
	if !ok || !isTerminal(file) {
		return "", errors.New("нет терминала для ввода пароля: используйте --password-stdin")
	}
	fmt.Fprintf(out, "Новый пароль веб-интерфейса (не меньше %d символов): ", webauth.MinPasswordLen)
	pw, err := readNoEcho(file)
	if err != nil {
		return "", err
	}
	if err := webauth.CheckPassword(pw); err != nil {
		return "", err
	}
	fmt.Fprint(out, "Повторите пароль: ")
	again, err := readNoEcho(file)
	if err != nil {
		return "", err
	}
	if again != pw {
		return "", errors.New("пароли не совпадают — ничего не изменено")
	}
	return pw, nil
}

// readNoEcho читает строку с выключенным эхо терминала.
func readNoEcho(f *os.File) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	noEcho.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return "", err
	}
	defer func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, old)
		fmt.Fprintln(os.Stderr)
	}()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
