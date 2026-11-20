package core

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"context"
	"sync"
	"time"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
)

// ErrBusy — уже выполняется операция (в этом процессе).
var ErrBusy = i18n.Errorf("core.busy")

// ErrOpNotFound — нет операции с таким ID (в памяти этого процесса).
var ErrOpNotFound = i18n.Errorf("core.op_not_found")

// maxKeptOps — сколько завершённых операций помнить (GET /v1/operations/{id}).
const maxKeptOps = 32

// Operation — асинхронно выполняемая операция (демон). ID совпадает с ID
// операции в журнале.
type Operation struct {
	ID        string
	PlanID    string
	Action    plan.Action
	Command   string
	StartedAt time.Time

	mu      sync.Mutex
	events  []EventView
	result  *Result
	changed chan struct{} // закрывается при каждом изменении (broadcast)
}

// OperationView — снимок операции для JSON.
type OperationView struct {
	ID        string       `json:"id"`
	PlanID    string       `json:"plan_id"`
	Action    plan.Action  `json:"action"`
	Command   string       `json:"command"`
	StartedAt time.Time    `json:"started_at"`
	Running   bool         `json:"running"`
	Status    ResultStatus `json:"status"` // running, затем итог
	Events    []EventView  `json:"events"`
	Result    *Result      `json:"result,omitempty"`
}

// StatusRunning — операция ещё выполняется.
const StatusRunning ResultStatus = "running"

// View — снимок операции.
func (o *Operation) View() OperationView {
	o.mu.Lock()
	defer o.mu.Unlock()
	v := OperationView{ID: o.ID, PlanID: o.PlanID, Action: o.Action, Command: o.Command, StartedAt: o.StartedAt,
		Running: o.result == nil, Status: StatusRunning, Events: append([]EventView{}, o.events...), Result: o.result}
	if o.result != nil {
		v.Status = o.result.Status
	}
	return v
}

// Watch — события начиная с from, итог (nil — ещё выполняется) и канал,
// который закроется при следующем изменении.
func (o *Operation) Watch(from int) ([]EventView, *Result, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var evs []EventView
	if from < len(o.events) {
		evs = o.events[from:len(o.events):len(o.events)]
	}
	return evs, o.result, o.changed
}

func (o *Operation) notifyLocked() {
	close(o.changed)
	o.changed = make(chan struct{})
}

func (o *Operation) addEvent(ev EventView) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, ev)
	o.notifyLocked()
}

func (o *Operation) finish(r Result) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.result = &r
	o.notifyLocked()
}

// StartOptions — параметры асинхронного выполнения.
type StartOptions struct {
	// ConfirmExperimental — пользователь подтвердил экспериментальный профиль.
	ConfirmExperimental bool
	// RequestedBy — кто запросил (SO_PEERCRED); в журнал с source=daemon.
	RequestedBy *journal.Requester
	// Lang — язык событий и ошибок операции.
	Lang i18n.Lang
}

// opRegistry — операции процесса: одна выполняется, несколько последних
// завершённых помнятся.
type opRegistry struct {
	mu      sync.Mutex
	busy    bool
	ops     map[string]*Operation
	order   []string
	running sync.WaitGroup
}

// Start выполняет план planID асинхронно (демон). Синхронная часть —
// поиск плана, блокировка, повторный сбор фактов и сверка отпечатка,
// проверки, подтверждение, запись операции в журнал — выполняется до
// возврата: если она не прошла, возвращается итог без операции (stale,
// plan_used, blocked, locked, …). Если записывать нечего — итог nothing.
// Иначе возвращается операция, шаги которой идут в фоне.
//
// Одновременно — одна операция: вторая получает итог busy.
func (c *Core) Start(planID string, opts StartOptions) (*Operation, *Result) {
	r := &c.ops
	r.mu.Lock()
	if r.busy {
		r.mu.Unlock()
		return nil, &Result{Status: ResultBusy, Error: i18n.ErrText(opts.Lang, ErrBusy), Events: []EventView{}}
	}
	r.busy = true
	r.running.Add(1)
	r.mu.Unlock()

	started := make(chan *Operation, 1)
	var built *plan.Plan // план, построенный под блокировкой (Render вызывается до Started)
	var op *Operation    // пишется в Started, читается в Progress — одна горутина
	h := journal.Hooks{
		Render: func(p *plan.Plan) error { built = p; return nil },
		Confirm: func(p *plan.Plan) (bool, error) {
			if err := ConfirmExperimental(p, opts.ConfirmExperimental); err != nil {
				return false, err
			}
			return true, nil
		},
		Started: func(id string) {
			op = &Operation{ID: id, PlanID: planID, Action: built.Action, Command: built.Command,
				StartedAt: c.Sys.Now(), changed: make(chan struct{})}
			c.ops.remember(op)
			started <- op
		},
		Progress: func(ev journal.Event) { op.addEvent(eventView(ev, opts.Lang)) },
		Origin:   journal.Origin{Source: journal.SourceDaemon, RequestedBy: opts.RequestedBy},
		Lang:     opts.Lang,
	}
	done := make(chan Result, 1)
	go func() {
		defer func() {
			r.mu.Lock()
			r.busy = false
			r.mu.Unlock()
			r.running.Done()
		}()
		// Операция не зависит от запроса, который её запустил: отключение
		// клиента и остановка демона её не прерывают (Shutdown ждёт Wait).
		res := c.Apply(context.Background(), planID, h)
		if op != nil {
			op.finish(res)
		}
		done <- res
	}()
	select {
	case op := <-started:
		return op, nil
	case res := <-done:
		// Операция могла успеть и начаться, и закончиться.
		select {
		case op := <-started:
			return op, nil
		default:
		}
		return nil, &res
	}
}

func eventView(ev journal.Event, l i18n.Lang) EventView {
	v := EventView{Undo: ev.Undo, Index: ev.Index, Total: ev.Total, Step: plan.ViewOf(ev.Step, l)}
	if ev.Err != nil {
		v.Error = i18n.ErrText(l, ev.Err)
	}
	return v
}

func (r *opRegistry) remember(op *Operation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ops == nil {
		r.ops = map[string]*Operation{}
	}
	for len(r.order) >= maxKeptOps {
		delete(r.ops, r.order[0])
		r.order = r.order[1:]
	}
	r.ops[op.ID] = op
	r.order = append(r.order, op.ID)
}

// Operation — операция по ID (запущенная этим процессом).
func (c *Core) Operation(id string) (*Operation, error) {
	c.ops.mu.Lock()
	defer c.ops.mu.Unlock()
	if op, ok := c.ops.ops[id]; ok {
		return op, nil
	}
	return nil, ErrOpNotFound
}

// Wait ждёт завершения выполняемой операции (остановка демона).
func (c *Core) Wait() { c.ops.running.Wait() }
