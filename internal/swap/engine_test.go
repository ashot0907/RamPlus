package swap

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/plan"
)

func run(t *testing.T, w *fakeWorld, o Options) (Plan, string, error) {
	t.Helper()
	var out bytes.Buffer
	m := &Manager{Sys: w.System(), Out: &out}
	p, err := m.Execute(context.Background(), o, yes)
	return p, out.String(), err
}

func mustRun(t *testing.T, w *fakeWorld, o Options) {
	t.Helper()
	if _, out, err := run(t, w, o); err != nil {
		t.Fatalf("%s: %v\n%s", o.Command(), err, out)
	}
}

var (
	apply4G  = Options{Action: ActionApply, Size: 4 * GiB}
	resize6G = Options{Action: ActionResize, Size: 6 * GiB}
	remove   = Options{Action: ActionRemove}
	rollback = Options{Action: ActionRollback}
)

func TestApplySuccess(t *testing.T) {
	w := newWorld(t)
	mustRun(t, w, apply4G)

	f := w.Files[ManagedPath]
	if f == nil || f.Size != 4*GiB || f.Mode != 0o600 || !f.Swap {
		t.Fatalf("swapfile = %+v", f)
	}
	if prio, ok := w.Active[ManagedPath]; !ok || prio != 10 {
		t.Errorf("active = %v", w.Active)
	}
	unit := UnitName(ManagedPath)
	if u := w.Files[filepath.Join(UnitDir, unit)]; u == nil || !strings.Contains(u.Data, "What=/ramplus.swap") || u.Mode != 0o644 {
		t.Errorf("unit = %+v", u)
	}
	if !w.Enabled[unit] {
		t.Errorf("unit not enabled: %v", w.Enabled)
	}
	if strings.Contains(w.Files[FstabPath].Data, "ramplus") {
		t.Error("fstab must not be touched for our swap")
	}
	st := stateOf(t, w)
	if ManagedOf(st) == nil || ManagedOf(st).Path != ManagedPath || ManagedOf(st).SizeBytes != 4*GiB {
		t.Errorf("managed = %+v", ManagedOf(st))
	}
	if len(st.Operations) != 1 || st.Operations[0].Status != OpCommitted || len(st.Operations[0].Applied) != 7 {
		t.Errorf("ops = %+v", st.Operations)
	}
}

func TestApplyNeedsRoot(t *testing.T) {
	w := newWorld(t)
	w.EUID = 1000
	_, _, err := run(t, w, apply4G)
	if !errors.Is(err, ErrNeedRoot) {
		t.Fatalf("err = %v", err)
	}
	if w.Mutations != 0 {
		t.Errorf("mutations without root: %v", w.Log)
	}
}

func TestPlanWithoutRootReadsOnly(t *testing.T) {
	w := newWorld(t).WithOldSwap(0)
	w.EUID = 1000
	m := &Manager{Sys: w.System()}
	p := m.Plan(context.Background(), apply4G)
	if p.Blocked() || len(p.Steps) == 0 {
		t.Errorf("plan must work without root: %+v", p.Failures())
	}
	if w.Mutations != 0 {
		t.Errorf("plan mutated the system: %v", w.Log)
	}
}

func TestApplyBlockedChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.Transport = "usb"
	before := w.Snap()
	_, out, err := run(t, w, apply4G)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v", err)
	}
	if d := before.Diff(w.Snap()); d != "" {
		t.Errorf("changed:\n%s", d)
	}
	if !strings.Contains(out, "USB") || hasKey(w.Files, StatePath) {
		t.Errorf("output/state: %s", out)
	}
}

func TestApplyCancelled(t *testing.T) {
	w := newWorld(t)
	before := w.Snap()
	m := &Manager{Sys: w.System(), Out: &bytes.Buffer{}}
	_, err := m.Execute(context.Background(), apply4G, func(*plan.Plan) (bool, error) { return false, nil })
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v", err)
	}
	if d := before.Diff(w.Snap()); d != "" {
		t.Errorf("changed:\n%s", d)
	}
}

func TestApplyUSBWithForce(t *testing.T) {
	w := newWorld(t)
	w.Transport = "usb"
	o := apply4G
	o.Force = true
	mustRun(t, w, o)
}

