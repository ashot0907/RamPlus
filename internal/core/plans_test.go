package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/journal"
)

// Одни и те же факты — один отпечаток; ID у каждого плана свой.
func TestFingerprintStable(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).WithOldSwap(0).WithKernelParams(false)
	c := New(w.System())
	for _, r := range []Request{{Swap: &apply4G}, {Rollback: true}, {Profile: &ProfileRequest{Name: "zswap-lz4"}}, {ProfileReset: true}} {
		a, b := c.Plan(ctx, r), c.Plan(ctx, r)
		if a.Fingerprint == "" || a.Fingerprint != b.Fingerprint {
			t.Errorf("%s: fingerprint %q vs %q", a.Command, a.Fingerprint, b.Fingerprint)
		}
		if a.ID == "" || a.ID == b.ID {
			t.Errorf("%s: plan IDs %q, %q", a.Command, a.ID, b.ID)
		}
	}
}

// Отпечаток меняется, когда меняются факты, на которых построен план.
func TestFingerprintChanges(t *testing.T) {
	ctx := context.Background()
	swapFP := func(w *fakeWorld) string { return New(w.System()).SwapPlan(ctx, apply4G).Fingerprint }
	profFP := func(w *fakeWorld) string {
		return New(w.System()).ProfilePlan(ctx, ProfileRequest{Name: "zswap-lz4"}).Fingerprint
	}
	base := func() *fakeWorld { return newWorld(t).WithOldSwap(0).WithKernelParams(false) }
	b := base()
	swap0, prof0 := swapFP(b), profFP(b)

	changes := []struct {
		name string
		mod  func(w *fakeWorld)
		fp   func(w *fakeWorld) string
		want bool // должен ли измениться
	}{
		{"swap used (volatile)", func(w *fakeWorld) { w.Used["/swap.img"] = 64 << 20 }, swapFP, false},
		{"free space within GiB", func(w *fakeWorld) { w.Avail += 1 << 20 }, swapFP, false},
		{"free space", func(w *fakeWorld) { w.Avail -= 3 * GiB }, swapFP, true},
		{"new swap", func(w *fakeWorld) {
			w.Files["/swap2"] = &fakeFile{Size: GiB, Mode: 0o600, Swap: true}
			w.Active["/swap2"] = -3
		}, swapFP, true},
		{"fstab", func(w *fakeWorld) { w.Files["/etc/fstab"].Data += "# x\n" }, swapFP, true},
		{"journal", func(w *fakeWorld) { loadState(t, w, "state/v1-rolled-back.json") }, swapFP, true},
		{"param value", func(w *fakeWorld) { w.Files["/proc/sys/vm/swappiness"].Data = "10\n" }, profFP, true},
		{"journal (profile)", func(w *fakeWorld) { loadState(t, w, "state/v1-rolled-back.json") }, profFP, true},
	}
	for _, tt := range changes {
		w := base()
		tt.mod(w)
		was := swap0
		if tt.name == "param value" || tt.name == "journal (profile)" {
			was = prof0
		}
		if changed := tt.fp(w) != was; changed != tt.want {
			t.Errorf("%s: changed=%v, want %v", tt.name, changed, tt.want)
		}
	}
}

func TestApplyByID(t *testing.T) {
	ctx := context.Background()
	h := journal.Hooks{Confirm: yes}

	w := newWorld(t)
	c := New(w.System())
	p := c.SwapPlan(ctx, apply4G)
	res := c.Apply(ctx, p.ID, h)
	if res.Status != ResultDone || res.Plan.ID != p.ID || res.Plan.Fingerprint != p.Fingerprint {
		t.Fatalf("apply: %+v", res)
	}
	// План выполняется один раз.
	if res := c.Apply(ctx, p.ID, h); res.Status != ResultPlanUsed || !errors.Is(errorOf(res), ErrPlanUsed) {
		t.Errorf("second apply: %+v", res)
	}
	if res := c.Apply(ctx, "nope", h); res.Status != ResultUnknownPlan {
		t.Errorf("unknown: %+v", res)
	}
}

// Система изменилась между plan и apply — отказ, ничего не изменено.
func TestApplyStale(t *testing.T) {
	ctx := context.Background()
	h := journal.Hooks{Confirm: yes}
	tests := []struct {
		name string
		r    Request
		mod  func(w *fakeWorld)
	}{
		{"swap added", Request{Swap: &apply4G}, func(w *fakeWorld) {
			w.Files["/swap2"] = &fakeFile{Size: GiB, Mode: 0o600, Swap: true}
			w.Active["/swap2"] = -3
		}},
		{"disk filled", Request{Swap: &apply4G}, func(w *fakeWorld) { w.Avail -= 10 * GiB }},
		{"param changed", Request{Profile: &ProfileRequest{Name: "zswap-lz4", Experimental: true}}, func(w *fakeWorld) {
			w.Files["/proc/sys/vm/swappiness"].Data = "10\n"
		}},
		{"other operation", Request{Profile: &ProfileRequest{Name: "zswap-lz4", Experimental: true}}, func(w *fakeWorld) {
			mustRun(t, w, apply4G)
		}},
	}
	for _, tt := range tests {
		w := newWorld(t).WithKernelParams(false)
		c := New(w.System())
		p := c.Plan(ctx, tt.r)
		if p.Blocked() {
			t.Fatalf("%s: plan blocked: %+v", tt.name, p.Failures())
		}
		tt.mod(w)
		before := w.Snap()
		res := c.Apply(ctx, p.ID, h)
		if res.Status != ResultStale || res.Error != ErrPlanStale.Error() {
			t.Errorf("%s: %+v", tt.name, res)
		}
		if d := before.Diff(w.Snap()); d != "" && !onlyJournal(d) {
			t.Errorf("%s: system changed:\n%s", tt.name, d)
		}
	}
}

// CLI: Run строит план и сразу выполняет его через Apply.
func TestRunUsesApply(t *testing.T) {
	w := newWorld(t)
	c := New(w.System())
	p, err := c.Run(context.Background(), Request{Swap: &apply4G}, journal.Hooks{Confirm: yes})
	if err != nil || p.ID == "" {
		t.Fatalf("run: %v %+v", err, p)
	}
	if _, err := c.plans.take(p.ID, w.Now()); !errors.Is(err, ErrPlanUsed) {
		t.Errorf("plan must be consumed by Run: %v", err)
	}
}

func TestPlanStoreLimits(t *testing.T) {
	var s planStore
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := s.put(Request{}, "a", now)
	for i := 0; i < maxStoredPlans; i++ {
		s.put(Request{}, "b", now)
	}
	if _, err := s.take(first, now); !errors.Is(err, ErrPlanNotFound) {
		t.Errorf("oldest plan must be evicted: %v", err)
	}
	id := s.put(Request{}, "c", now)
	if _, err := s.take(id, now.Add(planTTL+time.Second)); !errors.Is(err, ErrPlanNotFound) {
		t.Errorf("expired plan must be dropped: %v", err)
	}
	if len(s.plans) != len(s.order) {
		t.Errorf("store inconsistent: %d vs %d", len(s.plans), len(s.order))
	}
}

func errorOf(r Result) error {
	switch r.Status {
	case ResultUnknownPlan:
		return ErrPlanNotFound
	case ResultPlanUsed:
		return ErrPlanUsed
	case ResultStale:
		return ErrPlanStale
	}
	return nil
}
