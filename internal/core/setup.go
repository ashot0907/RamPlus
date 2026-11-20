package core

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/swap"
)

// ActionSetup — swap и профиль одной операцией: одна запись в журнале,
// один rollback (ramplus setup, веб-интерфейс).
const ActionSetup plan.Action = "setup"

func init() {
	plan.RegisterAction(ActionSetup, plan.ActionInfo{Domain: plan.DomainSetup})
}

// SetupRequest — желаемое состояние: swap RamPlus нужного размера и/или
// профиль. nil — эту часть не трогать.
type SetupRequest struct {
	// Swap — Size, Path, Existing, DeleteOld, Force (Action выбирает
	// планировщик: apply, resize или «без изменений»).
	Swap    *swap.Options
	Profile *profile.ProfileRequest
}

// SetupPlan — план setup.
func (c *Core) SetupPlan(ctx context.Context, r SetupRequest, l i18n.Lang) *plan.Plan {
	return c.Plan(ctx, Request{Setup: &r, Lang: l})
}

// setupSwapOptions — какая операция swap приведёт систему к желаемому
// размеру: apply (своего swap нет), resize (размер другой) или nil (уже
// так). Проверки выбора добавляются в p.
func setupSwapOptions(p *plan.Plan, f swap.Facts, want swap.Options) *swap.Options {
	m := f.Managed
	if m == nil {
		o := want
		o.Action = swap.ActionApply
		return &o
	}
	if want.Path != "" && want.Path != m.Path {
		p.Add(plan.LevelFail, "setup.swap_path_change", m.Path, want.Path)
		return nil
	}
	if want.Size == m.SizeBytes {
		p.Add(plan.LevelOK, "setup.swap_unchanged", swap.FormatSize(m.SizeBytes), m.Path)
		return nil
	}
	return &swap.Options{Action: swap.ActionResize, Size: want.Size, Force: want.Force}
}

// setupPlan — композиция планировщиков swap и профилей: проверки обеих
// частей, шаги подряд (сначала swap, потом профиль), один отпечаток, одна
// операция в журнале. Отказ любой части блокирует весь план.
func (c *Core) setupPlan(ctx context.Context, r SetupRequest, lang i18n.Lang) *plan.Plan {
	path := ""
	if r.Swap != nil {
		path = r.Swap.Path
	}
	sf := swap.Gather(ctx, c.Sys, path)
	p := &plan.Plan{Action: ActionSetup, Lang: lang}

	var sp *swap.Plan
	if r.Swap != nil {
		if o := setupSwapOptions(p, sf, *r.Swap); o != nil {
			o.Lang = lang
			x := swap.BuildPlan(sf, *o)
			sp = &x
		}
	}
	var pp *profile.Plan
	if r.Profile != nil {
		pr := *r.Profile
		pr.Lang = lang
		x := profile.PlanProfile(ctx, c.Sys, pr)
		pp = &x
	}

	own := p.Checks
	p.Checks = nil
	switch {
	case r.Swap == nil && r.Profile == nil:
		journal.CommonChecks(p, sf.Basics)
		p.Add(plan.LevelFail, "setup.empty")
	case sp == nil && pp == nil:
		journal.CommonChecks(p, sf.Basics)
	}
	if sp != nil {
		p.Checks = append(p.Checks, sp.Checks...)
		p.Choices = sp.Choices
		p.Steps = append(p.Steps, sp.Steps...)
		p.Swap = sp.Swap
	} else {
		p.Swap = swap.View(sf, lang)
	}
	p.Checks = append(p.Checks, own...)
	if pp != nil {
		p.Checks = append(p.Checks, pp.Checks...)
		p.Steps = append(p.Steps, pp.Steps...)
		if pp.Profile != nil {
			v := *pp.Profile
			// Подтверждение экспериментального профиля нужно, только если
			// эта часть что-то меняет.
			v.Experimental = v.Experimental && len(pp.Steps) > 0
			p.Profile = &v
		}
	}
	p.Checks = dedupChecks(p.Checks)
	if p.Steps == nil {
		p.Steps = []plan.Step{}
	}
	if len(p.Steps) == 0 && !p.Blocked() {
		p.Add(plan.LevelOK, "setup.nothing")
	}

	// Итог.
	if sp != nil {
		p.SwapBefore, p.SwapAfter = sp.SwapBefore, sp.SwapAfter
	} else {
		for _, s := range sf.Swaps {
			p.SwapBefore += s.SizeBytes
		}
		p.SwapAfter = p.SwapBefore
	}
	var parts []*plan.Plan
	if sp != nil {
		parts = append(parts, &sp.Plan)
	}
	if pp != nil {
		parts = append(parts, &pp.Plan)
	}
	p.Created, p.Disabled, p.Deleted = union(parts, func(x *plan.Plan) []string { return x.Created }),
		union(parts, func(x *plan.Plan) []string { return x.Disabled }),
		union(parts, func(x *plan.Plan) []string { return x.Deleted })

	// Разделы журнала: managed — от swap, profile и baseline — от профиля.
	p.Effects.Managed = sf.State.Managed
	p.Effects.Profile = sf.State.Profile
	if sp != nil {
		p.Effects.Managed = sp.Effects.Managed
	}
	if pp != nil {
		p.Effects.Profile = pp.Effects.Profile
		p.Effects.BaselineCapture = pp.Effects.BaselineCapture
	}
	var fins []func(*plan.Sections, time.Time, string, bool)
	for _, x := range parts {
		if x.Effects.Finalize != nil {
			fins = append(fins, x.Effects.Finalize)
		}
	}
	p.Effects.Finalize = func(s *plan.Sections, now time.Time, opID string, captured bool) {
		for _, f := range fins {
			f(s, now, opID, captured)
		}
	}

	p.Command = setupCommand(r, sp)
	if p.Swap != nil {
		v := *p.Swap
		v.ApplyCommand, v.Title = p.Command, plan.ActionTitle(lang, ActionSetup)
		p.Swap = &v
	}
	fp := []any{sf.Basics.FingerprintPart(), sf.FingerprintPart()}
	for _, x := range parts {
		fp = append(fp, x.Fingerprint)
	}
	p.Seal(fp...)
	return p
}

