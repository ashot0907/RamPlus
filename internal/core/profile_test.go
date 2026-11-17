package core

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/profile"
)

func profWorld(t *testing.T, withZpool bool) *fakeWorld {
	return newWorld(t).WithKernelParams(withZpool)
}

func req(name string, set ...string) ProfileRequest {
	m, err := profile.ParseSet(set)
	if err != nil {
		panic(err)
	}
	return ProfileRequest{Name: name, Set: m}
}

func planProfile(t *testing.T, w *fakeWorld, r ProfileRequest) Plan {
	t.Helper()
	return PlanProfile(context.Background(), w.System(), r)
}

func runProfile(t *testing.T, w *fakeWorld, r ProfileRequest) (Plan, string, error) {
	t.Helper()
	var out bytes.Buffer
	m := &Manager{Sys: w.System(), Out: &out}
	p, err := m.ExecuteFunc(context.Background(), func(ctx context.Context) Plan { return PlanProfile(ctx, w.System(), r) }, yes)
	return p, out.String(), err
}

func mustProfile(t *testing.T, w *fakeWorld, r ProfileRequest) Plan {
	t.Helper()
	p, out, err := runProfile(t, w, r)
	if err != nil {
		t.Fatalf("profile %s: %v\n%s", r.Name, err, out)
	}
	return p
}

func runReset(t *testing.T, w *fakeWorld) (string, error) {
	t.Helper()
	var out bytes.Buffer
	m := &Manager{Sys: w.System(), Out: &out}
	_, err := m.ExecuteFunc(context.Background(), func(ctx context.Context) Plan { return PlanProfileReset(ctx, w.System()) }, yes)
	return out.String(), err
}

func val(w *fakeWorld, key string) string {
	p, _ := profile.Lookup(key)
	if f, ok := w.Files[p.Path]; ok {
		return strings.TrimSpace(f.Data)
	}
	return ""
}

func rowsByKey(p Plan) map[string]ProfileRow {
	out := map[string]ProfileRow{}
	for _, r := range p.Profile.Rows {
		out[r.Key] = r
	}
	return out
}

