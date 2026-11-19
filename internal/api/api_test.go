package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/fakesys"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/plan"
)

const (
	gidRamplus = 990
	gidAdmin   = 991
)

// Пользователи для проверки прав (без реальных uid: PeerSource в тестах
// отдаёт заданного пользователя).
var (
	peerRoot     = Peer{UID: 0, GID: 0, PID: 1}
	peerReader   = Peer{UID: 1001, GID: 1001, PID: 2, Groups: []uint32{1001, gidRamplus}}
	peerAdmin    = Peer{UID: 1002, GID: 1002, PID: 3, Groups: []uint32{1002, gidRamplus, gidAdmin}}
	peerStranger = Peer{UID: 65534, GID: 65534, PID: 4, Groups: []uint32{}}
	identities   = map[string]Peer{"root": peerRoot, "reader": peerReader, "admin": peerAdmin, "stranger": peerStranger}
)

type fixedPeer Peer

func (f fixedPeer) PeerOf(net.Conn) (Peer, error) { return Peer(f), nil }

type harness struct {
	t       *testing.T
	w       *fakesys.World
	core    *core.Core
	srv     *Server
	clients map[string]*http.Client
}

func u32(v uint32) *uint32 { return &v }

func fixtureStatus() monitor.Status {
	td := func(p ...string) string {
		return filepath.Join(append([]string{"..", "monitor", "testdata"}, p...)...)
	}
	return monitor.CollectFrom(monitor.Paths{
		Meminfo: td("meminfo", "normal"), Swaps: td("swaps", "two-devices"), ZswapParams: td("zswap", "params-7.0"),
		ZswapDebug: td("absent"), VMSysctl: td("vm"), PSIMemory: td("psi", "memory"), SysBlock: td("sysblock"),
	})
}

// newHarness — демон над фейковой системой; для каждого пользователя свой
// unix-сокет во временном каталоге (общие Server и core).
func newHarness(t *testing.T, w *fakesys.World) *harness {
	t.Helper()
	c := core.New(w.System())
	c.Collect = fixtureStatus
	srv := New(c, "test-1.0", Policy{ReadGID: u32(gidRamplus), AdminGID: u32(gidAdmin)}, nil)
	srv.StatusInterval = 10 * time.Millisecond
	// Короткий путь: у unix-сокета ограничение ~108 байт.
	dir, err := os.MkdirTemp("", "rpd")
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, w: w, core: c, srv: srv, clients: map[string]*http.Client{}}
	var servers []*http.Server
	for name, p := range identities {
		sock := filepath.Join(dir, name+".sock")
		l, err := Listen(sock, nil)
		if err != nil {
			t.Fatal(err)
		}
		hs := srv.HTTPServer(fixedPeer(p), base)
		servers = append(servers, hs)
		go func() { _ = hs.Serve(l) }()
		h.clients[name] = NewClient(sock).HTTP
	}
	t.Cleanup(func() {
		cancel()
		for _, hs := range servers {
			_ = hs.Close()
		}
		c.Wait()
		os.RemoveAll(dir)
	})
	return h
}

type response struct {
	code int
	body []byte
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("bad JSON (%d): %v\n%s", r.code, err, r.body)
	}
}

func (r response) errCode(t *testing.T) string {
	t.Helper()
	var e Error
	r.decode(t, &e)
	return e.Code
}

func (h *harness) do(who, method, path string, body any) response {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, "http://ramplusd"+path, rd)
	resp, err := h.clients[who].Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, b}
}

func (h *harness) plan(who, path string, body any) *plan.Plan {
	h.t.Helper()
	r := h.do(who, "POST", path, body)
	if r.code != http.StatusOK {
		h.t.Fatalf("POST %s: %d %s", path, r.code, r.body)
	}
	var p struct {
		plan.Plan
		ID string `json:"id"`
	}
	r.decode(h.t, &p)
	p.Plan.ID = p.ID
	return &p.Plan
}

func (h *harness) apply(who, planID string, confirm bool) response {
	h.t.Helper()
	return h.do(who, "POST", "/v1/apply", ApplyRequest{PlanID: planID, ConfirmExperimental: confirm})
}

