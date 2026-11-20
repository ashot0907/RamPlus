// Команда ramplusd — демон RamPlus: HTTP/JSON API поверх unix-сокета.
// Тонкая оболочка над internal/core (через internal/api).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ashot0907/ramplus/internal/api"
	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/sys"
)

// version подставляется при сборке: -ldflags "-X main.version=..."
var version = "dev"

// defaultMemLimit — мягкий лимит памяти Go, если GOMEMLIMIT не задан.
const defaultMemLimit = 64 << 20

func main() { os.Exit(run()) }

func run() int {
	socket := flag.String("socket", api.DefaultSocket, "unix socket path")
	readGroup := flag.String("group", "ramplus", "group with read access (status, history, plan); group owner of the socket")
	adminGroup := flag.String("admin-group", "ramplus-admin", "group with change access (apply)")
	webUser := flag.String("web-user", "ramplus-web", "web interface user: its operations are recorded in the journal as «via the web interface»")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	// journald добавляет время сам; формат key=value.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultMemLimit)
	}
	lockMemory(log)
	if os.Geteuid() != 0 {
		log.Warn("daemon is not running as root: plans work, apply will fail", "euid", os.Geteuid())
	}

	pol := api.Policy{ReadGID: lookupGroup(log, *readGroup), AdminGID: lookupGroup(log, *adminGroup)}
	c := core.New(sys.DefaultSystem())
	c.Daemon = true
	srv := api.New(c, version, pol, log)
	srv.LookupUser = lookupUserName
	if u, err := user.Lookup(*webUser); err == nil {
		if id, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
			v := uint32(id)
			srv.WebUID = &v
		}
	}

	l, err := api.Listen(*socket, pol.ReadGID)
	if err != nil {
		log.Error("socket", "path", *socket, "err", err)
		return 1
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	hs := srv.HTTPServer(api.PeerCred{}, base)
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(l) }()
	log.Info("ramplusd started", "version", version, "socket", *socket, "pid", os.Getpid(),
		"read_group", *readGroup, "admin_group", *adminGroup)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case s := <-sig:
		log.Info("stopping", "signal", s.String())
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			return 1
		}
	}

	// Новые запросы не принимаются, SSE-потоки закрываются; выполняемая
	// операция доводится до конца (журнал write-ahead защищает и от
	// жёсткого убийства, но доделать лучше).
	cancel()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := hs.Shutdown(ctx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	log.Info("waiting for the current operation")
	c.Wait()
	log.Info("ramplusd stopped")
	return 0
}

// lockMemory — демон не должен уходить в swap: он нужен как раз при
// нехватке памяти. MCL_ONFAULT (Linux ≥ 4.4) закрепляет страницы по мере
// использования, а не всё зарезервированное адресное пространство Go
// сразу; без него — обычный MCL_CURRENT|MCL_FUTURE.
//
// Не от root (без CAP_IPC_LOCK) MCL_FUTURE ограничен RLIMIT_MEMLOCK (обычно
// 8 MiB): рост кучи Go упёрся бы в ENOMEM и уронил процесс. Поэтому без root
// память не закрепляется.
func lockMemory(log *slog.Logger) {
	if os.Geteuid() != 0 {
		log.Warn("not root: mlockall skipped (RLIMIT_MEMLOCK)")
		return
	}
	err := unix.Mlockall(unix.MCL_CURRENT | unix.MCL_FUTURE | unix.MCL_ONFAULT)
	if errors.Is(err, unix.EINVAL) {
		err = unix.Mlockall(unix.MCL_CURRENT | unix.MCL_FUTURE)
	}
	if err != nil {
		log.Warn("mlockall failed: the daemon may be swapped out", "err", err)
		return
	}
	log.Info("daemon memory locked (mlockall)")
}

func lookupGroup(log *slog.Logger, name string) *uint32 {
	g, err := user.LookupGroup(name)
	if err != nil {
		log.Warn("group does not exist: only root has access (sudo systemd-sysusers)", "group", name, "err", err)
		return nil
	}
	id, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		log.Warn("group gid", "group", name, "err", err)
		return nil
	}
	v := uint32(id)
	return &v
}

// lookupUserName — имя пользователя по uid ("" — нет в /etc/passwd).
func lookupUserName(uid uint32) string {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return ""
	}
	return u.Username
}
