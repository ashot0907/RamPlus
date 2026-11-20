// Package web — HTTP-сервер веб-интерфейса (ramplus-web): отдаёт
// встроенный фронтенд и проксирует /api/* на сокет демона /v1/* по явному
// списку путей. Вход по паролю (argon2id), сессии в памяти, CSRF,
// ограничение попыток, проверка Host, строгие заголовки безопасности.
// Логики RamPlus здесь нет: всё делает демон (internal/core).
package web

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ashot0907/ramplus/internal/webauth"
)

// Параметры по умолчанию.
const (
	DefaultListen   = "127.0.0.1:7781"
	maxBodyBytes    = 64 << 10 // как у демона
	maxLoginBytes   = 4 << 10
	requestDeadline = 60 * time.Second
	verifySlots     = 2 // одновременных проверок argon2id (по 64 MiB)
)

// Машинные имена событий журнала (поле event=). Пароли в журнал не пишутся.
const (
	EventRequest           = "request"
	EventLoginOK           = "login.ok"
	EventLoginFailed       = "login.failed"
	EventLoginRateLimited  = "login.rate_limited"
	EventCSRFInvalid       = "csrf.invalid"
	EventHostRejected      = "host.rejected"
	EventDaemonUnavailable = "daemon.unavailable"
	EventAuthUnreadable    = "auth.unreadable"
)

// CSP — никаких inline-скриптов и внешних источников.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
	"connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Config — настройки сервера.
type Config struct {
	// Hosts — дополнительные разрешённые значения Host (--host).
	Hosts []string
	// TLS — сервер работает по TLS (cookie с Secure и префиксом __Host-).
	TLS      bool
	Socket   string // сокет демона
	AuthFile string // файл пароля (webauth.DefaultPath)
	Assets   fs.FS  // собранный фронтенд
	Version  string
	Log      *slog.Logger
	Now      func() time.Time
	// Transport — до демона (тесты); nil — unix-сокет Socket.
	Transport http.RoundTripper
}

// Server — веб-сервер.
type Server struct {
	cfg      Config
	hosts    map[string]bool
	sessions *sessionStore
	limiter  *limiter
	proxy    *httputil.ReverseProxy
	mux      *http.ServeMux
	verify   chan struct{}
}

// apiRoutes — что проксируется на демон (/api/X → /v1/X). Остальное — 404.
var apiRoutes = []string{
	"GET /api/health",
	"GET /api/status",
	"GET /api/status/stream",
	"GET /api/history",
	"GET /api/recommendation",
	"GET /api/profiles",
	"GET /api/operations/{id}",
	"GET /api/operations/{id}/events",
	"POST /api/plan/setup",
	"POST /api/plan/swap",
	"POST /api/plan/profile",
	"POST /api/plan/profile-reset",
	"POST /api/plan/rollback",
	"POST /api/apply",
}

// streamPaths — SSE: без срока записи и буферизации.
func isStream(path string) bool {
	return path == "/api/status/stream" || strings.HasSuffix(path, "/events")
}

// New — сервер с конфигурацией cfg.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AuthFile == "" {
		cfg.AuthFile = webauth.DefaultPath
	}
	s := &Server{cfg: cfg, sessions: newSessionStore(), limiter: newLimiter(), verify: make(chan struct{}, verifySlots),
		hosts: map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}}
	for _, h := range cfg.Hosts {
		s.hosts[normHost(h)] = true
	}
	tr := cfg.Transport
	if tr == nil {
		tr = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", cfg.Socket)
			},
			MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second, DisableCompression: true,
		}
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "ramplusd", "ramplusd"
			pr.Out.URL.Path = "/v1" + strings.TrimPrefix(pr.In.URL.Path, "/api")
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			for _, h := range []string{"Cookie", "X-Csrf-Token", "Origin", "Referer", "Authorization"} {
				pr.Out.Header.Del(h)
			}
		},
		Transport:     tr,
		FlushInterval: -1, // SSE: каждое событие сразу
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // клиент ушёл
			}
			s.cfg.Log.Warn("daemon unavailable", "event", EventDaemonUnavailable, "path", r.URL.Path, "err", err)
			writeError(w, http.StatusBadGateway, "daemon.unavailable",
				i18n.T(langOf(r), "web.err.daemon_unavailable", err))
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			return nil
		},
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/session", s.sessionInfo)
	m.HandleFunc("POST /api/login", s.login)
	m.Handle("POST /api/logout", s.authed(http.HandlerFunc(s.logout)))
	for _, pat := range apiRoutes {
		m.Handle(pat, s.authed(http.HandlerFunc(s.proxyAPI)))
	}
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "request.not_found", i18n.T(langOf(r), "web.err.not_found", r.URL.Path))
	})
	m.HandleFunc("/", s.static)
	s.mux = m
}

