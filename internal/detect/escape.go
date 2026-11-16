package detect

import "strings"

// UnescapeOctal раскрывает восьмеричные escape-последовательности вида
// \040, которыми ядро экранирует пробелы, табы, переводы строк и обратный
// слэш в /proc/self/mountinfo и /proc/swaps.
func UnescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && isOctal3(s[i+1:]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal3(s string) bool {
	if len(s) < 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return s[0] <= '3' // значение должно поместиться в байт
}
