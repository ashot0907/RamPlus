package profile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// Операции профилей (общий журнал и стек отката со swap).
const (
	ActionProfileApply plan.Action = "profile-apply"
	ActionProfileReset plan.Action = "profile-reset"
)

// ProfileRequest — выбор пользователя для профиля.
type ProfileRequest struct {
	Name         string
	Set          map[string]string // --set (уже проверены ParseSet)
	Force        bool              // zswap поверх zram
	Experimental bool              // для командной строки в журнале
}

// Plan — план профиля: общая модель (plan.Plan) плюс типизированные
// разделы журнала.
type Plan struct {
	plan.Plan
	ProfileAfter    *AppliedProfile
	BaselineCapture *Baseline
}

func (p *Plan) add(l plan.Level, code, title, format string, a ...any) {
	p.Plan.Add(l, code, title, format, a...)
}

// seal — разделы журнала, которые операция запишет при коммите.
func (p *Plan) seal(f Facts) {
	p.Effects.Managed = f.State.Managed // профиль не трогает swap
	p.Effects.Profile = EncodeProfile(p.ProfileAfter)
	p.Effects.BaselineCapture = EncodeBaseline(p.BaselineCapture)
	p.Effects.Finalize = finalize
	if p.Steps == nil {
		p.Steps = []plan.Step{}
	}
	if p.Profile != nil {
		p.Profile.StatusText = StatusText(p.Profile.Status)
		if f.Profile != nil {
			p.Profile.Current, p.Profile.CurrentStatus = f.Profile.Name, f.Profile.Status
			p.Profile.CurrentStatusText = StatusText(f.Profile.Status)
		}
	}
	p.Plan.Seal(f.Basics.FingerprintPart(), f.fingerprintPart())
}

func profileCommand(req ProfileRequest, reset bool) string {
	if reset {
		return "ramplus profile reset"
	}
	var b strings.Builder
	b.WriteString("ramplus profile apply " + req.Name)
	keys := make([]string, 0, len(req.Set))
	for k := range req.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(" --set " + k + "=" + req.Set[k])
	}
	if req.Experimental {
		b.WriteString(" --experimental")
	}
	if req.Force {
		b.WriteString(" --force")
	}
	return b.String()
}

