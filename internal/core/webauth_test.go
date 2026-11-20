package core

import (
	"errors"
	"testing"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/webauth"
)

var cheapArgon = &webauth.Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestWebPassword(t *testing.T) {
	w := newWorld(t)
	c := New(w.System())
	req := func(mode WebPasswordMode, pw string) WebPasswordRequest {
		return WebPasswordRequest{Mode: mode, Password: pw, GID: 977, Params: cheapArgon}
	}
	// Смена до init — отказ.
	if _, err := c.WebPassword(req(WebPasswordChange, "password1")); !errors.Is(err, ErrWebPasswordNotSet) {
		t.Errorf("change before init: %v", err)
	}
	// dry-run ничего не пишет.
	before := w.Snap()
	r := req(WebPasswordInit, "password1")
	r.DryRun = true
	if res, err := c.WebPassword(r); err != nil || !res.DryRun || res.Mode != "0640" {
		t.Fatalf("dry-run: %+v %v", res, err)
	}
	if d := before.Diff(w.Snap()); d != "" {
		t.Errorf("dry-run changed the system:\n%s", d)
	}
	// Слабый пароль.
	if _, err := c.WebPassword(req(WebPasswordInit, "short")); err == nil {
		t.Error("short password accepted")
	}
	// Не root.
	w.EUID = 1000
	if _, err := c.WebPassword(req(WebPasswordInit, "password1")); !errors.Is(err, journal.ErrNeedRoot) {
		t.Errorf("non-root: %v", err)
	}
	w.EUID = 0
	if _, err := c.WebPassword(req(WebPasswordInit, "password1")); err != nil {
		t.Fatal(err)
	}
	f := w.Files[webauth.DefaultPath]
	if f == nil || f.Mode != 0o640 || f.UID != 0 || f.GID != 977 {
		t.Fatalf("file %+v", f)
	}
	hash, err := webauth.ParseFile([]byte(f.Data))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := webauth.Verify(hash, "password1"); !ok {
		t.Error("stored hash does not verify")
	}
	// Повторный init — отказ; passwd меняет хеш.
	if _, err := c.WebPassword(req(WebPasswordInit, "password2")); !errors.Is(err, ErrWebPasswordSet) {
		t.Errorf("second init: %v", err)
	}
	if _, err := c.WebPassword(req(WebPasswordChange, "password2")); err != nil {
		t.Fatal(err)
	}
	hash2, _ := webauth.ParseFile([]byte(w.Files[webauth.DefaultPath].Data))
	if ok, _ := webauth.Verify(hash2, "password2"); !ok || hash2 == hash {
		t.Error("passwd did not change the hash")
	}
	// --disable — откат init.
	if _, err := c.WebPassword(WebPasswordRequest{Mode: WebPasswordDisable}); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Files[webauth.DefaultPath]; ok {
		t.Error("disable must remove the file")
	}
}