func TestFallocateFallbackToZeros(t *testing.T) {
	w := newWorld(t)
	w.FallocUnsupported = true
	mustRun(t, w, apply4G)
	if f := w.Files[ManagedPath]; f == nil || f.Size != 4*GiB || !f.Swap {
		t.Fatalf("swapfile = %+v", f)
	}
	if !strings.Contains(strings.Join(w.Log, "\n"), "zeros "+ManagedPath) {
		t.Errorf("no zeros fallback in %v", w.Log)
	}
}

func TestReplaceDeleteOldAndRollback(t *testing.T) {
	w := newWorld(t).WithOldSwap(1 * GiB)
	initial := w.Snap()
	mustRun(t, w, Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace, DeleteOld: true})

	if hasKey(w.Files, "/swap.img") || hasKey(w.Active, "/swap.img") {
		t.Error("/swap.img must be off and deleted")
	}
	if !strings.Contains(w.Files[FstabPath].Data, "# disabled by ramplus: /swap.img\tnone\tswap") {
		t.Errorf("fstab:\n%s", w.Files[FstabPath].Data)
	}
	backups := 0
	for p := range w.Files {
		if strings.HasPrefix(p, BackupDir+"/") && strings.HasSuffix(p, "etc_fstab") {
			backups++
		}
	}
	if backups != 1 {
		t.Errorf("fstab backups = %d", backups)
	}

	mustRun(t, w, rollback)
	if d := initial.Diff(w.Snap()); d != "" {
		t.Errorf("rollback did not restore the system:\n%s", d)
	}
	st := stateOf(t, w)
	if ManagedOf(st) != nil || st.Operations[0].Status != OpRolledBack || st.Operations[1].Status != OpCommitted {
		t.Errorf("state = %+v", st)
	}
}

func TestRollbackKeepsUserFstabEdits(t *testing.T) {
	w := newWorld(t).WithOldSwap(0)
	mustRun(t, w, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace})
	w.Files[FstabPath].Data += "/dev/sdb1 /data ext4 defaults 0 2\n" // правка пользователя
	mustRun(t, w, rollback)
	fstab := w.Files[FstabPath].Data
	if !strings.Contains(fstab, "\n/swap.img\tnone\tswap") || strings.Contains(fstab, "disabled by ramplus") || !strings.Contains(fstab, "/data") {
		t.Errorf("fstab:\n%s", fstab)
	}
}

func TestLifecycleRollbackStack(t *testing.T) {
	w := newWorld(t)
	s0 := w.Snap()
	mustRun(t, w, apply4G)
	s1 := w.Snap()
	mustRun(t, w, resize6G)
	if w.Files[ManagedPath].Size != 6*GiB || w.Active[ManagedPath] != 10 {
		t.Fatalf("resize: %+v %v", w.Files[ManagedPath], w.Active)
	}
	s2 := w.Snap()
	mustRun(t, w, remove)
	if hasKey(w.Files, ManagedPath) || len(w.Enabled) != 0 || hasKey(w.Files, filepath.Join(UnitDir, UnitName(ManagedPath))) {
		t.Fatalf("remove left files: %v %v", w.Files, w.Enabled)
	}

	for i, want := range []snapshot{s2, s1, s0} {
		mustRun(t, w, rollback)
		if d := want.Diff(w.Snap()); d != "" {
			t.Fatalf("rollback #%d:\n%s", i+1, d)
		}
	}
	if ManagedOf(stateOf(t, w)) != nil {
		t.Error("managed must be nil after full rollback")
	}
	if _, _, err := run(t, w, rollback); !errors.Is(err, ErrBlocked) {
		t.Errorf("4th rollback: want ErrBlocked (nothing to undo), got %v", err)
	}
}

func TestBtrfsApplyAndRemove(t *testing.T) {
	for _, mk := range []bool{true, false} {
		w := newWorld(t)
		w.FSType, w.BtrfsTool, w.Mkswapfile = "btrfs", true, mk
		s0 := w.Snap()
		mustRun(t, w, apply4G)
		f := w.Files[BtrfsPath]
		if f == nil || !f.NoCOW || !f.Swap || !hasKey(w.Active, BtrfsPath) {
			t.Fatalf("mkswapfile=%v: file %+v active %v", mk, f, w.Active)
		}
		mustRun(t, w, remove)
		if hasKey(w.Files, BtrfsDir) {
			t.Errorf("mkswapfile=%v: subvolume left", mk)
		}
		mustRun(t, w, rollback) // вернуть remove
		mustRun(t, w, rollback) // вернуть apply
		if d := s0.Diff(w.Snap()); d != "" {
			t.Errorf("mkswapfile=%v:\n%s", mk, d)
		}
	}
}

