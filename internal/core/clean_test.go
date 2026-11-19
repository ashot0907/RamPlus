package core

import (
	"context"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/swap"
)

// Ровно случай из VM: state.json от прошлой версии, всё откачено —
// система чистая.
func TestCleanWithOldHistory(t *testing.T) {
	w := newWorld(t)
	loadState(t, w, "state/v1-rolled-back.json")
	f := swap.Gather(context.Background(), w.System(), "")
	if f.StateErr != nil {
		t.Fatal(f.StateErr)
	}
	if p := CleanProblems(f, w.System()); len(p) != 0 {
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
			st := State{Operations: []journalOp{{ID: "x", Action: ActionApply, Status: OpInProgress, Command: "ramplus swap apply"}}}
			b, _ := st.Marshal()
			w.Files[StateDir] = &fakeFile{Dir: true}
			w.Files[StatePath] = &fakeFile{Data: string(b)}
		}, "незавершённая операция x"},
		{"active swap without journal", func(w *fakeWorld) {
			w.Files[ManagedPath] = &fakeFile{Size: GiB, Mode: 0o600, Swap: true}
			w.Active[ManagedPath] = 10
		}, "активен swap RamPlus /ramplus.swapfile"},
		{"legacy unit", func(w *fakeWorld) {
			w.Files[UnitDir+"/ramplus.swap.swap"] = &fakeFile{Data: unitHeader}
		}, "юнит RamPlus /etc/systemd/system/ramplus.swap.swap"},
		{"custom path unit", func(w *fakeWorld) {
			w.Files[UnitDir+"/mnt-nvme-ramplus.swapfile.swap"] = &fakeFile{Data: unitHeader}
		}, "mnt-nvme-ramplus.swapfile.swap"},
		{"leftover file", func(w *fakeWorld) {
			w.Files[LegacyPath] = &fakeFile{Size: GiB, Mode: 0o600}
		}, "существует /ramplus.swap"},
		{"broken journal", func(w *fakeWorld) {
			w.Files[StateDir] = &fakeFile{Dir: true}
			w.Files[StatePath] = &fakeFile{Data: "{"}
		}, "не удалось прочитать"},
	}
	for _, tt := range tests {
		w := newWorld(t)
		tt.mod(w)
		p := cleanOf(w)
		if !strings.Contains(strings.Join(p, "\n"), tt.want) {
			t.Errorf("%s: want %q in %v", tt.name, tt.want, p)
		}
	}
	// Чужой swap (/swap.img) — не след RamPlus.
	w := newWorld(t).WithOldSwap(0)
	if p := cleanOf(w); len(p) != 0 {
		t.Errorf("foreign swap is clean: %v", p)
	}
}

// Выполненный apply из v1 (шаг remove_file в отмене) откатывается новой
// версией, а откат обратим.
func TestRollbackV1Apply(t *testing.T) {
	w := newWorld(t)
	clean := w.Snap()
	loadState(t, w, "state/v1-committed.json")
	unit := UnitDir + "/ramplus.swap.swap"
	st, _ := ParseState([]byte(readFixture(t, "state/v1-committed.json")))
	w.Files[LegacyPath] = &fakeFile{Size: 4 * GiB, Mode: 0o600, Swap: true}
	w.Files[unit] = &fakeFile{Data: st.Operations[0].Applied[4].Step.Content, Mode: 0o644}
	w.Active[LegacyPath] = 10
	w.Enabled["ramplus.swap.swap"] = true
	applied := w.Snap()

	// Сбой посреди отката legacy-журнала → система возвращается к applied.
	for n := 1; n <= 12; n++ {
		w2 := w.Clone()
		w2.FailAt = w2.Mutations + n
		if _, _, err := run(t, w2, rollback); err == nil {
			break
		}
		if d := applied.Diff(w2.Snap()); d != "" {
			t.Fatalf("n=%d: legacy rollback not reversible:\n%s", n, d)
		}
	}

	mustRun(t, w, rollback)
	if d := clean.Diff(w.Snap()); d != "" {
		t.Errorf("v1 apply not rolled back:\n%s", d)
	}
	if p := cleanOf(w); len(p) != 0 {
		t.Errorf("must be clean after rollback: %v", p)
	}
	if stateOf(t, w).Managed != nil {
		t.Error("managed must be nil")
	}
}

func TestAnnotateStatus(t *testing.T) {
	w := newWorld(t)
	st := State{Managed: swap.EncodeManaged(&swap.Managed{Path: "/custom/ramplus.swapfile"}),
		Operations: []journalOp{{ID: "1", Action: ActionApply, Status: OpInProgress}}}
	b, _ := st.Marshal()
	w.Files[StateDir] = &fakeFile{Dir: true}
	w.Files[StatePath] = &fakeFile{Data: string(b)}
	status := monitor.Status{Swaps: monitor.Swaps{Devices: []monitor.SwapDevice{
		{Path: "/swap.img"}, {Path: "/custom/ramplus.swapfile"}, {Path: ManagedPath},
	}}}
	AnnotateStatus(&status, w.System())
	got := []bool{status.Swaps.Devices[0].Managed, status.Swaps.Devices[1].Managed, status.Swaps.Devices[2].Managed}
	if got[0] || !got[1] || !got[2] {
		t.Errorf("managed = %v", got)
	}
	if len(status.Swaps.Warnings) != 1 || status.Swaps.Warnings[0].Code != WarnInterrupted {
		t.Errorf("warnings = %+v", status.Swaps.Warnings)
	}
}
