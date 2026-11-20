// Команда ramplus-web — веб-интерфейс RamPlus: отдаёт встроенный фронтенд и
// проксирует /api/* на демон ramplusd. Никогда не работает от root.
// Тонкая оболочка над internal/web.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ashot0907/ramplus/internal/api"
	"github.com/ashot0907/ramplus/internal/web"
	"github.com/ashot0907/ramplus/internal/webauth"
	"github.com/ashot0907/ramplus/internal/webui"
)

// version подставляется при сборке: -ldflags "-X main.version=..."
var version = "dev"

// defaultMemLimit — две одновременные проверки argon2id по 64 MiB + запас.
const defaultMemLimit = 256 << 20

type hostList []string

func (h *hostList) String() string     { return strings.Join(*h, ",") }
func (h *hostList) Set(v string) error { *h = append(*h, v); return nil }

func main() { os.Exit(run()) }

func run() int {
	listen := flag.String("listen", web.DefaultListen, "address:port; a non-local address requires TLS")
	certFile := flag.String("tls-cert", "", "TLS certificate (PEM)")
	keyFile := flag.String("tls-key", "", "TLS key (PEM)")
	selfSigned := flag.Bool("tls-self-signed", false, "TLS with a self-signed certificate (generated at start)")
	socket := flag.String("socket", api.DefaultSocket, "ramplusd daemon socket")
	authFile := flag.String("auth-file", webauth.DefaultPath, "password file (sudo ramplus init)")
	var hosts hostList
	flag.Var(&hosts, "host", "extra allowed Host name (repeatable), for example ramplus.lan")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{} // время добавляет journald
			}
			return a
		},
	}))
	fail := func(msg string, args ...any) int { log.Error(msg, args...); return 1 }

	if err := web.CheckNotRoot(os.Geteuid()); err != nil {
		return fail("refusing to start", "err", err)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultMemLimit)
	}
	withTLS := *selfSigned || *certFile != "" || *keyFile != ""
	if (*certFile == "") != (*keyFile == "") {
		return fail("both flags are required: --tls-cert and --tls-key")
	}
	if *selfSigned && *certFile != "" {
		return fail("--tls-self-signed and --tls-cert are mutually exclusive")
	}
	if err := web.ValidateListen(*listen, withTLS); err != nil {
		return fail("refusing to start", "err", err)
	}
	if h := web.ListenHost(*listen); h != "" {
		hosts = append(hosts, h)
	}

	srv := web.New(web.Config{Hosts: hosts, TLS: withTLS, Socket: *socket, AuthFile: *authFile,
		Assets: webui.Assets(), Version: version, Log: log})
	hs := srv.HTTPServer()
	base, stop := context.WithCancel(context.Background())
	defer stop()
	hs.BaseContext = func(net.Listener) context.Context { return base } // остановка закрывает SSE
	if withTLS {
		var cert tls.Certificate
		var err error
		if *selfSigned {
			var fp string
			cert, fp, err = web.SelfSigned(hosts, time.Now())
			if err == nil {
				log.Info("self-signed certificate: compare the fingerprint in the browser", "sha256", fp)
			}
		} else {
			cert, err = tls.LoadX509KeyPair(*certFile, *keyFile)
		}
		if err != nil {
			return fail("TLS", "err", err)
		}
		hs.TLSConfig = web.TLSConfig(cert)
	}

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fail("listen", "addr", *listen, "err", err)
	}
	if withTLS {
		l = tls.NewListener(l, hs.TLSConfig)
	}
	scheme := "http"
	if withTLS {
		scheme = "https"
	}
	if _, err := os.Stat(*authFile); errors.Is(err, os.ErrNotExist) {
		log.Warn("no password set: the web UI will show «run sudo ramplus init»", "file", *authFile)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(l) }()
	log.Info("ramplus-web started", "version", version, "url", scheme+"://"+*listen, "daemon", *socket, "pid", os.Getpid())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case s := <-sig:
		log.Info("stopping", "signal", s.String())
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fail("serve", "err", err)
		}
	}
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(ctx) // SSE-потоки прерываются по таймауту
	log.Info("ramplus-web stopped")
	return 0
}
