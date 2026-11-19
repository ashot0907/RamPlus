package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/sys"
)

// Recovery — незавершённая операция, найденная в журнале.
type Recovery struct {
	OpID    string
	Pending *Applied // запись pending (процесс упал посреди шага), может быть nil
	Verdict Verdict
	Detail  string
}

// Basics — общие для всех планов факты: права, systemd, журнал,
// незавершённая операция, сверка состояния для rollback.
type Basics struct {
	EUID     int
	Systemd  bool
	State    State
	StateErr error
	Recovery *Recovery
	// RollbackDiff — расхождения состояния системы с результатом операции,
	// которую отменит rollback (nil — расхождений нет или откатывать нечего).
	RollbackDiff []string
	// SwapsErr — не удалось прочитать /proc/swaps (заполняет домен).
	SwapsErr error
	// Now — время сбора фактов (возраст отменяемой операции). В отпечаток
	// плана не входит.
	Now time.Time
}

// LoadState читает журнал; нет файла — пустое состояние.
func LoadState(s sys.System) (State, error) {
	b, err := s.FS.ReadFile(StatePath)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	return ParseState(b)
}

// GatherBasics собирает общие факты. Только чтение.
func GatherBasics(s sys.System) Basics {
	b := Basics{EUID: s.Kernel.Geteuid(), Now: s.Now()}
	b.State, b.StateErr = LoadState(s)
	if b.StateErr != nil {
		b.State = State{}
	}
	_, err := s.FS.Stat(sys.SystemdRunDir)
	b.Systemd = err == nil

	if op := b.State.Pending(); op != nil {
		rec := &Recovery{OpID: op.ID}
		if n := len(op.Applied); n > 0 && op.Applied[n-1].Status == StepPending {
			a := op.Applied[n-1]
			rec.Pending = &a
			rec.Verdict, rec.Detail = VerifyPending(s, a)
		}
		b.Recovery = rec
	}
	if idx, ok := b.State.LastUndoable(); ok {
		op := b.State.Operations[idx]
		b.RollbackDiff = CheckExpected(s, op, b.ignore(op))
	}
	return b
}

// ignore — пути прерванного шага операции op.
func (b Basics) ignore(op Operation) map[string]bool {
	ignore := map[string]bool{}
	if r := b.Recovery; r != nil && r.OpID == op.ID && r.Pending != nil {
		ignore[r.Pending.Step.Path] = true
	}
	return ignore
}

