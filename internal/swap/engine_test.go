package swap

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func yes(Plan) (bool, error) { return true, nil }

func run(t *testing.T, w *fakeWorld, o Options) (Plan, string, error) {
	t.Helper()
	var out bytes.Buffer
	m := &Manager{Sys: w.sys(), Out: &out}
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

	f := w.files[ManagedPath]
	if f == nil || f.size != 4*GiB || f.mode != 0o600 || !f.swap {
		t.Fatalf("swapfile = %+v", f)
	}
	if prio, ok := w.active[ManagedPath]; !ok || prio != 10 {
		t.Errorf("active = %v", w.active)
	}
	unit := UnitName(ManagedPath)
	if u := w.files[filepath.Join(UnitDir, unit)]; u == nil || !strings.Contains(u.data, "What=/ramplus.swap") || u.mode != 0o644 {
		t.Errorf("unit = %+v", u)
	}
	if !w.enabled[unit] {
		t.Errorf("unit not enabled: %v", w.enabled)
	}
	if strings.Contains(w.files[FstabPath].data, "ramplus") {
		t.Error("fstab must not be touched for our swap")
	}
	st := w.state()
	if st.Managed == nil || st.Managed.Path != ManagedPath || st.Managed.SizeBytes != 4*GiB {
		t.Errorf("managed = %+v", st.Managed)
	}
	if len(st.Operations) != 1 || st.Operations[0].Status != OpCommitted || len(st.Operations[0].Applied) != 7 {
		t.Errorf("ops = %+v", st.Operations)
	}
}

func TestApplyNeedsRoot(t *testing.T) {
	w := newWorld(t)
	w.euid = 1000
	_, _, err := run(t, w, apply4G)
	if !errors.Is(err, ErrNeedRoot) {
		t.Fatalf("err = %v", err)
	}
	if w.mutations != 0 {
		t.Errorf("mutations without root: %v", w.log)
	}
}

func TestPlanWithoutRootReadsOnly(t *testing.T) {
	w := newWorld(t).withOldSwap(0)
	w.euid = 1000
	m := &Manager{Sys: w.sys()}
	p := m.Plan(context.Background(), apply4G)
	if p.Blocked() || len(p.Steps) == 0 {
		t.Errorf("plan must work without root: %+v", p.Failures())
	}
	if w.mutations != 0 {
		t.Errorf("plan mutated the system: %v", w.log)
	}
}

func TestApplyBlockedChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.transport = "usb"
	before := w.snap()
	_, out, err := run(t, w, apply4G)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v", err)
	}
	if d := before.diff(w.snap()); d != "" {
		t.Errorf("changed:\n%s", d)
	}
	if !strings.Contains(out, "USB") || hasKey(w.files, StatePath) {
		t.Errorf("output/state: %s", out)
	}
}

func TestApplyCancelled(t *testing.T) {
	w := newWorld(t)
	before := w.snap()
	m := &Manager{Sys: w.sys(), Out: &bytes.Buffer{}}
	_, err := m.Execute(context.Background(), apply4G, func(Plan) (bool, error) { return false, nil })
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v", err)
	}
	if d := before.diff(w.snap()); d != "" {
		t.Errorf("changed:\n%s", d)
	}
}

func TestApplyUSBWithForce(t *testing.T) {
	w := newWorld(t)
	w.transport = "usb"
	o := apply4G
	o.Force = true
	mustRun(t, w, o)
}

func TestFallocateFallbackToZeros(t *testing.T) {
	w := newWorld(t)
	w.fallocUnsupported = true
	mustRun(t, w, apply4G)
	if f := w.files[ManagedPath]; f == nil || f.size != 4*GiB || !f.swap {
		t.Fatalf("swapfile = %+v", f)
	}
	if !strings.Contains(strings.Join(w.log, "\n"), "zeros "+ManagedPath) {
		t.Errorf("no zeros fallback in %v", w.log)
	}
}

