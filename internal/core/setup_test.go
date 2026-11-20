package core

import (
	"context"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/swap"
)

func setupWorld(t *testing.T) *fakeWorld { return newWorld(t).WithKernelParams(false).WithOldSwap(0) }

func setupReq(size uint64, existing swap.ExistingMode, prof string) SetupRequest {
	var r SetupRequest
	if size > 0 {
		r.Swap = &swap.Options{Size: size, Existing: existing}
	}
	if prof != "" {
		r.Profile = &profile.ProfileRequest{Name: prof, Experimental: true}
	}
	return r
}

func execSetup(t *testing.T, w *fakeWorld, r SetupRequest) Result {
	t.Helper()
	c := New(w.System())
	return c.Execute(context.Background(), Request{Setup: &r}, journal.Hooks{Confirm: yes})
}

func hasCheck(p *plan.Plan, code string, level Level) bool {
	for _, c := range p.Checks {
		if c.Code == code && c.Level == level {
			return true
		}
	}
	return false
}

// Композиция: проверки обеих частей, шаги подряд (сначала swap, потом
// профиль), одна операция в журнале.
func TestSetupComposition(t *testing.T) {
	w := setupWorld(t)
	c := New(w.System())
	r := setupReq(4*GiB, swap.ExistingReplace, "zswap-lz4")
	p := c.SetupPlan(context.Background(), r)
	if p.Blocked() || p.Action != ActionSetup || p.ID == "" || p.Fingerprint == "" {
		t.Fatalf("plan: blocked=%v action=%s id=%q fp=%q %+v", p.Blocked(), p.Action, p.ID, p.Fingerprint, p.Failures())
	}
	sp := swap.BuildPlan(swap.Gather(context.Background(), w.System(), ""), swap.Options{Action: swap.ActionApply, Size: 4 * GiB, Existing: swap.ExistingReplace})
	pp := profile.PlanProfile(context.Background(), w.System(), *r.Profile)
	want := kinds(sp.Steps) + "," + kinds(pp.Steps)
	if got := kinds(p.Steps); got != want {
		t.Errorf("steps:\n got %s\nwant %s", got, want)
	}
	for _, code := range []string{"existing.selected", "profile.experimental", "profile.baseline_capture", "disk_space.ok"} {
		found := false
		for _, ch := range p.Checks {
			found = found || ch.Code == code
		}
		if !found {
			t.Errorf("check %s missing", code)
		}
	}
	roots := 0
	for _, ch := range p.Checks {
		if ch.Code == "root" {
			roots++
		}
	}
	if roots != 1 {
		t.Errorf("common checks must not repeat: root ×%d", roots)
	}
	if p.Command != "ramplus setup --size 4G --existing=replace --profile zswap-lz4 --experimental" {
		t.Errorf("command %q", p.Command)
	}
	if p.SwapBefore != 8*GiB || p.SwapAfter != 4*GiB || p.Profile == nil || !p.Profile.Experimental {
		t.Errorf("summary %d→%d profile %+v", p.SwapBefore, p.SwapAfter, p.Profile)
	}

	res := c.Apply(context.Background(), p.ID, journal.Hooks{Confirm: yes})
	if res.Status != ResultDone {
		t.Fatalf("apply: %s %s", res.Status, res.Error)
	}
	st := stateOf(t, w)
	if len(st.Operations) != 1 || st.Operations[0].Action != ActionSetup || st.Operations[0].Status != OpCommitted {
		t.Fatalf("journal: %+v", st.Operations)
	}
	if m := swap.ManagedOf(st); m == nil || m.SizeBytes != 4*GiB || m.CreatedAt.IsZero() {
		t.Errorf("managed: %+v", m)
	}
	if ap := profile.ProfileOf(st); ap == nil || ap.Name != "zswap-lz4" || ap.AppliedAt.IsZero() {
		t.Errorf("profile: %+v", ap)
	}
	if b := profile.BaselineOf(st); b == nil || b.OpID != st.Operations[0].ID {
		t.Errorf("baseline: %+v", b)
	}
	if _, ok := w.Active["/swap.img"]; ok {
		t.Error("old swap must be replaced")
	}
	h, err := c.History()
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Operations[0].Title; got != "Запас 4 GB из SSD + режим «Сжатие — быстрое»" {
		t.Errorf("title %q", got)
	}
	ch := h.Operations[0].Change
	if ch == nil || ch.SwapBefore != nil || ch.SwapAfter == nil || *ch.SwapAfter != 4*GiB || ch.ProfileAfter != "zswap-lz4" {
		t.Errorf("change %+v", ch)
	}
}