// Ядро 7.0 (нет параметра zpool) — как на этой машине.
func TestProfilePlanZswapLz4Kernel70(t *testing.T) {
	w := profWorld(t, false)
	p := planProfile(t, w, req("zswap-lz4"))
	if p.Blocked() {
		t.Fatalf("blocked: %+v", p.Failures())
	}
	if w.Mutations != 0 {
		t.Fatalf("plan mutated: %v", w.Log)
	}
	want := "set_value,set_value,write_file,write_file,systemctl,systemctl"
	if got := kinds(p.Steps); got != want {
		t.Errorf("steps = %s", got)
	}
	if p.Steps[0].Content != "lz4" || p.Steps[1].Path != profile.ZswapParamDir+"/enabled" || p.Steps[1].Content != "Y" {
		t.Errorf("compressor first, enabled last: %+v", p.Steps[:2])
	}
	rows := rowsByKey(p)
	if r := rows["zswap.zpool"]; r.Skip == "" || r.Change {
		t.Errorf("zpool row = %+v", r)
	}
	if r := rows["zswap.compressor"]; !r.Change || r.Current != "lzo" || r.Source != "ядро по умолчанию" {
		t.Errorf("compressor row = %+v", r)
	}
	if r := rows["vm.swappiness"]; r.Change || r.Current != "60" {
		t.Errorf("swappiness row = %+v", r)
	}
	wantLevel(t, p, "статус", LevelWarn)
	wantLevel(t, p, "zswap.zpool", LevelInfo)
	if !p.Profile.Experimental || p.ProfileAfter.Values["zswap.zpool"] != "" || p.ProfileAfter.Status != profile.StatusExperimental {
		t.Errorf("profile after = %+v", p.ProfileAfter)
	}
	out := render_(t, p, false)
	for _, want := range []string{"без изменений: zswap.max_pool_percent=20, vm.swappiness=60", "zswap.compressor", "kernel cmdline и GRUB: не меняются"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}

func TestProfileApplyIdempotentRollback(t *testing.T) {
	w := profWorld(t, false)
	s0 := w.Snap()
	mustProfile(t, w, req("zswap-lz4"))
	if val(w, "zswap.enabled") != "Y" || val(w, "zswap.compressor") != "lz4" {
		t.Fatalf("runtime: enabled=%s compressor=%s", val(w, "zswap.enabled"), val(w, "zswap.compressor"))
	}
	sc, unit := w.Files[profile.SysctlFilePath], w.Files[profile.ZswapUnitPath]
	if sc == nil || !strings.HasPrefix(sc.Data, profile.Marker) || unit == nil || !strings.Contains(unit.Data, "echo lz4") || !w.Enabled[profile.ZswapUnitName] {
		t.Fatalf("persistence: %+v %+v %v", sc, unit, w.Enabled)
	}
	st := stateOf(t, w)
	if profile.ProfileOf(st) == nil || profile.ProfileOf(st).Name != "zswap-lz4" || profile.BaselineOf(st) == nil || profile.BaselineOf(st).Values["zswap.compressor"] != "lzo" {
		t.Fatalf("state: %+v %+v", profile.ProfileOf(st), profile.BaselineOf(st))
	}
	// Повторный apply того же профиля — пустой план.
	p := planProfile(t, w, req("zswap-lz4"))
	if len(p.Steps) != 0 || p.Blocked() {
		t.Errorf("re-apply: steps=%s checks=%+v", kinds(p.Steps), p.Failures())
	}
	if _, out, err := runProfile(t, w, req("zswap-lz4")); err != nil || !strings.Contains(out, "Нечего делать") {
		t.Errorf("re-apply: %v\n%s", err, out)
	}
	// Rollback возвращает всё, первый apply забывает исходные значения.
	mustRun(t, w, rollback)
	if d := s0.Diff(w.Snap()); d != "" {
		t.Fatalf("rollback:\n%s", d)
	}
	if st := stateOf(t, w); profile.ProfileOf(st) != nil || profile.BaselineOf(st) != nil {
		t.Errorf("state after rollback: %+v %+v", profile.ProfileOf(st), profile.BaselineOf(st))
	}
}

func TestProfileReset(t *testing.T) {
	w := profWorld(t, false)
	s0 := w.Snap()
	if _, err := runReset(t, w); !errors.Is(err, ErrBlocked) {
		t.Errorf("reset without profile: %v", err)
	}
	mustProfile(t, w, req("zswap-lz4"))
	mustProfile(t, w, req("default", "vm.swappiness=80"))
	if val(w, "zswap.enabled") != "N" || val(w, "vm.swappiness") != "80" {
		t.Fatalf("default+set: enabled=%s swappiness=%s", val(w, "zswap.enabled"), val(w, "vm.swappiness"))
	}
	// Исходные значения сняты при первом apply и не перезаписаны вторым.
	if b := profile.BaselineOf(stateOf(t, w)); b.Values["vm.swappiness"] != "60" || b.Values["zswap.compressor"] != "lzo" {
		t.Errorf("baseline overwritten: %+v", b)
	}
	if out, err := runReset(t, w); err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}
	if d := s0.Diff(w.Snap()); d != "" {
		t.Fatalf("reset must restore the original system:\n%s", d)
	}
	if st := stateOf(t, w); profile.ProfileOf(st) != nil || profile.BaselineOf(st) == nil {
		t.Errorf("after reset: profile=%+v baseline=%v", profile.ProfileOf(st), profile.BaselineOf(st) != nil)
	}
	// reset тоже откатывается.
	mustRun(t, w, rollback)
	if val(w, "vm.swappiness") != "80" || profile.ProfileOf(stateOf(t, w)).Name != "default" {
		t.Error("rollback of reset must restore default+set")
	}
}