func TestReplaceDeleteOldAndRollback(t *testing.T) {
	w := newWorld(t).withOldSwap(1 * GiB)
	initial := w.snap()
	mustRun(t, w, Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace, DeleteOld: true})

	if hasKey(w.files, "/swap.img") || hasKey(w.active, "/swap.img") {
		t.Error("/swap.img must be off and deleted")
	}
	if !strings.Contains(w.files[FstabPath].data, "# disabled by ramplus: /swap.img\tnone\tswap") {
		t.Errorf("fstab:\n%s", w.files[FstabPath].data)
	}
	backups := 0
	for p := range w.files {
		if strings.HasPrefix(p, BackupDir+"/") && strings.HasSuffix(p, "etc_fstab") {
			backups++
		}
	}
	if backups != 1 {
		t.Errorf("fstab backups = %d", backups)
	}

	mustRun(t, w, rollback)
	if d := initial.diff(w.snap()); d != "" {
		t.Errorf("rollback did not restore the system:\n%s", d)
	}
	st := w.state()
	if st.Managed != nil || st.Operations[0].Status != OpRolledBack || st.Operations[1].Status != OpCommitted {
		t.Errorf("state = %+v", st)
	}
}

func TestRollbackKeepsUserFstabEdits(t *testing.T) {
	w := newWorld(t).withOldSwap(0)
	mustRun(t, w, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace})
	w.files[FstabPath].data += "/dev/sdb1 /data ext4 defaults 0 2\n" // правка пользователя
	mustRun(t, w, rollback)
	fstab := w.files[FstabPath].data
	if !strings.Contains(fstab, "\n/swap.img\tnone\tswap") || strings.Contains(fstab, "disabled by ramplus") || !strings.Contains(fstab, "/data") {
		t.Errorf("fstab:\n%s", fstab)
	}
}

func TestLifecycleRollbackStack(t *testing.T) {
	w := newWorld(t)
	s0 := w.snap()
	mustRun(t, w, apply4G)
	s1 := w.snap()
	mustRun(t, w, resize6G)
	if w.files[ManagedPath].size != 6*GiB || w.active[ManagedPath] != 10 {
		t.Fatalf("resize: %+v %v", w.files[ManagedPath], w.active)
	}
	s2 := w.snap()
	mustRun(t, w, remove)
	if hasKey(w.files, ManagedPath) || len(w.enabled) != 0 || hasKey(w.files, filepath.Join(UnitDir, UnitName(ManagedPath))) {
		t.Fatalf("remove left files: %v %v", w.files, w.enabled)
	}

	for i, want := range []snapshot{s2, s1, s0} {
		mustRun(t, w, rollback)
		if d := want.diff(w.snap()); d != "" {
			t.Fatalf("rollback #%d:\n%s", i+1, d)
		}
	}
	if w.state().Managed != nil {
		t.Error("managed must be nil after full rollback")
	}
	if _, _, err := run(t, w, rollback); !errors.Is(err, ErrBlocked) {
		t.Errorf("4th rollback: want ErrBlocked (nothing to undo), got %v", err)
	}
}

func TestBtrfsApplyAndRemove(t *testing.T) {
	for _, mk := range []bool{true, false} {
		w := newWorld(t)
		w.fsType, w.btrfsTool, w.mkswapfile = "btrfs", true, mk
		s0 := w.snap()
		mustRun(t, w, apply4G)
		f := w.files[BtrfsPath]
		if f == nil || !f.nocow || !f.swap || !hasKey(w.active, BtrfsPath) {
			t.Fatalf("mkswapfile=%v: file %+v active %v", mk, f, w.active)
		}
		mustRun(t, w, remove)
		if hasKey(w.files, BtrfsDir) {
			t.Errorf("mkswapfile=%v: subvolume left", mk)
		}
		mustRun(t, w, rollback) // вернуть remove
		mustRun(t, w, rollback) // вернуть apply
		if d := s0.diff(w.snap()); d != "" {
			t.Errorf("mkswapfile=%v:\n%s", mk, d)
		}
	}
}