// FingerprintPart — часть отпечатка плана от журнала и общих фактов:
// версия формата, операции и их статусы, разделы, незавершённая операция.
func (b Basics) FingerprintPart() any {
	type op struct{ ID, Status string }
	part := struct {
		Version              int
		EUID                 int
		Systemd              bool
		StateErr             string
		Ops                  []op
		Managed, Profile, Bl json.RawMessage
		Recovery             *Recovery
		RollbackDiff         []string
		SwapsErr             string
	}{Version: StateVersion, EUID: b.EUID, Systemd: b.Systemd, StateErr: errText(b.StateErr),
		Managed: b.State.Managed, Profile: b.State.Profile, Bl: b.State.Baseline,
		Recovery: b.Recovery, RollbackDiff: b.RollbackDiff, SwapsErr: errText(b.SwapsErr)}
	for _, o := range b.State.Operations {
		part.Ops = append(part.Ops, op{o.ID, string(o.Status)})
	}
	return part
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// CommonChecks — проверки, общие для всех планов.
func CommonChecks(p *plan.Plan, b Basics) {
	if b.EUID == 0 {
		p.Add(plan.LevelOK, "root", "root", "запущено от root")
	} else {
		p.Add(plan.LevelInfo, "root", "root", "план строится без root; для выполнения нужен sudo")
	}
	if !b.Systemd {
		p.Add(plan.LevelFail, "systemd.missing", "systemd", "systemd не обнаружен (%s): RamPlus настраивает swap через systemd .swap-юниты", sys.SystemdRunDir)
	}
	if b.StateErr != nil {
		p.Add(plan.LevelFail, "journal.unreadable", "журнал", "не удалось прочитать %s: %v", StatePath, b.StateErr)
	}
	if r := b.Recovery; r != nil && p.Action != plan.ActionRollback {
		msg := fmt.Sprintf("операция %s не завершена (процесс прервался)", r.OpID)
		if r.Pending != nil {
			msg += fmt.Sprintf(" на шаге «%s»: %s", r.Pending.Step.Describe(), r.Detail)
		}
		p.Add(plan.LevelFail, "journal.interrupted", "журнал", "%s — сначала выполните: sudo ramplus rollback", msg)
	}
	if b.SwapsErr != nil {
		p.Add(plan.LevelFail, "swaps.unreadable", "swap", "не удалось прочитать %s: %v", sys.SwapsPath, b.SwapsErr)
	}
}

// PlanRollback — общая часть плана rollback: цель по позиции в стеке,
// сверка состояния, прерванный шаг, шаги отмены, разделы журнала.
// Возвращает цель (nil — откатывать нельзя).
func PlanRollback(p *plan.Plan, b Basics) *Operation {
	st := b.State
	for i := len(st.Operations) - 1; i >= 0; i-- {
		op := st.Operations[i]
		if op.Status == OpRollbackFailed {
			p.Add(plan.LevelFail, "rollback.partial", "rollback", "операция %s (%s) откатилась не полностью — нужна ручная проверка по %s (поле applied) и бэкапам в %s",
				op.ID, op.Command, StatePath, BackupDir)
			return nil
		}
		if op.Status == OpCommitted || op.Status == OpInProgress {
			break
		}
	}
	idx, ok := st.LastUndoable()
	if !ok {
		p.Add(plan.LevelFail, "rollback.nothing", "rollback", "нечего откатывать: в %s нет выполненных операций", StatePath)
		return nil
	}
	op := st.Operations[idx]
	p.Effects.Target = &plan.Target{Index: idx, ID: op.ID}
	p.Add(plan.LevelInfo, "rollback.target", "rollback", "отменяется операция %s от %s: %s", op.ID, op.StartedAt.Local().Format("2006-01-02 15:04:05"), op.Command)
	rollbackContext(p, st, op, b.Now)

	// Состояние системы должно совпадать с результатом операции по журналу.
	if len(b.RollbackDiff) > 0 {
		p.Add(plan.LevelFail, "rollback.state_mismatch", "состояние", "состояние системы не совпадает с журналом, нужна ручная проверка (ничего не изменено):\n      - %s",
			strings.Join(b.RollbackDiff, "\n      - "))
		return &op
	}
	p.Add(plan.LevelOK, "rollback.state_ok", "состояние", "система в состоянии, записанном в журнале после операции %s", op.ID)

	if op.Status == OpInProgress {
		p.Add(plan.LevelWarn, "rollback.interrupted", "rollback", "операция была прервана посреди выполнения — откатываются записанные в журнал шаги")
		if r := b.Recovery; r != nil && r.OpID == op.ID && r.Pending != nil {
			step := r.Pending.Step.Describe()
			switch r.Verdict {
			case VerdictPresent:
				p.Add(plan.LevelInfo, "rollback.pending_undo", "прерванный шаг", "«%s»: %s — будет откачен", step, r.Detail)
				for _, u := range r.Pending.Undo {
					p.Steps = append(p.Steps, Recovered(u))
				}
			case VerdictAbsent:
				p.Add(plan.LevelInfo, "rollback.pending_absent", "прерванный шаг", "«%s»: %s — откатывать нечего", step, r.Detail)
			default:
				p.Add(plan.LevelFail, "rollback.pending_mismatch", "прерванный шаг", "«%s»: %s — RamPlus не будет это трогать, нужна ручная проверка", step, r.Detail)
			}
		}
	}
	p.Steps = append(p.Steps, UndoSteps(op.Applied)...)
	p.Effects.Managed = op.ManagedBefore
	p.Effects.Profile = op.ProfileBefore
	return &op
}

// rollbackOldAfter — с какого возраста отменяемая операция «старая».
const rollbackOldAfter = 24 * time.Hour

var opStatusPast = map[string]string{
	OpCommitted:      "выполнена",
	OpRolledBack:     "уже отменена",
	OpFailed:         "завершилась ошибкой",
	OpInProgress:     "прервана",
	OpRollbackFailed: "откатилась не полностью",
}

// rollbackContext — что именно откатывается, если это неочевидно:
// операция старше суток или другого домена, чем последняя операция
// пользователя.
func rollbackContext(p *plan.Plan, st State, op Operation, now time.Time) {
	info := plan.InfoOf(op.Action)
	age := max(now.Sub(op.StartedAt), 0)
	p.Rollback = &plan.RollbackView{OperationID: op.ID, Action: op.Action, Domain: info.Domain,
		Command: op.Command, StartedAt: op.StartedAt, AgeSeconds: int64(age / time.Second)}
	what := fmt.Sprintf("«%s» от %s (%s)", info.Title, plan.HumanDate(op.StartedAt), plan.HumanAge(age))
	if age > rollbackOldAfter {
		p.Add(plan.LevelWarn, "rollback.old", "rollback", "отменяемой операции больше суток: %s — %s", what, op.Command)
	}
	var last *Operation
	for i := len(st.Operations) - 1; i >= 0; i-- {
		if st.Operations[i].Action != plan.ActionRollback {
			last = &st.Operations[i]
			break
		}
	}
	if last == nil || last.ID == op.ID {
		return
	}
	lastDomain := plan.InfoOf(last.Action).Domain
	if info.Domain == "" || lastDomain == "" || lastDomain == info.Domain {
		return
	}
	p.Add(plan.LevelWarn, "rollback.other_domain", "rollback", "будет отменена операция %s, а не %s (последняя операция — %s — %s)",
		what, plan.DomainNoun[lastDomain], last.Command, opStatusPast[last.Status])
}
