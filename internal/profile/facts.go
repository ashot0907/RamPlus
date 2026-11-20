package profile

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"bufio"
	"bytes"
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/monitor"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// Пути, которые читает планировщик профилей.
const (
	CryptoPath    = "/proc/crypto"
	OSReleasePath = "/proc/sys/kernel/osrelease"
	ModulesDir    = "/lib/modules"
)

// zramGenerators — признаки установленного zram-generator / zram-tools /
// zram-config: zswap поверх zram бессмыслен.
var zramGenerators = []string{
	"/usr/lib/systemd/system-generators/zram-generator",
	"/lib/systemd/system-generators/zram-generator",
	"/etc/systemd/zram-generator.conf",
	"/usr/sbin/zramswap",
	"/usr/bin/init-zram-swapping",
}

// ZswapUnitPath — юнит, который восстанавливает параметры zswap при загрузке.
const ZswapUnitPath = system.UnitDir + "/" + ZswapUnitName

// Facts — всё, что нужно планировщику профилей. Собирается Gather (только
// чтение); в тестах — фейковая система.
type Facts struct {
	journal.Basics
	Swaps   []monitor.SwapDevice
	Cmdline string
	// Profile/Baseline — разделы журнала.
	Profile  *AppliedProfile
	Baseline *Baseline

	// Values — текущие runtime-значения (без пробелов); нет ключа —
	// параметра нет в этом ядре.
	Values       map[string]string
	ZswapPresent bool

	// SysctlFiles — файлы sysctl в порядке применения при загрузке
	// (включая наш 99-ramplus.conf, если он есть).
	SysctlFiles []ConfFile
	OurSysctl   *string // содержимое 99-ramplus.conf
	OurUnit     *string // содержимое ramplus-zswap.service
	UnitEnabled bool

	CompressorOK map[string]bool // компрессор есть в /proc/crypto или модулем
	ZpoolOK      map[string]bool // аллокатор встроен, загружен или есть модулем

	ZramActive    bool
	ZramGenerator string // путь, по которому найден генератор zram ("" — нет)
}

// Gather собирает факты для профилей. want — значения, для которых
// проверить доступность компрессора и zpool (профиль + --set).
func Gather(ctx context.Context, sys system.System, want map[string]string) Facts {
	pf := Facts{Basics: journal.GatherBasics(sys), Values: map[string]string{}, CompressorOK: map[string]bool{}, ZpoolOK: map[string]bool{}}
	pf.Profile, pf.Baseline = ProfileOf(pf.State), BaselineOf(pf.State)
	if b, err := sys.FS.ReadFile(system.SwapsPath); err != nil {
		pf.SwapsErr = err
	} else {
		pf.Swaps, pf.SwapsErr = monitor.ParseSwaps(bytes.NewReader(b))
	}
	if b, err := sys.FS.ReadFile(system.CmdlinePath); err == nil {
		pf.Cmdline = strings.TrimSpace(string(b))
	}
	for _, p := range Params {
		if b, err := sys.FS.ReadFile(p.Path); err == nil {
			pf.Values[p.Key] = strings.TrimSpace(string(b))
		}
	}
	if st, err := sys.FS.Stat(ZswapParamDir); err == nil && st.IsDir() {
		pf.ZswapPresent = true
	}

	pf.SysctlFiles = gatherSysctl(sys)
	if b, err := sys.FS.ReadFile(SysctlFilePath); err == nil {
		s := string(b)
		pf.OurSysctl = &s
	}
	if b, err := sys.FS.ReadFile(ZswapUnitPath); err == nil {
		s := string(b)
		pf.OurUnit = &s
	}
	if out, err := sys.Exec.Run(ctx, "systemctl", "is-enabled", ZswapUnitName); err == nil && strings.TrimSpace(string(out)) == "enabled" {
		pf.UnitEnabled = true
	}

	release := ""
	if b, err := sys.FS.ReadFile(OSReleasePath); err == nil {
		release = strings.TrimSpace(string(b))
	}
	builtin := map[string]bool{}
	if release != "" {
		if b, err := sys.FS.ReadFile(path.Join(ModulesDir, release, "modules.builtin")); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				builtin[strings.TrimSpace(l)] = true
			}
		}
	}
	moduleOK := func(sub, name string) bool {
		if builtin["kernel/"+sub+"/"+name+".ko"] {
			return true
		}
		if st, err := sys.FS.Stat("/sys/module/" + strings.ReplaceAll(name, "-", "_")); err == nil && st.IsDir() {
			return true
		}
		if release == "" {
			return false
		}
		m, _ := sys.FS.Glob(path.Join(ModulesDir, release, "kernel", sub, name+".ko*"))
		return len(m) > 0
	}
	crypto := parseCrypto(sys)
	check := func(key string, ok func(string) bool, into map[string]bool) {
		for _, v := range []string{want[key], pf.Values[key]} {
			if v != "" {
				into[v] = ok(v)
			}
		}
	}
	check("zswap.compressor", func(c string) bool { return crypto[c] || moduleOK("crypto", c) }, pf.CompressorOK)
	check("zswap.zpool", func(z string) bool { return moduleOK("mm", z) }, pf.ZpoolOK)

	for _, s := range pf.Swaps {
		if strings.HasPrefix(s.Path, "/dev/zram") {
			pf.ZramActive = true
		}
	}
	for _, g := range zramGenerators {
		if _, err := sys.FS.Stat(g); err == nil {
			pf.ZramGenerator = g
			break
		}
	}
	return pf
}

