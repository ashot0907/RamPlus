package profile

import (
	"os"
	"strings"
	"testing"
)

func TestCatalog(t *testing.T) {
	names := strings.Join(Names(), ",")
	if names != "default,zswap-lz4,zswap-zstd" {
		t.Fatalf("names = %s", names)
	}
	def, _ := Get("default")
	if def.Status != StatusStandard || def.Params["zswap.enabled"] != "N" || def.Params["vm.swappiness"] != "60" ||
		def.Params["vm.page-cluster"] != "3" || def.Params["vm.watermark_scale_factor"] != "10" || def.Params["vm.vfs_cache_pressure"] != "100" {
		t.Errorf("default = %+v", def)
	}
	for _, name := range []string{"zswap-lz4", "zswap-zstd"} {
		p, _ := Get(name)
		if p.Status != StatusExperimental || p.Evidence != "" || p.Params["zswap.zpool"] != "zsmalloc" || p.Params["zswap.max_pool_percent"] != "20" {
			t.Errorf("%s = %+v", name, p)
		}
		for k, v := range def.Params {
			if k != "zswap.enabled" && p.Params[k] != v {
				t.Errorf("%s: %s = %s, default %s («остальное как default»)", name, k, p.Params[k], v)
			}
		}
	}
	if p, _ := Get("zswap-zstd"); p.Params["zswap.compressor"] != "zstd" {
		t.Error("zstd")
	}
}

// Правило CLAUDE.md: «проверенный» — только с evidence от bench на SSD.
func TestVerifiedRequiresEvidence(t *testing.T) {
	p := Profile{Name: "x", Status: StatusVerified, Params: map[string]string{"vm.swappiness": "60"}}
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "без evidence") {
		t.Errorf("err = %v", err)
	}
	p.Evidence = "bench-results/2026-12-01-nvme/summary.md"
	if err := Validate(p); err != nil {
		t.Error(err)
	}
	for _, bad := range []Profile{
		{Name: "x", Status: "best", Params: map[string]string{"vm.swappiness": "60"}},
		{Name: "x", Status: StatusStandard, Params: map[string]string{"vm.nope": "1"}},
		{Name: "x", Status: StatusStandard, Params: map[string]string{"zswap.enabled": "1"}}, // нужно "Y"
		{Name: "x", Status: StatusStandard},
	} {
		if Validate(bad) == nil {
			t.Errorf("%+v must be invalid", bad)
		}
	}
}

