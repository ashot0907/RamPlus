package swap

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
)

// FstabEntry — активная (не закомментированная) строка /etc/fstab.
type FstabEntry struct {
	Line   string // строка целиком, как в файле
	Spec   string // /swap.img, UUID=..., /dev/sda3
	File   string
	VFS    string
	Opts   string
	LineNo int
}

// ParseFstab разбирает fstab. Комментарии и пустые строки пропускаются,
// строки с <4 полями — тоже (их не трогаем).
func ParseFstab(content string) []FstabEntry {
	var out []FstabEntry
	for i, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		f := strings.Fields(t)
		if len(f) < 4 {
			continue
		}
		out = append(out, FstabEntry{
			Line: line, Spec: detect.UnescapeOctal(f[0]), File: detect.UnescapeOctal(f[1]),
			VFS: f[2], Opts: f[3], LineNo: i + 1,
		})
	}
	return out
}

// SwapEntries возвращает строки fstab с типом swap.
func SwapEntries(entries []FstabEntry) []FstabEntry {
	var out []FstabEntry
	for _, e := range entries {
		if e.VFS == "swap" {
			out = append(out, e)
		}
	}
	return out
}

// SpecCandidates — пути, в которые может разрешаться spec из fstab.
// UUID=/LABEL=/PARTUUID=/PARTLABEL= указывают на /dev/disk/by-*.
func SpecCandidates(spec string) []string {
	for prefix, dir := range map[string]string{
		"UUID=": "by-uuid", "LABEL=": "by-label", "PARTUUID=": "by-partuuid", "PARTLABEL=": "by-partlabel",
	} {
		if v, ok := strings.CutPrefix(spec, prefix); ok {
			return []string{"/dev/disk/" + dir + "/" + strings.Trim(v, `"`)}
		}
	}
	return []string{spec}
}

// DisableFstabLine комментирует строку line (точное совпадение) с пометкой
// RamPlus. Ошибка, если строка не найдена или встречается несколько раз.
func DisableFstabLine(content, line string) (string, error) {
	lines := strings.Split(content, "\n")
	idx := -1
	for i, l := range lines {
		if l == line {
			if idx >= 0 {
				return "", i18n.Errorf("fstab.err.duplicate", line)
			}
			idx = i
		}
	}
	if idx < 0 {
		return "", i18n.Errorf("fstab.err.not_found", line)
	}
	lines[idx] = fstabDisabledBy + line
	return strings.Join(lines, "\n"), nil
}

// EnableFstabLine снимает комментарий RamPlus со строки line. Остальное
// содержимое (в т.ч. правки пользователя после apply) не трогается.
func EnableFstabLine(content, line string) (string, error) {
	marked := fstabDisabledBy + line
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if l == marked {
			lines[i] = line
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", i18n.Errorf("fstab.err.not_disabled", line)
}
