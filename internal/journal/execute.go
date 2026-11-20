package journal

import (
	"context"
	"errors"
	"time"

	"github.com/ashot0907/ramplus/internal/i18n"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/sys"
)

// Ошибки, которые CLI показывает пользователю.
var (
	ErrNeedRoot  = i18n.Errorf("journal.need_root")
	ErrBlocked   = i18n.Errorf("journal.blocked")
	ErrCancelled = i18n.Errorf("journal.cancelled")
)

// Hooks — взаимодействие с пользователем при выполнении (вывод CLI,
// события демона).
type Hooks struct {
	Render   func(p *plan.Plan) error         // показать план
	Confirm  func(p *plan.Plan) (bool, error) // подтверждение
	Nothing  func()                           // шагов нет
	Started  func(opID string)                // операция записана в журнал
	Progress func(Event)                      // шаг выполнен/упал/откачен
	Finished func()                           // операция зафиксирована
	// Origin — откуда и кем запрошена операция (пишется в журнал).
	Origin Origin
	// Lang — язык сообщений о ходе выполнения и ошибок (для вызывающего).
	Lang i18n.Lang
}

// Execute выполняет операцию. Порядок:
//  1. root; 2. блокировка от параллельного запуска; 3. сбор фактов и план
//     (build, под блокировкой); 4. показ плана; 5. подтверждение;
//  6. запись операции в журнал ДО первого изменения; 7. шаги с журналом
//     после каждого; при ошибке — откат выполненных шагов.
func Execute(ctx context.Context, s sys.System, build func(context.Context) (*plan.Plan, error), h Hooks) (*plan.Plan, error) {
	if s.Kernel.Geteuid() != 0 {
		return nil, ErrNeedRoot
	}
	if err := s.FS.MkdirAll(StateDir, 0o755); err != nil {
		return nil, i18n.Errorf("journal.mkdir_failed", StateDir, err)
	}
	unlock, err := s.FS.Lock(LockPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	p, err := build(ctx)
	if err != nil {
		return p, err
	}
	if h.Render != nil {
		if err := h.Render(p); err != nil {
			return p, err
		}
	}
	if p.Blocked() {
		return p, ErrBlocked
	}
	if len(p.Steps) == 0 {
		if h.Nothing != nil {
			h.Nothing()
		}
		return p, nil
	}
	if h.Confirm != nil {
		ok, err := h.Confirm(p)
		if err != nil {
			return p, err
		}
		if !ok {
			return p, ErrCancelled
		}
	}
	return p, run(ctx, s, p, h)
}

func run(ctx context.Context, s sys.System, p *plan.Plan, h Hooks) error {
	state, err := LoadState(s)
	if err != nil {
		return i18n.Errorf("journal.read_failed", StatePath, err)
	}
	now := s.Now()
	op := Operation{
		ID:            NewOpID(now, state.Operations),
		Action:        p.Action,
		Command:       p.Command,
		Status:        OpInProgress,
		StartedAt:     now,
		Applied:       []Applied{},
		ManagedBefore: state.Managed,
		ManagedAfter:  raw(p.Effects.Managed),
		ProfileBefore: state.Profile,
		ProfileAfter:  raw(p.Effects.Profile),
		Source:        h.Origin.Source,
		RequestedBy:   h.Origin.RequestedBy,
	}
	if p.Effects.BaselineCapture != nil && state.Baseline == nil {
		op.BaselineCaptured = true
	}
	if t := p.Effects.Target; t != nil {
		op.Target = t.ID
	}
	state.Operations = append(state.Operations, op)
	cur := len(state.Operations) - 1
	save := func() error {
		b, err := state.Marshal()
		if err != nil {
			return err
		}
		return s.FS.WriteFileAtomic(StatePath, b, 0o644)
	}
	if err := save(); err != nil {
		return i18n.Errorf("journal.write_before_failed", StatePath, err)
	}

	if h.Started != nil {
		h.Started(op.ID)
	}
	r := &Runner{
		Sys:  s,
		OpID: op.ID,
		Journal: func(entries []Applied) error {
			state.Operations[cur].Applied = entries
			return save()
		},
		Progress: h.Progress,
	}
	applied, runErr := r.Run(ctx, p.Steps)

	state.Operations[cur].FinishedAt = s.Now()
	state.Operations[cur].Applied = applied
	if runErr != nil {
		st := OpFailed
		var se *StepError
		if errors.As(runErr, &se) && len(se.RollbackErrors) > 0 {
			st = OpRollbackFailed
		}
		state.Operations[cur].Status = st
		state.Operations[cur].Error = runErr.Error()
		state.Operations[cur].ErrorMsg = i18n.Store(runErr)
		if st == OpFailed {
			state.Operations[cur].Applied = []Applied{} // всё откачено
		}
		if err := save(); err != nil {
			return i18n.Errorf("journal.update_failed_after_error", runErr, err)
		}
		return runErr
	}

	state.Operations[cur].Status = OpCommitted
	commitSections(&state, cur, p, now, op.ID)
	if t := p.Effects.Target; t != nil {
		// Ровно одна операция — по позиции в стеке, с проверкой ID.
		// Никаких массовых изменений статуса по ID.
		if t.Index < 0 || t.Index >= cur || state.Operations[t.Index].ID != t.ID {
			return i18n.Errorf("journal.rollback_target_missing", t.Index, t.ID, StatePath)
		}
		state.Operations[t.Index].Status = OpRolledBack
		if state.Operations[t.Index].BaselineCaptured {
			state.Baseline = nil // откатили первый apply профиля: исходные значения снова текущие
		}
	}
	if err := save(); err != nil {
		// Изменения выполнены, но журнал не обновился — это нужно знать.
		return i18n.Errorf("journal.commit_write_failed", StatePath, err)
	}
	if h.Finished != nil {
		h.Finished()
	}
	return nil
}

// commitSections — разделы журнала после операции.
func commitSections(state *State, cur int, p *plan.Plan, now time.Time, opID string) {
	captured := state.Operations[cur].BaselineCaptured
	secs := plan.Sections{Managed: raw(p.Effects.Managed), Profile: raw(p.Effects.Profile), Baseline: state.Baseline}
	if captured {
		secs.Baseline = raw(p.Effects.BaselineCapture)
	}
	if f := p.Effects.Finalize; f != nil {
		f(&secs, now, opID, captured)
	}
	state.Managed, state.Profile, state.Baseline = raw(secs.Managed), raw(secs.Profile), raw(secs.Baseline)
	// Итог операции в журнале — с проставленным временем (как раньше,
	// когда операция и состояние ссылались на одну структуру).
	state.Operations[cur].ManagedAfter, state.Operations[cur].ProfileAfter = state.Managed, state.Profile
}