func TestProfileSetModified(t *testing.T) {
	w := profWorld(t, false)
	p := mustProfile(t, w, req("default", "vm.swappiness=80"))
	if p.ProfileAfter.Status != profile.StatusModified || p.ProfileAfter.BaseStatus != profile.StatusStandard ||
		p.ProfileAfter.Overrides["vm.swappiness"] != "80" {
		t.Errorf("profile after = %+v", p.ProfileAfter)
	}
	if !strings.Contains(w.Files[profile.SysctlFilePath].Data, "vm.swappiness = 80") ||
		!strings.Contains(w.Files[profile.SysctlFilePath].Data, "изменённый пользователем") {
		t.Errorf("sysctl file:\n%s", w.Files[profile.SysctlFilePath].Data)
	}
	var st monitor.Status
	AnnotateStatus(&st, w.System())
	if st.Profile == nil || st.Profile.Status != profile.StatusModified || len(st.Drift) != 0 {
		t.Errorf("status = %+v drift=%v", st.Profile, st.Drift)
	}
	// --set, совпадающий с профилем, — не изменение.
	p = planProfile(t, profWorld(t, false), req("default", "vm.swappiness=60"))
	if p.Profile.Status != profile.StatusStandard {
		t.Errorf("same value is not a modification: %s", p.Profile.Status)
	}
}

func TestProfilePreflight(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *fakeWorld)
		r     ProfileRequest
		title string
		level Level
		text  string
	}{
		{"compressor unavailable", nil, req("zswap-lz4", "zswap.compressor=842"), "компрессор", LevelFail, "842 недоступен"},
		{"zbud without zpool param (7.0)", nil, req("zswap-lz4", "zswap.zpool=zbud"), "zswap.zpool", LevelFail, "нет в этом ядре"},
		{"zpool param, zbud unavailable", func(w *fakeWorld) {
			w.Files[profile.ZswapParamDir+"/zpool"] = &fakeFile{Data: "zsmalloc\n"}
		}, req("zswap-lz4", "zswap.zpool=zbud"), "zpool", LevelFail, "zbud недоступен"},
		{"no zswap in kernel", func(w *fakeWorld) {
			for p := range w.Files {
				if strings.HasPrefix(p, "/sys/module/zswap") {
					delete(w.Files, p)
				}
			}
		}, req("zswap-lz4"), "zswap", LevelFail, "zswap нет в этом ядре"},
		{"zram active", func(w *fakeWorld) {
			w.Files["/dev/zram0"] = &fakeFile{Size: 4 * GiB, Swap: true}
			w.Active["/dev/zram0"] = 100
		}, req("zswap-lz4"), "zram", LevelFail, "zswap поверх zram бессмыслен"},
		{"zram generator", func(w *fakeWorld) {
			w.Files["/usr/lib/systemd/system-generators/zram-generator"] = &fakeFile{Data: "x"}
		}, req("zswap-zstd"), "zram", LevelFail, "zram-generator"},
		{"zram + --force", func(w *fakeWorld) {
			w.Files["/etc/systemd/zram-generator.conf"] = &fakeFile{Data: "[zram0]\n"}
		}, ProfileRequest{Name: "zswap-lz4", Force: true}, "zram", LevelWarn, "--force"},
		{"zswap.* in cmdline", func(w *fakeWorld) {
			w.Cmdline += " zswap.enabled=1 zswap.compressor=zstd"
		}, req("zswap-lz4"), "cmdline", LevelWarn, "перезаписывает значения профиля"},
		{"sysctl.conf overrides (after 99-ramplus.conf)", func(w *fakeWorld) {
			w.Files["/etc/sysctl.conf"].Data = "vm.swappiness = 10\n"
			w.Files["/etc/sysctl.d/99-sysctl.conf"].Data = "vm.swappiness = 10\n"
		}, req("default"), "sysctl", LevelWarn, "/etc/sysctl.d/99-sysctl.conf задаёт vm.swappiness=10 и применяется после 99-ramplus.conf"},
		{"unknown profile", nil, ProfileRequest{Name: "turbo"}, "профиль", LevelFail, "default, zswap-lz4, zswap-zstd"},
	}
	for _, tt := range tests {
		w := profWorld(t, false)
		if tt.setup != nil {
			tt.setup(w)
		}
		p := planProfile(t, w, tt.r)
		c := wantLevel(t, p, tt.title, tt.level)
		if !strings.Contains(c.Message, tt.text) {
			t.Errorf("%s: %q lacks %q", tt.name, c.Message, tt.text)
		}
	}
}

