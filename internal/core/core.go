// Package core — сервисный слой RamPlus: единая точка входа для CLI сейчас
// и для демона/веба потом. Вся логика — здесь и ниже; CLI, демон и веб —
// тонкие оболочки над core.
package core

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/swap"
	"github.com/ashot0907/ramplus/internal/sys"
)

// Core — сервис над системой.
type Core struct {
	Sys sys.System
	// Collect — снимок памяти для Status (по умолчанию monitor.Collect).
	Collect func() monitor.Status
	// Daemon — core работает в демоне: пути, недоступные демону на запись
	// (ProtectHome=read-only и т.п.), блокируют план заранее.
	Daemon bool

	plans planStore
	ops   opRegistry
}

// New — сервис над системой s.
func New(s sys.System) *Core { return &Core{Sys: s, Collect: monitor.Collect} }

// Request — операция, которую нужно спланировать и выполнить.
type Request struct {
	Swap         *swap.Options
	Profile      *profile.ProfileRequest
	ProfileReset bool
	Rollback     bool
}

// Plan строит план операции (только чтение, root не нужен) и запоминает
// его: выполнить — Apply(p.ID).
func (c *Core) Plan(ctx context.Context, r Request) *plan.Plan {
	p := c.build(ctx, r)
	p.ID = c.plans.put(r, p.Fingerprint, c.Sys.Now())
	return p
}

// build строит план без запоминания.
func (c *Core) build(ctx context.Context, r Request) *plan.Plan {
	p := c.buildDomain(ctx, r)
	if c.Daemon {
		c.daemonPaths(p)
	}
	return p
}

func (c *Core) buildDomain(ctx context.Context, r Request) *plan.Plan {
	switch {
	case r.Swap != nil:
		return c.swapPlan(ctx, *r.Swap)
	case r.Profile != nil:
		p := profile.PlanProfile(ctx, c.Sys, *r.Profile)
		return &p.Plan
	case r.ProfileReset:
		p := profile.PlanProfileReset(ctx, c.Sys)
		return &p.Plan
	default:
		return c.swapPlan(ctx, swap.Options{Action: plan.ActionRollback})
	}
}

// SwapPlan — план apply/resize/remove swap.
func (c *Core) SwapPlan(ctx context.Context, o swap.Options) *plan.Plan {
	return c.Plan(ctx, Request{Swap: &o})
}

// ProfilePlan — план применения профиля.
func (c *Core) ProfilePlan(ctx context.Context, r profile.ProfileRequest) *plan.Plan {
	return c.Plan(ctx, Request{Profile: &r})
}

// ProfileResetPlan — план сброса профиля.
func (c *Core) ProfileResetPlan(ctx context.Context) *plan.Plan {
	return c.Plan(ctx, Request{ProfileReset: true})
}

// RollbackPlan — план отката последней операции (общий стек).
func (c *Core) RollbackPlan(ctx context.Context) *plan.Plan {
	return c.Plan(ctx, Request{Rollback: true})
}

func (c *Core) swapPlan(ctx context.Context, o swap.Options) *plan.Plan {
	p := swap.BuildPlan(swap.Gather(ctx, c.Sys, o.Path), o)
	return &p.Plan
}

// Run — CLI: построить план и сразу выполнить его через Apply (с той же
// проверкой отпечатка, что у демона). h.Render и h.Confirm задаёт
// вызывающий.
func (c *Core) Run(ctx context.Context, r Request, h journal.Hooks) (*plan.Plan, error) {
	return c.apply(ctx, c.Plan(ctx, r).ID, h)
}

// Apply выполняет ранее построенный план planID: под блокировкой заново
// собирает факты и строит план; если отпечаток изменился — отказ
// (ErrPlanStale), ничего не меняется. План выполняется не больше одного
// раза: после Apply его ID недействителен.
func (c *Core) Apply(ctx context.Context, planID string, h journal.Hooks) Result {
	return c.result(h, func(h journal.Hooks) (*plan.Plan, error) { return c.apply(ctx, planID, h) })
}

func (c *Core) apply(ctx context.Context, planID string, h journal.Hooks) (*plan.Plan, error) {
	sp, err := c.plans.take(planID, c.Sys.Now())
	if err != nil {
		return nil, fmt.Errorf("%w (%q)", err, planID)
	}
	return journal.Execute(ctx, c.Sys, func(ctx context.Context) (*plan.Plan, error) {
		p := c.build(ctx, sp.req)
		p.ID = planID
		if p.Fingerprint != sp.fingerprint {
			return p, ErrPlanStale
		}
		return p, nil
	}, h)
}

// WarnInterrupted — в журнале есть незавершённая операция RamPlus.
const WarnInterrupted = monitor.WarnInterrupted

// Status — снимок для ramplus status: память, swap (managed), профиль и
// дрейф.
func (c *Core) Status() monitor.Status {
	st := c.Collect()
	AnnotateStatus(&st, c.Sys)
	return st
}

// AnnotateStatus помечает swap, которым управляет RamPlus (managed: true),
// активный профиль и дрейф. Читает state.json; если он недоступен, узнаёт
// свой swap по пути.
func AnnotateStatus(st *monitor.Status, s sys.System) {
	paths := map[string]bool{swap.ManagedPath: true, swap.BtrfsPath: true}
	if b, err := s.FS.ReadFile(journal.StatePath); err == nil {
		if state, err := journal.ParseState(b); err == nil {
			for _, p := range swap.ManagedPaths(state) {
				paths[p] = true
			}
			if op := state.Pending(); op != nil {
				st.Swaps.Warnings = append(st.Swaps.Warnings, monitor.SwapWarning{Code: WarnInterrupted, Path: journal.StatePath})
			}
			annotateProfile(st, profile.ProfileOf(state), s)
		}
	}
	for i := range st.Swaps.Devices {
		st.Swaps.Devices[i].Managed = paths[st.Swaps.Devices[i].Path]
	}
}

