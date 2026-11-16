package swap

import (
	"github.com/ashot0907/ramplus/internal/monitor"
)

// AnnotateStatus помечает swap, которым управляет RamPlus (managed: true).
// Читает state.json; если он недоступен, узнаёт свой swap по пути.
func AnnotateStatus(st *monitor.Status, fsys FS) {
	paths := map[string]bool{ManagedPath: true, BtrfsPath: true}
	if b, err := fsys.ReadFile(StatePath); err == nil {
		if s, err := ParseState(b); err == nil {
			for _, p := range ManagedPaths(s) {
				paths[p] = true
			}
		}
	}
	for i := range st.Swaps.Devices {
		st.Swaps.Devices[i].Managed = paths[st.Swaps.Devices[i].Path]
	}
}