func TestProfileZpoolKernelWithParam(t *testing.T) {
	w := profWorld(t, true) // ядро 6.8: zpool есть, по умолчанию zbud
	mustProfile(t, w, req("zswap-zstd"))
	if val(w, "zswap.zpool") != "zsmalloc" || val(w, "zswap.compressor") != "zstd" {
		t.Errorf("zpool=%s compressor=%s", val(w, "zswap.zpool"), val(w, "zswap.compressor"))
	}
	if !strings.Contains(w.Files[profile.ZswapUnitPath].Data, "echo zsmalloc > /sys/module/zswap/parameters/zpool") {
		t.Errorf("unit:\n%s", w.Files[profile.ZswapUnitPath].Data)
	}
}

func TestProfileSysctlSources(t *testing.T) {
	w := profWorld(t, false)
	w.Files["/usr/lib/sysctl.d/30-tune.conf"] = &fakeFile{Data: "vm.page-cluster = 0\n"}
	w.Files["/proc/sys/vm/page-cluster"].Data = "0\n"
	w.Files["/proc/sys/vm/vfs_cache_pressure"].Data = "50\n" // руками
	p := planProfile(t, w, req("default"))
	rows := rowsByKey(p)
	if rows["vm.page-cluster"].Source != "/usr/lib/sysctl.d/30-tune.conf" || !rows["vm.page-cluster"].Change {
		t.Errorf("page-cluster = %+v", rows["vm.page-cluster"])
	}
	if rows["vm.vfs_cache_pressure"].Source != "ядро по умолчанию" {
		t.Errorf("vfs = %+v", rows["vm.vfs_cache_pressure"])
	}
	if !strings.Contains(p.Profile.SysctlOrder, "/usr/lib/sysctl.d/30-tune.conf (vm.page-cluster=0) → 99-ramplus.conf") {
		t.Errorf("order = %q", p.Profile.SysctlOrder)
	}
	if _, bad := findCheck(p, "sysctl"); bad {
		t.Error("30-tune.conf goes before ours — no warning")
	}
	mustProfile(t, w, req("default"))
	if profile.ProfileOf(stateOf(t, w)) == nil {
		t.Fatal("profile not recorded")
	}
	pf := profile.Gather(context.Background(), w.System(), nil)
	if s := pf.SourceOf("vm.swappiness"); s != "RamPlus (99-ramplus.conf)" {
		t.Errorf("source after apply = %q", s)
	}
	if s := pf.SourceOf("zswap.enabled"); s != "RamPlus (ramplus-zswap.service)" {
		t.Errorf("zswap source after apply = %q", s)
	}
}

func TestProfileDriftAndRollbackMismatch(t *testing.T) {
	w := profWorld(t, false)
	mustProfile(t, w, req("zswap-lz4"))
	w.Files["/proc/sys/vm/swappiness"].Data = "80\n" // руками
	var st monitor.Status
	AnnotateStatus(&st, w.System())
	if len(st.Drift) != 1 || st.Drift[0].Key != "vm.swappiness" || st.Drift[0].Expected != "60" || st.Drift[0].Actual != "80" {
		t.Errorf("drift = %+v", st.Drift)
	}
	// Ручная смена того, что профиль менял, блокирует rollback.
	w.Files[profile.ZswapParamDir+"/compressor"].Data = "lzo\n"
	before := w.Snap()
	_, out, err := run(t, w, rollback)
	if !errors.Is(err, ErrBlocked) || !strings.Contains(out, "zswap.compressor = lzo, по журналу lz4") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if d := before.Diff(w.Snap()); d != "" {
		t.Errorf("system changed:\n%s", d)
	}
}

