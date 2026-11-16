package swap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseSize(t *testing.T) {
	ok := map[string]uint64{
		"12G": 12 * GiB, "12g": 12 * GiB, "12GiB": 12 * GiB, "12GB": 12 * GiB,
		"512M": 512 * MiB, "1.5G": 1536 * MiB, "1T": TiB, "2048K": 2 * MiB,
		"1073741824": GiB, " 4G ": 4 * GiB,
	}
	for in, want := range ok {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "G", "-1G", "12X", "abc", "1.2.3G", "0", "1000000"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): want error", in)
		}
	}
}

func TestSizeFlagRoundTrip(t *testing.T) {
	for _, b := range []uint64{4 * GiB, 1536 * MiB, 256 * MiB} {
		got, err := ParseSize(sizeFlag(b))
		if err != nil || got != b {
			t.Errorf("sizeFlag(%d) = %s → %d, %v", b, sizeFlag(b), got, err)
		}
	}
}

// Ожидаемые значения получены `systemd-escape --path`.
func TestEscapePath(t *testing.T) {
	tests := map[string]string{
		"/ramplus.swapfile":              "ramplus.swapfile",
		"/ramplus-swap/ramplus.swapfile": `ramplus\x2dswap-ramplus.swapfile`,
		"/mnt/nvme/ramplus.swapfile":     "mnt-nvme-ramplus.swapfile",
		"/a b/c.d-e":                     `a\x20b-c.d\x2de`,
		"/swap.img":                      "swap.img",
		"/.hidden/x":                     `\x2ehidden-x`,
		"//double//slash/":               "double-slash",
		"/":                              "-",
	}
	for in, want := range tests {
		if got := EscapePath(in); got != want {
			t.Errorf("EscapePath(%q) = %q, want %q", in, got, want)
		}
	}
	if UnitName(ManagedPath) != "ramplus.swapfile.swap" {
		t.Errorf("UnitName = %s", UnitName(ManagedPath))
	}
}

func TestUnitContent(t *testing.T) {
	c := UnitContent(ManagedPath, 10)
	for _, want := range []string{"[Swap]\nWhat=/ramplus.swapfile\nPriority=10\n", "WantedBy=swap.target", "Managed by RamPlus"} {
		if !strings.Contains(c, want) {
			t.Errorf("unit lacks %q:\n%s", want, c)
		}
	}
}

func TestResumeParam(t *testing.T) {
	if got := ResumeParam(readFixture(t, "cmdline/resume")); got != "UUID=aaaa-bbbb" {
		t.Errorf("resume = %q", got)
	}
	if got := ResumeParam(readFixture(t, "cmdline/plain")); got != "" {
		t.Errorf("resume = %q, want empty", got)
	}
}

func TestParseFstab(t *testing.T) {
	swaps := SwapEntries(ParseFstab(readFixture(t, "fstab/ubuntu")))
	if len(swaps) != 1 || swaps[0].Spec != "/swap.img" || swaps[0].Line != "/swap.img\tnone\tswap\tsw\t0\t0" || swaps[0].LineNo != 5 {
		t.Errorf("swaps = %+v", swaps)
	}
	all := ParseFstab(readFixture(t, "fstab/uuid-partition"))
	swaps = SwapEntries(all)
	if len(swaps) != 1 || swaps[0].Spec != "UUID=aaaa-bbbb" {
		t.Errorf("закомментированная строка не должна учитываться: %+v", swaps)
	}
	if all[2].File != "/mnt/my data" {
		t.Errorf("escape: %q", all[2].File)
	}
	if c := SpecCandidates("UUID=aaaa-bbbb"); c[0] != "/dev/disk/by-uuid/aaaa-bbbb" {
		t.Errorf("candidates = %v", c)
	}
	if c := SpecCandidates(`LABEL="swap"`); c[0] != "/dev/disk/by-label/swap" {
		t.Errorf("candidates = %v", c)
	}
}

func TestFstabDisableEnableRoundTrip(t *testing.T) {
	orig := readFixture(t, "fstab/ubuntu")
	line := "/swap.img\tnone\tswap\tsw\t0\t0"
	disabled, err := DisableFstabLine(orig, line)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(disabled, "# disabled by ramplus: "+line) || len(SwapEntries(ParseFstab(disabled))) != 0 {
		t.Errorf("disabled:\n%s", disabled)
	}
	back, err := EnableFstabLine(disabled, line)
	if err != nil || back != orig {
		t.Errorf("round trip failed: %v\n%s", err, back)
	}
	if _, err := EnableFstabLine(orig, line); err == nil {
		t.Error("enable without marker must fail")
	}
	if _, err := DisableFstabLine(orig, "/nope none swap sw 0 0"); err == nil {
		t.Error("missing line must fail")
	}
	if _, err := DisableFstabLine(readFixture(t, "fstab/duplicate"), "/swap.img none swap sw 0 0"); err == nil {
		t.Error("ambiguous line must fail")
	}
}

func TestStateRoundTripAndStack(t *testing.T) {
	ts := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := State{
		Managed: &Managed{Path: ManagedPath, SizeBytes: 4 * GiB, Priority: 10, Unit: "ramplus.swap.swap", CreatedAt: ts},
		Operations: []Operation{
			{ID: "1", Action: ActionApply, Status: OpCommitted, StartedAt: ts},
			{ID: "2", Action: ActionResize, Status: OpFailed, StartedAt: ts},
			{ID: "3", Action: ActionRemove, Status: OpRolledBack, StartedAt: ts},
			{ID: "4", Action: ActionRollback, Status: OpCommitted, Target: "3", StartedAt: ts},
		},
	}
	b, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseState(b)
	if err != nil || got.Managed.SizeBytes != 4*GiB || len(got.Operations) != 4 {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	if i, ok := got.LastUndoable(); !ok || got.Operations[i].ID != "1" {
		t.Errorf("LastUndoable = %d %v", i, ok)
	}
	got.Operations = append(got.Operations, Operation{ID: "5", Action: ActionRollback, Status: OpInProgress})
	if i, _ := got.LastUndoable(); got.Operations[i].ID != "5" {
		t.Errorf("interrupted rollback must be undone first, got %s", got.Operations[i].ID)
	}
	if _, err := ParseState([]byte(`{"version": 99}`)); err == nil {
		t.Error("unknown version must fail")
	}
	if _, err := ParseState([]byte(`{`)); err == nil {
		t.Error("garbage must fail")
	}
}
