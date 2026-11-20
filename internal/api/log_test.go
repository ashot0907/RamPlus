package api

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/ashot0907/ramplus/internal/fakesys"
)

// lockedBuf — журнал, в который пишут горутины сервера.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Split(strings.TrimSpace(l.b.String()), "\n")
}

// fields — пары key=value строки текстового журнала slog (значения без
// пробелов — без кавычек).
func fields(line string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Fields(line) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// При 403 демон пишет одну запись с машинным полем event=auth.forbidden и
// полями uid, gid, gids, pid, method, path, need — формат, по которому
// ищет vm-test.sh (journalctl -o cat).
func TestForbiddenIsLogged(t *testing.T) {
	var buf lockedBuf
	log := slog.New(slog.NewTextHandler(&buf, nil))
	h := newHarnessLog(t, fakesys.New(), log)
	if r := h.apply("reader", "nope", false); r.code != 403 || r.errCode(t) != "auth.forbidden" {
		t.Fatalf("apply as reader: %d %s", r.code, r.body)
	}
	if r := h.do("stranger", "GET", "/v1/status", nil); r.code != 403 {
		t.Fatalf("status as stranger: %d", r.code)
	}
	var got []map[string]string
	for _, l := range buf.lines() {
		if f := fields(l); f["event"] == EventAuthForbidden {
			got = append(got, f)
		}
	}
	if len(got) != 2 {
		t.Fatalf("auth.forbidden records: %d\n%s", len(got), strings.Join(buf.lines(), "\n"))
	}
	want := []map[string]string{
		{"level": "WARN", "uid": "1001", "gid": "1001", "gids": "1001,990", "pid": "2", "method": "POST", "path": "/v1/apply", "need": "admin"},
		{"level": "WARN", "uid": "65534", "gid": "65534", "gids": "none", "pid": "4", "method": "GET", "path": "/v1/status", "need": "read"},
	}
	for i, w := range want {
		for k, v := range w {
			if got[i][k] != v {
				t.Errorf("record %d: %s=%q, want %q (%v)", i, k, got[i][k], v, got[i])
			}
		}
	}
	// Разрешённые запросы — без записи об отказе, но с event=request.
	before := len(buf.lines())
	h.do("reader", "GET", "/v1/status", nil)
	for _, l := range buf.lines()[before:] {
		if fields(l)["event"] == EventAuthForbidden {
			t.Errorf("allowed request logged as forbidden: %s", l)
		}
	}
	if last := fields(buf.lines()[len(buf.lines())-1]); last["event"] != EventRequest || last["status"] != "200" {
		t.Errorf("request record: %v", last)
	}
}