// TestFailureAtEveryStep: сбой на каждом изменяющем вызове → система
// возвращается в состояние до операции.
func TestFailureAtEveryStep(t *testing.T) {
	btrfs := func(mk bool) func(*fakeWorld) {
		return func(w *fakeWorld) { w.FSType, w.BtrfsTool, w.Mkswapfile = "btrfs", true, mk }
	}
	scenarios := []struct {
		name   string
		setup  func(*fakeWorld)
		before []Options
		op     Options
	}{
		{"apply", nil, nil, apply4G},
		{"apply-keep", func(w *fakeWorld) { w.WithOldSwap(0) }, nil, apply4G},
		{"apply-replace-delete", func(w *fakeWorld) { w.WithOldSwap(1 * GiB) }, nil,
			Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace, DeleteOld: true}},
		{"apply-btrfs-mkswapfile", btrfs(true), nil, apply4G},
		{"apply-btrfs-chattr", btrfs(false), nil, apply4G},
		{"resize", nil, []Options{apply4G}, resize6G},
		{"remove", nil, []Options{apply4G}, remove},
		{"remove-btrfs", btrfs(true), []Options{apply4G}, remove},
		{"rollback-replace", func(w *fakeWorld) { w.WithOldSwap(1 * GiB) },
			[]Options{{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace, DeleteOld: true}}, rollback},
		{"rollback-remove", nil, []Options{apply4G, remove}, rollback},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			prepare := func() *fakeWorld {
				w := newWorld(t)
				if sc.setup != nil {
					sc.setup(w)
				}
				for _, o := range sc.before {
					mustRun(t, w, o)
				}
				return w
			}
			// Эталон: тот же сценарий без сбоев.
			ref := prepare()
			start := ref.Mutations
			mustRun(t, ref, sc.op)
			after, total := ref.Snap(), ref.Mutations-start

			for n := 1; n <= total; n++ {
				w := prepare()
				before := w.Snap()
				w.FailAt = w.Mutations + n
				_, out, err := run(t, w, sc.op)
				if err == nil {
					t.Fatalf("n=%d: injected failure ignored\n%s", n, out)
				}
				got := w.Snap()
				if d := before.Diff(got); d == "" {
					var se *StepError
					if errors.As(err, &se) {
						if !strings.Contains(err.Error(), "шаг ") || len(se.RollbackErrors) != 0 {
							t.Fatalf("n=%d: %v", n, err)
						}
						ops := stateOf(t, w).Operations
						last := ops[len(ops)-1]
						if last.Status != OpFailed && !(last.Status == OpInProgress && len(last.Applied) == 0) {
							t.Fatalf("n=%d: last op %s with %d applied", n, last.Status, len(last.Applied))
						}
					}
					continue
				}
				// Единственный допустимый случай изменений: упала запись итога
				// в журнал после всех шагов. Операция остаётся in_progress, и
				// rollback должен её отменить.
				if d := after.Diff(got); d != "" || !strings.Contains(err.Error(), "не удалось записать итог") {
					t.Fatalf("n=%d (%s): system not restored: %v\n%s", n, w.Log[len(w.Log)-1], err, before.Diff(got))
				}
				w.FailAt = 0
				mustRun(t, w, rollback)
				if d := before.Diff(w.Snap()); d != "" {
					t.Fatalf("n=%d: rollback of interrupted op failed:\n%s", n, d)
				}
			}
			if total < 5 {
				t.Errorf("too few mutations: %d", total)
			}
		})
	}
}

func TestAllocateNeverRemovesForeignFile(t *testing.T) {
	w := newWorld(t)
	w.Files[ManagedPath] = &fakeFile{Data: "чужие данные", Mode: 0o644}
	r := &Runner{Sys: w.System()}
	_, err := r.Run(context.Background(), []Step{{Kind: KindAllocate, Path: ManagedPath, Size: GiB}})
	if err == nil || w.Files[ManagedPath] == nil || w.Files[ManagedPath].Data != "чужие данные" {
		t.Fatalf("foreign file damaged: err=%v file=%+v", err, w.Files[ManagedPath])
	}
}

