package swap

import (
	"fmt"

	"github.com/ashot0907/ramplus/internal/plan"
	"strconv"
	"strings"
)

const (
	KiB = plan.KiB
	MiB = plan.MiB
	GiB = plan.GiB
	TiB = plan.TiB
)

// MinSize — минимальный размер swapfile.
const MinSize = 256 * MiB

// ParseSize разбирает размер: "12G", "12GiB", "512M", "1.5G", "1T".
// Суффиксы двоичные (G = GiB). Без суффикса — байты. Результат округляется
// вниз до целых МиБ.
func ParseSize(s string) (uint64, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, fmt.Errorf("пустой размер")
	}
	num := strings.TrimRight(in, "KMGTkmgtiBb")
	suffix := strings.ToUpper(strings.TrimSpace(in[len(num):]))
	suffix = strings.TrimSuffix(strings.TrimSuffix(suffix, "B"), "I")
	mult := map[string]float64{"": 1, "K": KiB, "M": MiB, "G": GiB, "T": TiB}
	m, ok := mult[suffix]
	if !ok {
		return 0, fmt.Errorf("неизвестная единица в %q (используйте K, M, G, T)", s)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("неверный размер %q", s)
	}
	b := v * m
	if b > 1<<60 {
		return 0, fmt.Errorf("слишком большой размер %q", s)
	}
	if r := uint64(b) / MiB * MiB; r > 0 {
		return r, nil
	}
	return 0, fmt.Errorf("слишком маленький размер %q", s)
}

// FormatSize — человекочитаемый размер в IEC-единицах.
func FormatSize(b uint64) string { return plan.FormatSize(b) }

// sizeFlag — размер в виде, пригодном для --size (без потери точности).
func sizeFlag(b uint64) string {
	switch {
	case b%GiB == 0:
		return fmt.Sprintf("%dG", b/GiB)
	default:
		return fmt.Sprintf("%dM", b/MiB)
	}
}

// EscapePath повторяет `systemd-escape --path`: убирает крайние "/",
// заменяет "/" на "-", остальные символы вне [A-Za-z0-9:_.] и ведущую
// точку — на \xNN.
func EscapePath(p string) string {
	p = strings.Trim(p, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if p == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == '.' && i == 0:
			fmt.Fprintf(&b, `\x%02x`, c)
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_', c == '.':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

// UnitName — имя systemd .swap-юнита для пути.
func UnitName(path string) string { return EscapePath(path) + ".swap" }

// UnitContent — содержимое .swap-юнита RamPlus.
func UnitContent(path string, priority int) string {
	return unitHeader +
		"[Unit]\n" +
		"Description=RamPlus swapfile " + path + "\n" +
		"Documentation=https://github.com/ashot0907/ramplus\n" +
		"\n" +
		"[Swap]\n" +
		"What=" + path + "\n" +
		"Priority=" + strconv.Itoa(priority) + "\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=swap.target\n"
}

// ResumeParam возвращает значение resume= из /proc/cmdline ("" — нет).
func ResumeParam(cmdline string) string {
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, "resume="); ok {
			return v
		}
	}
	return ""
}

// SizeFlag — размер для командной строки: 4G, 512M.
func SizeFlag(b uint64) string { return sizeFlag(b) }
