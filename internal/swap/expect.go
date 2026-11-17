package swap

import (
	"fmt"
	"sort"
	"strings"
)

// swapSizeSlack — /proc/swaps показывает размер без заголовка (страница)
// и в KiB; допускаем такое расхождение с размером файла.
const swapSizeSlack = 1 << 20

type expectedFile struct {
	exists  bool
	dir     bool
	size    uint64 // 0 — не проверять
	content *string
}

type expectedSwap struct {
	active   bool
	priority int
	size     uint64
}

// CheckExpected сверяет текущее состояние системы с результатом операции
// op по журналу (выполненные шаги, по порядку). Возвращает расхождения;
// пустой список — можно откатывать. ignore — пути прерванного шага: их
// состояние неизвестно и проверяется отдельно (VerifyPending).
func CheckExpected(f Facts, op Operation, ignore map[string]bool) []string {
	files := map[string]expectedFile{}
	swaps := map[string]expectedSwap{}
	fstab := map[string]bool{} // строка → должна быть закомментирована RamPlus
	for _, a := range op.Applied {
		if a.Status != StepDone {
			continue
		}
		s := a.Step
		switch s.Kind {
		case KindAllocate, KindBtrfsMkswapfile:
			files[s.Path] = expectedFile{exists: true, size: s.Size}
		case KindDeleteSwapfile, KindDeleteConfig, KindRemoveFile:
			files[s.Path] = expectedFile{}
		case KindWriteFile:
			c := s.Content
			files[s.Path] = expectedFile{exists: true, content: &c}
		case KindMkdir, KindBtrfsSubvolCreate:
			files[s.Path] = expectedFile{exists: true, dir: true}
		case KindRmdir, KindBtrfsSubvolDelete:
			files[s.Path] = expectedFile{}
		case KindSwapon:
			swaps[s.Path] = expectedSwap{active: true, priority: s.Priority, size: s.Size}
		case KindSwapoff:
			swaps[s.Path] = expectedSwap{}
		case KindFstabDisable:
			fstab[s.Line] = true
		case KindFstabEnable:
			fstab[s.Line] = false
		}
	}

	var out []string
	for path, want := range files {
		if ignore[path] {
			continue
		}
		got, ok := f.Observed[path]
		if !ok {
			continue // не наблюдали — Gather не знал об этом пути
		}
		switch {
		case want.exists && !got.Exists:
			out = append(out, fmt.Sprintf("%s: нет, а по журналу должен существовать", path))
		case !want.exists && got.Exists:
			out = append(out, fmt.Sprintf("%s: существует, а по журналу удалён", path))
		case want.exists && !want.dir && want.size > 0 && got.Size != want.size:
			out = append(out, fmt.Sprintf("%s: размер %s, по журналу %s", path, FormatSize(got.Size), FormatSize(want.size)))
		case want.exists && want.content != nil && got.HasContent && got.Content != *want.content:
			out = append(out, fmt.Sprintf("%s: содержимое отличается от записанного RamPlus", path))
		}
	}
	for path, want := range swaps {
		if ignore[path] {
			continue
		}
		got, active := f.swapByPath(path)
		switch {
		case want.active && !active:
			out = append(out, fmt.Sprintf("swap %s не активен, а по журналу включён", path))
		case !want.active && active:
			out = append(out, fmt.Sprintf("swap %s активен, а по журналу отключён", path))
		case want.active && want.priority >= 0 && got.Priority != want.priority:
			out = append(out, fmt.Sprintf("swap %s: приоритет %d, по журналу %d", path, got.Priority, want.priority))
		case want.active && want.size > 0 && (got.SizeBytes > want.size || want.size-got.SizeBytes > swapSizeSlack):
			out = append(out, fmt.Sprintf("swap %s: размер %s, по журналу %s", path, FormatSize(got.SizeBytes), FormatSize(want.size)))
		}
	}
	for line, marked := range fstab {
		has := hasLine(f.Fstab, fstabDisabledBy+line)
		plain := hasLine(f.Fstab, line)
		switch {
		case f.FstabErr != nil:
			out = append(out, fmt.Sprintf("%s недоступен: %v", FstabPath, f.FstabErr))
		case marked && (!has || plain):
			out = append(out, fmt.Sprintf("%s: строка «%s» должна быть закомментирована RamPlus", FstabPath, strings.TrimSpace(line)))
		case !marked && (has || !plain):
			out = append(out, fmt.Sprintf("%s: строка «%s» должна быть активна", FstabPath, strings.TrimSpace(line)))
		}
	}
	sort.Strings(out)
	return out
}
