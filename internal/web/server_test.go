package web

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ashot0907/ramplus/internal/api"
	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/fakesys"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/webauth"
)

var cheap = webauth.Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const password = "correct-horse"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	clock *clock
	auth  string
	sock  string
	w     *fakesys.World
	api   *api.Server
}

type fixedPeer api.Peer

func (f fixedPeer) PeerOf(net.Conn) (api.Peer, error) { return api.Peer(f), nil }

func fixtureStatus() monitor.Status {
	td := func(p ...string) string { return filepath.Join(append([]string{"..", "monitor", "testdata"}, p...)...) }
	return monitor.CollectFrom(monitor.Paths{Meminfo: td("meminfo", "normal"), Swaps: td("swaps", "two-devices"),
		ZswapParams: td("zswap", "params-7.0"), ZswapDebug: td("absent"), VMSysctl: td("vm"), PSIMemory: td("psi", "memory"), SysBlock: td("sysblock")})
}

// startDaemon — настоящий API демона над фейковой системой на unix-сокете.
func startDaemon(t *testing.T, w *fakesys.World, dir string) (string, *api.Server) {
	t.Helper()
	c := core.New(w.System())
	c.Collect = fixtureStatus
	c.Daemon = true
	as := api.New(c, "test", api.Policy{}, nil)
	as.StatusInterval = 10 * time.Millisecond
	sock := filepath.Join(dir, "d.sock")
	l, err := api.Listen(sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	hs := as.HTTPServer(fixedPeer(api.Peer{}), base) // root
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() { cancel(); _ = hs.Close(); c.Wait() })
	return sock, as
}