type sseEvent struct{ name, data string }

// readSSE читает события потока до result (или n событий, n > 0).
func readSSE(t *testing.T, body io.Reader, n int) []sseEvent {
	t.Helper()
	var out []sseEvent
	var ev sseEvent
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		case line == "" && ev.name != "":
			out = append(out, ev)
			if ev.name == "result" || (n > 0 && len(out) == n) {
				return out
			}
			ev = sseEvent{}
		}
	}
	return out
}

func (h *harness) events(who, opID string) []sseEvent {
	h.t.Helper()
	resp, err := h.clients[who].Get("http://ramplusd/v1/operations/" + opID + "/events")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		h.t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return readSSE(h.t, resp.Body, 0)
}

func (h *harness) waitOp(who, opID string) core.OperationView {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var v core.OperationView
		r := h.do(who, "GET", "/v1/operations/"+opID, nil)
		r.decode(h.t, &v)
		if !v.Running {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("operation did not finish")
	return core.OperationView{}
}

func TestHealth(t *testing.T) {
	h := newHarness(t, fakesys.New())
	var hl Health
	r := h.do("stranger", "GET", "/v1/health", nil)
	r.decode(t, &hl)
	if r.code != 200 || hl.Version != "test-1.0" || hl.API != "v1" || hl.PID != os.Getpid() || !hl.Root {
		t.Errorf("health: %d %+v", r.code, hl)
	}
}

// Каждый эндпоинт × каждый пользователь: 403 auth.forbidden или нет.
func TestAuthMatrix(t *testing.T) {
	h := newHarness(t, fakesys.New())
	type ep struct {
		method, path string
		body         any
		need         Access
	}
	eps := []ep{
		{"GET", "/v1/health", nil, AccessAny},
		{"GET", "/v1/status", nil, AccessRead},
		{"GET", "/v1/status/stream", nil, AccessRead},
		{"GET", "/v1/history", nil, AccessRead},
		{"POST", "/v1/plan/swap", SwapPlanRequest{Action: "apply", Size: 4 << 30}, AccessRead},
		{"POST", "/v1/plan/profile", ProfilePlanRequest{Name: "default"}, AccessRead},
		{"POST", "/v1/plan/profile-reset", nil, AccessRead},
		{"POST", "/v1/plan/rollback", nil, AccessRead},
		{"POST", "/v1/apply", ApplyRequest{PlanID: "nope"}, AccessAdmin},
		{"GET", "/v1/operations/nope", nil, AccessRead},
		{"GET", "/v1/operations/nope/events", nil, AccessRead},
	}
	allowed := map[Access]map[string]bool{
		AccessAny:   {"root": true, "reader": true, "admin": true, "stranger": true},
		AccessRead:  {"root": true, "reader": true, "admin": true},
		AccessAdmin: {"root": true, "admin": true},
	}
	for _, e := range eps {
		for who := range identities {
			code := h.code(who, e.method, e.path, e.body)
			if forbidden := code == http.StatusForbidden; forbidden == allowed[e.need][who] {
				t.Errorf("%s %s as %s: HTTP %d (need %s)", e.method, e.path, who, code, e.need)
			}
		}
	}
	if c := h.do("stranger", "GET", "/v1/status", nil).errCode(t); c != "auth.forbidden" {
		t.Errorf("code = %q", c)
	}
}

// code — HTTP-код ответа; поток SSE закрывается сразу после заголовков.
func (h *harness) code(who, method, path string, body any) int {
	h.t.Helper()
	if !strings.HasSuffix(path, "/stream") {
		return h.do(who, method, path, body).code
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, "http://ramplusd"+path, nil)
	resp, err := h.clients[who].Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// plan → apply → события → history → plan rollback → apply.
func TestFullCycle(t *testing.T) {
	w := fakesys.New()
	clean := w.Snap()
	h := newHarness(t, w)

	p := h.plan("reader", "/v1/plan/swap", map[string]any{"action": "apply", "size": "4G"})
	if p.ID == "" || p.Blocked() || len(p.Steps) == 0 {
		t.Fatalf("plan: %+v", p)
	}
	r := h.apply("admin", p.ID, false)
	if r.code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", r.code, r.body)
	}
	var acc ApplyAccepted
	r.decode(t, &acc)
	if acc.OperationID == "" || acc.Events != "/v1/operations/"+acc.OperationID+"/events" {
		t.Fatalf("accepted: %+v", acc)
	}

	evs := h.events("reader", acc.OperationID)
	if len(evs) != len(p.Steps)+1 || evs[0].name != "step" || evs[len(evs)-1].name != "result" {
		t.Fatalf("events: %+v", evs)
	}
	var res core.Result
	if err := json.Unmarshal([]byte(evs[len(evs)-1].data), &res); err != nil || res.Status != core.ResultDone {
		t.Fatalf("result: %v %+v", err, res)
	}
	if v := h.waitOp("reader", acc.OperationID); v.Status != core.ResultDone || v.Command != p.Command || len(v.Events) != len(p.Steps) {
		t.Errorf("operation: %+v", v)
	}
	// Повторное подключение к завершённой операции — все шаги и итог.
	if again := h.events("reader", acc.OperationID); len(again) != len(evs) {
		t.Errorf("replay: %d events, want %d", len(again), len(evs))
	}

	var hist journal.History
	h.do("reader", "GET", "/v1/history", nil).decode(t, &hist)
	if len(hist.Operations) != 1 || hist.Operations[0].ID != acc.OperationID || !hist.Operations[0].RollbackNext {
		t.Fatalf("history: %+v", hist)
	}

	rb := h.plan("reader", "/v1/plan/rollback", nil)
	if rb.Rollback == nil || rb.Rollback.OperationID != acc.OperationID || rb.Rollback.Domain != plan.DomainSwap {
		t.Fatalf("rollback plan: %+v", rb.Rollback)
	}
	r = h.apply("root", rb.ID, false)
	if r.code != http.StatusAccepted {
		t.Fatalf("apply rollback: %d %s", r.code, r.body)
	}
	r.decode(t, &acc)
	if v := h.waitOp("root", acc.OperationID); v.Status != core.ResultDone {
		t.Fatalf("rollback op: %+v", v)
	}
	if d := clean.Diff(w.Snap()); d != "" && !onlyJournal(d) {
		t.Errorf("not clean after rollback:\n%s", d)
	}
	h.do("reader", "GET", "/v1/history", nil).decode(t, &hist)
	if len(hist.Operations) != 2 || hist.Operations[0].Status != journal.OpRolledBack {
		t.Errorf("history after rollback: %+v", hist.Operations)
	}
}

func onlyJournal(diff string) bool {
	for _, l := range strings.Split(strings.TrimSpace(diff), "\n") {
		if !strings.Contains(l, "/var/lib/ramplus") {
			return false
		}
	}
	return true
}

func TestApplyRefusals(t *testing.T) {
	w := fakesys.New().WithKernelParams(false)
	h := newHarness(t, w)

	// Неизвестный план.
	if r := h.apply("admin", "nope", false); r.code != 404 || r.errCode(t) != "plan.not_found" {
		t.Errorf("unknown: %d %s", r.code, r.body)
	}
	// Экспериментальный профиль без подтверждения.
	p := h.plan("admin", "/v1/plan/profile", ProfilePlanRequest{Name: "zswap-lz4"})
	if r := h.apply("admin", p.ID, false); r.code != 422 || r.errCode(t) != "profile.experimental_not_confirmed" {
		t.Errorf("experimental: %d %s", r.code, r.body)
	}
	// Заблокированный план — с проверками.
	p = h.plan("admin", "/v1/plan/swap", SwapPlanRequest{Action: "resize", Size: 4 << 30})
	r := h.apply("admin", p.ID, false)
	var e Error
	r.decode(t, &e)
	if r.code != 422 || e.Code != "plan.blocked" || e.Plan == nil || len(e.Plan.Checks) == 0 {
		t.Errorf("blocked: %d %s", r.code, r.body)
	}
	// Повторный apply того же плана.
	if r := h.apply("admin", p.ID, false); r.code != 409 || r.errCode(t) != "plan.used" {
		t.Errorf("used: %d %s", r.code, r.body)
	}
	// Система изменилась после plan.
	p = h.plan("admin", "/v1/plan/swap", SwapPlanRequest{Action: "apply", Size: 4 << 30})
	w.Files["/swap2"] = &fakesys.File{Size: 1 << 30, Mode: 0o600, Swap: true}
	w.Active["/swap2"] = -3
	if r := h.apply("admin", p.ID, false); r.code != 409 || r.errCode(t) != "plan.stale" {
		t.Errorf("stale: %d %s", r.code, r.body)
	}
	if st, _ := h.core.History(); len(st.Operations) != 0 {
		t.Errorf("journal must be untouched: %+v", st.Operations)
	}
	// Ничего делать не нужно — 200 nothing.
	p = h.plan("admin", "/v1/plan/profile", ProfilePlanRequest{Name: "zswap-lz4"})
	var acc ApplyAccepted
	h.apply("admin", p.ID, true).decode(t, &acc)
	h.waitOp("admin", acc.OperationID)
	p = h.plan("admin", "/v1/plan/profile", ProfilePlanRequest{Name: "zswap-lz4"})
	if r := h.apply("admin", p.ID, true); r.code != 200 || !strings.Contains(string(r.body), `"status": "nothing"`) {
		t.Errorf("nothing: %d %s", r.code, r.body)
	}
}

// Две операции одновременно → 409 op.busy; CLI во время операции демона →
// отказ по блокировке; журнал целый.
func TestBusyAndCLI(t *testing.T) {
	w := fakesys.New().WithKernelParams(false)
	h := newHarness(t, w)
	reached, release := make(chan struct{}), make(chan struct{})
	paused := false
	w.OnMutation = func(op string) {
		if !paused && strings.Contains(op, "swapon") {
			paused = true
			close(reached)
			<-release
		}
	}
	p1 := h.plan("admin", "/v1/plan/swap", SwapPlanRequest{Action: "apply", Size: 4 << 30})
	p2 := h.plan("admin", "/v1/plan/profile", ProfilePlanRequest{Name: "zswap-lz4"})
	r := h.apply("admin", p1.ID, false)
	if r.code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", r.code, r.body)
	}
	var acc ApplyAccepted
	r.decode(t, &acc)
	<-reached

	if r := h.apply("admin", p2.ID, true); r.code != 409 || r.errCode(t) != "op.busy" {
		t.Errorf("second op: %d %s", r.code, r.body)
	}
	var v core.OperationView
	h.do("reader", "GET", "/v1/operations/"+acc.OperationID, nil).decode(t, &v)
	if !v.Running || v.Status != core.StatusRunning {
		t.Errorf("running op: %+v", v)
	}
	// CLI работает напрямую, своим core: упирается в блокировку журнала.
	cli := core.New(w.System())
	res := cli.Execute(context.Background(), core.Request{Swap: &p1Opts}, journal.Hooks{Confirm: func(*plan.Plan) (bool, error) { return true, nil }})
	if res.Status != core.ResultLocked || !strings.Contains(res.Error, "другой экземпляр ramplus") {
		t.Errorf("CLI: %+v", res)
	}

	close(release)
	if v := h.waitOp("admin", acc.OperationID); v.Status != core.ResultDone {
		t.Fatalf("op: %+v", v)
	}
	hist, err := h.core.History()
	if err != nil || len(hist.Operations) != 1 || hist.Operations[0].Status != journal.OpCommitted {
		t.Errorf("journal: %v %+v", err, hist.Operations)
	}
}

