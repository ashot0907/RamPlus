package main

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/api"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: T("cli.daemon.short"),
	}
	cmd.AddCommand(newDaemonStatusCmd())
	return cmd
}

// DaemonStatus — ramplus daemon status --json.
type DaemonStatus struct {
	Socket       string      `json:"socket"`
	Available    bool        `json:"available"`
	Error        string      `json:"error,omitempty"`
	CLIVersion   string      `json:"cli_version"`
	Health       *api.Health `json:"health,omitempty"`
	VersionMatch bool        `json:"version_match"`
}

func newDaemonStatusCmd() *cobra.Command {
	var (
		socket string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: T("cli.daemon.status_short"),
		Long:  T("cli.daemon.status_long"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ds := DaemonStatus{Socket: socket, CLIVersion: version}
			h, err := api.NewClient(socket).Health(cmd.Context())
			if err == nil {
				ds.Available, ds.Health, ds.VersionMatch = true, &h, h.Version == version
			} else {
				ds.Error = daemonError(socket, err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if err := writeJSON(out, ds); err != nil {
					return err
				}
				if !ds.Available {
					return errSilent
				}
				return nil
			}
			if !ds.Available {
				return errors.New(ds.Error)
			}
			match := T("cli.daemon.version_match")
			if !ds.VersionMatch {
				match = fmt.Sprintf(T("cli.daemon.version_mismatch"), version)
			}
			fmt.Fprintf(out, T("cli.daemon.available"), socket)
			fmt.Fprintf(out, T("cli.daemon.version"), h.Version, match)
			fmt.Fprintf(out, "  API:      %s\n", h.API)
			fmt.Fprintf(out, T("cli.daemon.uptime"), (time.Duration(h.UptimeSeconds) * time.Second).String(), h.PID)
			if !h.Root {
				fmt.Fprintln(out, T("cli.daemon.not_root"))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", api.DefaultSocket, T("cli.daemon.socket_flag"))
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func daemonError(socket string, err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOENT):
		return fmt.Sprintf(T("cli.daemon.no_socket"), socket)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf(T("cli.daemon.refused"), socket)
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return fmt.Sprintf(T("cli.daemon.no_access"), socket)
	}
	return fmt.Sprintf(T("cli.daemon.unreachable"), socket, err)
}