func writeAuth(t *testing.T, path, pw string) {
	t.Helper()
	h, err := webauth.Hash(pw, cheap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, webauth.FileContent(h), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, withPassword bool, mut ...func(*Config)) *env {
	t.Helper()
	dir, err := os.MkdirTemp("", "rpw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := &env{t: t, clock: &clock{t: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}, auth: filepath.Join(dir, "web-auth"),
		w: fakesys.New().WithKernelParams(false).WithOldSwap(0)}
	if withPassword {
		writeAuth(t, e.auth, password)
	}
	e.sock, e.api = startDaemon(t, e.w, dir)
	cfg := Config{Socket: e.sock, AuthFile: e.auth, Now: e.clock.now, Version: "t",
		Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>RamPlus</title>")}, "assets/app-1.js": {Data: []byte("x")}}}
	for _, m := range mut {
		m(&cfg)
	}
	e.srv = New(cfg)
	e.ts = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

type resp struct {
	code   int
	body   []byte
	header http.Header
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("%d %s: %v", r.code, r.body, err)
	}
}

func (r resp) errCode(t *testing.T) string {
	t.Helper()
	var e Error
	r.json(t, &e)
	return e.Code
}

type client struct {
	e      *env
	cookie string
	csrf   string
}

func (e *env) client() *client { return &client{e: e} }

func (c *client) do(method, path, body string, hdr map[string]string) resp {
	c.e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, c.e.ts.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.e.srv.cookieName()+"="+c.cookie)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	for _, ck := range r.Cookies() {
		if ck.Name == c.e.srv.cookieName() {
			c.cookie = ck.Value
		}
	}
	return resp{r.StatusCode, b, r.Header}
}

func (c *client) login(pw string) resp {
	c.e.t.Helper()
	r := c.do("POST", "/api/login", `{"password":"`+pw+`"}`, nil)
	if r.code == 200 {
		var info SessionInfo
		r.json(c.e.t, &info)
		c.csrf = info.CSRFToken
	}
	return r
}

func (c *client) post(path, body string) resp {
	c.e.t.Helper()
	return c.do("POST", path, body, map[string]string{"X-CSRF-Token": c.csrf})
}

func TestNotConfigured(t *testing.T) {
	e := newEnv(t, false)
	c := e.client()
	var info SessionInfo
	c.do("GET", "/api/session", "", nil).json(t, &info)
	if info.PasswordSet || info.Authenticated {
		t.Errorf("session: %+v", info)
	}
	for _, r := range []resp{c.do("GET", "/api/status", "", nil), c.login(password)} {
		if r.code != 503 || r.errCode(t) != "auth.not_configured" {
			t.Errorf("%d %s", r.code, r.body)
		}
	}
	if r := c.do("GET", "/", "", nil); r.code != 200 || !strings.Contains(string(r.body), "RamPlus") {
		t.Errorf("index: %d", r.code)
	}
	// Пароль задан после запуска — подхватывается без перезапуска.
	writeAuth(t, e.auth, password)
	if r := c.login(password); r.code != 200 {
		t.Errorf("login after init: %d %s", r.code, r.body)
	}
}

func TestLoginAndCookie(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	if r := c.do("GET", "/api/status", "", nil); r.code != 401 || r.errCode(t) != "auth.required" {
		t.Fatalf("anon: %d %s", r.code, r.body)
	}
	r := c.login("wrong-password")
	var e1 Error
	r.json(t, &e1)
	if r.code != 401 || e1.Code != "auth.invalid" || e1.AttemptsLeft == nil || *e1.AttemptsLeft != 4 {
		t.Fatalf("wrong: %d %s", r.code, r.body)
	}
	if strings.Contains(string(r.body), "wrong-password") {
		t.Error("password echoed")
	}
	r = c.login(password)
	if r.code != 200 || c.csrf == "" {
		t.Fatalf("login: %d %s", r.code, r.body)
	}
	sc := r.header.Get("Set-Cookie")
	for _, want := range []string{"HttpOnly", "SameSite=Strict", "Path=/", "Max-Age=43200"} {
		if !strings.Contains(sc, want) {
			t.Errorf("cookie %q lacks %s", sc, want)
		}
	}
	if strings.Contains(sc, "Secure") {
		t.Error("Secure without TLS")
	}
	var info SessionInfo
	c.do("GET", "/api/session", "", nil).json(t, &info)
	if !info.Authenticated || info.CSRFToken != c.csrf {
		t.Errorf("session: %+v", info)
	}
	if r := c.do("GET", "/api/status", "", nil); r.code != 200 || !strings.Contains(string(r.body), `"schema_version"`) {
		t.Errorf("status via proxy: %d", r.code)
	}
	// Логин требует JSON (кросс-сайтовая форма без preflight не пройдёт).
	if r := c.do("POST", "/api/login", "", map[string]string{"Content-Type": "text/plain"}); r.code != 415 {
		t.Errorf("text/plain login: %d", r.code)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	for i := 1; i <= 4; i++ {
		if r := c.login("bad-password"); r.code != 401 {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	r := c.login("bad-password")
	var er Error
	r.json(t, &er)
	if r.code != 401 || er.RetryAfter != 30 {
		t.Fatalf("5th: %d %s", r.code, r.body)
	}
	// 6-я попытка — пауза, даже с верным паролем.
	r = c.login(password)
	r.json(t, &er)
	if r.code != 429 || er.Code != "auth.rate_limited" || er.RetryAfter != 30 || r.header.Get("Retry-After") != "30" {
		t.Fatalf("6th: %d %s", r.code, r.body)
	}
	e.clock.add(31 * time.Second)
	for i := 0; i < 5; i++ {
		c.login("bad-password")
	}
	r = c.login(password)
	r.json(t, &er)
	if r.code != 429 || er.RetryAfter != 60 {
		t.Fatalf("second lockout must double: %d %s", r.code, r.body)
	}
	e.clock.add(61 * time.Second)
	if r := c.login(password); r.code != 200 {
		t.Fatalf("after pause: %d %s", r.code, r.body)
	}
	// Успех сбрасывает счётчик.
	if r := c.login("bad-password"); r.code != 401 {
		t.Errorf("after success: %d", r.code)
	}
}

func TestCSRF(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	c.login(password)
	body := `{"profile":{"name":"default"}}`
	if r := c.do("POST", "/api/plan/setup", body, nil); r.code != 403 || r.errCode(t) != "csrf.invalid" {
		t.Errorf("no token: %d %s", r.code, r.body)
	}
	if r := c.do("POST", "/api/plan/setup", body, map[string]string{"X-CSRF-Token": "nope"}); r.code != 403 {
		t.Errorf("bad token: %d", r.code)
	}
	if r := c.do("POST", "/api/plan/setup", body, map[string]string{"X-CSRF-Token": c.csrf, "Origin": "http://evil.example"}); r.code != 403 || r.errCode(t) != "origin.forbidden" {
		t.Errorf("foreign origin: %d %s", r.code, r.body)
	}
	if r := c.do("POST", "/api/plan/setup", body, map[string]string{"X-CSRF-Token": c.csrf, "Sec-Fetch-Site": "cross-site"}); r.code != 403 {
		t.Errorf("cross-site: %d", r.code)
	}
	r := c.post("/api/plan/setup", body)
	if r.code != 200 || !strings.Contains(string(r.body), `"action": "setup"`) {
		t.Fatalf("with token: %d %s", r.code, r.body)
	}
	// Токен другой сессии не подходит.
	c2 := e.client()
	c2.login(password)
	c2.csrf = c.csrf
	if r := c2.post("/api/plan/setup", body); r.code != 403 {
		t.Errorf("foreign token: %d", r.code)
	}
}

func TestSessionLifetime(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	c.login(password)
	e.clock.add(SessionTTL - time.Minute)
	if r := c.do("GET", "/api/history", "", nil); r.code != 200 {
		t.Fatalf("before expiry: %d", r.code)
	}
	e.clock.add(2 * time.Minute)
	if r := c.do("GET", "/api/history", "", nil); r.code != 401 {
		t.Errorf("after 12h: %d", r.code)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	c.login(password)
	if r := c.do("POST", "/api/logout", "", nil); r.code != 403 {
		t.Errorf("logout without CSRF: %d", r.code)
	}
	old := c.cookie
	r := c.post("/api/logout", "")
	if r.code != 204 || !strings.Contains(r.header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d %v", r.code, r.header)
	}
	c.cookie = old // старая cookie больше недействительна
	if r := c.do("GET", "/api/status", "", nil); r.code != 401 {
		t.Errorf("after logout: %d", r.code)
	}
	if e.srv.sessions.len() != 0 {
		t.Error("session not deleted")
	}
}

func TestPasswdEndsSessions(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	c.login(password)
	writeAuth(t, e.auth, "another-password")
	if r := c.do("GET", "/api/status", "", nil); r.code != 401 {
		t.Errorf("session must end after passwd: %d", r.code)
	}
	if r := c.login("another-password"); r.code != 200 {
		t.Errorf("new password: %d", r.code)
	}
}

func TestHostCheck(t *testing.T) {
	e := newEnv(t, true, func(c *Config) { c.Hosts = []string{"RamPlus.LAN"} })
	c := e.client()
	for host, want := range map[string]int{
		"evil.example": 421, "evil.example:7781": 421, "127.0.0.2:7781": 421,
		"127.0.0.1:7781": 200, "localhost:7781": 200, "[::1]:7781": 200, "localhost": 200, "ramplus.lan:7781": 200,
	} {
		for _, path := range []string{"/", "/api/session"} {
			r := c.do("GET", path, "", map[string]string{"Host": host})
			if r.code != want {
				t.Errorf("Host %s %s: %d, want %d", host, path, r.code, want)
			}
			if want == 421 && r.errCode(t) != "host.not_allowed" {
				t.Errorf("code %s", r.body)
			}
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	for _, path := range []string{"/", "/api/session", "/api/nope"} {
		h := c.do("GET", path, "", nil).header
		want := map[string]string{"Content-Security-Policy": CSP, "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"}
		for k, v := range want {
			if h.Get(k) != v {
				t.Errorf("%s %s = %q", path, k, h.Get(k))
			}
		}
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
		if strings.Contains(CSP, bad) {
			t.Errorf("CSP contains %s", bad)
		}
	}
}

// Только явно разрешённые пути уходят на демон.
func TestProxyAllowlist(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"x=y"}},
			Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})
	e := newEnv(t, true, func(c *Config) { c.Transport = rt })
	c := e.client()
	c.login(password)
	allowed := []struct{ method, path, out string }{
		{"GET", "/api/health", "/v1/health"},
		{"GET", "/api/status", "/v1/status"},
		{"GET", "/api/history", "/v1/history"},
		{"GET", "/api/recommendation?path=/mnt/x/ramplus.swapfile", "/v1/recommendation"},
		{"GET", "/api/profiles", "/v1/profiles"},
		{"GET", "/api/operations/abc", "/v1/operations/abc"},
		{"POST", "/api/plan/setup", "/v1/plan/setup"},
		{"POST", "/api/plan/rollback", "/v1/plan/rollback"},
		{"POST", "/api/apply", "/v1/apply"},
	}
	for _, a := range allowed {
		var r resp
		if a.method == "GET" {
			r = c.do("GET", a.path, "", nil)
		} else {
			r = c.post(a.path, "{}")
		}
		if r.code != 200 || r.header.Get("Set-Cookie") != "" {
			t.Errorf("%s %s: %d %v", a.method, a.path, r.code, r.header)
		}
	}
	denied := []struct{ method, path string }{
		{"GET", "/api/apply"}, {"POST", "/api/status"}, {"GET", "/api/journal"}, {"GET", "/api/v1/status"},
		{"DELETE", "/api/apply"}, {"GET", "/api/operations/a%2Fb/events"},
	}
	for _, d := range denied {
		r := c.do(d.method, d.path, "", map[string]string{"X-CSRF-Token": c.csrf})
		if r.code < 400 {
			t.Errorf("%s %s: %d", d.method, d.path, r.code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(allowed) {
		t.Fatalf("daemon got %d requests, want %d", len(seen), len(allowed))
	}
	for i, r := range seen {
		if r.URL.Path != allowed[i].out {
			t.Errorf("forwarded %s, want %s", r.URL.Path, allowed[i].out)
		}
		for _, h := range []string{"Cookie", "X-Csrf-Token", "Origin"} {
			if r.Header.Get(h) != "" {
				t.Errorf("%s forwarded to daemon", h)
			}
		}
	}
	if seen[3].URL.RawQuery != "path=/mnt/x/ramplus.swapfile" {
		t.Errorf("query %q", seen[3].URL.RawQuery)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// SSE через прокси: события приходят по одному, без буферизации; поток
// операции — до result.
func TestSSEThroughProxy(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	c.login(password)
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/status/stream", nil)
	req.Header.Set("Cookie", e.srv.cookieName()+"="+c.cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	done := make(chan int)
	go func() {
		n := 0
		sc := bufio.NewScanner(r.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: status") {
				if n++; n == 3 {
					break
				}
			}
		}
		done <- n
	}()
	select {
	case n := <-done:
		if n != 3 {
			t.Errorf("events: %d", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SSE buffered or stalled")
	}
	r.Body.Close()
	// Поток статуса на стороне демона завершился (фейковая система не
	// потокобезопасна: дальше apply пишет в неё).
	for deadline := time.Now().Add(2 * time.Second); e.api.Streams() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("status stream not released after client disconnect")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Операция: план → apply → поток событий до result.
	var p struct {
		ID string `json:"id"`
	}
	c.post("/api/plan/setup", `{"swap":{"size":"4G","existing":"replace"},"profile":{"name":"zswap-lz4"}}`).json(t, &p)
	var acc api.ApplyAccepted
	ar := c.post("/api/apply", `{"plan_id":"`+p.ID+`","confirm_experimental":true}`)
	if ar.code != 202 {
		t.Fatalf("apply: %d %s", ar.code, ar.body)
	}
	ar.json(t, &acc)
	ev := c.do("GET", "/api/operations/"+acc.OperationID+"/events", "", nil)
	if ev.code != 200 || !strings.Contains(string(ev.body), "event: result") || !strings.Contains(string(ev.body), `"status":"done"`) {
		t.Errorf("events: %d %s", ev.code, ev.body)
	}
}

func TestDaemonDown(t *testing.T) {
	e := newEnv(t, true, func(c *Config) { c.Socket = "/nonexistent/ramplusd.sock" })
	c := e.client()
	c.login(password)
	if r := c.do("GET", "/api/status", "", nil); r.code != 502 || r.errCode(t) != "daemon.unavailable" {
		t.Errorf("%d %s", r.code, r.body)
	}
}

func TestStatic(t *testing.T) {
	e := newEnv(t, true)
	c := e.client()
	if r := c.do("GET", "/assets/app-1.js", "", nil); r.code != 200 || !strings.Contains(r.header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %v", r.code, r.header)
	}
	if r := c.do("GET", "/", "", nil); r.header.Get("Cache-Control") != "no-cache" {
		t.Errorf("index cache: %v", r.header)
	}
	if r := c.do("GET", "/missing.js", "", nil); r.code != 404 {
		t.Errorf("missing: %d", r.code)
	}
	if r := c.do("POST", "/", "", nil); r.code != 405 {
		t.Errorf("POST /: %d", r.code)
	}
	e2 := newEnv(t, true, func(c *Config) { c.Assets = fstest.MapFS{} })
	if r := e2.client().do("GET", "/", "", nil); !strings.Contains(string(r.body), "make web") {
		t.Errorf("not built: %s", r.body)
	}
}

func TestListenRules(t *testing.T) {
	for listen, wantErr := range map[string]bool{
		"127.0.0.1:7781": false, "localhost:7781": false, "[::1]:7781": false, "127.0.0.5:80": false,
		"0.0.0.0:7781": true, "192.168.1.5:7781": true, ":7781": true, "ramplus.lan:7781": true, "nonsense": true,
	} {
		if err := ValidateListen(listen, false); (err != nil) != wantErr {
			t.Errorf("%s without TLS: %v", listen, err)
		}
		if listen != "nonsense" {
			if err := ValidateListen(listen, true); err != nil {
				t.Errorf("%s with TLS: %v", listen, err)
			}
		}
	}
	if CheckNotRoot(0) == nil || CheckNotRoot(1000) != nil {
		t.Error("root check")
	}
	if ListenHost("0.0.0.0:7781") != "" || ListenHost("192.168.1.5:7781") != "192.168.1.5" || ListenHost(":7781") != "" {
		t.Error("ListenHost")
	}
}

// TLS: самоподписанный сертификат, cookie Secure с префиксом __Host-.
func TestTLS(t *testing.T) {
	cert, fp, err := SelfSigned([]string{"192.168.1.5", "ramplus.lan"}, time.Now())
	if err != nil || len(fp) != 95 {
		t.Fatalf("%v %q", err, fp)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if err := leaf.VerifyHostname("ramplus.lan"); err != nil {
		t.Error(err)
	}
	if err := leaf.VerifyHostname("192.168.1.5"); err != nil {
		t.Error(err)
	}
	e := newEnv(t, true, func(c *Config) { c.TLS = true })
	ts := httptest.NewUnstartedServer(e.srv.Handler())
	ts.TLS = TLSConfig(cert)
	ts.StartTLS()
	defer ts.Close()
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}}
	r, err := cl.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"password":"`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	sc := r.Header.Get("Set-Cookie")
	if r.StatusCode != 200 || !strings.HasPrefix(sc, "__Host-ramplus_session=") || !strings.Contains(sc, "Secure") {
		t.Errorf("%d %q", r.StatusCode, sc)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Неудачные входы и пауза — в журнале по полю event=, без паролей.
func TestLoginEventsLogged(t *testing.T) {
	var buf lockedBuf
	e := newEnv(t, true, func(c *Config) { c.Log = slog.New(slog.NewTextHandler(&buf, nil)) })
	c := e.client()
	for i := 0; i < 6; i++ {
		c.login("secret-wrong-pw")
	}
	out := buf.String()
	count := func(event string) int {
		n := 0
		for _, l := range strings.Split(out, "\n") {
			for _, kv := range strings.Fields(l) {
				if kv == "event="+event {
					n++
				}
			}
		}
		return n
	}
	if count(EventLoginFailed) != 5 || count(EventLoginRateLimited) != 1 {
		t.Errorf("failed=%d rate_limited=%d\n%s", count(EventLoginFailed), count(EventLoginRateLimited), out)
	}
	if strings.Contains(out, "secret-wrong-pw") || strings.Contains(out, password) {
		t.Errorf("password in log:\n%s", out)
	}
}
