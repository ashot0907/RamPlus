package swap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/journal"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// Часы стоят: все операции получают одно и то же время. ID всё равно
// уникальны, и каждый rollback отменяет ровно предыдущую операцию стека.
func TestFrozenClockRollbackStack(t *testing.T) {
	w := newWorld(t)
	w.FrozenClock = true
	s0 := w.Snap()
	mustRun(t, w, apply4G)
	s1 := w.Snap()
	mustRun(t, w, resize6G)
	s2 := w.Snap()
	mustRun(t, w, remove)

	ops := stateOf(t, w).Operations
	applyID, resizeID, removeID := ops[0].ID, ops[1].ID, ops[2].ID
	for i, want := range []struct {
		snap   snapshot
		target string
	}{{s2, removeID}, {s1, resizeID}, {s0, applyID}} {
		mustRun(t, w, rollback)
		if d := want.snap.Diff(w.Snap()); d != "" {
			t.Fatalf("rollback #%d undid the wrong operation:\n%s", i+1, d)
		}
		st := stateOf(t, w)
		last := st.Operations[len(st.Operations)-1]
		if last.Action != ActionRollback || last.Target != want.target {
			t.Fatalf("rollback #%d: target %q, want %q", i+1, last.Target, want.target)
		}
	}
	st := stateOf(t, w)
	seen := map[string]bool{}
	for _, op := range st.Operations {
		if seen[op.ID] {
			t.Errorf("duplicate ID %s", op.ID)
		}
		seen[op.ID] = true
		if !strings.HasPrefix(op.ID, "20261117T113308.000000000Z-") {
			t.Errorf("ID format: %s", op.ID)
		}
	}
	for i, wantStatus := range []string{OpRolledBack, OpRolledBack, OpRolledBack, OpCommitted, OpCommitted, OpCommitted} {
		if st.Operations[i].Status != wantStatus {
			t.Errorf("op %d (%s): %s, want %s", i, st.Operations[i].Action, st.Operations[i].Status, wantStatus)
		}
	}
	if _, _, err := run(t, w, rollback); !errors.Is(err, ErrBlocked) {
		t.Errorf("4th rollback: %v", err)
	}
}

func TestNewOpIDUnique(t *testing.T) {
	w := newWorld(t)
	w.FrozenClock = true
	now := w.System().Now()
	var ops []Operation
	for i := 0; i < 200; i++ {
		id := NewOpID(now, ops)
		for _, op := range ops {
			if op.ID == id {
				t.Fatalf("duplicate %s", id)
			}
		}
		ops = append(ops, Operation{ID: id})
	}
}

// Та же ситуация на «живой» системе: после миграции rollback отменяет
// resize (6G → 4G), следующий — apply.
func TestRollbackAfterMigration(t *testing.T) {
	w := newWorld(t)
	s0 := w.Snap()
	mustRun(t, w, apply4G)
	s1 := w.Snap()
	mustRun(t, w, resize6G)
	mustRun(t, w, remove)
	mustRun(t, w, rollback) // A7 — система как в VM: swapfile 6G
	loadState(t, w, "state/v2-duplicate-ids.json")

	mustRun(t, w, rollback)
	if d := s1.Diff(w.Snap()); d != "" {
		t.Fatalf("must undo resize (6G → 4G):\n%s", d)
	}
	mustRun(t, w, rollback)
	if d := s0.Diff(w.Snap()); d != "" {
		t.Fatalf("must undo apply:\n%s", d)
	}
}

// Ровно баг из VM: журнал говорит «отменить apply» (resize помечен
// отменённым), а файл 6G. Rollback отказывает и ничего не трогает.
func TestRollbackRefusesOnStateMismatch(t *testing.T) {
	w := newWorld(t)
	mustRun(t, w, apply4G)
	mustRun(t, w, resize6G)
	st := stateOf(t, w)
	st.Operations[1].Status = OpRolledBack // как сделала старая версия
	b, _ := st.Marshal()
	w.Files[StatePath].Data = string(b)
	before := w.Snap()

	_, out, err := run(t, w, rollback)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	for _, want := range []string{
		"состояние системы не совпадает с журналом, нужна ручная проверка",
		"/ramplus.swapfile: размер 6.0 GiB, по журналу 4.0 GiB",
		"swap /ramplus.swapfile: размер 6.0 GiB, по журналу 4.0 GiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if d := before.Diff(w.Snap()); d != "" {
		t.Errorf("system changed:\n%s", d)
	}
	if w.Files[ManagedPath].Size != 6*GiB || !hasKey(w.Active, ManagedPath) {
		t.Error("swapfile touched")
	}
}

