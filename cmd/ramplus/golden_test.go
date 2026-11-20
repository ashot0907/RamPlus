package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/fakesys"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/monitor"
)

// Golden-тесты фиксируют текстовый вывод CLI (plan/apply/rollback/status
// и др.) на фейковой системе. Обновить эталоны: go test ./cmd/ramplus -run Golden -update
var update = flag.Bool("update", false, "перезаписать эталоны testdata/golden")

// opID — ID операции (наносекунды + случайный суффикс): нормализуется.
var opID = regexp.MustCompile(`\d{8}T\d{6}\.\d{9}Z-[0-9a-f]{6}(\.\d+)?`)

// planID и отпечаток плана (зависит от ID операций в журнале) — тоже.
var (
	planID      = regexp.MustCompile(`"id": "[0-9a-f]{16}"`)
	fingerprint = regexp.MustCompile(`"fingerprint": "[0-9a-f]{32}"`)
)

type session struct {
	t   *testing.T
	w   *fakesys.World
	out strings.Builder
}

func newSession(t *testing.T, w *fakesys.World) *session {
	t.Helper()
	oldSys, oldCollect, oldLocal, oldOrigin := newSystem, collect, time.Local, cliOrigin
	t.Cleanup(func() { newSystem, collect, time.Local, cliOrigin = oldSys, oldCollect, oldLocal, oldOrigin })
	cliOrigin = func() journal.Origin {
		return journal.Origin{Source: journal.SourceCLI, RequestedBy: &journal.Requester{UID: 1000, User: "golden"}}
	}
	time.Local = time.UTC
	newSystem = w.System
	collect = func() monitor.Status { return goldenStatus(w) }
	return &session{t: t, w: w}
}

// goldenStatus — снимок из фикстур monitor; профиль, дрейф и managed
// добавляет core.Status по фейковой системе.
func goldenStatus(w *fakesys.World) monitor.Status {
	td := func(p ...string) string {
		return filepath.Join(append([]string{"..", "..", "internal", "monitor", "testdata"}, p...)...)
	}
	st := monitor.CollectFrom(monitor.Paths{
		Meminfo:     td("meminfo", "normal"),
		Swaps:       td("swaps", "two-devices"),
		ZswapParams: td("zswap", "params-7.0"),
		ZswapDebug:  td("absent"),
		VMSysctl:    td("vm"),
		PSIMemory:   td("psi", "memory"),
		SysBlock:    td("sysblock"),
	})
	st.CollectedAt = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	st.System = detect.System{Hostname: "golden", Kernel: fakesys.Release, Arch: "x86_64",
		OS: &detect.OSRelease{ID: "ubuntu", Name: "Ubuntu", PrettyName: "Ubuntu 24.04.5 LTS", VersionID: "24.04"}}
	st.RootDisk = w.DescribeFS("/")
	return st
}

// run выполняет команду CLI и дописывает её вывод в сессию.
func (s *session) run(args ...string) { s.t.Helper(); s.runIn("", args...) }

// runIn — run со stdin.
func (s *session) runIn(stdin string, args ...string) {
	s.t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	s.out.WriteString("$ ramplus " + strings.Join(args, " ") + "\n")
	s.out.WriteString(out.String())
	if err != nil {
		s.out.WriteString("error: " + err.Error() + "\n")
	}
	s.out.WriteString("\n")
}

func (s *session) check(name string) {
	s.t.Helper()
	got := opID.ReplaceAllString(s.out.String(), "<OPID>")
	got = planID.ReplaceAllString(got, `"id": "<PLANID>"`)
	got = fingerprint.ReplaceAllString(got, `"fingerprint": "<FP>"`)
	path := filepath.Join("testdata", "golden", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			s.t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatalf("нет эталона %s (go test -run Golden -update): %v", path, err)
	}
	if got != string(want) {
		s.t.Errorf("вывод отличается от %s:\n%s", path, firstDiff(string(want), got))
	}
}

// firstDiff — первая отличающаяся строка с контекстом.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < max(len(w), len(g)); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return "строка " + strconv.Itoa(i+1) + ":\n  эталон: " + a + "\n  сейчас: " + b
		}
	}
	return "(разная длина)"
}

func TestGoldenSwapKeep(t *testing.T) {
	s := newSession(t, fakesys.New().WithOldSwap(0))
	s.run("swap", "plan", "--size", "4G")
	s.run("swap", "apply", "--size", "4G", "--yes")
	s.run("swap", "plan", "--size", "4G")
	s.run("swap", "check-clean")
	s.run("rollback", "--dry-run")
	s.run("rollback", "--yes")
	s.run("swap", "check-clean")
	s.check("swap_keep")
}

func TestGoldenSwapReplaceDeleteOld(t *testing.T) {
	s := newSession(t, fakesys.New().WithOldSwap(1*fakesys.GiB))
	s.run("swap", "plan", "--size", "12G", "--existing=replace", "--delete-old", "--verbose")
	s.run("swap", "apply", "--size", "12G", "--existing=replace", "--delete-old", "--yes")
	s.run("rollback", "--yes")
	s.check("swap_replace")
}

