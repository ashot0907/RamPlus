package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/swap"
)

// pauseAt останавливает операцию перед первым изменением op (подстрока) до
// release(). Возвращает канал «дошли до точки» и release.
func pauseAt(w *fakeWorld, op string) (<-chan struct{}, func()) {
	reached, release := make(chan struct{}), make(chan struct{})
	var once bool
	w.OnMutation = func(m string) {
		if !once && strings.Contains(m, op) {
			once = true
			close(reached)
			<-release
		}
	}
	return reached, func() { close(release) }
}

func waitDone(t *testing.T, op *Operation) Result {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		_, res, ch := op.Watch(0)
		if res != nil {
			return *res
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatal("operation did not finish")
		}
	}
}

func TestStartAsync(t *testing.T) {
	w := newWorld(t)
	c := New(w.System())
	p := c.SwapPlan(context.Background(), apply4G)
	op, res := c.Start(p.ID, StartOptions{})
	if res != nil {
		t.Fatalf("start: %+v", res)
	}
	r := waitDone(t, op)
	if r.Status != ResultDone || r.OperationID != op.ID {
		t.Fatalf("result: %+v", r)
	}
	v := op.View()
	if v.Running || v.Status != ResultDone || len(v.Events) != len(p.Steps) || v.Command != p.Command || v.Action != p.Action {
		t.Errorf("view: %+v", v)
	}
	if got, err := c.Operation(op.ID); err != nil || got != op {
		t.Errorf("lookup: %v", err)
	}
	if _, err := c.Operation("nope"); err != ErrOpNotFound {
		t.Errorf("unknown op: %v", err)
	}
	if st := stateOf(t, w); st.Operations[0].ID != op.ID {
		t.Errorf("journal id %s, op id %s", st.Operations[0].ID, op.ID)
	}
}

// Синхронные отказы — без операции.
func TestStartRefusals(t *testing.T) {
	w := newWorld(t).WithKernelParams(false)
	c := New(w.System())
	ctx := context.Background()

	if _, res := c.Start("nope", StartOptions{}); res == nil || res.Status != ResultUnknownPlan {
		t.Errorf("unknown: %+v", res)
	}
	p := c.ProfilePlan(ctx, ProfileRequest{Name: "zswap-lz4"})
	if _, res := c.Start(p.ID, StartOptions{}); res == nil || res.Status != ResultNotConfirmed {
		t.Errorf("experimental: %+v", res)
	}
	if st := stateOf(t, w); len(st.Operations) != 0 {
		t.Errorf("journal must be untouched: %+v", st.Operations)
	}
	p = c.SwapPlan(ctx, swap4GResize())
	if _, res := c.Start(p.ID, StartOptions{}); res == nil || res.Status != ResultBlocked {
		t.Errorf("blocked: %+v", res)
	}
	if _, res := c.Start(p.ID, StartOptions{}); res == nil || res.Status != ResultPlanUsed {
		t.Errorf("used: %+v", res)
	}
}

// Одна операция за раз; CLI во время операции демона упирается в
// блокировку; журнал целый.
func TestStartBusyAndCLILock(t *testing.T) {
	w := newWorld(t).WithKernelParams(false)
	c := New(w.System())
	ctx := context.Background()
	reached, release := pauseAt(w, "swapon")

	p := c.SwapPlan(ctx, apply4G)
	p2 := c.ProfilePlan(ctx, ProfileRequest{Name: "zswap-lz4", Experimental: true})
	op, res := c.Start(p.ID, StartOptions{})
	if res != nil {
		t.Fatalf("start: %+v", res)
	}
	<-reached

	if _, res := c.Start(p2.ID, StartOptions{ConfirmExperimental: true}); res == nil || res.Status != ResultBusy {
		t.Errorf("second op: %+v", res)
	}
	// CLI (отдельный процесс, своё core) — блокировка журнала.
	cli := New(w.System())
	_, err := cli.Run(ctx, Request{Profile: &ProfileRequest{Name: "zswap-lz4", Experimental: true}}, journal.Hooks{Confirm: yes})
	if err == nil || !strings.Contains(err.Error(), "другой экземпляр ramplus уже выполняет операцию") {
		t.Errorf("CLI during daemon op: %v", err)
	}
	if res := cli.Execute(ctx, Request{Swap: &apply4G}, journal.Hooks{Confirm: yes}); res.Status != ResultLocked {
		t.Errorf("CLI result: %+v", res)
	}

	release()
	if r := waitDone(t, op); r.Status != ResultDone {
		t.Fatalf("daemon op: %+v", r)
	}
	st := stateOf(t, w)
	if len(st.Operations) != 1 || st.Operations[0].Status != journal.OpCommitted {
		t.Errorf("journal: %+v", st.Operations)
	}
	// После завершения — снова можно (план профиля отпечатан до apply swap:
	// журнал изменился → stale).
	if _, res := c.Start(p2.ID, StartOptions{ConfirmExperimental: true}); res == nil || res.Status != ResultStale {
		t.Errorf("after: %+v", res)
	}
	c.Wait()
}

func swap4GResize() swap.Options { return swap.Options{Action: ActionResize, Size: 4 * GiB} }