// TestFailureAtEveryStep: сбой на каждом изменяющем вызове → система
// возвращается в состояние до операции.
func TestFailureAtEveryStep(t *testing.T) {
	btrfs := func(mk bool) func(*fakeWorld) {
		return func(w *fakeWorld) { w.fsType, w.btrfsTool, w.mkswapfile = "btrfs", true, mk }
	}
	scenarios := []struct {
		name   string
		setup  func(*fakeWorld)
		before []Options
		op     Options
	}{
		{"apply", nil, nil, apply4G},
		{"apply-keep", func(w *fakeWorld) { w.withOldSwap(0) }, nil, apply4G},
		{"apply-replace-delete", func(w *fakeWorld) { w.withOldSwap(1 * GiB) }, nil,
			Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace, DeleteOld: true}},
		{"apply-btrfs-mkswapfile", btrfs(true), nil, apply4G},
		{"apply-btrfs-chattr", btrfs(false), nil, apply4G},
		{"resize", nil, []Options{apply4G}, resize6G},
		{"remove", nil, []Options{apply4G}, remove},
		{"remove-btrfs", btrfs(true), []Options{apply4G}, remove},
		{"rollback-replace", func(w *fakeWorld) { w.withOldSwap(1 * GiB) },
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
			start := ref.mutations
			mustRun(t, ref, sc.op)
			after, total := ref.snap(), ref.mutations-start

			for n := 1; n <= total; n++ {
				w := prepare()
				before := w.snap()
				w.failAt = w.mutations + n
				_, out, err := run(t, w, sc.op)
				if err == nil {
					t.Fatalf("n=%d: injected failure ignored\n%s", n, out)
				}
				got := w.snap()
				if d := before.diff(got); d == "" {
					var se *StepError
					if errors.As(err, &se) {
						if !strings.Contains(err.Error(), "шаг ") || len(se.RollbackErrors) != 0 {
							t.Fatalf("n=%d: %v", n, err)
						}
						ops := w.state().Operations
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
				if d := after.diff(got); d != "" || !strings.Contains(err.Error(), "не удалось записать итог") {
					t.Fatalf("n=%d (%s): system not restored: %v\n%s", n, w.log[len(w.log)-1], err, before.diff(got))
				}
				w.failAt = 0
				mustRun(t, w, rollback)
				if d := before.diff(w.snap()); d != "" {
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
	w.files[ManagedPath] = &fakeFile{data: "чужие данные", mode: 0o644}
	r := &Runner{Sys: w.sys()}
	_, err := r.Run(context.Background(), []Step{{Kind: KindAllocate, Path: ManagedPath, Size: GiB}})
	if err == nil || w.files[ManagedPath] == nil || w.files[ManagedPath].data != "чужие данные" {
		t.Fatalf("foreign file damaged: err=%v file=%+v", err, w.files[ManagedPath])
	}
}

func TestStepErrorMessage(t *testing.T) {
	w := newWorld(t)
	r := &Runner{Sys: w.sys()}
	// mkswap на несуществующем файле падает вторым шагом; первый откатывается.
	_, err := r.Run(context.Background(), []Step{
		{Kind: KindMkdir, Path: "/x"},
		{Kind: KindMkswap, Path: "/nope"},
	})
	var se *StepError
	if !errors.As(err, &se) || se.Index != 2 || se.Total != 2 || hasKey(w.files, "/x") {
		t.Fatalf("err = %v, files = %v", err, w.files["/x"])
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
			crashedAt = c.at
		}
	}()
	w.crash = true
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
		{"apply-replace-delete", func(w *fakeWorld) { w.withOldSwap(1 * GiB) }, nil,
			Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace, DeleteOld: true}},
		{"apply-btrfs", func(w *fakeWorld) { w.fsType, w.btrfsTool = "btrfs", true }, nil, apply4G},
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
			start := ref.mutations
			mustRun(t, ref, sc.op)
			total := ref.mutations - start

			recovered := 0
			for n := 1; n <= total; n++ {
				w := prepare()
				before := w.snap()
				w.failAt = w.mutations + n
				at := runCrash(t, w, sc.op)
				if at == "" {
					t.Fatalf("n=%d: no crash", n)
				}
				w.failAt, w.crash = 0, false

				if w.state().Pending() == nil {
					// Упали до записи операции в журнал — система не тронута.
					if d := before.diff(w.snap()); d != "" {
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
				if d := before.diff(w.snap()); d != "" {
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
	for i, op := range ref.log {
		if op == "fallocate "+ManagedPath {
			for j := i + 1; j < len(ref.log); j++ {
				if ref.log[j] == "write "+StatePath {
					idx = j + 1 // mutations считаются с 1
					break
				}
			}
			break
		}
	}
	if idx < 0 {
		t.Fatalf("log: %v", ref.log)
	}

	w := newWorld(t)
	before := w.snap()
	w.failAt = idx
	if at := runCrash(t, w, apply4G); at != "write "+StatePath {
		t.Fatalf("crashed at %q", at)
	}
	w.failAt, w.crash = 0, false
	if f := w.files[ManagedPath]; f == nil || f.size != 4*GiB {
		t.Fatalf("step must have happened: %+v", f)
	}
	op := w.state().Pending()
	last := op.Applied[len(op.Applied)-1]
	if last.Status != StepPending || last.Step.Kind != KindAllocate {
		t.Fatalf("journal: last entry %+v", last)
	}

	f := Gather(context.Background(), w.sys(), "")
	if f.Recovery == nil || f.Recovery.Verdict != VerdictPresent {
		t.Fatalf("recovery = %+v", f.Recovery)
	}
	p := BuildPlan(f, rollback)
	if c, _ := findCheck(p, "прерванный шаг"); !strings.Contains(c.Message, "будет откачен") {
		t.Errorf("plan must explain: %+v", p.Checks)
	}
	mustRun(t, w, rollback)
	if d := before.diff(w.snap()); d != "" {
		t.Errorf("not restored:\n%s", d)
	}
}

// Артефакт подменён после падения → RamPlus его не трогает.
func TestCrashArtifactMismatch(t *testing.T) {
	w := newWorld(t)
	ref := newWorld(t)
	mustRun(t, ref, apply4G)
	n := 0
	for i, op := range ref.log {
		if op == "create "+ManagedPath {
			n = i + 1 // упасть на самом create: pending записан, файла ещё нет
		}
	}
	w.failAt = n
	runCrash(t, w, apply4G)
	w.failAt, w.crash = 0, false
	// Кто-то положил на это место свой файл.
	w.files[ManagedPath] = &fakeFile{data: "важные данные", mode: 0o644}
	_, out, err := run(t, w, rollback)
	if !errors.Is(err, ErrBlocked) || !strings.Contains(out, "ручная проверка") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if w.files[ManagedPath].data != "важные данные" {
		t.Error("foreign file touched")
	}
}

func TestApplyCustomPathBtrfs(t *testing.T) {
	w := newWorld(t)
	w.mounts["/mnt/data"] = fakeMount{fsType: "btrfs", transport: "nvme", total: 1000 * GiB, avail: 800 * GiB}
	w.files["/mnt"] = &fakeFile{dir: true, mode: 0o755}
	w.files["/mnt/data"] = &fakeFile{dir: true, mode: 0o755}
	w.links["/mnt/link"] = "/mnt/data"
	w.btrfsTool = true
	s0 := w.snap()

	o := Options{Action: ActionApply, Size: 4 * GiB, Path: "/mnt/link/ramplus.swapfile"}
	p, out, err := run(t, w, o)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	const path = "/mnt/data/ramplus.swapfile"
	if p.Path != path {
		t.Errorf("symlink must be resolved: %s", p.Path)
	}
	if f := w.files[path]; f == nil || !f.nocow || !f.swap || !hasKey(w.active, path) {
		t.Fatalf("swapfile = %+v", f)
	}
	if !w.enabled["mnt-data-ramplus.swapfile.swap"] {
		t.Errorf("enabled = %v", w.enabled)
	}
	if !strings.Contains(out, "снапшоты подтома") {
		t.Error("custom path on btrfs must warn about snapshots")
	}
	mustRun(t, w, Options{Action: ActionResize, Size: 6 * GiB}) // NoCOW сохраняется при пересоздании
	mustRun(t, w, rollback)
	mustRun(t, w, rollback)
	if d := s0.diff(w.snap()); d != "" {
		t.Errorf("not restored:\n%s", d)
	}
}

func TestApplyRefusesForeignFS(t *testing.T) {
	w := newWorld(t)
	w.mounts["/mnt/win"] = fakeMount{fsType: "ntfs3", transport: "nvme", total: 900 * GiB, avail: 400 * GiB}
	w.files["/mnt"] = &fakeFile{dir: true}
	w.files["/mnt/win"] = &fakeFile{dir: true}
	_, out, err := run(t, w, Options{Action: ActionApply, Size: 4 * GiB, Path: "/mnt/win/ramplus.swapfile"})
	if !errors.Is(err, ErrBlocked) || !strings.Contains(out, "создайте раздел ext4/xfs") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}