func TestSetupParts(t *testing.T) {
	ctx := context.Background()
	t.Run("only swap", func(t *testing.T) {
		w := setupWorld(t)
		p := New(w.System()).SetupPlan(ctx, setupReq(4*GiB, swap.ExistingKeep, ""))
		if p.Blocked() || p.Profile != nil || p.HasKind(profile.KindSetValue) || !p.HasKind(swap.KindSwapon) {
			t.Fatalf("%s %+v", kinds(p.Steps), p.Failures())
		}
		if res := execSetup(t, w, setupReq(4*GiB, swap.ExistingKeep, "")); res.Status != ResultDone {
			t.Fatal(res.Status, res.Error)
		}
		h, _ := New(w.System()).History()
		if got := h.Operations[0].Title; got != "Запас 4 GB из SSD" {
			t.Errorf("title %q", got)
		}
	})
	t.Run("only profile", func(t *testing.T) {
		w := setupWorld(t)
		p := New(w.System()).SetupPlan(ctx, setupReq(0, "", "zswap-lz4"))
		if p.Blocked() || p.HasKind(swap.KindSwapon) || !p.HasKind(profile.KindSetValue) || p.SwapBefore != 8*GiB || p.SwapAfter != 8*GiB {
			t.Fatalf("%s %+v", kinds(p.Steps), p.Failures())
		}
	})
	t.Run("both unchanged", func(t *testing.T) {
		w := setupWorld(t)
		if res := execSetup(t, w, setupReq(4*GiB, swap.ExistingKeep, "zswap-lz4")); res.Status != ResultDone {
			t.Fatal(res.Status, res.Error)
		}
		p := New(w.System()).SetupPlan(ctx, setupReq(4*GiB, swap.ExistingKeep, "zswap-lz4"))
		if p.Blocked() || len(p.Steps) != 0 || !hasCheck(p, "setup.nothing", LevelOK) || !hasCheck(p, "setup.swap_unchanged", LevelOK) {
			t.Fatalf("want empty plan: %s %+v", kinds(p.Steps), p.Checks)
		}
		if p.Profile.Experimental {
			t.Error("unchanged experimental profile must not require confirmation")
		}
		if res := execSetup(t, w, setupReq(4*GiB, swap.ExistingKeep, "zswap-lz4")); res.Status != ResultNothing {
			t.Errorf("status %s", res.Status)
		}
	})
	t.Run("resize", func(t *testing.T) {
		w := setupWorld(t)
		if res := execSetup(t, w, setupReq(4*GiB, swap.ExistingKeep, "")); res.Status != ResultDone {
			t.Fatal(res.Status, res.Error)
		}
		p := New(w.System()).SetupPlan(ctx, setupReq(6*GiB, "", ""))
		if p.Blocked() || !hasCheck(p, "size.change", LevelInfo) {
			t.Fatalf("%+v", p.Checks)
		}
		p = New(w.System()).SetupPlan(ctx, SetupRequest{Swap: &swap.Options{Size: 6 * GiB, Path: "/mnt/x/ramplus.swapfile"}})
		if !hasCheck(p, "setup.swap_path_change", LevelFail) {
			t.Errorf("path change must block: %+v", p.Checks)
		}
	})
	t.Run("empty", func(t *testing.T) {
		p := New(setupWorld(t).System()).SetupPlan(ctx, SetupRequest{})
		if !hasCheck(p, "setup.empty", LevelFail) {
			t.Errorf("%+v", p.Checks)
		}
	})
}

// Отказ любой части блокирует весь план; ничего не меняется.
func TestSetupAnyPartBlocks(t *testing.T) {
	cases := map[string]func(*fakeWorld) SetupRequest{
		"swap on usb": func(w *fakeWorld) SetupRequest {
			w.Transport = "usb"
			return setupReq(4*GiB, swap.ExistingReplace, "zswap-lz4")
		},
		"no zswap in kernel": func(w *fakeWorld) SetupRequest {
			for p := range w.Files {
				if strings.HasPrefix(p, "/sys/module/zswap") {
					delete(w.Files, p)
				}
			}
			return setupReq(4*GiB, swap.ExistingReplace, "zswap-lz4")
		},
		"unknown profile": func(*fakeWorld) SetupRequest { return setupReq(4*GiB, "", "nope") },
		"swap too big":    func(*fakeWorld) SetupRequest { return setupReq(400*GiB, "", "default") },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			w := setupWorld(t)
			r := mk(w)
			before := w.Snap()
			p := New(w.System()).SetupPlan(context.Background(), r)
			if !p.Blocked() {
				t.Fatalf("must be blocked: %+v", p.Checks)
			}
			res := execSetup(t, w, r)
			if res.Status != ResultBlocked {
				t.Errorf("status %s", res.Status)
			}
			if d := before.Diff(w.Snap()); d != "" {
				t.Errorf("system changed:\n%s", d)
			}
		})
	}
}