func TestStepErrorMessage(t *testing.T) {
	w := newWorld(t)
	r := &Runner{Sys: w.System()}
	// mkswap на несуществующем файле падает вторым шагом; первый откатывается.
	_, err := r.Run(context.Background(), []Step{
		{Kind: KindMkdir, Path: "/x"},
		{Kind: KindMkswap, Path: "/nope"},
	})
	var se *StepError
	if !errors.As(err, &se) || se.Index != 2 || se.Total != 2 || hasKey(w.Files, "/x") {
		t.Fatalf("err = %v, files = %v", err, w.Files["/x"])
	}
	if !strings.Contains(err.Error(), "шаг 2/2 (mkswap /nope)") || !strings.Contains(err.Error(), "откачены") {
		t.Errorf("message: %v", err)
	}
}

// runCrash запускает операцию, которая «падает» (panic crashed{}) на
// failAt-м изменяющем вызове. Возвращает true, если падение произошло.
func runCrash(t *testing.T, w *fakeWorld, o Options) (crashedAt string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			c, ok := r.(crashed)
			if !ok {
				panic(r)
			}
			crashedAt = c.At
		}
	}()
	w.Crash = true
	_, _, _ = run(t, w, o)
	return ""
}

// TestCrashAtEveryPoint: процесс падает в любой точке → следующий запуск
// видит незавершённую операцию, apply блокируется, rollback возвращает
// систему в исходное состояние.
func TestCrashAtEveryPoint(t *testing.T) {
	scenarios := []struct {
		name   string
		setup  func(*fakeWorld)
		before []Options
		op     Options
	}{
		{"apply", nil, nil, apply4G},
		{"apply-replace-delete", func(w *fakeWorld) { w.WithOldSwap(1 * GiB) }, nil,
			Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace, DeleteOld: true}},
		{"apply-btrfs", func(w *fakeWorld) { w.FSType, w.BtrfsTool = "btrfs", true }, nil, apply4G},
		{"resize", nil, []Options{apply4G}, resize6G},
		{"remove", nil, []Options{apply4G}, remove},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			prepare := func() *fakeWorld {
				w := newWorld(t)
				if sc.setup != nil {
					sc.setup(w)
				}
				for _, o := range sc.before {
					mustRun(t, w, o)
				}
				return w
			}
			ref := prepare()
			start := ref.Mutations
			mustRun(t, ref, sc.op)
			total := ref.Mutations - start

			recovered := 0
			for n := 1; n <= total; n++ {
				w := prepare()
				before := w.Snap()
				w.FailAt = w.Mutations + n
				at := runCrash(t, w, sc.op)
				if at == "" {
					t.Fatalf("n=%d: no crash", n)
				}
				w.FailAt, w.Crash = 0, false

				if stateOf(t, w).Pending() == nil {
					// Упали до записи операции в журнал — система не тронута.
					if d := before.Diff(w.Snap()); d != "" {
						t.Fatalf("n=%d (%s): no journal entry, but system changed:\n%s", n, at, d)
					}
					continue
				}
				if _, _, err := run(t, w, apply4G); !errors.Is(err, ErrBlocked) {
					t.Fatalf("n=%d: apply after crash must be blocked, got %v", n, err)
				}
				if _, out, err := run(t, w, rollback); err != nil {
					t.Fatalf("n=%d (crash at %s): rollback: %v\n%s", n, at, err, out)
				}
				if d := before.Diff(w.Snap()); d != "" {
					t.Fatalf("n=%d (crash at %s): not restored:\n%s", n, at, d)
				}
				recovered++
			}
			if recovered < total/2 {
				t.Errorf("rollback after crash exercised only %d of %d points", recovered, total)
			}
			t.Logf("%d точек падения, из них с восстановлением через rollback: %d", total, recovered)
		})
	}
}