var p1Opts = swapOpts()

func TestRequestErrors(t *testing.T) {
	h := newHarness(t, fakesys.New())
	big := `{"name":"` + strings.Repeat("x", MaxRequestBytes) + `"}`
	tests := []struct {
		method, path string
		body         any
		code         int
		errCode      string
	}{
		{"POST", "/v1/plan/profile", big, 413, "request.too_large"},
		{"POST", "/v1/plan/swap", "{", 400, "request.invalid"},
		{"POST", "/v1/plan/swap", `{"action":"apply","size":"4G","bogus":1}`, 400, "request.invalid"},
		{"POST", "/v1/plan/swap", `{"action":"grow"}`, 400, "request.invalid"},
		{"POST", "/v1/plan/swap", `{"action":"apply","size":"4X"}`, 400, "request.invalid"},
		{"POST", "/v1/plan/swap", `{"action":"apply","existing":"maybe"}`, 400, "request.invalid"},
		{"POST", "/v1/plan/profile", `{"name":"default","set":{"vm.nope":"1"}}`, 400, "request.invalid"},
		{"POST", "/v1/apply", `{}`, 400, "request.invalid"},
		{"GET", "/v1/nope", nil, 404, "request.not_found"},
		{"DELETE", "/v1/status", nil, 405, "request.method_not_allowed"},
		{"GET", "/v1/plan/swap", nil, 405, "request.method_not_allowed"},
	}
	for _, tt := range tests {
		r := h.do("root", tt.method, tt.path, tt.body)
		if r.code != tt.code || r.errCode(t) != tt.errCode {
			t.Errorf("%s %s: %d %s, want %d %s", tt.method, tt.path, r.code, r.body, tt.code, tt.errCode)
		}
	}
	// Размер числом байт.
	p := h.plan("root", "/v1/plan/swap", `{"action":"apply","size":4294967296}`)
	if p.Command != "ramplus swap apply --size 4G" {
		t.Errorf("command = %q", p.Command)
	}
}

