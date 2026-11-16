package swap

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Ошибки, которые CLI показывает пользователю.
var (
	ErrNeedRoot  = errors.New("нужны права root: запустите через sudo")
	ErrBlocked   = errors.New("план заблокирован проверками — ничего не изменено")
	ErrCancelled = errors.New("отменено пользователем — ничего не изменено")
)

// Confirm спрашивает пользователя, применять ли план.
type Confirm func(p Plan) (bool, error)

// Manager выполняет операции: план → подтверждение → шаги с журналом.
type Manager struct {
	Sys System
	Out io.Writer
}

// Plan строит план без изменений системы (root не нужен).
func (m *Manager) Plan(ctx context.Context, o Options) Plan {
	return BuildPlan(Gather(ctx, m.Sys), o)
}

// Execute выполняет операцию. Порядок:
//  1. root; 2. блокировка от параллельного запуска; 3. сбор фактов и план
//     (под блокировкой); 4. печать плана; 5. подтверждение;
//  6. запись операции в журнал ДО первого изменения; 7. шаги с журналом
//     после каждого; при ошибке — откат выполненных шагов.
func (m *Manager) Execute(ctx context.Context, o Options, confirm Confirm) (Plan, error) {
	if m.Sys.Kernel.Geteuid() != 0 {
		return Plan{}, ErrNeedRoot
	}
	if err := m.Sys.FS.MkdirAll(StateDir, 0o755); err != nil {
		return Plan{}, fmt.Errorf("создание %s: %w", StateDir, err)
	}
	unlock, err := m.Sys.FS.Lock(LockPath)
	if err != nil {
		return Plan{}, err
	}
	defer unlock()

	f := Gather(ctx, m.Sys)
	p := BuildPlan(f, o)
	if err := p.Render(m.Out); err != nil {
		return p, err
	}
	if p.Blocked() {
		return p, ErrBlocked
	}
	if len(p.Steps) == 0 {
		fmt.Fprintln(m.Out, "Нечего делать.")
		return p, nil
	}
	ok, err := confirm(p)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, ErrCancelled
	}

	now := m.Sys.Now()
	state := f.State
	op := Operation{
		ID:            now.UTC().Format("20060102T150405Z"),
		Action:        p.Action,
		Command:       p.Options.Command(),
		Status:        OpInProgress,
		StartedAt:     now,
		Applied:       []Applied{},
		ManagedBefore: state.Managed,
		ManagedAfter:  p.ManagedAfter,
	}
	if p.Target != nil {
		op.Target = p.Target.ID
	}
	state.Operations = append(state.Operations, op)
	cur := len(state.Operations) - 1
	save := func() error {
		b, err := state.Marshal()
		if err != nil {
			return err
		}
		return m.Sys.FS.WriteFileAtomic(StatePath, b, 0o644)
	}
	if err := save(); err != nil {
		return p, fmt.Errorf("запись журнала %s (до изменений, ничего не изменено): %w", StatePath, err)
	}

	fmt.Fprintf(m.Out, "\nВыполнение (операция %s)\n", op.ID)
	r := &Runner{
		Sys:  m.Sys,
		OpID: op.ID,
		Journal: func(applied []Applied) error {
			state.Operations[cur].Applied = applied
			return save()
		},
		Progress: func(ev Event) {
			prefix, status := "", "ok"
			if ev.Undo {
				prefix = "откат "
			}
			if ev.Err != nil {
				status = "ОШИБКА: " + ev.Err.Error()
			}
			fmt.Fprintf(m.Out, "  %s[%d/%d] %s ... %s\n", prefix, ev.Index, ev.Total, ev.Step.Describe(), status)
		},
	}
	applied, runErr := r.Run(ctx, p.Steps)

	state.Operations[cur].FinishedAt = m.Sys.Now()
	state.Operations[cur].Applied = applied
	if runErr != nil {
		st := OpFailed
		var se *StepError
		if errors.As(runErr, &se) && len(se.RollbackErrors) > 0 {
			st = OpRollbackFailed
		}
		state.Operations[cur].Status = st
		state.Operations[cur].Error = runErr.Error()
		if st == OpFailed {
			state.Operations[cur].Applied = []Applied{} // всё откачено
		}
		if err := save(); err != nil {
			return p, fmt.Errorf("%w (и не удалось обновить журнал: %v)", runErr, err)
		}
		return p, runErr
	}

	state.Operations[cur].Status = OpCommitted
	state.Managed = p.ManagedAfter
	if state.Managed != nil && state.Managed.CreatedAt.IsZero() {
		state.Managed.CreatedAt = now
	}
	if p.Target != nil {
		for i := range state.Operations {
			if state.Operations[i].ID == p.Target.ID && i != cur {
				state.Operations[i].Status = OpRolledBack
			}
		}
	}
	if err := save(); err != nil {
		// Изменения выполнены, но журнал не обновился — это нужно знать.
		return p, fmt.Errorf("шаги выполнены, но не удалось записать итог в %s: %w", StatePath, err)
	}
	fmt.Fprintln(m.Out, "Готово.")
	return p, nil
}