// Сбой на каждом шаге → откат всего (и swap, и профиля).
func TestSetupFailureEveryStep(t *testing.T) {
	r := setupReq(4*GiB, swap.ExistingReplace, "zswap-lz4")
	ref := setupWorld(t)
	start := ref.Mutations
	if res := execSetup(t, ref, r); res.Status != ResultDone {
		t.Fatal(res.Status, res.Error)
	}
	after, total := ref.Snap(), ref.Mutations-start
	if total < 15 {
		t.Fatalf("too few mutations: %d", total)
	}
	for n := 1; n <= total; n++ {
		w := setupWorld(t)
		before := w.Snap()
		w.FailAt = w.Mutations + n
		res := execSetup(t, w, r)
		if res.OK() {
			t.Fatalf("n=%d: failure ignored", n)
		}
		got := w.Snap()
		if before.Diff(got) == "" {
			st := stateOf(t, w)
			if m := swap.ManagedOf(st); m != nil || profile.ProfileOf(st) != nil || profile.BaselineOf(st) != nil {
				t.Fatalf("n=%d: journal sections not restored: managed=%+v", n, m)
			}
			continue
		}
		if after.Diff(got) != "" || !strings.Contains(res.Error, "не удалось записать итог") {
			t.Fatalf("n=%d (%s): not restored: %s\n%s", n, w.Log[len(w.Log)-1], res.Error, before.Diff(got))
		}
	}
}

// rollback отменяет setup целиком, в обратном порядке.
func TestSetupRollback(t *testing.T) {
	w := setupWorld(t)
	before := w.Snap()
	if res := execSetup(t, w, setupReq(4*GiB, swap.ExistingReplace, "zswap-lz4")); res.Status != ResultDone {
		t.Fatal(res.Status, res.Error)
	}
	c := New(w.System())
	p := c.RollbackPlan(context.Background())
	if p.Blocked() || p.Rollback == nil || p.Rollback.Action != ActionSetup || p.Rollback.Domain != plan.DomainSetup {
		t.Fatalf("rollback plan: %+v %+v", p.Rollback, p.Failures())
	}
	// Отмена в обратном порядке: сначала профиль (unit, sysctl), потом swap.
	ks := kinds(p.Steps)
	if strings.Index(ks, "set_value") > strings.Index(ks, "swapoff") {
		t.Errorf("profile must be undone before swap: %s", ks)
	}
	res := c.Apply(context.Background(), p.ID, journal.Hooks{Confirm: yes})
	if res.Status != ResultDone {
		t.Fatalf("rollback: %s %s", res.Status, res.Error)
	}
	if d := before.Diff(w.Snap()); d != "" {
		t.Errorf("not restored:\n%s", d)
	}
	st := stateOf(t, w)
	if swap.ManagedOf(st) != nil || profile.ProfileOf(st) != nil || profile.BaselineOf(st) != nil {
		t.Errorf("sections: managed=%s profile=%s baseline=%s", st.Managed, st.Profile, st.Baseline)
	}
	if st.Operations[0].Status != OpRolledBack {
		t.Errorf("setup status %s", st.Operations[0].Status)
	}
	if got := cleanOf(w); len(got) != 0 {
		t.Errorf("not clean: %v", got)
	}
	h, _ := c.History()
	if got := h.Operations[1].Title; got != "Отмена: запас 4 GB из SSD + режим «Сжатие — быстрое»" {
		t.Errorf("rollback title %q", got)
	}
}

// Экспериментальный профиль в setup требует подтверждения (как profile apply).
func TestSetupConfirmExperimental(t *testing.T) {
	w := setupWorld(t)
	c := New(w.System())
	p := c.SetupPlan(context.Background(), setupReq(4*GiB, swap.ExistingKeep, "zswap-lz4"))
	if err := ConfirmExperimental(p, false); err == nil {
		t.Error("experimental profile in setup must need confirmation")
	}
	p = c.SetupPlan(context.Background(), setupReq(4*GiB, swap.ExistingKeep, "default"))
	if err := ConfirmExperimental(p, false); err != nil {
		t.Errorf("default profile: %v", err)
	}
}

func journalHooksYes() journal.Hooks { return journal.Hooks{Confirm: yes} }