func annotateProfile(st *monitor.Status, ap *profile.AppliedProfile, s sys.System) {
	if st.Drift == nil {
		st.Drift = []monitor.DriftItem{}
	}
	if ap == nil {
		return
	}
	st.Profile = &monitor.ProfileInfo{Name: ap.Name, Status: ap.Status, Overrides: ap.Overrides, AppliedAt: ap.AppliedAt}
	values := map[string]string{}
	for _, p := range profile.Params {
		if b, err := s.FS.ReadFile(p.Path); err == nil {
			values[p.Key] = strings.TrimSpace(string(b))
		}
	}
	for _, d := range profile.ProfileDrift(ap, values) {
		actual := d.Current
		if actual == "" {
			actual = "—"
		}
		st.Drift = append(st.Drift, monitor.DriftItem{Key: d.Key, Expected: d.New, Actual: actual})
	}
}

// CleanProblems — что мешает считать систему чистой от RamPlus: по
// фактическому состоянию, а не по наличию state.json. Журнал с историей
// (все операции откачены, managed: null) — это чисто.
func (c *Core) CleanProblems(ctx context.Context) []string {
	return CleanProblems(swap.Gather(ctx, c.Sys, ""), c.Sys)
}

// CleanProblems — то же по уже собранным фактам swap.
func CleanProblems(f swap.Facts, s sys.System) []string {
	var out []string
	if f.StateErr != nil {
		out = append(out, fmt.Sprintf("не удалось прочитать %s: %v", journal.StatePath, f.StateErr))
	}
	if m := f.Managed; m != nil {
		out = append(out, fmt.Sprintf("RamPlus управляет swap %s (%s)", m.Path, journal.StatePath))
	}
	if len(f.Orphans) > 0 {
		labels := make([]string, len(f.Orphans))
		for i, o := range f.Orphans {
			labels[i] = o.Label()
		}
		out = append(out, fmt.Sprintf("найдены следы RamPlus без записи в журнале: %s — sudo ramplus swap remove --orphan",
			strings.Join(labels, "; ")))
	}
	if p := profile.ProfileOf(f.State); p != nil {
		out = append(out, fmt.Sprintf("применён профиль RamPlus %s — ramplus profile reset", p.Name))
	}
	for _, op := range f.State.Operations {
		switch op.Status {
		case journal.OpInProgress:
			out = append(out, fmt.Sprintf("незавершённая операция %s (%s) — sudo ramplus rollback", op.ID, op.Command))
		case journal.OpRollbackFailed:
			out = append(out, fmt.Sprintf("операция %s (%s) откатилась не полностью — ручная проверка", op.ID, op.Command))
		}
	}
	for _, sw := range f.Swaps {
		if f.IsOurPath(sw.Path) {
			out = append(out, "активен swap RamPlus "+sw.Path)
		}
	}
	for _, u := range f.Units {
		out = append(out, "есть юнит RamPlus "+u)
	}
	var files []string
	for _, p := range []string{swap.ManagedPath, swap.BtrfsPath, swap.BtrfsDir, swap.LegacyPath} {
		if f.Exists[p] {
			files = append(files, p)
		}
	}
	for _, p := range []string{profile.SysctlFilePath, profile.ZswapUnitPath} {
		if _, err := s.FS.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	for _, p := range files {
		out = append(out, "существует "+p)
	}
	return out
}

// ProfileFacts — факты профилей (для profile list/current).
func (c *Core) ProfileFacts(ctx context.Context) profile.Facts {
	return profile.Gather(ctx, c.Sys, nil)
}

// noPathWrite — шаги, которым не нужна запись в каталог Path (swapon/off,
// systemctl, значения в /proc/sys и /sys — они у демона доступны).
var noPathWrite = map[plan.StepKind]bool{
	swap.KindSwapon: true, swap.KindSwapoff: true, plan.KindSystemctl: true, profile.KindSetValue: true,
}

// daemonPaths — каталоги, в которые план пишет, должны быть доступны
// демону на запись: иначе шаг упал бы посреди операции. Блокирует план
// проверкой path.not_writable_by_daemon.
func (c *Core) daemonPaths(p *plan.Plan) {
	seen := map[string]bool{}
	var bad []string
	for _, s := range p.Steps {
		if s.Path == "" || noPathWrite[s.Kind] {
			continue
		}
		dir := filepath.Dir(s.Path)
		for dir != "/" {
			if _, err := c.Sys.FS.Stat(dir); err == nil {
				break
			}
			dir = filepath.Dir(dir) // каталог создаст сам план — проверяем ближайший существующий
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if err := c.Sys.FS.Writable(dir); err != nil {
			bad = append(bad, dir)
			p.Add(plan.LevelFail, "path.not_writable_by_daemon", "путь",
				"%s: через веб-интерфейс/демон этот путь недоступен (%v), используйте CLI: sudo %s", dir, err, p.Command)
		}
	}
	if len(bad) > 0 {
		p.Fingerprint = plan.Fingerprint(p.Fingerprint, bad)
	}
}
