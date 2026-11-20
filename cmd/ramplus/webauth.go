package main

import (
	"github.com/ashot0907/ramplus/internal/i18n"

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
		return 0, i18n.Errorf("cli.webauth.no_group", webauth.Group, err)
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
		Short: T("cli.init.short"),
		Long: T("cli.init.long1") + webauth.DefaultPath + " (root:" + webauth.Group + ", 0640).\n" +
			T("cli.init.long2") +
			T("cli.init.long3"),
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
		Short: T("cli.passwd.short"),
		Long: T("cli.passwd.long1") +
			T("cli.passwd.long2") + webauth.DefaultPath + T("cli.passwd.long3"),
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
	cmd.Flags().BoolVar(&f.disable, "disable", false, T("cli.flag.disable"))
	return cmd
}

func addPasswordFlags(cmd *cobra.Command, f *passwordFlags) {
	cmd.Flags().BoolVar(&f.stdin, "password-stdin", false, T("cli.flag.password_stdin"))
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, T("cli.flag.dry_run_write"))
}

func runWebPassword(cmd *cobra.Command, mode core.WebPasswordMode, f passwordFlags) error {
	out := cmd.OutOrStdout()
	c := newCore()
	if !f.dryRun && c.Sys.Kernel.Geteuid() != 0 {
		return i18n.Errorf("cli.need_root_cmd", journal.ErrNeedRoot, cmd.Name())
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
		fmt.Fprintf(out, T("cli.webauth.dry_remove"), res.Path)
	case res.DryRun:
		fmt.Fprintf(out, T("cli.webauth.dry_write"), res.Path, res.Owner, res.Mode)
	case res.Action == "remove":
		fmt.Fprintf(out, T("cli.webauth.removed"), res.Path)
	case mode == core.WebPasswordChange:
		fmt.Fprintf(out, T("cli.webauth.changed"), res.Path)
	default:
		fmt.Fprintf(out, T("cli.webauth.set"), res.Path)
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
			return "", errors.New(T("cli.webauth.empty"))
		}
		return pw, webauth.CheckPassword(pw)
	}
	file, ok := in.(*os.File)
	if !ok || !isTerminal(file) {
		return "", errors.New(T("cli.webauth.no_tty"))
	}
	fmt.Fprintf(out, T("cli.webauth.prompt"), webauth.MinPasswordLen)
	pw, err := readNoEcho(file)
	if err != nil {
		return "", err
	}
	if err := webauth.CheckPassword(pw); err != nil {
		return "", err
	}
	fmt.Fprint(out, T("cli.webauth.prompt_again"))
	again, err := readNoEcho(file)
	if err != nil {
		return "", err
	}
	if again != pw {
		return "", errors.New(T("cli.webauth.mismatch"))
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