// dedupChecks — общие проверки (root, systemd, журнал) есть в обеих
// частях: оставляется первая.
func dedupChecks(in []plan.Check) []plan.Check {
	seen := map[string]bool{}
	out := make([]plan.Check, 0, len(in))
	for _, c := range in {
		k := string(c.Level) + "\x00" + c.Code + "\x00" + c.Message
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	return out
}

func union(parts []*plan.Plan, get func(*plan.Plan) []string) []string {
	set := map[string]bool{}
	for _, x := range parts {
		for _, s := range get(x) {
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// setupCommand — командная строка setup (с подставленными значениями по
// умолчанию, как у swap apply).
func setupCommand(r SetupRequest, sp *swap.Plan) string {
	var b strings.Builder
	b.WriteString("ramplus setup")
	if o := r.Swap; o != nil {
		if o.Size > 0 {
			b.WriteString(" --size " + swap.SizeFlag(o.Size))
		}
		if o.Path != "" {
			b.WriteString(" --path " + o.Path)
		}
		existing := o.Existing
		if sp != nil && sp.Options.Action == swap.ActionApply {
			existing = sp.Options.Existing
		}
		if existing != "" {
			b.WriteString(" --existing=" + string(existing))
		}
		if o.DeleteOld {
			b.WriteString(" --delete-old")
		}
		if o.Force {
			b.WriteString(" --force")
		}
	}
	if pr := r.Profile; pr != nil {
		b.WriteString(" --profile " + pr.Name)
		keys := make([]string, 0, len(pr.Set))
		for k := range pr.Set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" --set " + k + "=" + pr.Set[k])
		}
		if pr.Experimental {
			b.WriteString(" --experimental")
		}
		if pr.Force && (r.Swap == nil || !r.Swap.Force) {
			b.WriteString(" --force")
		}
	}
	return b.String()
}

// gbText — размер для человека в «GB» интерфейса (двоичные гигабайты):
// «16 GB», «1.5 GB» (одинаково на всех языках).
func gbText(b uint64) string {
	if b%swap.GiB == 0 {
		return fmt.Sprintf("%d GB", b/swap.GiB)
	}
	return fmt.Sprintf("%.1f GB", float64(b)/float64(swap.GiB))
}
