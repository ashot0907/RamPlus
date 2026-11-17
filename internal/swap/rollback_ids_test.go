package swap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Часы стоят: все операции получают одно и то же время. ID всё равно
// уникальны, и каждый rollback отменяет ровно предыдущую операцию стека.
func TestFrozenClockRollbackStack(t *testing.T) {
	w := newWorld(t)
	w.frozenClock = true
	s0 := w.snap()
	mustRun(t, w, apply4G)
	s1 := w.snap()
	mustRun(t, w, resize6G)
	s2 := w.snap()
	mustRun(t, w, remove)

	ops := w.state().Operations
	applyID, resizeID, removeID := ops[0].ID, ops[1].ID, ops[2].ID
	for i, want := range []struct {
		snap   snapshot
		target string
	}{{s2, removeID}, {s1, resizeID}, {s0, applyID}} {
		mustRun(t, w, rollback)
		if d := want.snap.diff(w.snap()); d != "" {
			t.Fatalf("rollback #%d undid the wrong operation:\n%s", i+1, d)
		}
		st := w.state()
		last := st.Operations[len(st.Operations)-1]
		if last.Action != ActionRollback || last.Target != want.target {
			t.Fatalf("rollback #%d: target %q, want %q", i+1, last.Target, want.target)
		}
	}
	st := w.state()
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
	w.frozenClock = true
	now := w.sys().Now()
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

// Та же ситуация на «живой» системе: после миграции rollback отменяет
// resize (6G → 4G), следующий — apply.
func TestRollbackAfterMigration(t *testing.T) {
	w := newWorld(t)
	s0 := w.snap()
	mustRun(t, w, apply4G)
	s1 := w.snap()
	mustRun(t, w, resize6G)
	mustRun(t, w, remove)
	mustRun(t, w, rollback) // A7 — система как в VM: swapfile 6G
	loadState(t, w, "state/v2-duplicate-ids.json")

	mustRun(t, w, rollback)
	if d := s1.diff(w.snap()); d != "" {
		t.Fatalf("must undo resize (6G → 4G):\n%s", d)
	}
	mustRun(t, w, rollback)
	if d := s0.diff(w.snap()); d != "" {
		t.Fatalf("must undo apply:\n%s", d)
	}
}

// Ровно баг из VM: журнал говорит «отменить apply» (resize помечен
// отменённым), а файл 6G. Rollback отказывает и ничего не трогает.
func TestRollbackRefusesOnStateMismatch(t *testing.T) {
	w := newWorld(t)
	mustRun(t, w, apply4G)
	mustRun(t, w, resize6G)
	st := w.state()
	st.Operations[1].Status = OpRolledBack // как сделала старая версия
	b, _ := st.Marshal()
	w.files[StatePath].data = string(b)
	before := w.snap()

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
	if d := before.diff(w.snap()); d != "" {
		t.Errorf("system changed:\n%s", d)
	}
	if w.files[ManagedPath].size != 6*GiB || !hasKey(w.active, ManagedPath) {
		t.Error("swapfile touched")
	}
}

func TestCheckExpectedDiscrepancies(t *testing.T) {
	mk := func(mod func(w *fakeWorld)) []string {
		w := newWorld(t).withOldSwap(0)
		mustRun(t, w, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace})
		mod(w)
		f := Gather(context.Background(), w.sys(), "")
		i, _ := f.State.LastUndoable()
		return CheckExpected(f, f.State.Operations[i], nil)
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
		{"priority", func(w *fakeWorld) { w.active[ManagedPath] = 5 }, "приоритет 5, по журналу 10"},
		{"swapoff by hand", func(w *fakeWorld) { delete(w.active, ManagedPath) }, "не активен, а по журналу включён"},
		{"unit removed", func(w *fakeWorld) { delete(w.files, unit) }, unit + ": нет"},
		{"unit edited", func(w *fakeWorld) { w.files[unit].data += "# edit\n" }, "содержимое отличается"},
		{"old swap back on", func(w *fakeWorld) { w.active["/swap.img"] = -2 }, "swap /swap.img активен, а по журналу отключён"},
		{"fstab restored by hand", func(w *fakeWorld) {
			w.files[FstabPath].data = strings.Replace(w.files[FstabPath].data, fstabDisabledBy, "", 1)
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
		{"swapfile as recorded", &fakeFile{size: 4 * GiB, mode: 0o600, swap: true},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: 4 * GiB}, true},
		{"size differs (the VM bug)", &fakeFile{size: 6 * GiB, mode: 0o600, swap: true},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: 4 * GiB}, false},
		{"no swap signature", &fakeFile{data: "user data", size: 9, mode: 0o644},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: 9}, false},
		{"fresh zero file before mkswap", &fakeFile{size: GiB, mode: 0o600},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: GiB}, true},
		{"partial after crash", &fakeFile{size: GiB / 2, mode: 0o600},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: GiB, SizeAtMost: true}, true},
		{"partial without SizeAtMost", &fakeFile{size: GiB / 2, mode: 0o600},
			Step{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: GiB}, false},
		{"config as written", &fakeFile{data: "x", size: 1, mode: 0o644},
			Step{Kind: KindDeleteConfig, Path: ManagedPath, Content: "x"}, true},
		{"config edited", &fakeFile{data: "y", size: 1, mode: 0o644},
			Step{Kind: KindDeleteConfig, Path: ManagedPath, Content: "x"}, false},
		{"config without marker", &fakeFile{data: "[Swap]\n", size: 7, mode: 0o644},
			Step{Kind: KindDeleteConfig, Path: ManagedPath}, false},
		{"config with marker", &fakeFile{data: unitHeader + "[Swap]\n", mode: 0o644},
			Step{Kind: KindDeleteConfig, Path: ManagedPath}, true},
		{"legacy remove_file of foreign config", &fakeFile{data: "[Swap]\n", size: 7, mode: 0o644},
			Step{Kind: KindRemoveFile, Path: ManagedPath}, false},
	}
	for _, tt := range tests {
		w := newWorld(t)
		w.files[ManagedPath] = tt.file
		r := &Runner{Sys: w.sys()}
		_, err := r.Run(ctx, []Step{tt.step})
		_, exists := w.files[ManagedPath]
		if exists == tt.deleted {
			t.Errorf("%s: deleted=%v, want %v (err=%v)", tt.name, !exists, tt.deleted, err)
		}
		if !tt.deleted && !errors.Is(err, ErrNotOurs) {
			t.Errorf("%s: want ErrNotOurs, got %v", tt.name, err)
		}
	}

	// Подтом с чужими файлами не удаляется (btrfs subvolume delete рекурсивен).
	w := newWorld(t)
	w.files[BtrfsDir] = &fakeFile{dir: true}
	w.files[BtrfsDir+"/photos.tar"] = &fakeFile{data: "!", size: 1}
	r := &Runner{Sys: w.sys()}
	if _, err := r.Run(ctx, []Step{{Kind: KindBtrfsSubvolDelete, Path: BtrfsDir}}); err == nil || !hasKey(w.files, BtrfsDir+"/photos.tar") {
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
	b, err := OSFS{}.ReadPrefix(p, 4096)
	if err != nil || string(b) != "abc" {
		t.Errorf("ReadPrefix = %q, %v", b, err)
	}
}
