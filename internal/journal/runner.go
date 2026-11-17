package journal

import (
	"context"
	"fmt"
	"strings"

	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/sys"
)

// Event — прогресс выполнения для вывода пользователю.
type Event struct {
	Undo  bool // шаг отката
	Index int  // 1-based
	Total int
	Step  plan.Step
	Err   error
}

// Runner выполняет шаги по одному с write-ahead журналом:
//
//	prepare (вычислить отмену, сохранить бэкап) → журнал pending →
//	выполнить → журнал done.
//
// Если шаг падает, уже выполненные шаги этого запуска откатываются в
// обратном порядке.
type Runner struct {
	Sys  sys.System
	OpID string
	// Journal вызывается с полным списком записей (done + последняя
	// pending). Ошибка журнала до шага — шаг не выполняется; после шага —
	// шаг откатывается (без журнала нельзя гарантировать rollback).
	Journal  func(entries []Applied) error
	Progress func(Event)
}

// StepError — шаг упал; RollbackErrors — что не удалось откатить.
type StepError struct {
	Index, Total   int
	Step           plan.Step
	Err            error
	RollbackErrors []error
}

func (e *StepError) Error() string {
	msg := fmt.Sprintf("шаг %d/%d (%s): %v", e.Index, e.Total, e.Step.Describe(), e.Err)
	if len(e.RollbackErrors) == 0 {
		return msg + "; выполненные шаги откачены, система в исходном состоянии"
	}
	var b strings.Builder
	b.WriteString(msg + "; ОТКАТ ВЫПОЛНЕН НЕ ПОЛНОСТЬЮ, нужна ручная проверка:")
	for _, re := range e.RollbackErrors {
		b.WriteString("\n  - " + re.Error())
	}
	return b.String()
}

func (e *StepError) Unwrap() error { return e.Err }

func (r *Runner) env() *Env { return &Env{Sys: r.Sys, OpID: r.OpID} }

func (r *Runner) progress(ev Event) {
	if r.Progress != nil {
		r.Progress(ev)
	}
}

func (r *Runner) journal(entries []Applied) error {
	if r.Journal == nil {
		return nil
	}
	if err := r.Journal(entries); err != nil {
		return fmt.Errorf("запись журнала %s: %w", StatePath, err)
	}
	return nil
}

// Run выполняет steps. При ошибке возвращает *StepError и выполненные
// (уже откаченные) шаги.
func (r *Runner) Run(ctx context.Context, steps []plan.Step) ([]Applied, error) {
	var done []Applied
	for i, s := range steps {
		fail := func(err error, undoAlso *Applied) ([]Applied, error) {
			r.progress(Event{Index: i + 1, Total: len(steps), Step: s, Err: err})
			toUndo := append([]Applied(nil), done...)
			if undoAlso != nil {
				toUndo = append(toUndo, *undoAlso)
			}
			se := &StepError{Index: i + 1, Total: len(steps), Step: s, Err: err}
			se.RollbackErrors = r.undo(ctx, toUndo)
			return toUndo, se
		}

		a, err := r.prepare(s)
		if err != nil {
			return fail(err, nil)
		}
		a.Status = StepPending
		if err := r.journal(append(append([]Applied(nil), done...), a)); err != nil {
			return fail(err, nil)
		}
		if err := r.do(ctx, s); err != nil {
			return fail(err, nil)
		}
		a.Status = StepDone
		if err := r.journal(append(append([]Applied(nil), done...), a)); err != nil {
			return fail(err, &a)
		}
		done = append(done, a)
		r.progress(Event{Index: i + 1, Total: len(steps), Step: s})
	}
	return done, nil
}

// undo откатывает записи в обратном порядке, продолжая после ошибок.
// После отката каждой записи журнал укорачивается, чтобы state.json не
// содержал уже отменённых шагов.
func (r *Runner) undo(ctx context.Context, entries []Applied) []error {
	var errs []error
	total := len(UndoSteps(entries))
	n := 0
	for i := len(entries) - 1; i >= 0; i-- {
		ok := true
		for _, s := range entries[i].Undo {
			n++
			err := r.Exec(ctx, s)
			r.progress(Event{Undo: true, Index: n, Total: total, Step: s, Err: err})
			if err != nil {
				ok = false
				errs = append(errs, fmt.Errorf("%s: %w", s.Describe(), err))
			}
		}
		if ok && r.Journal != nil {
			// Ошибка журнала здесь не делает откат неполным: система уже
			// восстановлена, итоговый статус запишет вызывающий.
			_ = r.Journal(entries[:i])
		}
	}
	return errs
}

// Exec — prepare + do без журнала (для шагов отката).
func (r *Runner) Exec(ctx context.Context, s plan.Step) error {
	if _, err := r.prepare(s); err != nil {
		return err
	}
	return r.do(ctx, s)
}

// prepare читает состояние до шага и вычисляет его отмену.
func (r *Runner) prepare(s plan.Step) (Applied, error) {
	a := Applied{Step: s}
	h, ok := handler(s.Kind)
	if !ok {
		return a, unknown(s.Kind)
	}
	if h.Prepare != nil {
		undo, backup, err := h.Prepare(r.env(), s)
		if err != nil {
			return a, err
		}
		a.Undo, a.Backup = undo, backup
	}
	if s.Reload {
		// Шаг меняет конфигурацию systemd: после отмены — daemon-reload.
		a.Undo = append(a.Undo, plan.DaemonReload())
	}
	return a, nil
}

func (r *Runner) do(ctx context.Context, s plan.Step) error {
	h, ok := handler(s.Kind)
	if !ok || h.Do == nil {
		return unknown(s.Kind)
	}
	return h.Do(ctx, r.env(), s)
}
