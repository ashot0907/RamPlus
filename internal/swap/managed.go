package swap

import (
	"encoding/json"
	"time"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
)

// Managed — swap, которым управляет RamPlus (раздел managed журнала).
type Managed struct {
	Path      string    `json:"path"`
	Dir       string    `json:"dir,omitempty"` // подтом btrfs, созданный RamPlus
	SizeBytes uint64    `json:"size_bytes"`
	Priority  int       `json:"priority"`
	Unit      string    `json:"unit"`
	FSType    string    `json:"fs_type"`
	CreatedAt time.Time `json:"created_at"`
}

// decodeManaged — раздел журнала → Managed (nil — нет или не разобрать).
func decodeManaged(raw json.RawMessage) *Managed {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m Managed
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return &m
}

// ManagedOf — swap RamPlus из журнала.
func ManagedOf(st journal.State) *Managed { return decodeManaged(st.Managed) }

// EncodeManaged — Managed → раздел журнала.
func EncodeManaged(m *Managed) json.RawMessage {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	return b
}

// SetManaged — записать раздел managed (для тестов и миграций).
func SetManaged(st *journal.State, m *Managed) { st.Managed = EncodeManaged(m) }

// finalize — проставить время создания swap RamPlus при коммите.
func finalize(s *plan.Sections, now time.Time, _ string, _ bool) {
	if m := decodeManaged(s.Managed); m != nil && m.CreatedAt.IsZero() {
		m.CreatedAt = now
		s.Managed = EncodeManaged(m)
	}
}

// ManagedPaths — пути swap, которыми управляет RamPlus (для status).
func ManagedPaths(st journal.State) []string {
	if m := ManagedOf(st); m != nil {
		return []string{m.Path}
	}
	return nil
}
