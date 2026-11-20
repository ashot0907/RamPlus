package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/fakesys"
)

// SSE-поток живёт дольше ReadTimeout сервера: иначе net/http отменяет
// контекст запроса по истечении срока чтения и поток статуса рвётся.
func TestSSEOutlivesReadTimeout(t *testing.T) {
	c := core.New(fakesys.New().System())
	c.Collect = fixtureStatus
	srv := New(c, "t", Policy{}, nil)
	srv.StatusInterval = 20 * time.Millisecond
	dir, _ := os.MkdirTemp("", "rpd")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	l, err := Listen(sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	hs := srv.HTTPServer(fixedPeer(peerRoot), base)
	hs.ReadTimeout = 150 * time.Millisecond
	go func() { _ = hs.Serve(l) }()
	defer hs.Close()

	cl := NewClient(sock).HTTP
	cl.Timeout = 0
	resp, err := cl.Get("http://ramplusd/v1/status/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	start := time.Now()
	evs := readSSE(t, resp.Body, 30) // 30 × 20 мс = 600 мс > 150 мс
	if len(evs) != 30 {
		t.Fatalf("stream ended after %d events (%v)", len(evs), time.Since(start))
	}
}
