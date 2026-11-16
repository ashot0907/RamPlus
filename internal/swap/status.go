package swap

import (
	"github.com/ashot0907/ramplus/internal/monitor"
)

// WarnInterrupted — в журнале есть незавершённая операция RamPlus.
const WarnInterrupted = "ramplus_interrupted_operation"

// AnnotateStatus помечает swap, которым управляет RamPlus (managed: true).
// Читает state.json; если он недоступен, узнаёт свой swap по пути.
func AnnotateStatus(st *monitor.Status, fsys FS) {
	paths := map[string]bool{ManagedPath: true, BtrfsPath: true}
	if b, err := fsys.ReadFile(StatePath); err == nil {
		if s, err := ParseState(b); err == nil {
			for _, p := range ManagedPaths(s) {
				paths[p] = true
			}
			if op := s.Pending(); op != nil {
				st.Swaps.Warnings = append(st.Swaps.Warnings, monitor.SwapWarning{Code: WarnInterrupted, Path: StatePath})
			}
		}
	}
	for i := range st.Swaps.Devices {
		st.Swaps.Devices[i].Managed = paths[st.Swaps.Devices[i].Path]
	}
}