// Handler — корневой обработчик: заголовки безопасности, проверка Host,
// журнал запросов.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	start := s.cfg.Now()
	h := w.Header()
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
	rc := http.NewResponseController(w)
	if isStream(r.URL.Path) {
		_ = rc.SetWriteDeadline(time.Time{})
		_ = rc.SetReadDeadline(time.Time{})
	} else {
		_ = rc.SetWriteDeadline(time.Now().Add(requestDeadline))
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	switch {
	case !s.hostAllowed(r.Host):
		// Защита от DNS rebinding: чужое имя в Host — не наш сайт.
		s.cfg.Log.Warn("host rejected", "event", EventHostRejected, "host", r.Host, "ip", clientIP(r))
		writeError(sw, http.StatusMisdirectedRequest, "host.not_allowed",
			i18n.T(langOf(r), "web.err.host", r.Host))
	case r.URL.RawPath != "":
		writeError(sw, http.StatusBadRequest, "request.invalid", i18n.T(langOf(r), "web.err.encoded_path"))
	default:
		s.mux.ServeHTTP(sw, r)
	}
	lvl := slog.LevelInfo
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		lvl = slog.LevelDebug
	}
	s.cfg.Log.Log(r.Context(), lvl, "request", "event", EventRequest, "method", r.Method, "path", r.URL.Path, "status", sw.status,
		"ip", clientIP(r), "ms", s.cfg.Now().Sub(start).Milliseconds())
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func normHost(h string) string {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

func (s *Server) hostAllowed(host string) bool { return host != "" && s.hosts[normHost(host)] }

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// Error — тело ответа с ошибкой (как у демона).
type Error struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	RetryAfter   int    `json:"retry_after_seconds,omitempty"`
	AttemptsLeft *int   `json:"attempts_left,omitempty"`
}

// langOf — язык текстов ответа (Accept-Language, по умолчанию английский).
func langOf(r *http.Request) i18n.Lang {
	return i18n.FromAcceptLanguage(r.Header.Get("Accept-Language"))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, Error{Code: errCode, Message: msg})
}

// --- пароль и сессии ---

var errNotConfigured = errors.New("password not set")

// authState — хеш пароля и его версия (меняется при passwd).
func (s *Server) authState() (hash, version string, err error) {
	b, err := os.ReadFile(s.cfg.AuthFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", errNotConfigured
	}
	if err != nil {
		return "", "", err
	}
	hash, err = webauth.ParseFile(b)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", s.cfg.AuthFile, err)
	}
	sum := sha256.Sum256([]byte(hash))
	return hash, hex.EncodeToString(sum[:8]), nil
}

func (s *Server) cookieName() string {
	if s.cfg.TLS {
		return "__Host-ramplus_session"
	}
	return "ramplus_session"
}

func (s *Server) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.cfg.TLS, SameSite: http.SameSiteStrictMode})
}

func (s *Server) authError(w http.ResponseWriter, l i18n.Lang, err error) {
	if errors.Is(err, errNotConfigured) {
		writeError(w, http.StatusServiceUnavailable, "auth.not_configured", i18n.T(l, "web.err.not_configured"))
		return
	}
	s.cfg.Log.Error("password file unreadable", "event", EventAuthUnreadable, "file", s.cfg.AuthFile, "err", err)
	writeError(w, http.StatusInternalServerError, "auth.unreadable",
		i18n.T(l, "web.err.auth_unreadable", err, webauth.Group))
}

// current — сессия запроса (nil — не вошёл).
func (s *Server) current(r *http.Request, version string) (string, *session) {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return "", nil
	}
	return c.Value, s.sessions.get(c.Value, s.cfg.Now(), version)
}

type ctxKey struct{}