// parseCrypto — имена алгоритмов из /proc/crypto.
func parseCrypto(sys system.System) map[string]bool {
	out := map[string]bool{}
	b, err := sys.FS.ReadFile(CryptoPath)
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == "name" {
			out[strings.TrimSpace(v)] = true
		}
	}
	return out
}

// gatherSysctl читает sysctl.d во всех каталогах и /etc/sysctl.conf.
func gatherSysctl(sys system.System) []ConfFile {
	var files []ConfFile
	viaSymlink := false
	for _, dir := range SysctlDirs {
		paths, _ := sys.FS.Glob(path.Join(dir, "*.conf"))
		sort.Strings(paths)
		for _, p := range paths {
			b, err := sys.FS.ReadFile(p)
			if err != nil {
				continue
			}
			if real, err := sys.FS.EvalSymlinks(p); err == nil && real == SysctlConf {
				viaSymlink = true
			}
			files = append(files, ConfFile{
				Path: p, Name: path.Base(p), Values: ParseSysctlConf(string(b)),
				Ours: p == SysctlFilePath,
			})
		}
	}
	var extra []ConfFile
	if !viaSymlink {
		if b, err := sys.FS.ReadFile(SysctlConf); err == nil {
			extra = append(extra, ConfFile{Path: SysctlConf, Values: ParseSysctlConf(string(b))})
		}
	}
	return OrderSysctl(files, extra...)
}

var unitEchoRe = regexp.MustCompile(`echo (\S+) > (\S+)'`)

// unitValues — какие значения пишет наш ramplus-zswap.service (путь → значение).
func unitValues(content *string) map[string]string {
	out := map[string]string{}
	if content == nil {
		return out
	}
	for _, m := range unitEchoRe.FindAllStringSubmatch(*content, -1) {
		out[m[2]] = m[1]
	}
	return out
}

// cmdlineParams — параметры вида prefix.* из /proc/cmdline.
func cmdlineParams(cmdline, prefix string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Fields(cmdline) {
		if k, v, ok := strings.Cut(f, "="); ok && strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}

// SourceOf — откуда текущее значение параметра.
func (pf Facts) SourceOf(key string) i18n.Text {
	p, _ := Lookup(key)
	cur, present := pf.Values[key]
	if !present {
		return i18n.Msg("profile.source.absent")
	}
	if p.Group == GroupSysctl {
		setters := Setters(pf.SysctlFiles, key)
		if n := len(setters); n > 0 {
			last := setters[n-1]
			var label i18n.Text = i18n.Raw(last.Path)
			if last.Ours {
				label = i18n.Raw("RamPlus (" + SysctlFileName + ")")
			}
			if v := last.Values[key]; v != cur {
				return i18n.Msg("profile.source.changed_in", label, v)
			}
			return label
		}
		if v, ok := cmdlineParams(pf.Cmdline, "sysctl.")["sysctl."+key]; ok {
			if v != cur {
				return i18n.Msg("profile.source.changed_cmdline", v)
			}
			return i18n.Raw("cmdline")
		}
		return i18n.Msg("profile.source.kernel_default")
	}
	if v, ok := unitValues(pf.OurUnit)[p.Path]; ok {
		if v != cur {
			return i18n.Msg("profile.source.changed_ramplus", v)
		}
		return i18n.Raw("RamPlus (" + ZswapUnitName + ")")
	}
	if v, ok := cmdlineParams(pf.Cmdline, "zswap.")[key]; ok {
		return i18n.Raw("cmdline (" + key + "=" + v + ")")
	}
	return i18n.Msg("profile.source.kernel_default")
}

// fingerprintPart — часть отпечатка плана от фактов профиля: текущие
// значения параметров, файлы sysctl и их порядок, наши файлы и юнит,
// доступность компрессоров/аллокаторов, zram, cmdline, активные swap.
func (f Facts) fingerprintPart() any {
	swaps := make([]string, 0, len(f.Swaps))
	for _, s := range f.Swaps {
		swaps = append(swaps, s.Path)
	}
	return struct {
		Swaps         []string
		Cmdline       string
		Values        map[string]string
		ZswapPresent  bool
		SysctlFiles   []ConfFile
		OurSysctl     *string
		OurUnit       *string
		UnitEnabled   bool
		CompressorOK  map[string]bool
		ZpoolOK       map[string]bool
		ZramActive    bool
		ZramGenerator string
	}{swaps, f.Cmdline, f.Values, f.ZswapPresent, f.SysctlFiles, f.OurSysctl, f.OurUnit, f.UnitEnabled,
		f.CompressorOK, f.ZpoolOK, f.ZramActive, f.ZramGenerator}
}