func TestCheckExpectedDiscrepancies(t *testing.T) {
	mk := func(mod func(w *fakeWorld)) []string {
		w := newWorld(t).WithOldSwap(0)
		mustRun(t, w, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace})
		mod(w)
		f := Gather(context.Background(), w.System(), "")
		i, _ := f.State.LastUndoable()
		return journal.CheckExpected(w.System(), f.State.Operations[i], nil)
	}
	if d := mk(func(*fakeWorld) {}); len(d) != 0 {
		t.Fatalf("untouched system must match: %v", d)
	}
	unit := filepath.Join(UnitDir, UnitName(ManagedPath))
	tests := []struct {
		name string
		mod  func(w *fakeWorld)
		want string
	}{
		{"priority", func(w *fakeWorld) { w.Active[ManagedPath] = 5 }, "приоритет 5, по журналу 10"},
		{"swapoff by hand", func(w *fakeWorld) { delete(w.Active, ManagedPath) }, "не активен, а по журналу включён"},
		{"unit removed", func(w *fakeWorld) { delete(w.Files, unit) }, unit + ": нет"},
		{"unit edited", func(w *fakeWorld) { w.Files[unit].Data += "# edit\n" }, "содержимое отличается"},
		{"old swap back on", func(w *fakeWorld) { w.Active["/swap.img"] = -2 }, "swap /swap.img активен, а по журналу отключён"},
		{"fstab restored by hand", func(w *fakeWorld) {
			w.Files[FstabPath].Data = strings.Replace(w.Files[FstabPath].Data, fstabDisabledBy, "", 1)
		}, "должна быть закомментирована RamPlus"},
	}
	for _, tt := range tests {
		if d := strings.Join(mk(tt.mod), "\n"); !strings.Contains(d, tt.want) {
			t.Errorf("%s: want %q in:\n%s", tt.name, tt.want, d)
		}
	}
}

// Удаляющие шаги проверяют, что удаляют именно то, что записано в журнале.
func TestDeleteGuards(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		file    *fakeFile
		step    Step
		deleted bool
	}{
		{"swapfile as recorded", &fakeFile{Size: 4 * GiB, Mode: 0o600, Swap: true},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: 4 * GiB}, true},
		{"size differs (the VM bug)", &fakeFile{Size: 6 * GiB, Mode: 0o600, Swap: true},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: 4 * GiB}, false},
		{"no swap signature", &fakeFile{Data: "user data", Size: 9, Mode: 0o644},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: 9}, false},
		{"fresh zero file before mkswap", &fakeFile{Size: GiB, Mode: 0o600},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: GiB}, true},
		{"partial after crash", &fakeFile{Size: GiB / 2, Mode: 0o600},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: GiB, SizeAtMost: true}, true},
		{"partial without SizeAtMost", &fakeFile{Size: GiB / 2, Mode: 0o600},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: GiB}, false},
		{"config as written", &fakeFile{Data: "x", Size: 1, Mode: 0o644},
			Step{Kind: KindDeleteConfig, Path: ManagedPath, Content: "x"}, true},
		{"config edited", &fakeFile{Data: "y", Size: 1, Mode: 0o644},
			Step{Kind: KindDeleteConfig, Path: ManagedPath, Content: "x"}, false},
		{"config without marker", &fakeFile{Data: "[Swap]\n", Size: 7, Mode: 0o644},
			Step{Kind: KindDeleteConfig, Path: ManagedPath}, false},
		{"config with marker", &fakeFile{Data: unitHeader + "[Swap]\n", Mode: 0o644},
			Step{Kind: KindDeleteConfig, Path: ManagedPath}, true},
		{"legacy remove_file of foreign config", &fakeFile{Data: "[Swap]\n", Size: 7, Mode: 0o644},
			Step{Kind: KindRemoveFile, Path: ManagedPath}, false},
	}
	for _, tt := range tests {
		w := newWorld(t)
		w.Files[ManagedPath] = tt.file
		r := &Runner{Sys: w.System()}
		_, err := r.Run(ctx, []Step{tt.step})
		_, exists := w.Files[ManagedPath]
		if exists == tt.deleted {
			t.Errorf("%s: deleted=%v, want %v (err=%v)", tt.name, !exists, tt.deleted, err)
		}
		if !tt.deleted && !errors.Is(err, ErrNotOurs) {
			t.Errorf("%s: want ErrNotOurs, got %v", tt.name, err)
		}
	}

	// Подтом с чужими файлами не удаляется (btrfs subvolume delete рекурсивен).
	w := newWorld(t)
	w.Files[BtrfsDir] = &fakeFile{Dir: true}
	w.Files[BtrfsDir+"/photos.tar"] = &fakeFile{Data: "!", Size: 1}
	r := &Runner{Sys: w.System()}
	if _, err := r.Run(ctx, []Step{{Kind: KindBtrfsSubvolDelete, Path: BtrfsDir}}); err == nil || !hasKey(w.Files, BtrfsDir+"/photos.tar") {
		t.Errorf("non-empty subvolume: err=%v", err)
	}
}

func TestSwapHeader(t *testing.T) {
	page := make([]byte, 4096)
	if IsSwapHeader(page) {
		t.Error("zero page is not a swap header")
	}
	copy(page[4086:], "SWAPSPACE2")
	if !IsSwapHeader(page) {
		t.Error("SWAPSPACE2 at page end")
	}
	// OSFS.ReadPrefix читает не больше n и не падает на коротком файле.
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := system.OSFS{}.ReadPrefix(p, 4096)
	if err != nil || string(b) != "abc" {
		t.Errorf("ReadPrefix = %q, %v", b, err)
	}
}
