package swap

import (
	"fmt"
	"sort"
)

// CleanProblems — что мешает считать систему чистой от RamPlus: по
// фактическому состоянию, а не по наличию state.json. Журнал с историей
// (все операции откачены, managed: null) — это чисто.
func CleanProblems(f Facts) []string {
	var out []string
	if f.StateErr != nil {
		out = append(out, fmt.Sprintf("не удалось прочитать %s: %v", StatePath, f.StateErr))
	}
	if m := f.State.Managed; m != nil {
		out = append(out, fmt.Sprintf("RamPlus управляет swap %s (%s)", m.Path, StatePath))
	}
	for _, op := range f.State.Operations {
		switch op.Status {
		case OpInProgress:
			out = append(out, fmt.Sprintf("незавершённая операция %s (%s) — sudo ramplus rollback", op.ID, op.Command))
		case OpRollbackFailed:
			out = append(out, fmt.Sprintf("операция %s (%s) откатилась не полностью — ручная проверка", op.ID, op.Command))
		}
	}
	for _, s := range f.Swaps {
		if f.isOurPath(s.Path) {
			out = append(out, "активен swap RamPlus "+s.Path)
		}
	}
	for _, u := range f.Units {
		out = append(out, "есть юнит RamPlus "+u)
	}
	var files []string
	for _, p := range []string{ManagedPath, BtrfsPath, BtrfsDir, LegacyPath} {
		if f.Exists[p] {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	for _, p := range files {
		out = append(out, "существует "+p)
	}
	return out
}