// BuildProfilePlan — план применения профиля. Чистая функция.
func BuildProfilePlan(pf Facts, req ProfileRequest) Plan {
	f := pf
	var p Plan
	p.Plan.Action, p.Plan.Command = ActionProfileApply, profileCommand(req, false)
	journal.CommonChecks(&p.Plan, f.Basics)

	prof, ok := Get(req.Name)
	if !ok {
		p.add(plan.LevelFail, "profile.unknown", "профиль", "нет профиля %q; доступны: %s", req.Name, strings.Join(Names(), ", "))
		p.ProfileAfter = f.Profile
		p.seal(f)
		return p
	}
	values := Effective(prof, req.Set)
	status := prof.Status
	overrides := map[string]string{}
	for k, v := range req.Set {
		if prof.Params[k] != v {
			overrides[k] = v
		}
	}
	if len(overrides) > 0 {
		status = StatusModified
	}
	info := &plan.ProfileView{
		Name: prof.Name, Title: prof.Title, Description: prof.Description, Status: status, BaseStatus: prof.Status,
		Evidence: prof.Evidence, Overrides: overrides, Experimental: prof.Status == StatusExperimental,
	}
	p.Profile = info

	switch prof.Status {
	case StatusExperimental:
		p.add(plan.LevelWarn, "profile.experimental", "статус", "профиль экспериментальный: не подтверждён scripts/bench.sh на реальном SSD — применяется только с явным подтверждением (в скриптах: --experimental)")
	case StatusStandard:
		p.add(plan.LevelInfo, "profile.standard", "статус", "стандарт Ubuntu: применение возвращает стоковые значения")
	case StatusVerified:
		p.add(plan.LevelOK, "profile.verified", "статус", "проверенный: %s", prof.Evidence)
	}
	if len(overrides) > 0 {
		var parts []string
		for _, k := range Keys() {
			if v, ok := overrides[k]; ok {
				parts = append(parts, fmt.Sprintf("%s=%s (в профиле: %s)", k, v, orDash(prof.Params[k])))
			}
		}
		p.add(plan.LevelInfo, "profile.overrides", "--set", "профиль изменён пользователем: %s", strings.Join(parts, ", "))
	}

	skip := map[string]bool{}
	zswapKeys := ZswapKeys(values)
	if len(zswapKeys) > 0 && !pf.ZswapPresent {
		p.add(plan.LevelFail, "zswap.missing", "zswap", "zswap нет в этом ядре (%s отсутствует), а профиль задаёт %s", ZswapParamDir, strings.Join(zswapKeys, ", "))
	}
	for _, k := range zswapKeys {
		if !pf.ZswapPresent {
			skip[k] = true
			continue
		}
		if _, present := pf.Values[k]; present {
			continue
		}
		skip[k] = true
		if k == "zswap.zpool" && values[k] == "zsmalloc" {
			p.add(plan.LevelInfo, "zswap.zpool_skipped", "zswap.zpool", "параметра zpool нет в этом ядре: используется zsmalloc — пропускается")
		} else {
			p.add(plan.LevelFail, "zswap.param_missing", k, "параметра нет в этом ядре — значение %s применить нельзя", values[k])
		}
	}
	if c, ok := values["zswap.compressor"]; ok && pf.ZswapPresent && !pf.CompressorOK[c] {
		p.add(plan.LevelFail, "zswap.compressor_unavailable", "компрессор", "%s недоступен в ядре: нет в %s и нет модуля crypto/%s.ko — выберите другой (--set zswap.compressor=...)", c, CryptoPath, c)
	}
	if z, ok := values["zswap.zpool"]; ok && !skip["zswap.zpool"] && !pf.ZpoolOK[z] {
		p.add(plan.LevelFail, "zswap.zpool_unavailable", "zpool", "аллокатор %s недоступен: не встроен, не загружен и нет модуля mm/%s.ko", z, z)
	}
	if values["zswap.enabled"] == "Y" && (pf.ZramActive || pf.ZramGenerator != "") {
		why := "активен zram"
		if !pf.ZramActive {
			why = "установлен генератор zram (" + pf.ZramGenerator + ")"
		}
		if req.Force {
			p.add(plan.LevelWarn, "zswap.over_zram", "zram", "%s: zswap поверх zram бессмыслен (сжатые страницы сжимаются повторно) — продолжаем из-за --force", why)
		} else {
			p.add(plan.LevelFail, "zswap.over_zram", "zram", "%s: zswap поверх zram бессмыслен (сжатые страницы сжимаются повторно). Отключите zram или добавьте --force", why)
		}
	}
	if cl := cmdlineParams(f.Cmdline, "zswap."); len(cl) > 0 && len(zswapKeys) > 0 {
		var parts []string
		for k, v := range cl {
			parts = append(parts, k+"="+v)
		}
		sort.Strings(parts)
		p.add(plan.LevelWarn, "zswap.cmdline", "cmdline", "в /proc/cmdline задано %s: ядро применяет это при загрузке, затем %s перезаписывает значения профиля — действует профиль. Kernel cmdline и GRUB RamPlus не меняет; чтобы не путаться, уберите zswap.* из GRUB вручную",
			strings.Join(parts, " "), ZswapUnitName)
	}

	// sysctl: порядок файлов при загрузке.
	sysctlVals := map[string]string{}
	for _, k := range SysctlKeys(values) {
		sysctlVals[k] = values[k]
	}
	if len(sysctlVals) > 0 {
		order := WithOurs(pf.SysctlFiles, sysctlVals)
		var chain []string
		for _, cf := range order {
			var set []string
			for _, k := range SysctlKeys(sysctlVals) {
				if v, ok := cf.Values[k]; ok && !cf.Ours {
					set = append(set, k+"="+v)
				}
			}
			switch {
			case cf.Ours:
				chain = append(chain, SysctlFileName)
			case len(set) > 0:
				chain = append(chain, fmt.Sprintf("%s (%s)", cf.Path, strings.Join(set, ", ")))
			}
		}
		if len(chain) > 1 {
			info.SysctlOrder = strings.Join(chain, " → ")
		}
		for _, k := range SysctlKeys(sysctlVals) {
			for _, o := range Overriders(order, k) {
				p.add(plan.LevelWarn, "sysctl.overridden", "sysctl", "%s задаёт %s=%s и применяется после %s — при загрузке перебьёт значение профиля (%s). Уберите ключ из этого файла",
					o.Path, k, o.Values[k], SysctlFileName, sysctlVals[k])
			}
			if v, ok := cmdlineParams(f.Cmdline, "sysctl.")["sysctl."+k]; ok {
				p.add(plan.LevelInfo, "sysctl.cmdline", "cmdline", "sysctl.%s=%s в cmdline применяется ядром раньше, %s — позже: действует профиль", k, v, SysctlFileName)
			}
		}
	}

	// Строки таблицы.
	for _, pr := range Params {
		nv, ok := values[pr.Key]
		if !ok {
			continue
		}
		cur := pf.Values[pr.Key]
		row := plan.ProfileRow{Key: pr.Key, Title: pr.Title, Current: cur, New: nv, Source: pf.SourceOf(pr.Key), FromSet: overrides[pr.Key] != ""}
		switch {
		case skip[pr.Key]:
			row.Skip = "нет в этом ядре"
		default:
			row.Change = cur != nv
		}
		info.Rows = append(info.Rows, row)
	}

	// Шаги: runtime.
	enabling := values["zswap.enabled"] == "Y"
	for _, k := range ZswapWriteOrder(enabling) {
		p.addSetValue(pf, values, skip, k)
	}
	for _, k := range SysctlKeys(values) {
		p.addSetValue(pf, values, skip, k)
	}

	// Шаги: постоянство.
	if len(sysctlVals) > 0 {
		content := SysctlFileContent(prof.Name, status, sysctlVals)
		if pf.OurSysctl == nil || *pf.OurSysctl != content {
			p.Steps = append(p.Steps, plan.Step{Kind: plan.KindWriteFile, Path: SysctlFilePath, Content: content, Mode: 0o644})
		}
	} else if pf.OurSysctl != nil {
		p.Steps = append(p.Steps, plan.Step{Kind: plan.KindDeleteConfig, Path: SysctlFilePath})
	}
	zswapVals := map[string]string{}
	for _, k := range zswapKeys {
		if !skip[k] {
			zswapVals[k] = values[k]
		}
	}
	p.unitSteps(pf, prof.Name, status, zswapVals, skip)

	if len(p.Steps) == 0 && !p.Blocked() {
		p.add(plan.LevelOK, "profile.up_to_date", "профиль", "система уже соответствует профилю %s — менять нечего", prof.Name)
	}

	after := map[string]string{}
	for k, v := range values {
		if !skip[k] {
			after[k] = v
		}
	}
	p.ProfileAfter = &AppliedProfile{Name: prof.Name, Status: status, BaseStatus: prof.Status, Overrides: overrides, Values: after}
	if f.Baseline == nil {
		base := map[string]string{}
		for k, v := range pf.Values {
			base[k] = v
		}
		p.BaselineCapture = &Baseline{Values: base}
		p.add(plan.LevelInfo, "profile.baseline_capture", "исходные значения", "первый apply профиля: текущие значения системы сохраняются в %s для ramplus profile reset", journal.StatePath)
	}
	p.summarizeProfile(pf)
	p.seal(f)
	return p
}

