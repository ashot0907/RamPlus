package profile

import (
	"encoding/json"
	"time"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
)

// AppliedProfile — профиль, применённый RamPlus (раздел profile журнала).
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

func decode[T any](raw json.RawMessage) *T {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return &v
}

func encode[T any](v *T) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

// ProfileOf — активный профиль из журнала.
func ProfileOf(st journal.State) *AppliedProfile { return decode[AppliedProfile](st.Profile) }

// BaselineOf — исходные значения из журнала.
func BaselineOf(st journal.State) *Baseline { return decode[Baseline](st.Baseline) }

// EncodeProfile / EncodeBaseline — разделы журнала.
func EncodeProfile(p *AppliedProfile) json.RawMessage { return encode(p) }
func EncodeBaseline(b *Baseline) json.RawMessage      { return encode(b) }

// finalize — время применения профиля и снятия исходных значений.
func finalize(s *plan.Sections, now time.Time, opID string, baselineCaptured bool) {
	if p := decode[AppliedProfile](s.Profile); p != nil && p.AppliedAt.IsZero() {
		p.AppliedAt = now
		s.Profile = encode(p)
	}
	if baselineCaptured {
		if b := decode[Baseline](s.Baseline); b != nil {
			b.CapturedAt, b.OpID = now, opID
			s.Baseline = encode(b)
		}
	}
}