// Сбой после шага, но до записи "done": в журнале pending, артефакт
// совпадает с записью → rollback его убирает.
func TestCrashAfterStepBeforeDone(t *testing.T) {
	ref := newWorld(t)
	mustRun(t, ref, apply4G)
	// Номер записи журнала сразу после fallocate.
	idx := -1
	for i, op := range ref.Log {
		if op == "fallocate "+ManagedPath {
			for j := i + 1; j < len(ref.Log); j++ {
				if ref.Log[j] == "write "+StatePath {
					idx = j + 1 // mutations считаются с 1
					break
				}
			}
			break
		}
	}
	if idx < 0 {
		t.Fatalf("log: %v", ref.Log)
	}

	w := newWorld(t)
	before := w.Snap()
	w.FailAt = idx
	if at := runCrash(t, w, apply4G); at != "write "+StatePath {
		t.Fatalf("crashed at %q", at)
	}
	w.FailAt, w.Crash = 0, false
	if f := w.Files[ManagedPath]; f == nil || f.Size != 4*GiB {
		t.Fatalf("step must have happened: %+v", f)
	}
	op := stateOf(t, w).Pending()
	last := op.Applied[len(op.Applied)-1]
	if last.Status != StepPending || last.Step.Kind != KindAllocate {
		t.Fatalf("journal: last entry %+v", last)
	}

	f := Gather(context.Background(), w.System(), "")
	if f.Recovery == nil || f.Recovery.Verdict != VerdictPresent {
		t.Fatalf("recovery = %+v", f.Recovery)
	}
	p := BuildPlan(f, rollback)
	if c, _ := findCheck(p, "прерванный шаг"); !strings.Contains(c.Message, "будет откачен") {
		t.Errorf("plan must explain: %+v", p.Checks)
	}
	mustRun(t, w, rollback)
	if d := before.Diff(w.Snap()); d != "" {
		t.Errorf("not restored:\n%s", d)
	}
}

// Артефакт подменён после падения → RamPlus его не трогает.
func TestCrashArtifactMismatch(t *testing.T) {
	w := newWorld(t)
	ref := newWorld(t)
	mustRun(t, ref, apply4G)
	n := 0
	for i, op := range ref.Log {
		if op == "create "+ManagedPath {
			n = i + 1 // упасть на самом create: pending записан, файла ещё нет
		}
	}
	w.FailAt = n
	runCrash(t, w, apply4G)
	w.FailAt, w.Crash = 0, false
	// Кто-то положил на это место свой файл.
	w.Files[ManagedPath] = &fakeFile{Data: "важные данные", Mode: 0o644}
	_, out, err := run(t, w, rollback)
	if !errors.Is(err, ErrBlocked) || !strings.Contains(out, "ручная проверка") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if w.Files[ManagedPath].Data != "важные данные" {
		t.Error("foreign file touched")
	}
}

func TestApplyCustomPathBtrfs(t *testing.T) {
	w := newWorld(t)
	w.Mounts["/mnt/data"] = fakeMount{FSType: "btrfs", Transport: "nvme", Total: 1000 * GiB, Avail: 800 * GiB}
	w.Files["/mnt"] = &fakeFile{Dir: true, Mode: 0o755}
	w.Files["/mnt/data"] = &fakeFile{Dir: true, Mode: 0o755}
	w.Links["/mnt/link"] = "/mnt/data"
	w.BtrfsTool = true
	s0 := w.Snap()

	o := Options{Action: ActionApply, Size: 4 * GiB, Path: "/mnt/link/ramplus.swapfile"}
	p, out, err := run(t, w, o)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	const path = "/mnt/data/ramplus.swapfile"
	if p.Path != path {
		t.Errorf("symlink must be resolved: %s", p.Path)
	}
	if f := w.Files[path]; f == nil || !f.NoCOW || !f.Swap || !hasKey(w.Active, path) {
		t.Fatalf("swapfile = %+v", f)
	}
	if !w.Enabled["mnt-data-ramplus.swapfile.swap"] {
		t.Errorf("enabled = %v", w.Enabled)
	}
	if !strings.Contains(out, "снапшоты подтома") {
		t.Error("custom path on btrfs must warn about snapshots")
	}
	mustRun(t, w, Options{Action: ActionResize, Size: 6 * GiB}) // NoCOW сохраняется при пересоздании
	mustRun(t, w, rollback)
	mustRun(t, w, rollback)
	if d := s0.Diff(w.Snap()); d != "" {
		t.Errorf("not restored:\n%s", d)
	}
}

func TestApplyRefusesForeignFS(t *testing.T) {
	w := newWorld(t)
	w.Mounts["/mnt/win"] = fakeMount{FSType: "ntfs3", Transport: "nvme", Total: 900 * GiB, Avail: 400 * GiB}
	w.Files["/mnt"] = &fakeFile{Dir: true}
	w.Files["/mnt/win"] = &fakeFile{Dir: true}
	_, out, err := run(t, w, Options{Action: ActionApply, Size: 4 * GiB, Path: "/mnt/win/ramplus.swapfile"})
	if !errors.Is(err, ErrBlocked) || !strings.Contains(out, "создайте раздел ext4/xfs") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}
