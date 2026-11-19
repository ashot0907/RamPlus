package core

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/swap"
)

var codeRe = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)*$`)

// Каждая проверка любого плана имеет стабильный машинный код.
func TestCheckCodes(t *testing.T) {
	ctx := context.Background()
	usb := newWorld(t)
	usb.Transport = "usb"
	applied := newWorld(t).WithOldSwap(1 * GiB)
	mustRun(t, applied, swap.Options{Action: ActionApply, Size: 4 * GiB, Existing: swap.ExistingReplace, DeleteOld: true})
	prof := profWorld(t, false)
	worlds := map[string]*fakeWorld{"clean": newWorld(t), "old": newWorld(t).WithOldSwap(0), "usb": usb, "applied": applied, "prof": prof}
	requests := []Request{
		{Swap: &apply4G},
		{Swap: &swap.Options{Action: ActionApply}},
		{Swap: &swap.Options{Action: ActionApply, Size: 4 * GiB, Path: "rel/x"}},
		{Swap: &swap.Options{Action: ActionApply, Size: 4 * GiB, Path: "/tmp/x"}},
		{Swap: &swap.Options{Action: ActionApply, Size: 4 * GiB, Existing: swap.ExistingReplace, DeleteOld: true}},
		{Swap: &swap.Options{Action: ActionResize, Size: 6 * GiB}},
		{Swap: &swap.Options{Action: ActionRemove}},
		{Rollback: true},
		{Profile: &ProfileRequest{Name: "zswap-lz4"}},
		{Profile: &ProfileRequest{Name: "nope"}},
		{Profile: &ProfileRequest{Name: "default", Set: map[string]string{"vm.swappiness": "80"}}},
		{ProfileReset: true},
	}
	seen := 0
	for name, w := range worlds {
		c := New(w.System())
		for _, r := range requests {
			p := c.Plan(ctx, r)
			for _, ch := range p.Checks {
				seen++
				if !codeRe.MatchString(ch.Code) {
					t.Errorf("%s: %s: check %q (%s) has bad code %q", name, p.Command, ch.Title, ch.Message, ch.Code)
				}
			}
		}
	}
	if seen < 50 {
		t.Errorf("only %d checks seen", seen)
	}
}

func TestPlanJSON(t *testing.T) {
	w := newWorld(t).WithOldSwap(0)
	p := New(w.System()).SwapPlan(context.Background(), apply4G)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		SchemaVersion int    `json:"schema_version"`
		Action        string `json:"action"`
		Command       string `json:"command"`
		Blocked       *bool  `json:"blocked"`
		Checks        []struct{ Level, Code, Title, Message string }
		Steps         []struct{ Kind, Description, Path string }
		Summary       struct {
			Created, Disabled, Deleted []string
		}
		Swap    map[string]any `json:"swap"`
		Effects any            `json:"Effects"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != plan.SchemaVersion || got.Action != "apply" || got.Blocked == nil || *got.Blocked {
		t.Errorf("header: %s", b)
	}
	if got.Command != "ramplus swap apply --size 4G --existing=keep" {
		t.Errorf("command = %q", got.Command)
	}
	if len(got.Steps) != len(p.Steps) || got.Steps[0].Kind != string(p.Steps[0].Kind) || got.Steps[0].Description == "" {
		t.Errorf("steps: %+v", got.Steps)
	}
	if got.Summary.Disabled == nil || got.Summary.Deleted == nil || len(got.Summary.Created) == 0 {
		t.Errorf("summary arrays must not be null: %+v", got.Summary)
	}
	if got.Swap == nil || got.Effects != nil {
		t.Errorf("swap view must be present, effects hidden")
	}
}

func TestExecuteResult(t *testing.T) {
	ctx := context.Background()
	h := journal.Hooks{Confirm: yes}
	no := journal.Hooks{Confirm: func(*plan.Plan) (bool, error) { return false, nil }}

	w := newWorld(t)
	c := New(w.System())
	res := c.Execute(ctx, Request{Swap: &apply4G}, h)
	if res.Status != ResultDone || res.OperationID == "" || res.Error != "" || !res.OK() {
		t.Fatalf("apply: %+v", res)
	}
	if len(res.Events) != len(res.Plan.Steps) || res.Events[0].Step.Kind != res.Plan.Steps[0].Kind {
		t.Errorf("events: %+v", res.Events)
	}
	if st := stateOf(t, w); st.Operations[0].ID != res.OperationID {
		t.Errorf("op id %s, journal %s", res.OperationID, st.Operations[0].ID)
	}
	if res := c.Execute(ctx, Request{Swap: &swap.Options{Action: ActionResize, Size: 4 * GiB}}, h); res.Status != ResultBlocked || res.Plan == nil || !res.Plan.Blocked() {
		t.Errorf("same size: %+v", res)
	}
	if res := c.Execute(ctx, Request{Rollback: true}, no); res.Status != ResultCancelled {
		t.Errorf("cancel: %+v", res)
	}
	if res := c.Execute(ctx, Request{Rollback: true}, h); res.Status != ResultDone {
		t.Errorf("rollback: %+v", res)
	}

	p := profWorld(t, false)
	pc := New(p.System())
	lz4 := Request{Profile: &ProfileRequest{Name: "zswap-lz4"}}
	if res := pc.Execute(ctx, lz4, h); res.Status != ResultDone {
		t.Fatalf("profile: %+v", res)
	}
	if res := pc.Execute(ctx, lz4, h); res.Status != ResultNothing || !res.OK() {
		t.Errorf("profile again: %+v", res)
	}

	nr := newWorld(t)
	nr.EUID = 1000
	if res := New(nr.System()).Execute(ctx, Request{Swap: &apply4G}, h); res.Status != ResultNeedRoot || res.OK() {
		t.Errorf("not root: %+v", res)
	}

	// Сбой на позднем шаге: события показывают и ошибку, и откат.
	for n := 1; ; n++ {
		f := newWorld(t)
		snap := f.Snap()
		f.FailAt = f.Mutations + n
		res := New(f.System()).Execute(ctx, Request{Swap: &apply4G}, h)
		if res.Status == ResultDone {
			t.Fatal("no failure produced an undo event")
		}
		if res.Status != ResultFailed || res.Error == "" || res.OperationID == "" {
			continue // упало до записи операции в журнал
		}
		var failed, undone bool
		for _, ev := range res.Events {
			failed = failed || ev.Error != ""
			undone = undone || ev.Undo
		}
		if !failed {
			t.Fatalf("n=%d: no failed event: %+v", n, res.Events)
		}
		if !undone {
			continue
		}
		if d := snap.Diff(f.Snap()); d != "" && !onlyJournal(d) {
			t.Errorf("not rolled back:\n%s", d)
		}
		break
	}
}

// onlyJournal — различие только в журнале/каталоге состояния.
func onlyJournal(diff string) bool {
	return regexp.MustCompile(`^(?:[^\n]*(?:/var/lib/ramplus|/run/lock)[^\n]*\n?)*$`).MatchString(diff)
}