func (p *Plan) addSetValue(pf Facts, values map[string]string, skip map[string]bool, key string) {
	nv, ok := values[key]
	if !ok || skip[key] {
		return
	}
	cur, present := pf.Values[key]
	if !present || cur == nv {
		return
	}
	pr, _ := Lookup(key)
	p.Steps = append(p.Steps, plan.Step{Kind: KindSetValue, Path: pr.Path, Content: nv})
}

// unitSteps — ramplus-zswap.service: создать/обновить/удалить.
func (p *Plan) unitSteps(pf Facts, name, status string, zswapVals map[string]string, skip map[string]bool) {
	if len(zswapVals) > 0 {
		content := ZswapUnitContent(name, status, zswapVals, skip)
		if pf.OurUnit == nil || *pf.OurUnit != content {
			p.Steps = append(p.Steps,
				plan.Step{Kind: plan.KindWriteFile, Path: ZswapUnitPath, Content: content, Mode: 0o644, Reload: true},
				plan.DaemonReload())
		}
		if !pf.UnitEnabled {
			p.Steps = append(p.Steps, plan.Step{Kind: plan.KindSystemctl, Args: []string{"enable", ZswapUnitName}})
		}
		return
	}
	p.removeUnitSteps(pf)
}

func (p *Plan) removeUnitSteps(pf Facts) {
	if pf.OurUnit == nil {
		return
	}
	if pf.UnitEnabled {
		p.Steps = append(p.Steps, plan.Step{Kind: plan.KindSystemctl, Args: []string{"disable", ZswapUnitName}})
	}
	p.Steps = append(p.Steps, plan.Step{Kind: plan.KindDeleteConfig, Path: ZswapUnitPath, Reload: true}, plan.DaemonReload())
}

