package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/monitor"
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
	if err := renderStatus(&buf, st); err != nil {
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
		return filepath.Join(append([]string{"..", "..", "internal", "monitor", "testdata"}, p...)...)
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
	if err := renderStatus(&buf, st); err != nil {
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

func TestStatusJSONCommand(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"status", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"schema_version", "system", "memory", "swaps", "zswap", "vm", "psi", "root_disk", "zram"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
}
