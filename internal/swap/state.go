package swap

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
)

// StateVersion — версия формата state.json.
//
//	1 — до write-ahead журнала: у записей applied нет поля status (все
//	    записи — выполненные шаги), пути /ramplus.swap, шаг remove_file.
//	2 — write-ahead журнал (status: pending|done). ID операций — с
//	    точностью до секунды: могли совпадать.
//	3 — уникальные ID (наносекунды + случайный суффикс), проверяются при
//	    загрузке.
const StateVersion = 3

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

// AppliedProfile — профиль, применённый RamPlus.
type AppliedProfile struct {
	Name       string            `json:"name"`
	Status     string            `json:"status"`      // standard | experimental | verified | modified
	BaseStatus string            `json:"base_status"` // статус профиля без --set
	Overrides  map[string]string `json:"overrides,omitempty"`
	Values     map[string]string `json:"values"` // ожидаемые runtime-значения
	AppliedAt  time.Time         `json:"applied_at"`
}

// Baseline — значения системы до первого apply профиля (для reset).
// Снимается один раз и не перезаписывается последующими apply.
type Baseline struct {
	Values     map[string]string `json:"values"`
	CapturedAt time.Time         `json:"captured_at"`
	OpID       string            `json:"op_id"`
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
	// ProfileBefore/After — активный профиль до и после (rollback
	// восстанавливает ProfileBefore). BaselineCaptured — эта операция
	// сняла исходные значения (её откат их забывает).
	ProfileBefore    *AppliedProfile `json:"profile_before,omitempty"`
	ProfileAfter     *AppliedProfile `json:"profile_after,omitempty"`
	BaselineCaptured bool            `json:"baseline_captured,omitempty"`
	Error            string          `json:"error,omitempty"`
}

// State — /var/lib/ramplus/state.json.
type State struct {
	Version    int             `json:"version"`
	Managed    *Managed        `json:"managed"`
	Profile    *AppliedProfile `json:"profile"`
	Baseline   *Baseline       `json:"baseline,omitempty"`
	Operations []Operation     `json:"operations"`
}

// ParseState разбирает state.json.
func ParseState(b []byte) (State, error) {
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%w: state.json: %v", detect.ErrParse, err)
	}
	switch s.Version {
	case 1:
		// В v1 журнал писался после шага: каждая запись — выполненный шаг.
		for i := range s.Operations {
			for j := range s.Operations[i].Applied {
				if s.Operations[i].Applied[j].Status == "" {
					s.Operations[i].Applied[j].Status = StepDone
				}
			}
		}
		fallthrough
	case 2:
		if hasDuplicateIDs(s.Operations) {
			migrateDuplicateIDs(&s)
		}
	case StateVersion:
		if hasDuplicateIDs(s.Operations) {
			return State{}, fmt.Errorf("%w: state.json: повторяющиеся ID операций в журнале v%d", detect.ErrParse, s.Version)
		}
	default:
		return State{}, fmt.Errorf("%w: state.json: неизвестная версия %d", detect.ErrParse, s.Version)
	}
	return s, nil
}

func hasDuplicateIDs(ops []Operation) bool {
	seen := map[string]bool{}
	for _, op := range ops {
		if seen[op.ID] {
			return true
		}
		seen[op.ID] = true
	}
	return false
}

// migrateDuplicateIDs чинит журнал v1/v2 с совпадающими ID (точность до
// секунды). Старая версия при коммите rollback помечала rolled_back ВСЕ
// операции с ID цели — статусы тоже могли испортиться. Поэтому:
//  1. ID делаются уникальными по позиции;
//  2. статусы rolled_back пересчитываются повтором стека: каждый выполненный
//     rollback отменяет последнюю неотменённую операцию перед ним (так же,
//     как LastUndoable), и его Target указывает на неё по новому ID.
func migrateDuplicateIDs(s *State) {
	counts := map[string]int{}
	for _, op := range s.Operations {
		counts[op.ID]++
	}
	for i := range s.Operations {
		if id := s.Operations[i].ID; counts[id] > 1 {
			s.Operations[i].ID = fmt.Sprintf("%s#%d", id, i+1)
		}
	}
	for i := range s.Operations {
		if s.Operations[i].Status == OpRolledBack {
			s.Operations[i].Status = OpCommitted
		}
	}
	for i := range s.Operations {
		op := &s.Operations[i]
		if op.Action != ActionRollback {
			continue
		}
		prefix := State{Operations: s.Operations[:i]}
		j, ok := prefix.LastUndoable()
		if !ok {
			op.Target = ""
			continue
		}
		op.Target = s.Operations[j].ID
		if op.Status == OpCommitted {
			s.Operations[j].Status = OpRolledBack
		}
	}
}

func countID(ops []Operation, id string) int {
	n := 0
	for _, op := range ops {
		if op.ID == id {
			n++
		}
	}
	return n
}

// NewOpID — уникальный ID операции: время UTC с наносекундами и случайный
// суффикс; при совпадении с уже существующим (часы стоят, тот же суффикс)
// добавляется счётчик.
func NewOpID(now time.Time, existing []Operation) string {
	base := now.UTC().Format("20060102T150405.000000000Z") + "-" + randSuffix()
	id := base
	for n := 2; countID(existing, id) > 0; n++ {
		id = fmt.Sprintf("%s.%d", base, n)
	}
	return id
}

func randSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b[:])
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
