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

type session struct {
	t   *testing.T
	w   *fakesys.World
	out strings.Builder
}

func newSession(t *testing.T, w *fakesys.World) *session {
	t.Helper()
	oldSys, oldCollect, oldLocal := newSystem, collect, time.Local
	t.Cleanup(func() { newSystem, collect, time.Local = oldSys, oldCollect, oldLocal })
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
func (s *session) run(args ...string) {
	s.t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(""))
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
