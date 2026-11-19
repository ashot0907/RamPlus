package core

import (
	"context"
	"errors"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
)

// ResultStatus — итог выполнения операции (стабильный машинный код).
type ResultStatus string

// Итоги выполнения.
const (
	ResultDone           ResultStatus = "done"            // операция зафиксирована
	ResultNothing        ResultStatus = "nothing"         // шагов нет, система уже в нужном состоянии
	ResultBlocked        ResultStatus = "blocked"         // план заблокирован проверками, ничего не изменено
	ResultCancelled      ResultStatus = "cancelled"       // не подтверждено, ничего не изменено
	ResultNeedRoot       ResultStatus = "need_root"       // нужны права root, ничего не изменено
	ResultStale          ResultStatus = "stale"           // система изменилась после построения плана, ничего не изменено
	ResultUnknownPlan    ResultStatus = "unknown_plan"    // нет плана с таким ID (не строился, истёк или уже выполнен)
	ResultFailed         ResultStatus = "failed"          // шаг упал, выполненные шаги откачены
	ResultRollbackFailed ResultStatus = "rollback_failed" // шаг упал и откат не удался — ручная проверка
	ResultError          ResultStatus = "error"           // прочая ошибка
)

// EventView — выполненный (или откаченный) шаг в JSON.
type EventView struct {
	Undo  bool          `json:"undo"`
	Index int           `json:"index"`
	Total int           `json:"total"`
	Step  plan.StepView `json:"step"`
	Error string        `json:"error,omitempty"`
}

// Result — итог выполнения для --json и API.
type Result struct {
	Status      ResultStatus `json:"status"`
	OperationID string       `json:"operation_id,omitempty"`
	Error       string       `json:"error,omitempty"`
	Events      []EventView  `json:"events"`
	Plan        *plan.Plan   `json:"plan,omitempty"`
}

// OK — операция выполнена или делать было нечего.
func (r Result) OK() bool { return r.Status == ResultDone || r.Status == ResultNothing }

// Execute — Run с итогом в виде данных: хуки h вызываются как обычно,
// дополнительно собираются ID операции и события шагов.
func (c *Core) Execute(ctx context.Context, r Request, h journal.Hooks) Result {
	return c.result(h, func(h journal.Hooks) (*plan.Plan, error) { return c.Run(ctx, r, h) })
}

// result выполняет run с хуками, собирающими итог.
func (c *Core) result(h journal.Hooks, run func(journal.Hooks) (*plan.Plan, error)) Result {
	res := Result{Events: []EventView{}}
	started, progress := h.Started, h.Progress
	h.Started = func(id string) {
		res.OperationID = id
		if started != nil {
			started(id)
		}
	}
	h.Progress = func(ev journal.Event) {
		v := EventView{Undo: ev.Undo, Index: ev.Index, Total: ev.Total, Step: plan.ViewOf(ev.Step)}
		if ev.Err != nil {
			v.Error = ev.Err.Error()
		}
		res.Events = append(res.Events, v)
		if progress != nil {
			progress(ev)
		}
	}
	p, err := run(h)
	res.Plan = p
	res.Status = statusOf(p, err)
	if err != nil {
		res.Error = err.Error()
	}
	return res
}

func statusOf(p *plan.Plan, err error) ResultStatus {
	var se *journal.StepError
	switch {
	case err == nil && p != nil && len(p.Steps) == 0:
		return ResultNothing
	case err == nil:
		return ResultDone
	case errors.Is(err, journal.ErrNeedRoot):
		return ResultNeedRoot
	case errors.Is(err, journal.ErrBlocked):
		return ResultBlocked
	case errors.Is(err, journal.ErrCancelled):
		return ResultCancelled
	case errors.Is(err, ErrPlanStale):
		return ResultStale
	case errors.Is(err, ErrPlanNotFound):
		return ResultUnknownPlan
	case errors.As(err, &se) && len(se.RollbackErrors) > 0:
		return ResultRollbackFailed
	case errors.As(err, &se):
		return ResultFailed
	}
	return ResultError
}
