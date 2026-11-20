package plan

import (
	"encoding/json"

	"github.com/ashot0907/ramplus/internal/i18n"
)

// SchemaVersion — версия JSON-схемы плана (--json, API демона).
const SchemaVersion = 1

// StepView — шаг плана в JSON: машинный код (kind) с параметрами и
// описание для человека отдельно.
type StepView struct {
	Step
	Description string `json:"description"`
}

// ViewOf — шаг для JSON с описанием на языке l.
func ViewOf(s Step, l i18n.Lang) StepView { return StepView{Step: s, Description: s.Describe(l)} }

// planJSON — JSON-представление плана. Порядок и имена полей — часть
// публичной схемы (schema_version).
type planJSON struct {
	SchemaVersion int           `json:"schema_version"`
	ID            string        `json:"id,omitempty"`
	Fingerprint   string        `json:"fingerprint"`
	Action        Action        `json:"action"`
	Command       string        `json:"command"`
	Blocked       bool          `json:"blocked"`
	Choices       []Choice      `json:"choices"`
	Checks        []Check       `json:"checks"`
	Steps         []StepView    `json:"steps"`
	Summary       Summary       `json:"summary"`
	Swap          *SwapView     `json:"swap,omitempty"`
	Profile       *ProfileView  `json:"profile,omitempty"`
	Rollback      *RollbackView `json:"rollback,omitempty"`
}

// MarshalJSON — план как данные: стабильные коды проверок и шагов, тексты
// отдельно, массивы никогда не null. Effects (что запишет журнал) наружу
// не отдаются.
func (p Plan) MarshalJSON() ([]byte, error) {
	j := planJSON{
		SchemaVersion: SchemaVersion,
		ID:            p.ID,
		Fingerprint:   p.Fingerprint,
		Action:        p.Action,
		Command:       p.Command,
		Blocked:       p.Blocked(),
		Choices:       nonNil(p.Choices),
		Checks:        nonNil(p.Checks),
		Steps:         make([]StepView, 0, len(p.Steps)),
		Summary:       p.Summary,
		Swap:          p.Swap,
		Profile:       p.Profile,
		Rollback:      p.Rollback,
	}
	if p.Profile != nil && p.Profile.Rows == nil {
		v := *p.Profile
		v.Rows = []ProfileRow{}
		j.Profile = &v
	}
	for _, s := range p.Steps {
		j.Steps = append(j.Steps, ViewOf(s, p.Lang))
	}
	j.Summary.Created = nonNil(j.Summary.Created)
	j.Summary.Disabled = nonNil(j.Summary.Disabled)
	j.Summary.Deleted = nonNil(j.Summary.Deleted)
	return json.Marshal(j)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
