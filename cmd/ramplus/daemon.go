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
		Short: "Демон ramplusd: доступность и версия",
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
		Short: "Доступен ли демон, его версия и совпадает ли она с версией CLI",
		Long:  "Только чтение. Код выхода 1 — демон недоступен.",
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
			match := "совпадает с CLI"
			if !ds.VersionMatch {
				match = fmt.Sprintf("НЕ совпадает с CLI (%s) — перезапустите демон после обновления: sudo systemctl restart ramplusd", version)
			}
			fmt.Fprintf(out, "ramplusd: доступен (%s)\n", socket)
			fmt.Fprintf(out, "  версия:   %s — %s\n", h.Version, match)
			fmt.Fprintf(out, "  API:      %s\n", h.API)
			fmt.Fprintf(out, "  работает: %s (pid %d)\n", (time.Duration(h.UptimeSeconds) * time.Second).String(), h.PID)
			if !h.Root {
				fmt.Fprintln(out, "  внимание: демон запущен не от root — apply будет отказывать")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", api.DefaultSocket, "путь сокета демона")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func daemonError(socket string, err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOENT):
		return fmt.Sprintf("ramplusd недоступен: нет сокета %s (демон не запущен: sudo systemctl start ramplusd)", socket)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("ramplusd недоступен: %s никто не слушает (демон упал? journalctl -u ramplusd)", socket)
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return fmt.Sprintf("нет доступа к %s: нужно быть в группе ramplus (sudo usermod -aG ramplus $USER и перелогиниться)", socket)
	}
	return fmt.Sprintf("ramplusd недоступен (%s): %v", socket, err)
}
