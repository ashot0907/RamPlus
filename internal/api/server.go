// Package api — HTTP/JSON API демона ramplusd поверх unix-сокета. Тонкая
// оболочка над internal/core: разбор запросов, авторизация по SO_PEERCRED,
// коды ошибок, SSE. Логики планирования и выполнения здесь нет.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ashot0907/ramplus/internal/core"
)

// Параметры транспорта.
const (
	DefaultSocket   = "/run/ramplus/ramplusd.sock"
	APIVersion      = "v1"
	MaxRequestBytes = 64 << 10
	SocketMode      = 0o660
	SocketDirMode   = 0o750
	sseWriteTimeout = 10 * time.Second
	sseHeartbeat    = 15 * time.Second
	maxSocketPath   = 108 // sizeof(sockaddr_un.sun_path) в Linux
)

// Server — API демона.
type Server struct {
	Core    *core.Core
	Version string
	Policy  Policy
	Log     *slog.Logger
	// StatusInterval — период снимков /v1/status/stream (по умолчанию 1 с).
	StatusInterval time.Duration
	// WebUID — uid пользователя ramplus-web: его операции помечаются в
	// журнале via=web (по SO_PEERCRED, заголовком не подделать). nil — нет.
	WebUID *uint32
	// LookupUser — имя пользователя по uid для журнала ("" — нет).
	LookupUser func(uid uint32) string

	started time.Time
	streams atomic.Int64 // открытые SSE-потоки (тесты: отключение освобождает)
	mux     *http.ServeMux
}

// New — API над core.
func New(c *core.Core, version string, pol Policy, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{Core: c, Version: version, Policy: pol, Log: log, StatusInterval: time.Second, started: time.Now()}
	s.routes()
	return s
}

// Streams — сколько SSE-потоков открыто сейчас.
func (s *Server) Streams() int64 { return s.streams.Load() }

// HTTPServer — http.Server с таймаутами; peers определяет клиента каждого
// соединения; отмена base завершает SSE-потоки (остановка демона).
func (s *Server) HTTPServer(peers PeerSource, base context.Context) *http.Server {
	return &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// SSE продлевает срок записи перед каждым событием.
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
		BaseContext:    func(net.Listener) context.Context { return base },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			p, err := peers.PeerOf(c)
			return withPeer(ctx, p, err)
		},
		ErrorLog: slog.NewLogLogger(s.Log.Handler(), slog.LevelWarn),
	}
}

// Listen создаёт сокет: каталог 0750 root:группа, сокет 0660 владелец
// процесса:группа (gid nil — группа не меняется). Оставшийся от упавшего
// демона сокет удаляется; если на нём кто-то слушает — ошибка.
func Listen(path string, gid *uint32) (net.Listener, error) {
	if len(path) >= maxSocketPath {
		return nil, fmt.Errorf("путь сокета %s длиннее %d байт (ограничение unix-сокетов)", path, maxSocketPath-1)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, SocketDirMode); err != nil {
		return nil, err
	}
	if gid != nil {
		if err := os.Chown(dir, -1, int(*gid)); err != nil {
			return nil, fmt.Errorf("chown %s: %w", dir, err)
		}
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s существует и это не сокет", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: демон уже запущен", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	// Сокет создаётся сразу без прав для остальных; права и группа — ниже.
	old := syscall.Umask(0o177)
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	l.(*net.UnixListener).SetUnlinkOnClose(true)
	if gid != nil {
		if err := os.Chown(path, -1, int(*gid)); err != nil {
			l.Close()
			return nil, fmt.Errorf("chown %s: %w", path, err)
		}
	}
	if err := os.Chmod(path, SocketMode); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// ServeHTTP — запись в лог и маршрутизация.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	s.mux.ServeHTTP(rw, r)
	p, _ := PeerFrom(r.Context())
	s.Log.Info("запрос", "event", EventRequest, "method", r.Method, "path", r.URL.Path, "status", rw.status,
		"uid", p.UID, "pid", p.PID, "ms", time.Since(start).Milliseconds())
}

// statusWriter запоминает код ответа; Unwrap — для http.ResponseController
// (Flush и сроки записи в SSE).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
