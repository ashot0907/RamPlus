package journal

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/plan"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Журнал прошлой схемы (v1, пути /ramplus.swap, без status) читается.
func TestParseStateV1(t *testing.T) {
	s, err := ParseState([]byte(readFixture(t, "state/v1-rolled-back.json")))
	if err != nil {
		t.Fatal(err)
	}
	if s.Managed != nil || len(s.Operations) != 2 || s.Operations[0].Status != OpRolledBack {
		t.Fatalf("state = %+v", s)
	}
	for _, op := range s.Operations {
		for _, a := range op.Applied {
			if a.Status != StepDone {
				t.Errorf("v1 entry must become done: %+v", a)
			}
		}
	}
	if _, ok := s.LastUndoable(); ok {
		t.Error("everything is rolled back — nothing to undo")
	}
	b, _ := s.Marshal()
	if !strings.Contains(string(b), `"version": 3`) {
		t.Error("state must be written as v3")
	}
}

// Журнал v2 из VM после A7: ID совпадают, старая версия пометила
// rolled_back и resize, и remove.
func TestMigrateDuplicateIDs(t *testing.T) {
	s, err := ParseState([]byte(readFixture(t, "state/v2-duplicate-ids.json")))
	if err != nil {
		t.Fatal(err)
	}
	ops := s.Operations
	if hasDuplicateIDs(ops) {
		t.Fatalf("IDs not unique: %v", []string{ops[0].ID, ops[1].ID, ops[2].ID, ops[3].ID})
	}
	if ops[0].ID != "20261117T113307Z" || ops[1].ID != "20261117T113308Z#2" || ops[3].ID != "20261117T113308Z#4" {
		t.Errorf("ids = %s %s %s %s", ops[0].ID, ops[1].ID, ops[2].ID, ops[3].ID)
	}
	// Повтор стека: A7 отменил remove (последнюю неотменённую), resize цел.
	if ops[1].Status != OpCommitted || ops[2].Status != OpRolledBack || ops[3].Target != ops[2].ID {
		t.Errorf("statuses: resize=%s remove=%s rollback.target=%s", ops[1].Status, ops[2].Status, ops[3].Target)
	}
	if i, _ := s.LastUndoable(); i != 1 {
		t.Errorf("next rollback must undo resize, got op %d", i)
	}
	b, _ := s.Marshal()
	if _, err := ParseState(b); err != nil || !bytes.Contains(b, []byte(`"version": 3`)) {
		t.Errorf("re-parse v3: %v", err)
	}
	// В v3 дубликаты — ошибка, а не молчаливая миграция.
	bad := bytes.Replace(b, []byte(`"20261117T113308Z#2"`), []byte(`"20261117T113307Z"`), 1)
	if _, err := ParseState(bad); err == nil {
		t.Error("v3 with duplicate IDs must fail")
	}
}

func TestUndoable(t *testing.T) {
	op := func(action plan.Action, status string) Operation {
		return Operation{ID: string(action) + status, Action: action, Status: status}
	}
	tests := []struct {
		name string
		ops  []Operation
		want []bool
	}{
		{"empty", nil, []bool{}},
		{"stack", []Operation{op("apply", OpCommitted), op("resize", OpRolledBack), op(plan.ActionRollback, OpCommitted), op("remove", OpFailed)},
			[]bool{true, false, false, false}},
		{"interrupted", []Operation{op("apply", OpCommitted), op("profile-apply", OpInProgress)}, []bool{true, true}},
		{"barrier", []Operation{op("apply", OpCommitted), op("resize", OpRollbackFailed), op("profile-apply", OpCommitted)},
			[]bool{false, false, true}},
	}
	for _, tt := range tests {
		got := State{Operations: tt.ops}.Undoable()
		if fmt.Sprint(got) != fmt.Sprint(tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Журналы без source/requested_by (до аудита) читаются: источник неизвестен.
func TestOldJournalSourceUnknown(t *testing.T) {
	for _, f := range []string{"state/v1-committed.json", "state/v2-duplicate-ids.json"} {
		s, err := ParseState([]byte(readFixture(t, f)))
		if err != nil {
			t.Fatal(err)
		}
		h := HistoryOf(s)
		if len(h.Operations) == 0 {
			t.Fatalf("%s: no operations", f)
		}
		for _, e := range h.Operations {
			if e.Source != SourceUnknown || e.RequestedBy != nil {
				t.Errorf("%s: %s source=%q requested_by=%+v", f, e.ID, e.Source, e.RequestedBy)
			}
		}
		// Без полей в журнале — и при записи без них (формат не меняется).
		b, err := s.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"source"`) || strings.Contains(string(b), `"requested_by"`) {
			t.Errorf("%s: empty source written", f)
		}
	}
}

func TestSourceRoundTrip(t *testing.T) {
	st := State{Version: StateVersion, Operations: []Operation{
		{ID: "a", Status: OpCommitted, Applied: []Applied{}, Source: SourceCLI, RequestedBy: &Requester{UID: 1000, User: "ashot"}},
		{ID: "b", Status: OpCommitted, Applied: []Applied{}, Source: SourceDaemon, RequestedBy: &Requester{UID: 997, User: "ramplus-web", Via: ViaWeb}},
	}}
	b, err := st.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseState(b)
	if err != nil {
		t.Fatal(err)
	}
	h := HistoryOf(got)
	if h.Operations[0].Source != SourceCLI || h.Operations[0].RequestedBy.User != "ashot" ||
		h.Operations[1].Source != SourceDaemon || h.Operations[1].RequestedBy.Via != ViaWeb || h.Operations[1].RequestedBy.UID != 997 {
		t.Errorf("history: %+v %+v", h.Operations[0], h.Operations[1])
	}
}
