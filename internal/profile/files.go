package profile

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"bufio"
	"fmt"
	"sort"
	"strings"
)

// Файлы, которые делают профиль постоянным.
const (
	Marker         = "# Managed by RamPlus"
	SysctlFileName = "99-ramplus.conf"
	SysctlFilePath = "/etc/sysctl.d/" + SysctlFileName
	ZswapUnitName  = "ramplus-zswap.service"
)

// SysctlDirs — каталоги sysctl.d в порядке приоритета systemd-sysctl:
// файл с тем же именем из более раннего каталога заменяет более поздний.
var SysctlDirs = []string{"/etc/sysctl.d", "/run/sysctl.d", "/usr/local/lib/sysctl.d", "/usr/lib/sysctl.d", "/lib/sysctl.d"}

// SysctlConf — /etc/sysctl.conf: systemd-sysctl читает его только через
// симлинк /etc/sysctl.d/99-sysctl.conf, `sysctl --system` — последним.
const SysctlConf = "/etc/sysctl.conf"

// ParseSysctlConf разбирает файл sysctl.d: "ключ = значение", комментарии
// # и ;, "-" перед ключом (игнорировать ошибки), "/" как разделитель.
func ParseSysctlConf(content string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimPrefix(strings.TrimSpace(k), "-")
		out[strings.ReplaceAll(k, "/", ".")] = strings.TrimSpace(v)
	}
	return out
}

// ConfFile — файл конфигурации sysctl.
type ConfFile struct {
	Path   string // где найден
	Name   string // имя, по которому сортирует systemd-sysctl
	Values map[string]string
	Ours   bool // 99-ramplus.conf
}

// OrderSysctl — порядок применения: files подаются в порядке каталогов
// SysctlDirs; одноимённые файлы из менее приоритетных каталогов
// отбрасываются; затем сортировка по имени. extraLast — файлы, которые
// читаются после всех (/etc/sysctl.conf без симлинка).
func OrderSysctl(files []ConfFile, extraLast ...ConfFile) []ConfFile {
	seen := map[string]bool{}
	var out []ConfFile
	for _, f := range files {
		if seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return append(out, extraLast...)
}

// WithOurs — порядок, в котором окажется наш файл с заданными значениями.
func WithOurs(ordered []ConfFile, values map[string]string) []ConfFile {
	var files []ConfFile
	for _, f := range ordered {
		if !f.Ours {
			files = append(files, f)
		}
	}
	var main, last []ConfFile
	for _, f := range files {
		if f.Name == "" { // extraLast
			last = append(last, f)
		} else {
			main = append(main, f)
		}
	}
	main = append(main, ConfFile{Path: SysctlFilePath, Name: SysctlFileName, Values: values, Ours: true})
	sort.SliceStable(main, func(i, j int) bool { return main[i].Name < main[j].Name })
	return append(main, last...)
}

// Setters — файлы, задающие ключ, в порядке применения.
func Setters(ordered []ConfFile, key string) []ConfFile {
	var out []ConfFile
	for _, f := range ordered {
		if _, ok := f.Values[key]; ok {
			out = append(out, f)
		}
	}
	return out
}

// Overriders — файлы, которые применяются после нашего и задают key
// (они перебьют значение профиля при загрузке).
func Overriders(ordered []ConfFile, key string) []ConfFile {
	var out []ConfFile
	after := false
	for _, f := range ordered {
		if f.Ours {
			after = true
			continue
		}
		if _, ok := f.Values[key]; ok && after {
			out = append(out, f)
		}
	}
	return out
}

func sortedKeys(m map[string]string, group string) []string {
	var out []string
	for _, p := range Params {
		if _, ok := m[p.Key]; ok && p.Group == group {
			out = append(out, p.Key)
		}
	}
	return out
}

// SysctlKeys / ZswapKeys — ключи группы из набора значений (порядок реестра).
func SysctlKeys(values map[string]string) []string { return sortedKeys(values, GroupSysctl) }
func ZswapKeys(values map[string]string) []string  { return sortedKeys(values, GroupZswap) }

func header(what, name, status string) string {
	return fmt.Sprintf("%s: do not edit, use `ramplus profile`.\n# Профиль: %s (%s)\n", Marker, name, StatusText(i18n.RU, status)) +
		"# " + what + "\n"
}

// SysctlFileContent — /etc/sysctl.d/99-ramplus.conf.
func SysctlFileContent(name, status string, values map[string]string) string {
	var b strings.Builder
	b.WriteString(header("Применяется systemd-sysctl при загрузке; имя 99-ramplus.conf — чтобы идти после остальных.", name, status))
	for _, k := range SysctlKeys(values) {
		fmt.Fprintf(&b, "%s = %s\n", k, values[k])
	}
	return b.String()
}

// ZswapUnitContent — ramplus-zswap.service: при загрузке пишет значения в
// /sys/module/zswap/parameters. Значения встроены в юнит (не зависит от
// пути к бинарнику ramplus); cmdline и GRUB не трогаются. skip — ключи,
// которых нет в этом ядре (например, zpool в 7.0).
func ZswapUnitContent(name, status string, values map[string]string, skip map[string]bool) string {
	var b strings.Builder
	b.WriteString(header("Пишет параметры zswap при загрузке (kernel cmdline не меняется).", name, status))
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=RamPlus zswap parameters (profile %s)\n", name)
	b.WriteString("Documentation=https://github.com/ashot0907/ramplus\n")
	b.WriteString("DefaultDependencies=no\n")
	b.WriteString("After=local-fs.target systemd-modules-load.service\n")
	b.WriteString("Before=swap.target\n")
	b.WriteString("ConditionPathIsDirectory=" + ZswapParamDir + "\n\n")
	b.WriteString("[Service]\nType=oneshot\nRemainAfterExit=yes\n")
	for _, k := range ZswapWriteOrder(values["zswap.enabled"] == "Y") {
		v, ok := values[k]
		if !ok || skip[k] {
			continue
		}
		p, _ := Lookup(k)
		fmt.Fprintf(&b, "ExecStart=/bin/sh -c 'echo %s > %s'\n", v, p.Path)
	}
	b.WriteString("\n[Install]\nWantedBy=swap.target\n")
	return b.String()
}
