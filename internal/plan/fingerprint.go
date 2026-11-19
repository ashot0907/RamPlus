package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Fingerprint — отпечаток фактов, на которых построен план: swap, ФС и
// свободное место, значения параметров, журнал. Apply по ID плана заново
// собирает факты и отказывается, если отпечаток изменился. Части —
// любые JSON-сериализуемые значения (ключи map сортируются, поэтому
// результат детерминирован).
func Fingerprint(parts ...any) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, p := range parts {
		if err := enc.Encode(p); err != nil {
			// Несериализуемая часть — отпечаток, который ни с чем не совпадёт.
			_, _ = h.Write([]byte("unencodable: " + err.Error()))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// decision — что план решил (без текстов): шаги и вердикты проверок.
// Входит в отпечаток: факты, которые не попали в него явно (например,
// занятость swap для проверки swapoff), учитываются через вердикты.
type decision struct {
	Action  Action
	Command string
	Steps   []Step
	Checks  []checkVerdict
}

type checkVerdict struct {
	Code  string
	Level Level
}

// Seal вычисляет отпечаток плана по фактам и решению плана; вызывается
// планировщиком последним.
func (p *Plan) Seal(facts ...any) {
	d := decision{Action: p.Action, Command: p.Command, Steps: p.Steps}
	for _, c := range p.Checks {
		d.Checks = append(d.Checks, checkVerdict{c.Code, c.Level})
	}
	p.Fingerprint = Fingerprint(append(facts, d)...)
}