// BuildProfileReset — вернуть значения системы до первого apply и удалить
// файлы RamPlus.
func BuildProfileReset(pf Facts) Plan {
	f := pf
	var p Plan
	p.Plan.Action, p.Plan.Command = ActionProfileReset, profileCommand(ProfileRequest{}, true)
	journal.CommonChecks(&p.Plan, f.Basics)
	info := &plan.ProfileView{Name: "исходные значения", Reset: true}
	p.Profile = info
	base := f.Baseline
	if base == nil && pf.OurSysctl == nil && pf.OurUnit == nil {
		p.add(plan.LevelFail, "reset.nothing", "reset", "профиль RamPlus не применялся — сбрасывать нечего")
		p.Steps = []plan.Step{}
		p.ProfileAfter = f.Profile
		p.seal(f)
		return p
	}
	if base == nil {
		p.add(plan.LevelWarn, "reset.no_baseline", "reset", "исходные значения не сохранены в %s — удаляются только файлы RamPlus, runtime-значения не меняются", journal.StatePath)
	} else {
		p.add(plan.LevelInfo, "reset.baseline", "reset", "возврат к значениям, снятым %s перед первым apply (операция %s)", base.CapturedAt.Local().Format("2006-01-02 15:04:05"), base.OpID)
		values := base.Values
		for _, pr := range Params {
			bv, ok := values[pr.Key]
			if !ok {
				continue
			}
			cur, present := pf.Values[pr.Key]
			row := plan.ProfileRow{Key: pr.Key, Title: pr.Title, Current: cur, New: bv, Source: pf.SourceOf(pr.Key), Change: present && cur != bv}
			if !present {
				row.Skip = "нет в этом ядре"
			}
			info.Rows = append(info.Rows, row)
		}
		for _, k := range ZswapWriteOrder(values["zswap.enabled"] == "Y") {
			p.addSetValue(pf, values, nil, k)
		}
		for _, k := range SysctlKeys(values) {
			p.addSetValue(pf, values, nil, k)
		}
	}
	if pf.OurSysctl != nil {
		p.Steps = append(p.Steps, plan.Step{Kind: plan.KindDeleteConfig, Path: SysctlFilePath})
	}
	p.removeUnitSteps(pf)
	if p.Steps == nil {
		p.Steps = []plan.Step{}
	}
	p.ProfileAfter = nil
	p.summarizeProfile(pf)
	p.seal(f)
	return p
}

// summarizeProfile — созданные/удалённые файлы для блока «Итог».
func (p *Plan) summarizeProfile(pf Facts) {
	created, deleted := map[string]bool{}, map[string]bool{}
	for _, s := range p.Steps {
		switch s.Kind {
		case plan.KindWriteFile:
			exists := (s.Path == SysctlFilePath && pf.OurSysctl != nil) || (s.Path == ZswapUnitPath && pf.OurUnit != nil)
			if !exists {
				created[s.Path] = true
			}
		case plan.KindDeleteConfig:
			deleted[s.Path] = true
		}
	}
	p.Created, p.Deleted, p.Disabled = sortedSet(created), sortedSet(deleted), []string{}
}

// ProfileDrift — расхождения runtime-значений с применённым профилем.
func ProfileDrift(applied *AppliedProfile, values map[string]string) []plan.ProfileRow {
	if applied == nil {
		return nil
	}
	var out []plan.ProfileRow
	for _, pr := range Params {
		want, ok := applied.Values[pr.Key]
		if !ok {
			continue
		}
		if got := values[pr.Key]; got != want {
			out = append(out, plan.ProfileRow{Key: pr.Key, Current: got, New: want})
		}
	}
	return out
}

// PlanProfile собирает факты и строит план применения профиля (только чтение).
func PlanProfile(ctx context.Context, s system.System, req ProfileRequest) Plan {
	want := req.Set
	if prof, ok := Get(req.Name); ok {
		want = Effective(prof, req.Set)
	}
	return BuildProfilePlan(Gather(ctx, s, want), req)
}

// PlanProfileReset собирает факты и строит план сброса (только чтение).
func PlanProfileReset(ctx context.Context, s system.System) Plan {
	f := Gather(ctx, s, nil)
	if f.Baseline != nil {
		f = Gather(ctx, s, f.Baseline.Values)
	}
	return BuildProfileReset(f)
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// dash — "—" для пустого значения.
func dash(s string) string { return orDash(s) }
