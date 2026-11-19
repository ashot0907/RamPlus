// Package journal — движок изменений RamPlus: журнал /var/lib/ramplus/state.json
// (v3), операции и шаги с отменой, write-ahead (pending/done), стек отката,
// сверка состояния, миграции v1/v2→v3, блокировка. Про swap и профили не
// знает: шаги регистрируются через Register, разделы managed/profile/
// baseline хранятся как непрозрачный JSON.
package journal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/plan"
)

// Пути журнала.
const (
	StateDir  = "/var/lib/ramplus"
	StatePath = StateDir + "/state.json" // журнал операций
	BackupDir = StateDir + "/backups"    // копии изменённых файлов
	LockPath  = "/run/lock/ramplus.lock" // защита от параллельного запуска
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

// Operation — запись журнала об одной операции.
type Operation struct {
	ID         string      `json:"id"`
	Action     plan.Action `json:"action"`
	Command    string      `json:"command"`
	Status     string      `json:"status"`
	Target     string      `json:"target,omitempty"` // для rollback — ID отменяемой операции
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt time.Time   `json:"finished_at,omitzero"`
	Applied    []Applied   `json:"applied"`
	// Разделы до/после операции — содержимое задают домены (swap, profile).
	ManagedBefore json.RawMessage `json:"managed_before"`
	ManagedAfter  json.RawMessage `json:"managed_after"`
	// ProfileBefore/After — активный профиль до и после (rollback
	// восстанавливает ProfileBefore). BaselineCaptured — эта операция
	// сняла исходные значения (её откат их забывает).
	ProfileBefore    json.RawMessage `json:"profile_before,omitempty"`
	ProfileAfter     json.RawMessage `json:"profile_after,omitempty"`
	BaselineCaptured bool            `json:"baseline_captured,omitempty"`
	Error            string          `json:"error,omitempty"`
}

// State — /var/lib/ramplus/state.json.
type State struct {
	Version    int             `json:"version"`
	Managed    json.RawMessage `json:"managed"`            // swap RamPlus (internal/swap)
	Profile    json.RawMessage `json:"profile"`            // активный профиль (internal/profile)
	Baseline   json.RawMessage `json:"baseline,omitempty"` // исходные значения (internal/profile)
	Operations []Operation     `json:"operations"`
}

// raw — null и пусто приводятся к nil (как прежние nil-указатели: с
// omitempty поле не пишется).
func raw(m json.RawMessage) json.RawMessage {
	if len(m) == 0 || string(m) == "null" {
		return nil
	}
	return m
}

func (s *State) normalize() {
	s.Managed, s.Profile, s.Baseline = raw(s.Managed), raw(s.Profile), raw(s.Baseline)
	for i := range s.Operations {
		op := &s.Operations[i]
		op.ManagedBefore, op.ManagedAfter = raw(op.ManagedBefore), raw(op.ManagedAfter)
		op.ProfileBefore, op.ProfileAfter = raw(op.ProfileBefore), raw(op.ProfileAfter)
	}
}

// ParseState разбирает state.json.
func ParseState(b []byte) (State, error) {
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%w: state.json: %v", detect.ErrParse, err)
	}
	s.normalize()
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
		if op.Action != plan.ActionRollback {
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
		case op.Action == plan.ActionRollback:
			continue
		case op.Status == OpCommitted:
			return i, true
		}
	}
	return -1, false
}

// Undoable — какие операции можно отменить повторными ramplus rollback
// (по позиции в журнале). Выполненные и прерванные операции (кроме самих
// rollback) — да, пока между ними и концом журнала нет операции с
// неудавшимся откатом: на ней rollback останавливается (PlanRollback).
func (s State) Undoable() []bool {
	out := make([]bool, len(s.Operations))
	for i := len(s.Operations) - 1; i >= 0; i-- {
		op := s.Operations[i]
		if op.Status == OpRollbackFailed {
			break
		}
		out[i] = op.Status == OpInProgress || (op.Status == OpCommitted && op.Action != plan.ActionRollback)
	}
	return out
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
