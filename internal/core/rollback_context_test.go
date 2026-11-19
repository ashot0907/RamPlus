package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/plan"
)

func hasCode(p *plan.Plan, code string) (plan.Check, bool) {
	for _, c := range p.Checks {
		if c.Code == code {
			return c, true
		}
	}
	return plan.Check{}, false
}

func setStarted(t *testing.T, w *fakeWorld, i int, at time.Time) {
	t.Helper()
	st := stateOf(t, w)
	st.Operations[i].StartedAt = at
	b, _ := st.Marshal()
	w.Files[StatePath].Data = string(b)
}

func TestRollbackOld(t *testing.T) {
	w := newWorld(t)
	mustRun(t, w, apply4G)
	c := New(w.System())

	p := c.RollbackPlan(context.Background())
	if _, ok := hasCode(p, "rollback.old"); ok {
		t.Error("fresh operation is not old")
	}
	if p.Rollback == nil || p.Rollback.Domain != plan.DomainSwap || p.Rollback.AgeSeconds > 3600 {
		t.Errorf("rollback view: %+v", p.Rollback)
	}

	setStarted(t, w, 0, w.Now().Add(-75*time.Hour))
	p = c.RollbackPlan(context.Background())
	ch, ok := hasCode(p, "rollback.old")
	if !ok || ch.Level != plan.LevelWarn || !strings.Contains(ch.Message, "3 дня назад") || !strings.Contains(ch.Message, "«создание swap»") {
		t.Errorf("rollback.old: %+v", ch)
	}
	if p.Blocked() {
		t.Errorf("warning must not block: %+v", p.Failures())
	}
	if p.Rollback.AgeSeconds < 75*3600 || p.Rollback.OperationID != stateOf(t, w).Operations[0].ID {
		t.Errorf("rollback view: %+v", p.Rollback)
	}
}

func TestRollbackOtherDomain(t *testing.T) {
	w := profWorld(t, false)
	mustProfile(t, w, ProfileRequest{Name: "zswap-lz4", Experimental: true})
	mustRun(t, w, apply4G)
	c := New(w.System())

	// Отменяется последняя операция (swap) — всё очевидно.
	p := c.RollbackPlan(context.Background())
	if _, ok := hasCode(p, "rollback.other_domain"); ok {
		t.Error("undoing the last operation needs no warning")
	}
	mustRun(t, w, rollback)

	// Swap уже отменён, следующий rollback дойдёт до профиля.
	p = c.RollbackPlan(context.Background())
	ch, ok := hasCode(p, "rollback.other_domain")
	if !ok || ch.Level != plan.LevelWarn {
		t.Fatalf("rollback.other_domain missing: %+v", p.Checks)
	}
	for _, want := range []string{"«применение профиля»", "а не изменение swap", "ramplus swap apply", "уже отменена"} {
		if !strings.Contains(ch.Message, want) {
			t.Errorf("message %q lacks %q", ch.Message, want)
		}
	}
	if p.Rollback == nil || p.Rollback.Domain != plan.DomainProfile {
		t.Errorf("rollback view: %+v", p.Rollback)
	}
}
