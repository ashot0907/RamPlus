package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