// Общий стек: swap apply → profile apply → rollback (профиль) → rollback (swap).
func TestProfileSharedRollbackStack(t *testing.T) {
	w := profWorld(t, false)
	s0 := w.Snap()
	mustRun(t, w, apply4G)
	s1 := w.Snap()
	mustProfile(t, w, req("zswap-lz4"))
	if st := stateOf(t, w); st.Managed == nil || profile.ProfileOf(st) == nil {
		t.Fatalf("state: %+v %+v", st.Managed, profile.ProfileOf(st))
	}
	mustRun(t, w, rollback)
	if d := s1.Diff(w.Snap()); d != "" {
		t.Fatalf("first rollback must undo the profile:\n%s", d)
	}
	if st := stateOf(t, w); st.Managed == nil || profile.ProfileOf(st) != nil {
		t.Errorf("state after profile rollback: managed=%v profile=%v", st.Managed, profile.ProfileOf(st))
	}
	mustRun(t, w, rollback)
	if d := s0.Diff(w.Snap()); d != "" {
		t.Fatalf("second rollback must undo swap:\n%s", d)
	}
}

// Сбой на каждом изменяющем вызове профиля и reset → система как была.
func TestProfileFailureAtEveryStep(t *testing.T) {
	scenarios := []struct {
		name   string
		before func(w *fakeWorld)
		op     func(w *fakeWorld) error
	}{
		{"apply", nil, func(w *fakeWorld) error {
			_, _, err := runProfile(t, w, req("zswap-lz4", "vm.swappiness=100"))
			return err
		}},
		{"apply-over-profile", func(w *fakeWorld) { mustProfile(t, w, req("zswap-lz4")) },
			func(w *fakeWorld) error { _, _, err := runProfile(t, w, req("default")); return err }},
		{"reset", func(w *fakeWorld) { mustProfile(t, w, req("zswap-zstd", "vm.page-cluster=0")) },
			func(w *fakeWorld) error { _, err := runReset(t, w); return err }},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			prepare := func() *fakeWorld {
				w := profWorld(t, false)
				if sc.before != nil {
					sc.before(w)
				}
				return w
			}
			ref := prepare()
			start := ref.Mutations
			if err := sc.op(ref); err != nil {
				t.Fatal(err)
			}
			after, total := ref.Snap(), ref.Mutations-start
			for n := 1; n <= total; n++ {
				w := prepare()
				before := w.Snap()
				w.FailAt = w.Mutations + n
				err := sc.op(w)
				if err == nil {
					t.Fatalf("n=%d: failure ignored", n)
				}
				got := w.Snap()
				if before.Diff(got) == "" {
					continue
				}
				if after.Diff(got) != "" || !strings.Contains(err.Error(), "не удалось записать итог") {
					t.Fatalf("n=%d (%s): not restored: %v\n%s", n, w.Log[len(w.Log)-1], err, before.Diff(got))
				}
			}
			if total < 5 {
				t.Errorf("too few mutations: %d", total)
			}
		})
	}
}

func TestProfileCheckClean(t *testing.T) {
	w := profWorld(t, false)
	mustProfile(t, w, req("default"))
	p := strings.Join(cleanOf(w), "\n")
	for _, want := range []string{"применён профиль RamPlus default", "существует " + profile.SysctlFilePath, "существует " + profile.ZswapUnitPath} {
		if !strings.Contains(p, want) {
			t.Errorf("lacks %q:\n%s", want, p)
		}
	}
	if _, err := runReset(t, w); err != nil {
		t.Fatal(err)
	}
	// После reset журнал помнит baseline, но следов нет.
	if p := cleanOf(w); len(p) != 0 {
		t.Errorf("after reset: %v", p)
	}
}

func TestProfileResetTwiceIsEmpty(t *testing.T) {
	w := profWorld(t, false)
	mustProfile(t, w, req("zswap-lz4"))
	if _, err := runReset(t, w); err != nil {
		t.Fatal(err)
	}
	out, err := runReset(t, w)
	if err != nil || !strings.Contains(out, "Нечего делать") {
		t.Errorf("second reset: %v\n%s", err, out)
	}
}
