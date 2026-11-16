package monitor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/detect"
)

const kB = 1024

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func td(parts ...string) string { return filepath.Join(append([]string{"testdata"}, parts...)...) }

// missing — путь, которого гарантированно нет (имитация отсутствующего источника).
func missing(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }

func eqU(t *testing.T, field string, got *uint64, want uint64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %d", field, want)
	} else if *got != want {
		t.Errorf("%s = %d, want %d", field, *got, want)
	}
}

func isNil[T any](t *testing.T, field string, got *T) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %v, want nil (n/a)", field, *got)
	}
}

func wantReason(t *testing.T, what string, u *detect.Unavailable, reason string) {
	t.Helper()
	if u == nil {
		t.Errorf("%s: error = nil, want %s", what, reason)
	} else if u.Reason != reason {
		t.Errorf("%s: reason = %s, want %s (%s)", what, u.Reason, reason, u.Detail)
	}
}

// --- meminfo ---

func TestParseMeminfoNormal(t *testing.T) {
	m, err := ParseMeminfo(strings.NewReader(string(fixture(t, "meminfo/normal"))))
	if err != nil {
		t.Fatal(err)
	}
	if m["HugePages_Total"] != 0 || m["Hugepagesize"] != 2048*kB {
		t.Errorf("unit handling broken: %v / %v", m["HugePages_Total"], m["Hugepagesize"])
	}
	mem := MemoryFromMeminfo(m)
	eqU(t, "total", mem.TotalBytes, 32617064*kB)
	eqU(t, "available", mem.AvailableBytes, 27423064*kB)
	eqU(t, "used", mem.UsedBytes, (32617064-27423064)*kB)
	eqU(t, "cached", mem.CachedBytes, 4917684*kB)
	eqU(t, "swap_total", mem.SwapTotalBytes, 8388604*kB)
	eqU(t, "swap_free", mem.SwapFreeBytes, 8126460*kB)
	eqU(t, "swap_used", mem.SwapUsedBytes, 262144*kB)
}

func TestParseMeminfoEmptySwap(t *testing.T) {
	m, err := ParseMeminfo(strings.NewReader(string(fixture(t, "meminfo/empty-swap"))))
	if err != nil {
		t.Fatal(err)
	}
	mem := MemoryFromMeminfo(m)
	eqU(t, "swap_total", mem.SwapTotalBytes, 0)
	eqU(t, "swap_used", mem.SwapUsedBytes, 0)
}

func TestParseMeminfoOldKernel(t *testing.T) {
	// Ядра до 3.14 не знают MemAvailable: used и available — n/a, а не выдумка.
	m, err := ParseMeminfo(strings.NewReader(string(fixture(t, "meminfo/old-kernel"))))
	if err != nil {
		t.Fatal(err)
	}
	mem := MemoryFromMeminfo(m)
	isNil(t, "available", mem.AvailableBytes)
	isNil(t, "used", mem.UsedBytes)
	eqU(t, "total", mem.TotalBytes, 2048000*kB)
}

func TestParseMeminfoBad(t *testing.T) {
	for _, in := range []string{"", string(fixture(t, "meminfo/garbage"))} {
		if _, err := ParseMeminfo(strings.NewReader(in)); !errors.Is(err, detect.ErrParse) {
			t.Errorf("%q: want ErrParse, got %v", in, err)
		}
	}
}

func TestReadMemoryMissing(t *testing.T) {
	mem, mi := ReadMemory(missing(t))
	wantReason(t, "memory", mem.Error, detect.ReasonNotPresent)
	if mi != nil || mem.TotalBytes != nil {
		t.Error("want empty result")
	}
}

// --- swaps ---

func TestParseSwaps(t *testing.T) {
	devs, err := ParseSwaps(strings.NewReader(string(fixture(t, "swaps/two-devices"))))
	if err != nil {
		t.Fatal(err)
	}
	want := []SwapDevice{
		{Path: "/dev/nvme0n1p3", Type: "partition", SizeBytes: 16777212 * kB, UsedBytes: 524288 * kB, Priority: -2},
		{Path: "/mnt/fast ssd/swapfile", Type: "file", SizeBytes: 8388604 * kB, UsedBytes: 0, Priority: 10},
	}
	if !reflect.DeepEqual(devs, want) {
		t.Errorf("got %+v\nwant %+v", devs, want)
	}
}

