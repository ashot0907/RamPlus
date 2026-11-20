package journal

import (
	"github.com/ashot0907/ramplus/internal/i18n"
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
func VerifyPending(s sys.System, a Applied) (Verdict, i18n.Text) {
	h, ok := handler(a.Step.Kind)
	if !ok || h.Verify == nil {
		return VerdictAbsent, i18n.Msg("verify.nothing")
	}
	return h.Verify(&Env{Sys: s}, a)
}

// Expectation — каким должно быть состояние после выполненного шага.
// Следующая ожидаемость с тем же Key заменяет предыдущую (например, файл
// создан, потом удалён).
type Expectation struct {
	Key  string
	Path string // для ignore ("" — не игнорируется)
	// Check — расхождение (nil — совпадает).
	Check func(s sys.System) i18n.Text
}

// ConfigMax — файлы до этого размера читаются целиком (конфиги).
const ConfigMax = 1 << 20

// FileExpect — файл (не) существует; size > 0 — с таким размером;
// content != nil — с таким содержимым.
func FileExpect(path string, exists, dir bool, size uint64, content *string) Expectation {
	return Expectation{Key: "file:" + path, Path: path, Check: func(s sys.System) i18n.Text {
		st, err := s.FS.Stat(path)
		got := err == nil
		switch {
		case exists && !got:
			return i18n.Msg("expect.file_missing", path)
		case !exists && got:
			return i18n.Msg("expect.file_exists", path)
		case !exists:
			return nil
		case !dir && size > 0 && uint64(st.Size()) != size:
			return i18n.Msg("expect.file_size", path, plan.FormatSize(uint64(st.Size())), plan.FormatSize(size))
		case content != nil && st.Mode().IsRegular() && st.Size() <= ConfigMax:
			if b, err := s.FS.ReadFile(path); err == nil && string(b) != *content {
				return i18n.Msg("expect.file_content", path)
			}
		}
		return nil
	}}
}

// CheckExpected сверяет текущее состояние системы с результатом операции
// op по журналу (выполненные шаги, по порядку). Возвращает расхождения;
// пустой список — можно откатывать. ignore — пути прерванного шага: их
// состояние неизвестно и проверяется отдельно (VerifyPending).
func CheckExpected(s sys.System, op Operation, ignore map[string]bool) []i18n.Text {
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
	var out []i18n.Text
	for _, k := range order {
		e := byKey[k]
		if e.Path != "" && ignore[e.Path] {
			continue
		}
		if msg := e.Check(s); msg != nil {
			out = append(out, msg)
		}
	}
	return out
}