func TestNormalize(t *testing.T) {
	ok := map[[2]string]string{
		{"zswap.enabled", "1"}: "Y", {"zswap.enabled", "off"}: "N", {"zswap.enabled", "Y"}: "Y",
		{"vm.swappiness", " 080 "}: "80", {"vm.swappiness", "200"}: "200", {"vm.page-cluster", "0"}: "0",
		{"zswap.compressor", "lz4"}: "lz4", {"zswap.zpool", "zbud"}: "zbud", {"zswap.max_pool_percent", "35"}: "35",
	}
	for in, want := range ok {
		if got, err := Normalize(in[0], in[1]); err != nil || got != want {
			t.Errorf("Normalize(%v) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := [][2]string{
		{"vm.swappiness", "201"}, {"vm.swappiness", "-1"}, {"vm.page-cluster", "11"}, {"vm.watermark_scale_factor", "0"},
		{"zswap.max_pool_percent", "0"}, {"zswap.zpool", "jemalloc"}, {"zswap.compressor", "lz4; rm -rf /"},
		{"zswap.enabled", "maybe"}, {"vm.dirty_ratio", "10"}, {"vm.swappiness", "abc"},
	}
	for _, in := range bad {
		if _, err := Normalize(in[0], in[1]); err == nil {
			t.Errorf("Normalize(%v) must fail", in)
		}
	}
}

func TestParseSetAndEffective(t *testing.T) {
	set, err := ParseSet([]string{"vm.swappiness=80", "zswap.max_pool_percent = 35"})
	if err != nil || set["vm.swappiness"] != "80" || set["zswap.max_pool_percent"] != "35" {
		t.Fatalf("%v %v", set, err)
	}
	for _, bad := range []string{"vm.swappiness", "vm.swappiness=999", "x=1"} {
		if _, err := ParseSet([]string{bad}); err == nil {
			t.Errorf("--set %s must fail", bad)
		}
	}
	p, _ := Get("zswap-lz4")
	eff := Effective(p, set)
	if eff["vm.swappiness"] != "80" || eff["zswap.compressor"] != "lz4" || p.Params["vm.swappiness"] != "60" {
		t.Errorf("effective = %v (profile must not change)", eff)
	}
}

func TestParseSysctlConf(t *testing.T) {
	b, err := os.ReadFile("testdata/sysctl-mixed.conf")
	if err != nil {
		t.Fatal(err)
	}
	m := ParseSysctlConf(string(b))
	want := map[string]string{"vm.swappiness": "10", "vm.page-cluster": "0", "vm.vfs_cache_pressure": "50", "kernel.sysrq": "176", "net.ipv4.ip_forward": "1"}
	if len(m) != len(want) {
		t.Fatalf("m = %v", m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
}

func TestSysctlOrder(t *testing.T) {
	files := []ConfFile{
		{Path: "/etc/sysctl.d/10-tune.conf", Name: "10-tune.conf", Values: map[string]string{"vm.swappiness": "10"}},
		{Path: "/etc/sysctl.d/99-sysctl.conf", Name: "99-sysctl.conf", Values: map[string]string{"vm.swappiness": "30"}},
		{Path: "/usr/lib/sysctl.d/10-tune.conf", Name: "10-tune.conf", Values: map[string]string{"vm.swappiness": "99"}}, // перекрыт /etc
		{Path: "/usr/lib/sysctl.d/50-default.conf", Name: "50-default.conf", Values: map[string]string{"vm.page-cluster": "2"}},
	}
	ordered := OrderSysctl(files)
	var names []string
	for _, f := range ordered {
		names = append(names, f.Path)
	}
	if strings.Join(names, " ") != "/etc/sysctl.d/10-tune.conf /usr/lib/sysctl.d/50-default.conf /etc/sysctl.d/99-sysctl.conf" {
		t.Errorf("order = %v", names)
	}
	with := WithOurs(ordered, map[string]string{"vm.swappiness": "60"})
	if with[2].Path != SysctlFilePath || with[3].Name != "99-sysctl.conf" {
		t.Errorf("99-ramplus.conf must sort before 99-sysctl.conf: %+v", with)
	}
	// 99-sysctl.conf (→ /etc/sysctl.conf) идёт после нашего и перебьёт swappiness.
	if o := Overriders(with, "vm.swappiness"); len(o) != 1 || o[0].Name != "99-sysctl.conf" {
		t.Errorf("overriders = %+v", o)
	}
	if o := Overriders(with, "vm.page-cluster"); len(o) != 0 {
		t.Errorf("50-default.conf goes before ours: %+v", o)
	}
	if s := Setters(with, "vm.swappiness"); len(s) != 3 {
		t.Errorf("setters = %+v", s)
	}
	// /etc/sysctl.conf без симлинка: `sysctl --system` читает его последним.
	last := ConfFile{Path: SysctlConf, Values: map[string]string{"vm.page-cluster": "0"}}
	with = WithOurs(OrderSysctl(files[:1], last), map[string]string{"vm.page-cluster": "3"})
	if o := Overriders(with, "vm.page-cluster"); len(o) != 1 || o[0].Path != SysctlConf {
		t.Errorf("sysctl.conf last: %+v", with)
	}
}

func TestFileContents(t *testing.T) {
	p, _ := Get("zswap-lz4")
	sc := SysctlFileContent(p.Name, p.Status, p.Params)
	if !strings.HasPrefix(sc, Marker) || !strings.Contains(sc, "vm.swappiness = 60\n") || strings.Contains(sc, "zswap.") {
		t.Errorf("sysctl file:\n%s", sc)
	}
	if got := ParseSysctlConf(sc); len(got) != 4 || got["vm.page-cluster"] != "3" {
		t.Errorf("round trip: %v", got)
	}
	unit := ZswapUnitContent(p.Name, p.Status, p.Params, map[string]bool{"zswap.zpool": true})
	for _, want := range []string{
		Marker, "Type=oneshot", "After=local-fs.target", "Before=swap.target", "WantedBy=swap.target",
		"ExecStart=/bin/sh -c 'echo lz4 > /sys/module/zswap/parameters/compressor'",
		"ExecStart=/bin/sh -c 'echo 20 > /sys/module/zswap/parameters/max_pool_percent'",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "zpool") && strings.Contains(unit, "parameters/zpool") {
		t.Error("zpool absent in kernel must be skipped")
	}
	// enabled — последним при включении.
	lines := strings.Split(unit, "\n")
	var execs []string
	for _, l := range lines {
		if strings.HasPrefix(l, "ExecStart=") {
			execs = append(execs, l)
		}
	}
	if !strings.Contains(execs[len(execs)-1], "echo Y > /sys/module/zswap/parameters/enabled") {
		t.Errorf("enabled must be written last: %v", execs)
	}
	if ZswapWriteOrder(false)[0] != "zswap.enabled" {
		t.Error("disabling: enabled first")
	}
}

func TestKeyForPath(t *testing.T) {
	if KeyForPath("/proc/sys/vm/swappiness") != "vm.swappiness" || KeyForPath("/etc/fstab") != "" {
		t.Error("KeyForPath")
	}
}
