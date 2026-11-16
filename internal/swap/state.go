package swap

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
)

// StateVersion — версия формата state.json.
const StateVersion = 1

// Статусы операций.
const (
	OpInProgress     = "in_progress"     // идёт (или процесс упал посреди — rollback разберётся)
	OpCommitted      = "committed"       // выполнена
	OpFailed         = "failed"          // упала, шаги откачены
	OpRollbackFailed = "rollback_failed" // упала, откат не полный
	OpRolledBack     = "rolled_back"     // отменена через ramplus rollback
)

// Managed — swap, которым управляет RamPlus.
type Managed struct {
	Path      string    `json:"path"`
	Dir       string    `json:"dir,omitempty"` // подтом btrfs, созданный RamPlus
	SizeBytes uint64    `json:"size_bytes"`
	Priority  int       `json:"priority"`
	Unit      string    `json:"unit"`
	FSType    string    `json:"fs_type"`
	CreatedAt time.Time `json:"created_at"`
}

// Operation — запись журнала об одной операции.
type Operation struct {
	ID            string    `json:"id"`
	Action        Action    `json:"action"`
	Command       string    `json:"command"`
	Status        string    `json:"status"`
	Target        string    `json:"target,omitempty"` // для rollback — ID отменяемой операции
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
	Applied       []Applied `json:"applied"`
	ManagedBefore *Managed  `json:"managed_before"`
	ManagedAfter  *Managed  `json:"managed_after"`
	Error         string    `json:"error,omitempty"`
}

// State — /var/lib/ramplus/state.json.
type State struct {
	Version    int         `json:"version"`
	Managed    *Managed    `json:"managed"`
	Operations []Operation `json:"operations"`
}

// ParseState разбирает state.json.
func ParseState(b []byte) (State, error) {
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%w: state.json: %v", detect.ErrParse, err)
	}
	if s.Version != StateVersion {
		return State{}, fmt.Errorf("%w: state.json: неизвестная версия %d", detect.ErrParse, s.Version)
	}
	return s, nil
}

// Marshal сериализует состояние.
func (s State) Marshal() ([]byte, error) {
	s.Version = StateVersion
	if s.Operations == nil {
		s.Operations = []Operation{}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// LastUndoable — индекс последней операции, которую может отменить rollback:
// прерванная (in_progress после падения процесса) или последняя выполненная.
// Выполненные rollback, отменённые и неудачные (уже откаченные) операции
// пропускаются, поэтому повторные rollback идут по стеку назад.
func (s State) LastUndoable() (int, bool) {
	for i := len(s.Operations) - 1; i >= 0; i-- {
		op := s.Operations[i]
		switch {
		case op.Status == OpInProgress:
			// Прерванная операция (в т.ч. прерванный rollback) отменяется
			// первой: система возвращается к состоянию до неё.
			return i, true
		case op.Action == ActionRollback:
			continue
		case op.Status == OpCommitted:
			return i, true
		}
	}
	return -1, false
}

// Pending — незавершённая операция (процесс упал посреди apply).
func (s State) Pending() *Operation {
	for i := len(s.Operations) - 1; i >= 0; i-- {
		if s.Operations[i].Status == OpInProgress {
			return &s.Operations[i]
		}
	}
	return nil
}

// ManagedPaths — пути swap, которыми управляет RamPlus (для status).
func ManagedPaths(s State) []string {
	if s.Managed != nil {
		return []string{s.Managed.Path}
	}
	return nil
}