func TestStatusStream(t *testing.T) {
	h := newHarness(t, fakesys.New())
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://ramplusd/v1/status/stream", nil)
	resp, err := h.clients["reader"].Do(req)
	if err != nil {
		t.Fatal(err)
	}
	evs := readSSE(t, resp.Body, 3)
	if len(evs) != 3 {
		t.Fatalf("events: %+v", evs)
	}
	var f StatusFrame
	if err := json.Unmarshal([]byte(evs[2].data), &f); err != nil || f.Peaks.Samples != 3 || f.Status.Memory.TotalBytes == nil {
		t.Errorf("frame: %v %+v", err, f.Peaks)
	}
	cancel()
	resp.Body.Close()
	waitStreams(t, h.srv)
}

// Клиент отключился посреди потока событий операции — горутина и таймеры
// освобождены, операция продолжается.
func TestSSEDisconnect(t *testing.T) {
	w := fakesys.New()
	h := newHarness(t, w)
	base := runtime.NumGoroutine()
	reached, release := make(chan struct{}), make(chan struct{})
	paused := false
	w.OnMutation = func(op string) {
		if !paused && strings.Contains(op, "swapon") {
			paused = true
			close(reached)
			<-release
		}
	}
	p := h.plan("admin", "/v1/plan/swap", SwapPlanRequest{Action: "apply", Size: 4 << 30})
	var acc ApplyAccepted
	h.apply("admin", p.ID, false).decode(t, &acc)
	<-reached

	for range 5 {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://ramplusd"+acc.Events, nil)
		resp, err := h.clients["reader"].Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if evs := readSSE(t, resp.Body, 1); len(evs) != 1 || evs[0].name != "step" {
			t.Fatalf("events: %+v", evs)
		}
		cancel()
		resp.Body.Close()
	}
	waitStreams(t, h.srv)
	close(release)
	if v := h.waitOp("admin", acc.OperationID); v.Status != core.ResultDone {
		t.Fatalf("op must finish after clients left: %+v", v)
	}
	for _, c := range h.clients {
		c.CloseIdleConnections()
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		buf := make([]byte, 1<<16)
		t.Errorf("goroutines leaked: %d > %d\n%s", n, base, buf[:runtime.Stack(buf, true)])
	}
}

func waitStreams(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for s.Streams() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := s.Streams(); n != 0 {
		t.Fatalf("%d SSE streams still open after client disconnect", n)
	}
}