func TestGoldenSwapLifecycle(t *testing.T) {
	s := newSession(t, fakesys.New())
	s.run("swap", "apply", "--size", "4G", "--yes")
	s.run("swap", "resize", "--size", "6G", "--dry-run")
	s.run("swap", "resize", "--size", "6G", "--yes")
	s.run("swap", "remove", "--yes")
	s.run("rollback", "--yes")
	s.run("rollback", "--yes")
	s.run("rollback", "--yes")
	s.run("rollback", "--yes")
	s.check("swap_lifecycle")
}

func TestGoldenSwapBlocked(t *testing.T) {
	w := fakesys.New()
	w.Transport = "usb"
	w.Avail = 20 * fakesys.GiB
	s := newSession(t, w)
	s.run("swap", "plan", "--size", "12G")
	s.run("swap", "apply", "--size", "12G", "--yes")
	s.run("swap", "plan", "--size", "4G", "--path", "/tmp/x.swapfile")
	s.check("swap_blocked")
}

func TestGoldenNotRoot(t *testing.T) {
	w := fakesys.New()
	w.EUID = 1000
	s := newSession(t, w)
	s.run("swap", "apply", "--size", "4G", "--yes")
	s.run("rollback", "--yes")
	s.run("profile", "reset", "--yes")
	s.check("not_root")
}

func TestGoldenSwapPathBtrfs(t *testing.T) {
	w := fakesys.New()
	w.Mounts["/mnt/data"] = fakesys.Mount{FSType: "btrfs", Transport: "nvme", Total: 1000 * fakesys.GiB, Avail: 800 * fakesys.GiB}
	w.Files["/mnt"] = &fakesys.File{Dir: true, Mode: 0o755}
	w.Files["/mnt/data"] = &fakesys.File{Dir: true, Mode: 0o755}
	w.BtrfsTool = true
	s := newSession(t, w)
	s.run("swap", "plan", "--size", "4G", "--path", "/mnt/data/ramplus.swapfile", "--verbose")
	s.run("swap", "apply", "--size", "4G", "--path", "/mnt/data/ramplus.swapfile", "--yes")
	s.run("swap", "remove", "--dry-run")
	s.check("swap_path_btrfs")
}

// Журнал говорит «отменить apply», а файл уже 6G: rollback отказывает.
func TestGoldenRollbackMismatch(t *testing.T) {
	w := fakesys.New()
	s := newSession(t, w)
	s.run("swap", "apply", "--size", "4G", "--yes")
	s.run("swap", "resize", "--size", "6G", "--yes")
	st, err := journal.ParseState([]byte(w.Files[journal.StatePath].Data))
	if err != nil {
		t.Fatal(err)
	}
	st.Operations[1].Status = journal.OpRolledBack
	b, _ := st.Marshal()
	w.Files[journal.StatePath].Data = string(b)
	s.run("rollback", "--yes")
	s.check("rollback_mismatch")
}

func TestGoldenProfile(t *testing.T) {
	s := newSession(t, fakesys.New().WithKernelParams(false))
	s.run("profile", "list")
	s.run("profile", "show", "zswap-lz4")
	s.run("profile", "plan", "zswap-lz4", "--verbose")
	s.run("profile", "apply", "zswap-lz4", "--yes")
	s.run("profile", "apply", "zswap-lz4", "--experimental", "--yes")
	s.run("profile", "current")
	s.run("profile", "plan", "zswap-lz4")
	s.run("profile", "apply", "default", "--set", "vm.swappiness=80", "--yes")
	s.run("profile", "list")
	s.run("profile", "reset", "--dry-run")
	s.run("profile", "reset", "--yes")
	s.run("profile", "reset", "--yes")
	s.run("rollback", "--dry-run")
	s.run("rollback", "--yes")
	s.run("profile", "current")
	s.check("profile")
}

func TestGoldenProfilePreflight(t *testing.T) {
	w := fakesys.New().WithKernelParams(true)
	w.Cmdline += " zswap.enabled=1"
	w.Files["/etc/sysctl.conf"].Data = "vm.swappiness = 10\n"
	w.Files["/etc/sysctl.d/99-sysctl.conf"].Data = "vm.swappiness = 10\n"
	w.Files["/usr/lib/sysctl.d/30-tune.conf"] = &fakesys.File{Data: "vm.page-cluster = 0\n"}
	w.Files["/etc/systemd/zram-generator.conf"] = &fakesys.File{Data: "[zram0]\n"}
	s := newSession(t, w)
	s.run("profile", "plan", "zswap-zstd", "--set", "zswap.compressor=842")
	s.run("profile", "plan", "zswap-zstd", "--force")
	s.run("profile", "plan", "default", "--set", "vm.swappiness=500")
	s.check("profile_preflight")
}