// authed — нужен вход; изменяющим запросам — ещё и CSRF-токен.
func (s *Server) authed(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, version, err := s.authState()
		if err != nil {
			s.authError(w, langOf(r), err)
			return
		}
		id, ss := s.current(r, version)
		if ss == nil {
			writeError(w, http.StatusUnauthorized, "auth.required", i18n.T(langOf(r), "web.err.login_required"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.sameOrigin(r) {
				writeError(w, http.StatusForbidden, "origin.forbidden", i18n.T(langOf(r), "web.err.cross_site"))
				return
			}
			if !csrfOK(ss, r.Header.Get("X-CSRF-Token")) {
				s.cfg.Log.Warn("invalid CSRF token", "event", EventCSRFInvalid, "path", r.URL.Path, "ip", clientIP(r))
				writeError(w, http.StatusForbidden, "csrf.invalid", i18n.T(langOf(r), "web.err.csrf"))
				return
			}
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// sameOrigin — Origin (если браузер его прислал) — разрешённый хост;
// Sec-Fetch-Site не cross-site.
func (s *Server) sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && s.hostAllowed(u.Host)
}

// SessionInfo — GET /api/session.
type SessionInfo struct {
	PasswordSet   bool      `json:"password_set"`
	Authenticated bool      `json:"authenticated"`
	CSRFToken     string    `json:"csrf_token,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitzero"`
	Version       string    `json:"version"`
}

func (s *Server) sessionInfo(w http.ResponseWriter, r *http.Request) {
	_, version, err := s.authState()
	info := SessionInfo{Version: s.cfg.Version}
	switch {
	case errors.Is(err, errNotConfigured):
	case err != nil:
		s.authError(w, langOf(r), err)
		return
	default:
		info.PasswordSet = true
		if _, ss := s.current(r, version); ss != nil {
			info.Authenticated, info.CSRFToken, info.ExpiresAt = true, ss.csrf, ss.expires
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// LoginRequest — POST /api/login.
type LoginRequest struct {
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip, now := clientIP(r), s.cfg.Now()
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "request.invalid", i18n.T(langOf(r), "web.err.content_type"))
		return
	}
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin.forbidden", i18n.T(langOf(r), "web.err.cross_site"))
		return
	}
	hash, version, err := s.authState()
	if err != nil {
		s.authError(w, langOf(r), err)
		return
	}
	if wait := s.limiter.blocked(ip, now); wait > 0 {
		s.cfg.Log.Warn("login paused", "event", EventLoginRateLimited, "ip", ip, "retry_after_seconds", int(wait.Seconds()))
		retryAfter(w, langOf(r), wait)
		return
	}
	var req LoginRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxLoginBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request.invalid", i18n.T(langOf(r), "web.err.bad_json"))
		return
	}
	select {
	case s.verify <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	ok, verr := webauth.Verify(hash, req.Password)
	<-s.verify
	if verr != nil {
		s.authError(w, langOf(r), verr)
		return
	}
	if !ok {
		lock, left := s.limiter.fail(ip, now)
		// В журнал — без пароля.
		s.cfg.Log.Warn("login failed", "event", EventLoginFailed, "ip", ip, "attempts_left", left, "lockout_seconds", int(lock.Seconds()))
		e := Error{Code: "auth.invalid", Message: i18n.T(langOf(r), "web.err.bad_password"), AttemptsLeft: &left}
		if lock > 0 {
			e.RetryAfter = int((lock + time.Second - 1) / time.Second)
			e.Message = i18n.T(langOf(r), "web.err.bad_password_pause", e.RetryAfter)
		}
		writeJSON(w, http.StatusUnauthorized, e)
		return
	}
	s.limiter.success(ip)
	id, ss := s.sessions.create(now, SessionTTL, version)
	s.setCookie(w, id, int(SessionTTL/time.Second))
	s.cfg.Log.Info("login ok", "event", EventLoginOK, "ip", ip)
	writeJSON(w, http.StatusOK, SessionInfo{PasswordSet: true, Authenticated: true, CSRFToken: ss.csrf, ExpiresAt: ss.expires, Version: s.cfg.Version})
}

func retryAfter(w http.ResponseWriter, l i18n.Lang, wait time.Duration) {
	sec := int((wait + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(sec))
	writeJSON(w, http.StatusTooManyRequests, Error{Code: "auth.rate_limited", RetryAfter: sec,
		Message: i18n.T(l, "web.err.rate_limited", sec)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if id, ok := r.Context().Value(ctxKey{}).(string); ok {
		s.sessions.delete(id)
	}
	s.setCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// --- прокси и статика ---

func (s *Server) proxyAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.ContentLength != 0 {
		writeError(w, http.StatusUnsupportedMediaType, "request.invalid", i18n.T(langOf(r), "web.err.content_type"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if !isStream(r.URL.Path) {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.proxy.ServeHTTP(w, r)
}

const notBuilt = `<!doctype html><html lang="ru"><head><meta charset="utf-8"><title>RamPlus</title></head>` +
	`<body><p>Фронтенд не собран: выполните <code>make web</code> и пересоберите ramplus-web.</p></body></html>`

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "request.method_not_allowed", i18n.T(langOf(r), "web.err.method"))
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	b, err := fs.ReadFile(s.cfg.Assets, name)
	if err != nil {
		if name == "index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, notBuilt)
			return
		}
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // имена с хешем
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(b)))
}
