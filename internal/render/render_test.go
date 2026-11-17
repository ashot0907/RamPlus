package render

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/profile"
)

func TestHumanBytes(t *testing.T) {
	tests := map[uint64]string{
		0:                  "0 B",
		1023:               "1023 B",
		1024:               "1.0 KiB",
		8589930496:         "8.0 GiB",
		1536 * 1024 * 1024: "1.5 GiB",
	}
	for in, want := range tests {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// Пустой Status (всё недоступно) должен рендериться без паники и с n/a.
func TestRenderAllUnavailable(t *testing.T) {
	m := filepath.Join(t.TempDir(), "absent")
	st := monitor.CollectFrom(monitor.Paths{
		Meminfo: m, Swaps: m, ZswapParams: m, ZswapDebug: m, VMSysctl: m, PSIMemory: m, SysBlock: m,
	})
	st.System.OSError = &detect.Unavailable{Reason: detect.ReasonNotPresent}
	st.RootDisk.Error = &detect.Unavailable{Reason: detect.ReasonPermissionDenied}

	var buf bytes.Buffer
	if err := Status(&buf, st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"n/a (ядро без PSI)", "n/a (zswap нет в ядре)", "n/a (нужен root)", "Swap-устройства"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRenderFixtures(t *testing.T) {
	td := func(p ...string) string {
		return filepath.Join(append([]string{"..", "monitor", "testdata"}, p...)...)
	}
	st := monitor.CollectFrom(monitor.Paths{
		Meminfo:     td("meminfo", "empty-swap"),
		Swaps:       td("swaps", "empty"),
		ZswapParams: td("zswap", "params-7.0"),
		ZswapDebug:  filepath.Join(t.TempDir(), "absent"),
		VMSysctl:    td("vm"),
		PSIMemory:   td("psi", "memory"),
		SysBlock:    td("sysblock"),
	})
	var buf bytes.Buffer
	if err := Status(&buf, st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"swap не включён",
		"n/a (параметра нет в этом ядре)",
		"n/a (debugfs не смонтирован)",
		"zram0", "4.00x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestPressureText(t *testing.T) {
	for level, want := range map[string]string{
		monitor.PressureNormal: "норма", monitor.PressureElevated: "повышенное давление",
		monitor.PressureHigh: "система тормозит", monitor.PressureSevere: "сильные тормоза", "": "n/a",
	} {
		if got := pressureText(level); got != want {
			t.Errorf("pressureText(%q) = %q", level, got)
		}
	}
}

func TestStatusProfileSection(t *testing.T) {
	var out bytes.Buffer
	st := monitor.Status{
		Profile: &monitor.ProfileInfo{Name: "zswap-lz4", Status: "experimental", AppliedAt: time.Date(2026, 11, 17, 12, 0, 0, 0, time.Local)},
		Drift:   []monitor.DriftItem{{Key: "vm.swappiness", Expected: "60", Actual: "80"}},
	}
	if err := renderProfile(&out, st); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"zswap-lz4", "экспериментальный", "ВНИМАНИЕ: значения изменены вручную", "vm.swappiness = 80 (профиль: 60)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	renderProfile(&out, monitor.Status{})
	if !strings.Contains(out.String(), "не применялся") {
		t.Errorf("no profile:\n%s", out.String())
	}
}

func TestRenderCurrent(t *testing.T) {
	pf := profile.Facts{Values: map[string]string{"vm.swappiness": "80", "zswap.enabled": "Y"}}
	pf.Profile = &profile.AppliedProfile{Name: "default", Status: "modified", Values: map[string]string{"vm.swappiness": "80", "zswap.enabled": "N"}}
	pf.Baseline = &profile.Baseline{Values: map[string]string{"vm.swappiness": "60", "zswap.enabled": "N"}}
	var out bytes.Buffer
	if err := ProfileCurrent(&out, pf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"изменённый пользователем", "Отличия от исходных значений", "vm.swappiness: 60 → 80", "zswap.enabled: N → Y",
		"ВНИМАНИЕ: значения изменены вручную после apply: zswap.enabled = Y (профиль: N)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
}
