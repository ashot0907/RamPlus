package swap

import (
	"strings"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/profile"
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
			annotateProfile(st, s.Profile, fsys)
		}
	}
	for i := range st.Swaps.Devices {
		st.Swaps.Devices[i].Managed = paths[st.Swaps.Devices[i].Path]
	}
}

// annotateProfile — активный профиль и дрейф runtime-значений.
func annotateProfile(st *monitor.Status, ap *AppliedProfile, fsys FS) {
	if st.Drift == nil {
		st.Drift = []monitor.DriftItem{}
	}
	if ap == nil {
		return
	}
	st.Profile = &monitor.ProfileInfo{Name: ap.Name, Status: ap.Status, Overrides: ap.Overrides, AppliedAt: ap.AppliedAt}
	values := map[string]string{}
	for _, p := range profile.Params {
		if b, err := fsys.ReadFile(p.Path); err == nil {
			values[p.Key] = strings.TrimSpace(string(b))
		}
	}
	for _, d := range ProfileDrift(ap, values) {
		st.Drift = append(st.Drift, monitor.DriftItem{Key: d.Key, Expected: d.New, Actual: dash(d.Current)})
	}
}
