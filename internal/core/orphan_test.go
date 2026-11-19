package core

import (
	"context"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/swap"
)

var removeOrphan = swap.Options{Action: ActionRemove, Orphan: true}

// orphanWorld — swap RamPlus создан, а журнал удалён вручную (как в VM).
func orphanWorld(t *testing.T) (w *fakeWorld, clean, orphaned snapshot) {
	t.Helper()
	w = newWorld(t)
	clean = w.Snap()
	mustRun(t, w, apply4G)
	delete(w.Files, StatePath)
	return w, clean, w.Snap()
}

func orphanPlan(t *testing.T, w *fakeWorld) *plan.Plan {
	t.Helper()
	return New(w.System()).SwapPlan(context.Background(), removeOrphan)
}

func TestOrphanCheckClean(t *testing.T) {
	w, _, _ := orphanWorld(t)
	got := strings.Join(cleanOf(w), "\n")
	for _, want := range []string{"найдены следы RamPlus без записи в журнале", ManagedPath, "юнит ramplus.swapfile.swap", "remove --orphan"} {
		if !strings.Contains(got, want) {
			t.Errorf("check-clean lacks %q:\n%s", want, got)
		}
	}
	// С записью в журнале это не сирота.
	w2 := newWorld(t)
	mustRun(t, w2, apply4G)
	if strings.Contains(strings.Join(cleanOf(w2), "\n"), "без записи в журнале") {
		t.Error("managed swap is not an orphan")
	}
}

func TestRemoveOrphan(t *testing.T) {
	w, clean, orphaned := orphanWorld(t)
	p := orphanPlan(t, w)
	if p.Blocked() {
		t.Fatalf("blocked: %+v", p.Failures())
	}
	if p.Command != "ramplus swap remove --orphan" {
		t.Errorf("command = %q", p.Command)
	}
	if ch, ok := hasCode(p, "orphan.found"); !ok || !strings.Contains(ch.Message, "SWAPSPACE2") || !strings.Contains(ch.Message, "маркером") {
		t.Errorf("orphan.found: %+v", ch)
	}
	if got := kinds(p.Steps); got != "swapoff,systemctl,delete_config,systemctl,delete_swapfile" {
		t.Errorf("steps = %s", got)
	}
	mustRun(t, w, removeOrphan)
	if d := clean.Diff(w.Snap()); d != "" && !onlyJournal(d) {
		t.Errorf("not clean:\n%s", d)
	}
	if p := cleanOf(w); len(p) != 0 {
		t.Errorf("check-clean after remove --orphan: %v", p)
	}
	// Обратимо: rollback возвращает следы как были.
	mustRun(t, w, rollback)
	if d := orphaned.Diff(w.Snap()); d != "" && !onlyJournal(d) {
		t.Errorf("rollback did not restore:\n%s", d)
	}
}

func TestRemoveOrphanRefusals(t *testing.T) {
	tests := []struct {
		name string
		mod  func(w *fakeWorld)
		code string
	}{
		{"nothing", func(*fakeWorld) {}, "orphan.none"},
		{"managed only", func(w *fakeWorld) { mustRun(t, w, apply4G) }, "orphan.none"},
		{"unit without marker", func(w *fakeWorld) {
			w.Files[UnitDir+"/ramplus.swapfile.swap"] = &fakeFile{Data: "[Swap]\nWhat=/ramplus.swapfile\n", Mode: 0o644}
		}, "orphan.not_ours"},
		{"file without signature", func(w *fakeWorld) {
			w.Files[ManagedPath] = &fakeFile{Data: "not a swapfile", Mode: 0o600}
		}, "orphan.not_ours"},
	}
	for _, tt := range tests {
		w := newWorld(t)
		tt.mod(w)
		before := w.Snap()
		p := orphanPlan(t, w)
		if _, ok := hasCode(p, tt.code); !ok || !p.Blocked() {
			t.Errorf("%s: want blocked %s, got %+v", tt.name, tt.code, p.Checks)
		}
		if _, _, err := run(t, w, removeOrphan); err == nil {
			t.Errorf("%s: must refuse", tt.name)
		}
		if d := before.Diff(w.Snap()); d != "" && !onlyJournal(d) {
			t.Errorf("%s: system changed:\n%s", tt.name, d)
		}
	}
}

// Юнит с маркером и нестандартным путём: путь берётся из What=.
func TestOrphanCustomPath(t *testing.T) {
	w := newWorld(t)
	w.Files["/mnt"] = &fakeFile{Dir: true, Mode: 0o755}
	w.Files["/mnt/data"] = &fakeFile{Dir: true, Mode: 0o755}
	custom := swap.Options{Action: ActionApply, Size: 4 * GiB, Path: "/mnt/data/ramplus.swapfile"}
	mustRun(t, w, custom)
	delete(w.Files, StatePath)
	p := orphanPlan(t, w)
	if p.Blocked() || !strings.Contains(kinds(p.Steps), "delete_swapfile") {
		t.Fatalf("plan: %+v %s", p.Checks, kinds(p.Steps))
	}
	if ch, _ := hasCode(p, "orphan.found"); !strings.Contains(ch.Message, "/mnt/data/ramplus.swapfile") {
		t.Errorf("orphan.found: %s", ch.Message)
	}
	mustRun(t, w, removeOrphan)
	if p := cleanOf(w); len(p) != 0 {
		t.Errorf("not clean: %v", p)
	}
}

// apply поверх сироты подсказывает remove --orphan.
func TestApplyOverOrphan(t *testing.T) {
	w, _, _ := orphanWorld(t)
	p := New(w.System()).SwapPlan(context.Background(), apply4G)
	if ch, ok := hasCode(p, "files.orphan"); !ok || !strings.Contains(ch.Message, "remove --orphan") {
		t.Errorf("files.orphan: %+v", p.Checks)
	}
}