func TestParseSwapsEmpty(t *testing.T) {
	devs, err := ParseSwaps(strings.NewReader(string(fixture(t, "swaps/empty"))))
	if err != nil {
		t.Fatal(err)
	}
	if devs == nil || len(devs) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", devs)
	}
	b, _ := json.Marshal(ReadSwaps(td("swaps", "empty")))
	if string(b) != `{"devices":[],"warnings":[]}` {
		t.Errorf("JSON = %s", b)
	}
}

func TestParseSwapsMalformed(t *testing.T) {
	if _, err := ParseSwaps(strings.NewReader(string(fixture(t, "swaps/malformed")))); !errors.Is(err, detect.ErrParse) {
		t.Errorf("want ErrParse, got %v", err)
	}
}

func TestReadSwapsMissing(t *testing.T) {
	s := ReadSwaps(missing(t))
	wantReason(t, "swaps", s.Error, detect.ReasonNotPresent)
	if s.Devices == nil {
		t.Error("Devices must be non-nil")
	}
}

// --- PSI ---

func TestParsePSI(t *testing.T) {
	p, err := ParsePSI(strings.NewReader(string(fixture(t, "psi/memory"))))
	if err != nil {
		t.Fatal(err)
	}
	if *p.Some != (PSILine{1.53, 0.87, 0.25, 123456789}) {
		t.Errorf("some = %+v", *p.Some)
	}
	if *p.Full != (PSILine{0.40, 0.12, 0.03, 23456789}) {
		t.Errorf("full = %+v", *p.Full)
	}
}

func TestParsePSISomeOnly(t *testing.T) {
	p, err := ParsePSI(strings.NewReader(string(fixture(t, "psi/some-only"))))
	if err != nil {
		t.Fatal(err)
	}
	if p.Some == nil || p.Full != nil {
		t.Errorf("got %+v", p)
	}
}

func TestParsePSIBad(t *testing.T) {
	for _, f := range []string{"psi/empty", "psi/garbage"} {
		if _, err := ParsePSI(strings.NewReader(string(fixture(t, f)))); !errors.Is(err, detect.ErrParse) {
			t.Errorf("%s: want ErrParse, got %v", f, err)
		}
	}
}

func TestReadPSIKernelWithoutPSI(t *testing.T) {
	// CONFIG_PSI=n: каталога /proc/pressure нет.
	p := ReadPSI(missing(t))
	wantReason(t, "psi", p.Error, detect.ReasonNotPresent)
	if p.Some != nil || p.Full != nil {
		t.Error("want nil lines")
	}
}

// --- zswap ---

