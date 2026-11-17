package journal

import (
	"fmt"
	"sort"

	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/sys"
)

// Verdict — что осталось в системе от шага, прерванного на середине.
type Verdict string

const (
	VerdictAbsent   Verdict = "absent"   // шаг не успел изменить систему — откатывать нечего
	VerdictPresent  Verdict = "present"  // артефакт есть и совпадает с записью — откатить
	VerdictMismatch Verdict = "mismatch" // что-то есть, но не то, что записано — нужна ручная проверка
)

// VerifyPending проверяет артефакт записи со статусом pending (процесс
// упал между записью pending и done). Только чтение.
func VerifyPending(s sys.System, a Applied) (Verdict, string) {
	h, ok := handler(a.Step.Kind)
	if !ok || h.Verify == nil {
		return VerdictAbsent, "отменять нечего"
	}
	return h.Verify(&Env{Sys: s}, a)
}

// Expectation — каким должно быть состояние после выполненного шага.
// Следующая ожидаемость с тем же Key заменяет предыдущую (например, файл
// создан, потом удалён).
type Expectation struct {
	Key   string
	Path  string // для ignore ("" — не игнорируется)
	Check func(s sys.System) string
}

// ConfigMax — файлы до этого размера читаются целиком (конфиги).
const ConfigMax = 1 << 20

// FileExpect — файл (не) существует; size > 0 — с таким размером;
// content != nil — с таким содержимым.
func FileExpect(path string, exists, dir bool, size uint64, content *string) Expectation {
	return Expectation{Key: "file:" + path, Path: path, Check: func(s sys.System) string {
		st, err := s.FS.Stat(path)
		got := err == nil
		switch {
		case exists && !got:
			return fmt.Sprintf("%s: нет, а по журналу должен существовать", path)
		case !exists && got:
			return fmt.Sprintf("%s: существует, а по журналу удалён", path)
		case !exists:
			return ""
		case !dir && size > 0 && uint64(st.Size()) != size:
			return fmt.Sprintf("%s: размер %s, по журналу %s", path, plan.FormatSize(uint64(st.Size())), plan.FormatSize(size))
		case content != nil && st.Mode().IsRegular() && st.Size() <= ConfigMax:
			if b, err := s.FS.ReadFile(path); err == nil && string(b) != *content {
				return fmt.Sprintf("%s: содержимое отличается от записанного RamPlus", path)
			}
		}
		return ""
	}}
}

// CheckExpected сверяет текущее состояние системы с результатом операции
// op по журналу (выполненные шаги, по порядку). Возвращает расхождения;
// пустой список — можно откатывать. ignore — пути прерванного шага: их
// состояние неизвестно и проверяется отдельно (VerifyPending).
func CheckExpected(s sys.System, op Operation, ignore map[string]bool) []string {
	var order []string
	byKey := map[string]Expectation{}
	for _, a := range op.Applied {
		if a.Status != StepDone {
			continue
		}
		h, ok := handler(a.Step.Kind)
		if !ok || h.Expect == nil {
			continue
		}
		for _, e := range h.Expect(a.Step) {
			if _, seen := byKey[e.Key]; !seen {
				order = append(order, e.Key)
			}
			byKey[e.Key] = e
		}
	}
	var out []string
	for _, k := range order {
		e := byKey[k]
		if e.Path != "" && ignore[e.Path] {
			continue
		}
		if msg := e.Check(s); msg != "" {
			out = append(out, msg)
		}
	}
	sort.Strings(out)
	return out
}
