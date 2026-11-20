package swap

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"bytes"
	"context"
	"io"
	"testing"

	"github.com/ashot0907/ramplus/internal/fakesys"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/render"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// Журнал и выполнение — internal/journal, фейковая система —
// internal/fakesys; тесты swap пользуются ими через короткие имена.
type (
	State     = journal.State
	Operation = journal.Operation
	Applied   = journal.Applied
	Recovery  = journal.Recovery
	Runner    = journal.Runner
	StepError = journal.StepError
	Verdict   = journal.Verdict

	fakeWorld = fakesys.World
	fakeFile  = fakesys.File
	fakeMount = fakesys.Mount
	snapshot  = fakesys.Snapshot
	crashed   = fakesys.Crashed
)

const (
	OpInProgress     = journal.OpInProgress
	OpCommitted      = journal.OpCommitted
	OpFailed         = journal.OpFailed
	OpRollbackFailed = journal.OpRollbackFailed
	OpRolledBack     = journal.OpRolledBack
	StepPending      = journal.StepPending
	StepDone         = journal.StepDone
	VerdictPresent   = journal.VerdictPresent
	VerdictAbsent    = journal.VerdictAbsent
	VerdictMismatch  = journal.VerdictMismatch
	StateDir         = journal.StateDir

	defaultFstab = fakesys.DefaultFstab
)

var (
	ParseState   = journal.ParseState
	NewOpID      = journal.NewOpID
	ErrBlocked   = journal.ErrBlocked
	ErrNeedRoot  = journal.ErrNeedRoot
	ErrCancelled = journal.ErrCancelled
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

func yes(*plan.Plan) (bool, error) { return true, nil }

// Manager — план и выполнение swap через journal (как core.Run в CLI).
type Manager struct {
	Sys system.System
	Out io.Writer
}

func (m *Manager) Plan(ctx context.Context, o Options) Plan {
	return BuildPlan(Gather(ctx, m.Sys, o.Path), o)
}

func (m *Manager) Execute(ctx context.Context, o Options, confirm func(*plan.Plan) (bool, error)) (Plan, error) {
	out := m.Out
	if out == nil {
		out = &bytes.Buffer{}
	}
	var sp Plan
	h := render.Exec(i18n.RU, out)
	h.Render = func(p *plan.Plan) error { return render.Plan(out, p, render.Options{Lang: i18n.RU}) }
	h.Confirm = confirm
	_, err := journal.Execute(ctx, m.Sys, func(ctx context.Context) (*plan.Plan, error) {
		sp = m.Plan(ctx, o)
		return &sp.Plan, nil
	}, h)
	return sp, err
}

func renderPlan(t *testing.T, p Plan, verbose bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := render.Plan(&out, &p.Plan, render.Options{Verbose: verbose, Lang: i18n.RU}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func loadState(t *testing.T, w *fakesys.World, fixture string) {
	t.Helper()
	w.Files[StateDir] = &fakesys.File{Dir: true, Mode: 0o755}
	w.Files[journal.StatePath] = &fakesys.File{Data: readFixture(t, "../../journal/testdata/"+fixture), Mode: 0o644}
}
