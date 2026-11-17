package core

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/fakesys"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/render"
	"github.com/ashot0907/ramplus/internal/swap"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// Сквозные тесты core: swap и профили на одной фейковой системе, общий
// журнал и стек отката.
type (
	Plan           = profile.Plan
	ProfileRequest = profile.ProfileRequest
	ProfileRow     = plan.ProfileRow
	AppliedProfile = profile.AppliedProfile
	State          = journal.State
	Operation      = journal.Operation
	Level          = plan.Level
	fakeWorld      = fakesys.World
	fakeFile       = fakesys.File
	snapshot       = fakesys.Snapshot
)

const (
	LevelOK          = plan.LevelOK
	LevelInfo        = plan.LevelInfo
	LevelWarn        = plan.LevelWarn
	LevelFail        = plan.LevelFail
	StateDir         = journal.StateDir
	StatePath        = journal.StatePath
	OpInProgress     = journal.OpInProgress
	OpCommitted      = journal.OpCommitted
	OpRolledBack     = journal.OpRolledBack
	OpRollbackFailed = journal.OpRollbackFailed
	StepDone         = journal.StepDone
	ActionApply      = swap.ActionApply
	ActionResize     = swap.ActionResize
	ActionRemove     = swap.ActionRemove
	ActionRollback   = plan.ActionRollback
	ManagedPath      = swap.ManagedPath
	LegacyPath       = swap.LegacyPath
	UnitDir          = system.UnitDir
	GiB              = swap.GiB
	unitHeader       = "# Managed by RamPlus: do not edit, use `ramplus swap`.\n"
)

var (
	ParseState       = journal.ParseState
	ErrBlocked       = journal.ErrBlocked
	PlanProfile      = profile.PlanProfile
	PlanProfileReset = profile.PlanProfileReset
	apply4G          = swap.Options{Action: swap.ActionApply, Size: 4 * swap.GiB}
	rollback         = swap.Options{Action: plan.ActionRollback}
)

func hasKey[V any](m map[string]V, k string) bool { return fakesys.HasKey(m, k) }

func newWorld(*testing.T) *fakesys.World { return fakesys.New() }

func stateOf(t *testing.T, w *fakesys.World) State {
	t.Helper()
	f, ok := w.Files[journal.StatePath]
	if !ok {
		return State{}
	}
	s, err := journal.ParseState([]byte(f.Data))
	if err != nil {
		t.Fatalf("state.json: %v", err)
	}
	return s
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "journal", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func loadState(t *testing.T, w *fakesys.World, fixture string) {
	t.Helper()
	w.Files[StateDir] = &fakesys.File{Dir: true, Mode: 0o755}
	w.Files[StatePath] = &fakesys.File{Data: readFixture(t, fixture), Mode: 0o644}
}

func yes(*plan.Plan) (bool, error) { return true, nil }

// Manager — выполнение через journal (как core.Run), с типизированным планом.
type Manager struct {
	Sys system.System
	Out io.Writer
}

func (m *Manager) exec(ctx context.Context, build func(context.Context) *plan.Plan, confirm func(*plan.Plan) (bool, error)) error {
	out := m.Out
	if out == nil {
		out = &bytes.Buffer{}
	}
	h := render.Exec(out)
	h.Render = func(p *plan.Plan) error { return render.Plan(out, p, render.Options{}) }
	h.Confirm = confirm
	_, err := journal.Execute(ctx, m.Sys, func(ctx context.Context) (*plan.Plan, error) { return build(ctx), nil }, h)
	return err
}

func (m *Manager) ExecuteFunc(ctx context.Context, build func(context.Context) Plan, confirm func(*plan.Plan) (bool, error)) (Plan, error) {
	var p Plan
	err := m.exec(ctx, func(ctx context.Context) *plan.Plan { p = build(ctx); return &p.Plan }, confirm)
	return p, err
}

func (m *Manager) Execute(ctx context.Context, o swap.Options, confirm func(*plan.Plan) (bool, error)) (swap.Plan, error) {
	var p swap.Plan
	err := m.exec(ctx, func(ctx context.Context) *plan.Plan {
		p = swap.BuildPlan(swap.Gather(ctx, m.Sys, o.Path), o)
		return &p.Plan
	}, confirm)
	return p, err
}

func run(t *testing.T, w *fakesys.World, o swap.Options) (swap.Plan, string, error) {
	t.Helper()
	var out bytes.Buffer
	m := &Manager{Sys: w.System(), Out: &out}
	p, err := m.Execute(context.Background(), o, yes)
	return p, out.String(), err
}

func mustRun(t *testing.T, w *fakesys.World, o swap.Options) {
	t.Helper()
	if _, out, err := run(t, w, o); err != nil {
		t.Fatalf("%s: %v\n%s", o.Command(), err, out)
	}
}

func checksOf(p any) []plan.Check {
	switch v := p.(type) {
	case Plan:
		return v.Checks
	case swap.Plan:
		return v.Checks
	case plan.Plan:
		return v.Checks
	}
	panic("checksOf: неизвестный план")
}

func stepsOf(p any) []plan.Step {
	switch v := p.(type) {
	case Plan:
		return v.Steps
	case swap.Plan:
		return v.Steps
	}
	panic("stepsOf: неизвестный план")
}

func findCheck(p any, title string) (plan.Check, bool) {
	for _, c := range checksOf(p) {
		if c.Title == title || strings.HasPrefix(c.Title, title) {
			return c, true
		}
	}
	return plan.Check{}, false
}

func wantLevel(t *testing.T, p any, title string, level Level) plan.Check {
	t.Helper()
	c, ok := findCheck(p, title)
	if !ok {
		t.Fatalf("check %q not found in %+v", title, checksOf(p))
	}
	if c.Level != level {
		t.Errorf("check %q: level %s, want %s (%s)", title, c.Level, level, c.Message)
	}
	return c
}

func kinds(steps []plan.Step) string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = string(s.Kind)
	}
	return strings.Join(out, ",")
}

func render_(t *testing.T, p Plan, verbose bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := render.Plan(&out, &p.Plan, render.Options{Verbose: verbose}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func cleanOf(w *fakesys.World) []string {
	return CleanProblems(swap.Gather(context.Background(), w.System(), ""), w.System())
}
