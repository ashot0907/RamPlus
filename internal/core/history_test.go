package core

import (
	"context"
	"testing"

	"github.com/ashot0907/ramplus/internal/journal"
)

func TestHistory(t *testing.T) {
	w := newWorld(t)
	c := New(w.System())
	h, err := c.History()
	if err != nil || len(h.Operations) != 0 || h.Operations == nil {
		t.Fatalf("empty: %+v %v", h, err)
	}
	mustRun(t, w, apply4G)
	mustRun(t, w, rollback)
	mustRun(t, w, apply4G)
	before := w.Snap()
	h, err = c.History()
	if err != nil {
		t.Fatal(err)
	}
	if d := before.Diff(w.Snap()); d != "" {
		t.Errorf("history must be read-only:\n%s", d)
	}
	ops := h.Operations
	if len(ops) != 3 {
		t.Fatalf("ops: %+v", ops)
	}
	if ops[0].Status != journal.OpRolledBack || ops[0].Undoable || ops[0].RollbackNext {
		t.Errorf("rolled back apply: %+v", ops[0])
	}
	if ops[1].Action != ActionRollback || ops[1].Target != ops[0].ID || ops[1].Undoable {
		t.Errorf("rollback: %+v", ops[1])
	}
	if ops[2].Status != journal.OpCommitted || !ops[2].Undoable || !ops[2].RollbackNext || ops[2].Steps == 0 {
		t.Errorf("last apply: %+v", ops[2])
	}
	// RollbackNext — ровно то, что выберет rollback.
	p := c.RollbackPlan(context.Background())
	if p.Effects.Target == nil || p.Effects.Target.ID != ops[2].ID {
		t.Errorf("rollback target %+v, history next %s", p.Effects.Target, ops[2].ID)
	}

	w.Files[StatePath].Data = "{"
	if _, err := c.History(); err == nil {
		t.Error("broken journal must be an error")
	}
}