func TestGoldenStatus(t *testing.T) {
	w := fakesys.New().WithKernelParams(false)
	s := newSession(t, w)
	s.run("status")
	s.run("profile", "apply", "zswap-lz4", "--experimental", "--yes")
	w.Files["/proc/sys/vm/swappiness"].Data = "133\n"
	s.run("status")
	s.run("status", "--json")
	s.check("status")
}

// --json: план и итог выполнения как данные (схема для API демона).
func TestGoldenJSON(t *testing.T) {
	w := fakesys.New().WithOldSwap(0).WithKernelParams(false)
	s := newSession(t, w)
	s.run("swap", "plan", "--size", "4G", "--json")
	s.run("swap", "apply", "--size", "4G", "--json")
	s.run("swap", "apply", "--size", "4G", "--json", "--yes")
	s.run("swap", "resize", "--size", "4G", "--json", "--yes")
	s.run("profile", "plan", "zswap-lz4", "--json")
	s.run("profile", "apply", "zswap-lz4", "--json", "--yes")
	s.run("profile", "apply", "zswap-lz4", "--json", "--yes", "--experimental")
	s.run("rollback", "--dry-run", "--json")
	s.run("rollback", "--json", "--yes")
	s.run("profile", "reset", "--dry-run", "--json")
	s.check("json")
}

func TestGoldenHistory(t *testing.T) {
	w := fakesys.New().WithKernelParams(false)
	s := newSession(t, w)
	s.run("history")
	s.run("swap", "apply", "--size", "4G", "--yes")
	s.run("profile", "apply", "zswap-lz4", "--experimental", "--yes")
	s.run("swap", "resize", "--size", "6G", "--yes")
	s.run("rollback", "--yes")
	w.FailAt = w.Mutations + 5
	s.run("swap", "remove", "--yes")
	w.FailAt = 0
	s.run("history")
	s.run("history", "--json")
	s.check("history")
}

// Rollback, когда неочевидно, что откатывается: последней была swap-операция
// (уже отменена), а rollback дойдёт до профиля двухдневной давности.
func TestGoldenRollbackContext(t *testing.T) {
	w := fakesys.New().WithKernelParams(false)
	s := newSession(t, w)
	s.run("profile", "apply", "zswap-lz4", "--experimental", "--yes")
	st, err := journal.ParseState([]byte(w.Files[journal.StatePath].Data))
	if err != nil {
		t.Fatal(err)
	}
	st.Operations[0].StartedAt = time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	b, _ := st.Marshal()
	w.Files[journal.StatePath].Data = string(b)
	s.run("swap", "apply", "--size", "4G", "--yes")
	s.run("rollback", "--yes")
	s.run("rollback", "--dry-run")
	s.run("rollback", "--dry-run", "--json")
	s.check("rollback_context")
}

// Журнал удалён вручную, swapfile и юнит RamPlus остались.
func TestGoldenOrphan(t *testing.T) {
	w := fakesys.New()
	s := newSession(t, w)
	s.run("swap", "apply", "--size", "4G", "--yes")
	delete(w.Files, journal.StatePath)
	s.run("swap", "check-clean")
	s.run("swap", "apply", "--size", "4G", "--dry-run")
	s.run("swap", "remove", "--orphan", "--dry-run")
	s.run("swap", "remove", "--orphan", "--yes")
	s.run("swap", "check-clean")
	s.check("orphan")
}

// setup: swap и профиль одной операцией, один rollback.
func TestGoldenSetup(t *testing.T) {
	s := newSession(t, fakesys.New().WithKernelParams(false).WithOldSwap(0))
	s.run("recommend")
	s.run("recommend", "--json")
	s.run("setup", "--size", "8G", "--existing=replace", "--profile", "zswap-lz4", "--dry-run")
	s.run("setup", "--size", "8G", "--existing=replace", "--profile", "zswap-lz4", "--yes")
	s.run("setup", "--size", "8G", "--existing=replace", "--profile", "zswap-lz4", "--experimental", "--yes")
	s.run("setup", "--size", "8G", "--profile", "zswap-lz4", "--dry-run")
	s.run("history")
	s.run("rollback", "--dry-run")
	s.run("rollback", "--yes")
	s.run("swap", "check-clean")
	s.run("setup", "--dry-run")
	s.check("setup")
}

// init / passwd: пароль веба (хеш в файле не печатается).
func TestGoldenWebPassword(t *testing.T) {
	w := fakesys.New()
	s := newSession(t, w)
	old := lookupWebGroup
	t.Cleanup(func() { lookupWebGroup = old })
	lookupWebGroup = func() (int, error) { return 977, nil }
	s.runIn("password1\n", "passwd", "--password-stdin")
	s.runIn("short\n", "init", "--password-stdin")
	s.runIn("password1\n", "init", "--password-stdin", "--dry-run")
	s.runIn("password1\n", "init", "--password-stdin")
	s.runIn("password1\n", "init", "--password-stdin")
	s.runIn("password2\n", "passwd", "--password-stdin")
	s.run("passwd", "--disable")
	s.run("init")
	w.EUID = 1000
	s.runIn("password1\n", "init", "--password-stdin")
	s.check("webauth")
}