func TestParseZswapParams(t *testing.T) {
	files, err := readDirFiles(td("zswap", "params-6.8"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseZswapParams(files)
	if err != nil {
		t.Fatal(err)
	}
	if !*p.Enabled || *p.Compressor != "zstd" || *p.Zpool != "zsmalloc" || *p.MaxPoolPercent != 25 ||
		*p.AcceptThresholdPercent != 90 || !*p.ShrinkerEnabled {
		t.Errorf("got %+v", p)
	}
}

func TestParseZswapParamsNoZpool(t *testing.T) {
	// Новые ядра убрали параметр zpool — он должен стать n/a, а не ошибкой.
	files, err := readDirFiles(td("zswap", "params-7.0"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseZswapParams(files)
	if err != nil {
		t.Fatal(err)
	}
	isNil(t, "zpool", p.Zpool)
	if *p.Enabled || *p.Compressor != "lzo" || *p.MaxPoolPercent != 20 {
		t.Errorf("got %+v", p)
	}
}

func TestParseZswapParamsBad(t *testing.T) {
	_, err := ParseZswapParams(map[string]string{"enabled": "maybe\n"})
	if !errors.Is(err, detect.ErrParse) {
		t.Errorf("want ErrParse, got %v", err)
	}
}

func TestParseZswapStats(t *testing.T) {
	files, err := readDirFiles(td("zswap", "debug"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseZswapStats(files, 4096)
	if err != nil {
		t.Fatal(err)
	}
	eqU(t, "pool_total_size", s.PoolTotalSizeBytes, 41943040)
	eqU(t, "stored_pages", s.StoredPages, 30720)
	eqU(t, "written_back", s.WrittenBackPages, 1500)
	if s.CompressionRatio == nil || *s.CompressionRatio != 3.0 {
		t.Errorf("ratio = %v, want 3.0", s.CompressionRatio)
	}
	if s.Counters["reject_compress_poor"] != 12 || len(s.Counters) != 6 {
		t.Errorf("counters = %v", s.Counters)
	}
	if _, err := ParseZswapStats(map[string]string{"x": "text"}, 4096); !errors.Is(err, detect.ErrParse) {
		t.Errorf("want ErrParse for non-numeric dir, got %v", err)
	}
}

func TestReadZswapNoZswap(t *testing.T) {
	// Ядро без zswap: /sys/module/zswap отсутствует.
	z := ReadZswap(missing(t), missing(t), nil)
	if z.Present {
		t.Error("Present must be false")
	}
	wantReason(t, "zswap", z.Error, detect.ReasonNotPresent)
	isNil(t, "pool", z.PoolBytes)
	if z.Stats != nil {
		t.Error("Stats must be nil")
	}
}

func TestReadZswapNoDebugfs(t *testing.T) {
	m, _ := ParseMeminfo(strings.NewReader(string(fixture(t, "meminfo/normal"))))
	z := ReadZswap(td("zswap", "params-7.0"), missing(t), m)
	if !z.Present || z.Error != nil {
		t.Fatalf("got %+v", z)
	}
	eqU(t, "pool", z.PoolBytes, 10240*kB)
	eqU(t, "stored", z.StoredBytes, 40960*kB)
	if z.Stats != nil {
		t.Error("Stats must be nil without debugfs")
	}
	wantReason(t, "stats", z.StatsError, detect.ReasonNotPresent)
}

func TestReadZswapWithDebugfs(t *testing.T) {
	z := ReadZswap(td("zswap", "params-6.8"), td("zswap", "debug"), nil)
	if z.Stats == nil || z.StatsError != nil {
		t.Fatalf("stats=%v err=%v", z.Stats, z.StatsError)
	}
}

func TestReadZswapDebugfsPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "debug")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	z := ReadZswap(td("zswap", "params-7.0"), filepath.Join(dir, "zswap"), nil)
	wantReason(t, "stats", z.StatsError, detect.ReasonPermissionDenied)
}

// --- VM ---

func TestReadVM(t *testing.T) {
	vm := ReadVM(td("vm"))
	if vm.Swappiness == nil || *vm.Swappiness != 60 || vm.VFSCachePressure == nil || *vm.VFSCachePressure != 100 {
		t.Errorf("got %+v", vm)
	}
	// watermark_scale_factor нет в фикстуре (ядра до 4.6).
	isNil(t, "watermark_scale_factor", vm.WatermarkScaleFactor)
	wantReason(t, "watermark_scale_factor", vm.Errors["watermark_scale_factor"], detect.ReasonNotPresent)
}

func TestParseValues(t *testing.T) {
	if v, err := ParseIntValue([]byte(" -5\n")); err != nil || v != -5 {
		t.Errorf("ParseIntValue = %d, %v", v, err)
	}
	if _, err := ParseIntValue([]byte("x")); !errors.Is(err, detect.ErrParse) {
		t.Errorf("want ErrParse, got %v", err)
	}
	for in, want := range map[string]bool{"Y\n": true, "N": false, "1": true, "0\n": false} {
		if v, err := ParseBoolValue([]byte(in)); err != nil || v != want {
			t.Errorf("ParseBoolValue(%q) = %v, %v", in, v, err)
		}
	}
}

// --- zram ---

func TestParseZramCompAlgorithm(t *testing.T) {
	sel, all, err := ParseZramCompAlgorithm(fixture(t, "sysblock/zram0/comp_algorithm"))
	if err != nil || sel != "zstd" || len(all) != 6 || all[5] != "zstd" {
		t.Errorf("sel=%q all=%v err=%v", sel, all, err)
	}
	if _, _, err := ParseZramCompAlgorithm([]byte("lzo lz4")); !errors.Is(err, detect.ErrParse) {
		t.Errorf("want ErrParse, got %v", err)
	}
}

func TestParseZramMMStat(t *testing.T) {
	st, err := ParseZramMMStat(fixture(t, "sysblock/zram0/mm_stat"))
	if err != nil {
		t.Fatal(err)
	}
	if st.OrigDataBytes != 1073741824 || st.ComprDataBytes != 268435456 || st.MemUsedMax != 300000000 {
		t.Errorf("got %+v", st)
	}
	eqU(t, "same_pages", st.SamePages, 1200)
	eqU(t, "huge_pages", st.HugePages, 56)

	// Старое ядро: только 5 колонок.
	st, err = ParseZramMMStat([]byte("1 2 3 4 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	isNil(t, "same_pages", st.SamePages)

	if _, err := ParseZramMMStat([]byte("1 2 3")); !errors.Is(err, detect.ErrParse) {
		t.Errorf("want ErrParse, got %v", err)
	}
}

func TestReadZram(t *testing.T) {
	z := ReadZram(td("sysblock"))
	if z.Error != nil || len(z.Devices) != 2 {
		t.Fatalf("got %+v", z)
	}
	d := z.Devices[0]
	if d.Name != "zram0" || *d.Algorithm != "zstd" || d.Error != nil {
		t.Errorf("zram0 = %+v", d)
	}
	eqU(t, "disksize", d.DiskSizeBytes, 4294967296)
	if d.CompressionRatio == nil || *d.CompressionRatio != 4.0 {
		t.Errorf("ratio = %v", d.CompressionRatio)
	}
	// Неинициализированное устройство: ratio n/a, без деления на ноль.
	d = z.Devices[1]
	if d.Name != "zram1" || *d.Algorithm != "lzo-rle" || d.CompressionRatio != nil {
		t.Errorf("zram1 = %+v", d)
	}
}

func TestReadZramNone(t *testing.T) {
	z := ReadZram(td("sysblock-nozram"))
	if z.Error != nil || z.Devices == nil || len(z.Devices) != 0 {
		t.Errorf("got %+v", z)
	}
	z = ReadZram(missing(t))
	wantReason(t, "zram", z.Error, detect.ReasonNotPresent)
}

// --- Collect ---

func TestCollectFromFixtures(t *testing.T) {
	s := CollectFrom(Paths{
		Meminfo:     td("meminfo", "normal"),
		Swaps:       td("swaps", "two-devices"),
		ZswapParams: td("zswap", "params-6.8"),
		ZswapDebug:  td("zswap", "debug"),
		VMSysctl:    td("vm"),
		PSIMemory:   td("psi", "memory"),
		SysBlock:    td("sysblock"),
	})
	if s.SchemaVersion != SchemaVersion || s.Memory.Error != nil || len(s.Swaps.Devices) != 2 ||
		!s.Zswap.Present || s.Zswap.Stats == nil || s.PSI.Some == nil || len(s.Zram.Devices) != 2 {
		t.Errorf("unexpected status: %+v", s)
	}
}

func TestCollectFromNothing(t *testing.T) {
	// Всё недоступно: не паникуем, каждая секция объясняет причину,
	// а JSON остаётся валидным с [] вместо null для списков.
	m := missing(t)
	s := CollectFrom(Paths{m, m, m, m, m, m, m})
	wantReason(t, "memory", s.Memory.Error, detect.ReasonNotPresent)
	wantReason(t, "swaps", s.Swaps.Error, detect.ReasonNotPresent)
	wantReason(t, "zswap", s.Zswap.Error, detect.ReasonNotPresent)
	wantReason(t, "psi", s.PSI.Error, detect.ReasonNotPresent)
	wantReason(t, "zram", s.Zram.Error, detect.ReasonNotPresent)
	if len(s.VM.Errors) != 3 {
		t.Errorf("vm errors = %v", s.VM.Errors)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"devices":[]`, `"disks":[]`, `"total_bytes":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
}

func TestAnnotateSwapDisks(t *testing.T) {
	s := ReadSwaps(td("swaps", "two-devices"))
	s.Devices = append(s.Devices, SwapDevice{Path: "/dev/zram0", Type: "partition"})
	calls := 0
	AnnotateSwapDisks(&s, func(path string) (detect.PathDisks, error) {
		calls++
		switch path {
		case "/dev/nvme0n1p3":
			return detect.PathDisks{Kind: detect.KindNVMe, Disks: []detect.BlockDevice{{Name: "nvme0n1", Transport: "nvme"}}}, nil
		default:
			return detect.PathDisks{Kind: detect.KindHDD, Disks: []detect.BlockDevice{{Name: "sdb", Transport: "usb"}}}, nil
		}
	})
	if calls != 2 {
		t.Errorf("zram must be skipped, calls = %d", calls)
	}
	if s.Devices[0].Transport != "nvme" || s.Devices[1].Transport != "usb" || s.Devices[2].Transport != "" {
		t.Errorf("devices = %+v", s.Devices)
	}
	want := []SwapWarning{{Code: WarnSwapOnUSB, Path: "/mnt/fast ssd/swapfile"}}
	if !reflect.DeepEqual(s.Warnings, want) {
		t.Errorf("warnings = %+v", s.Warnings)
	}
}
