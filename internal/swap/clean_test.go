package swap

import (
	"context"
	"strings"
	"testing"
)

func loadState(t *testing.T, w *fakeWorld, fixture string) {
	t.Helper()
	w.files[StateDir] = &fakeFile{dir: true, mode: 0o755}
	w.files[StatePath] = &fakeFile{data: readFixture(t, fixture), mode: 0o644}
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
	if !strings.Contains(string(b), `"version": 2`) {
		t.Error("state must be written as v2")
	}
}

// Ровно случай из VM: state.json от прошлой версии, всё откачено —
// система чистая.
func TestCleanWithOldHistory(t *testing.T) {
	w := newWorld(t)
	loadState(t, w, "state/v1-rolled-back.json")
	f := Gather(context.Background(), w.sys(), "")
	if f.StateErr != nil {
		t.Fatal(f.StateErr)
	}
	if p := CleanProblems(f); len(p) != 0 {
		t.Errorf("must be clean, got %v", p)
	}
}

func TestCleanProblems(t *testing.T) {
	tests := []struct {
		name string
		mod  func(w *fakeWorld)
		want string
	}{
		{"managed", func(w *fakeWorld) { loadState(t, w, "state/v1-committed.json") }, "управляет swap /ramplus.swap"},
		{"pending", func(w *fakeWorld) {
			st := State{Operations: []Operation{{ID: "x", Action: ActionApply, Status: OpInProgress, Command: "ramplus swap apply"}}}
			b, _ := st.Marshal()
			w.files[StateDir] = &fakeFile{dir: true}
			w.files[StatePath] = &fakeFile{data: string(b)}
		}, "незавершённая операция x"},
		{"active swap without journal", func(w *fakeWorld) {
			w.files[ManagedPath] = &fakeFile{size: GiB, mode: 0o600, swap: true}
			w.active[ManagedPath] = 10
		}, "активен swap RamPlus /ramplus.swapfile"},
		{"legacy unit", func(w *fakeWorld) {
			w.files[UnitDir+"/ramplus.swap.swap"] = &fakeFile{data: unitHeader}
		}, "юнит RamPlus /etc/systemd/system/ramplus.swap.swap"},
		{"custom path unit", func(w *fakeWorld) {
			w.files[UnitDir+"/mnt-nvme-ramplus.swapfile.swap"] = &fakeFile{data: unitHeader}
		}, "mnt-nvme-ramplus.swapfile.swap"},
		{"leftover file", func(w *fakeWorld) {
			w.files[LegacyPath] = &fakeFile{size: GiB, mode: 0o600}
		}, "существует /ramplus.swap"},
		{"broken journal", func(w *fakeWorld) {
			w.files[StateDir] = &fakeFile{dir: true}
			w.files[StatePath] = &fakeFile{data: "{"}
		}, "не удалось прочитать"},
	}
	for _, tt := range tests {
		w := newWorld(t)
		tt.mod(w)
		p := CleanProblems(Gather(context.Background(), w.sys(), ""))
		if !strings.Contains(strings.Join(p, "\n"), tt.want) {
			t.Errorf("%s: want %q in %v", tt.name, tt.want, p)
		}
	}
	// Чужой swap (/swap.img) — не след RamPlus.
	w := newWorld(t).withOldSwap(0)
	if p := CleanProblems(Gather(context.Background(), w.sys(), "")); len(p) != 0 {
		t.Errorf("foreign swap is clean: %v", p)
	}
}

// Выполненный apply из v1 (шаг remove_file в отмене) откатывается новой
// версией, а откат обратим.
func TestRollbackV1Apply(t *testing.T) {
	w := newWorld(t)
	clean := w.snap()
	loadState(t, w, "state/v1-committed.json")
	unit := UnitDir + "/ramplus.swap.swap"
	st, _ := ParseState([]byte(readFixture(t, "state/v1-committed.json")))
	w.files[LegacyPath] = &fakeFile{size: 4 * GiB, mode: 0o600, swap: true}
	w.files[unit] = &fakeFile{data: st.Operations[0].Applied[4].Step.Content, mode: 0o644}
	w.active[LegacyPath] = 10
	w.enabled["ramplus.swap.swap"] = true
	applied := w.snap()

	// Сбой посреди отката legacy-журнала → система возвращается к applied.
	for n := 1; n <= 12; n++ {
		w2 := cloneWorld(t, w)
		w2.failAt = w2.mutations + n
		if _, _, err := run(t, w2, rollback); err == nil {
			break
		}
		if d := applied.diff(w2.snap()); d != "" {
			t.Fatalf("n=%d: legacy rollback not reversible:\n%s", n, d)
		}
	}

	mustRun(t, w, rollback)
	if d := clean.diff(w.snap()); d != "" {
		t.Errorf("v1 apply not rolled back:\n%s", d)
	}
	if p := CleanProblems(Gather(context.Background(), w.sys(), "")); len(p) != 0 {
		t.Errorf("must be clean after rollback: %v", p)
	}
	if w.state().Managed != nil {
		t.Error("managed must be nil")
	}
}

// cloneWorld — независимая копия фейковой системы.
func cloneWorld(t *testing.T, w *fakeWorld) *fakeWorld {
	t.Helper()
	c := *w
	c.files, c.links, c.active, c.used, c.enabled, c.mounts =
		map[string]*fakeFile{}, map[string]string{}, map[string]int{}, map[string]uint64{}, map[string]bool{}, map[string]fakeMount{}
	for k, v := range w.files {
		f := *v
		c.files[k] = &f
	}
	for k, v := range w.links {
		c.links[k] = v
	}
	for k, v := range w.active {
		c.active[k] = v
	}
	for k, v := range w.used {
		c.used[k] = v
	}
	for k, v := range w.enabled {
		c.enabled[k] = v
	}
	for k, v := range w.mounts {
		c.mounts[k] = v
	}
	c.log = nil
	return &c
}
