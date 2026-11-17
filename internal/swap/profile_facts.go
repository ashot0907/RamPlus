package swap

import (
	"bufio"
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/profile"
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

// ProfileFacts — всё, что нужно планировщику профилей. Собирается
// GatherProfile (только чтение); в тестах — фейковая система.
type ProfileFacts struct {
	Facts

	// Values — текущие runtime-значения (без пробелов); нет ключа —
	// параметра нет в этом ядре.
	Values       map[string]string
	ZswapPresent bool

	// SysctlFiles — файлы sysctl в порядке применения при загрузке
	// (включая наш 99-ramplus.conf, если он есть).
	SysctlFiles []profile.ConfFile
	OurSysctl   *string // содержимое 99-ramplus.conf
	OurUnit     *string // содержимое ramplus-zswap.service
	UnitEnabled bool

	CompressorOK map[string]bool // компрессор есть в /proc/crypto или модулем
	ZpoolOK      map[string]bool // аллокатор встроен, загружен или есть модулем

	ZramActive    bool
	ZramGenerator string // путь, по которому найден генератор zram ("" — нет)
}

// GatherProfile дополняет факты сведениями для профилей. want — значения,
// для которых проверить доступность компрессора и zpool (профиль + --set).
func GatherProfile(ctx context.Context, sys System, f Facts, want map[string]string) ProfileFacts {
	pf := ProfileFacts{Facts: f, Values: map[string]string{}, CompressorOK: map[string]bool{}, ZpoolOK: map[string]bool{}}
	for _, p := range profile.Params {
		if b, err := sys.FS.ReadFile(p.Path); err == nil {
			pf.Values[p.Key] = strings.TrimSpace(string(b))
		}
	}
	if st, err := sys.FS.Stat(profile.ZswapParamDir); err == nil && st.IsDir() {
		pf.ZswapPresent = true
	}

	pf.SysctlFiles = gatherSysctl(sys)
	if b, err := sys.FS.ReadFile(profile.SysctlFilePath); err == nil {
		s := string(b)
		pf.OurSysctl = &s
	}
	if b, err := sys.FS.ReadFile(ZswapUnitPath); err == nil {
		s := string(b)
		pf.OurUnit = &s
	}
	if out, err := sys.Exec.Run(ctx, "systemctl", "is-enabled", profile.ZswapUnitName); err == nil && strings.TrimSpace(string(out)) == "enabled" {
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

	for _, s := range f.Swaps {
		if isZram(s.Path) {
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
func parseCrypto(sys System) map[string]bool {
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
func gatherSysctl(sys System) []profile.ConfFile {
	var files []profile.ConfFile
	viaSymlink := false
	for _, dir := range profile.SysctlDirs {
		paths, _ := sys.FS.Glob(path.Join(dir, "*.conf"))
		sort.Strings(paths)
		for _, p := range paths {
			b, err := sys.FS.ReadFile(p)
			if err != nil {
				continue
			}
			if real, err := sys.FS.EvalSymlinks(p); err == nil && real == profile.SysctlConf {
				viaSymlink = true
			}
			files = append(files, profile.ConfFile{
				Path: p, Name: path.Base(p), Values: profile.ParseSysctlConf(string(b)),
				Ours: p == profile.SysctlFilePath,
			})
		}
	}
	var extra []profile.ConfFile
	if !viaSymlink {
		if b, err := sys.FS.ReadFile(profile.SysctlConf); err == nil {
			extra = append(extra, profile.ConfFile{Path: profile.SysctlConf, Values: profile.ParseSysctlConf(string(b))})
		}
	}
	return profile.OrderSysctl(files, extra...)
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
func (pf ProfileFacts) SourceOf(key string) string {
	p, _ := profile.Lookup(key)
	cur, present := pf.Values[key]
	if !present {
		return "нет в этом ядре"
	}
	if p.Group == profile.GroupSysctl {
		setters := profile.Setters(pf.SysctlFiles, key)
		if n := len(setters); n > 0 {
			last := setters[n-1]
			label := last.Path
			if last.Ours {
				label = "RamPlus (" + profile.SysctlFileName + ")"
			}
			if v := last.Values[key]; v != cur {
				return "изменено вручную (в " + label + ": " + v + ")"
			}
			return label
		}
		if v, ok := cmdlineParams(pf.Cmdline, "sysctl.")["sysctl."+key]; ok {
			if v != cur {
				return "изменено вручную (cmdline: " + v + ")"
			}
			return "cmdline"
		}
		return "ядро по умолчанию"
	}
	if v, ok := unitValues(pf.OurUnit)[p.Path]; ok {
		if v != cur {
			return "изменено вручную (RamPlus: " + v + ")"
		}
		return "RamPlus (" + profile.ZswapUnitName + ")"
	}
	if v, ok := cmdlineParams(pf.Cmdline, "zswap.")[key]; ok {
		return "cmdline (" + key + "=" + v + ")"
	}
	return "ядро по умолчанию"
}
