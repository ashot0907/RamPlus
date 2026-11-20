package journal

import (
	"time"

	"github.com/ashot0907/ramplus/internal/plan"
)

// HistoryEntry — операция из журнала.
type HistoryEntry struct {
	ID         string      `json:"id"`
	Action     plan.Action `json:"action"`
	Command    string      `json:"command"`
	Status     string      `json:"status"`
	Target     string      `json:"target,omitempty"` // rollback: ID отменённой операции
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt time.Time   `json:"finished_at,omitzero"`
	Steps      int         `json:"steps"` // шагов в журнале (у откаченных и упавших — 0)
	Error      string      `json:"error,omitempty"`
	// Undoable — операцию можно отменить повторными ramplus rollback.
	Undoable bool `json:"undoable"`
	// RollbackNext — её отменит следующий ramplus rollback.
	RollbackNext bool `json:"rollback_next"`
	// Title — название для человека («Запас 16 GB из SSD + режим «Сжатие —
	// быстрое»»); Change — то же как данные (для перевода в вебе).
	// Заполняет core: журнал не знает swap и профилей.
	Title  string           `json:"title"`
	Change *plan.ChangeView `json:"change,omitempty"`
	// Source — cli | daemon | unknown (журнал без этого поля);
	// RequestedBy — кто запросил (nil — неизвестно).
	Source      string     `json:"source"`
	RequestedBy *Requester `json:"requested_by,omitempty"`
}

// History — операции журнала в порядке выполнения.
type History struct {
	SchemaVersion  int            `json:"schema_version"`
	Journal        string         `json:"journal"`
	JournalVersion int            `json:"journal_version"`
	Operations     []HistoryEntry `json:"operations"`
}

// HistorySchemaVersion — версия JSON-схемы History.
const HistorySchemaVersion = 1

// HistoryOf — история по журналу st.
func HistoryOf(st State) History {
	h := History{SchemaVersion: HistorySchemaVersion, Journal: StatePath, JournalVersion: StateVersion, Operations: []HistoryEntry{}}
	if st.Version != 0 {
		h.JournalVersion = st.Version
	}
	undoable := st.Undoable()
	next, hasNext := st.LastUndoable()
	for i, op := range st.Operations {
		h.Operations = append(h.Operations, HistoryEntry{
			ID: op.ID, Action: op.Action, Command: op.Command, Status: op.Status, Target: op.Target,
			StartedAt: op.StartedAt, FinishedAt: op.FinishedAt, Steps: len(op.Applied), Error: op.Error,
			Undoable: undoable[i], RollbackNext: hasNext && i == next && undoable[i],
			Source: sourceOf(op), RequestedBy: op.RequestedBy,
		})
	}
	return h
}

func sourceOf(op Operation) string {
	if op.Source == "" {
		return SourceUnknown
	}
	return op.Source
}
