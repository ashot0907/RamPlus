package core

import (
	"context"
	"errors"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/sys"
)

// ErrExperimentalNotConfirmed — экспериментальный профиль без явного
// подтверждения.
var ErrExperimentalNotConfirmed = errors.New("профиль экспериментальный (не подтверждён бенчмарком на SSD): для скриптов добавьте --experimental")

// ConfirmExperimental — проверка перед выполнением: экспериментальный
// профиль применяется только с явным подтверждением.
func ConfirmExperimental(p *plan.Plan, confirmed bool) error {
	if p.Profile != nil && p.Profile.Experimental && !confirmed {
		return ErrExperimentalNotConfirmed
	}
	return nil
}

// ResultStatus — итог выполнения операции (стабильный машинный код).
type ResultStatus string

// Итоги выполнения.
const (
	ResultDone           ResultStatus = "done"                       // операция зафиксирована
	ResultNothing        ResultStatus = "nothing"                    // шагов нет, система уже в нужном состоянии
	ResultBlocked        ResultStatus = "blocked"                    // план заблокирован проверками, ничего не изменено
	ResultCancelled      ResultStatus = "cancelled"                  // не подтверждено, ничего не изменено
	ResultNeedRoot       ResultStatus = "need_root"                  // нужны права root, ничего не изменено
	ResultStale          ResultStatus = "stale"                      // система изменилась после построения плана, ничего не изменено
	ResultUnknownPlan    ResultStatus = "unknown_plan"               // нет плана с таким ID (не строился или истёк)
	ResultPlanUsed       ResultStatus = "plan_used"                  // план уже выполнялся
	ResultLocked         ResultStatus = "locked"                     // блокировку держит другой процесс RamPlus, ничего не изменено
	ResultBusy           ResultStatus = "busy"                       // демон уже выполняет операцию, ничего не изменено
	ResultNotConfirmed   ResultStatus = "experimental_not_confirmed" // экспериментальный профиль без подтверждения
	ResultFailed         ResultStatus = "failed"                     // шаг упал, выполненные шаги откачены
	ResultRollbackFailed ResultStatus = "rollback_failed"            // шаг упал и откат не удался — ручная проверка
	ResultError          ResultStatus = "error"                      // прочая ошибка
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
		res.Events = append(res.Events, eventView(ev))
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
	case errors.Is(err, ErrPlanUsed):
		return ResultPlanUsed
	case errors.Is(err, sys.ErrLocked):
		return ResultLocked
	case errors.Is(err, ErrBusy):
		return ResultBusy
	case errors.Is(err, ErrExperimentalNotConfirmed):
		return ResultNotConfirmed
	case errors.As(err, &se) && len(se.RollbackErrors) > 0:
		return ResultRollbackFailed
	case errors.As(err, &se):
		return ResultFailed
	}
	return ResultError
}
