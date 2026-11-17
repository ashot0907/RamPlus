package swap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/profile"
)

// Операции профилей (общий журнал и стек отката со swap).
const (
	ActionProfileApply Action = "profile-apply"
	ActionProfileReset Action = "profile-reset"
)

// ProfileRequest — выбор пользователя для профиля.
type ProfileRequest struct {
	Name         string
	Set          map[string]string // --set (уже проверены profile.ParseSet)
	Force        bool              // zswap поверх zram
	Experimental bool              // для командной строки в журнале
}

// ProfileRow — строка таблицы параметров в плане.
type ProfileRow struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Current string `json:"current"` // "" — нет в ядре
	New     string `json:"new"`
	Source  string `json:"source"` // откуда текущее
	Change  bool   `json:"change"`
	Skip    string `json:"skip,omitempty"` // почему не применяется
	FromSet bool   `json:"from_set,omitempty"`
}

// ProfilePlan — часть плана, специфичная для профилей.
type ProfilePlan struct {
	Name         string            `json:"name"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Status       string            `json:"status"`
	BaseStatus   string            `json:"base_status"`
	Evidence     string            `json:"evidence"`
	Overrides    map[string]string `json:"overrides,omitempty"`
	Experimental bool              `json:"experimental"`
	Reset        bool              `json:"reset"`
	Rows         []ProfileRow      `json:"rows"`
	SysctlOrder  string            `json:"sysctl_order,omitempty"`
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
func BuildProfilePlan(pf ProfileFacts, req ProfileRequest) Plan {
	f := pf.Facts
	p := Plan{Action: ActionProfileApply, ManagedBefore: f.State.Managed, ManagedAfter: f.State.Managed, facts: f,
		CommandLine: profileCommand(req, false)}
	p.commonChecks(f)
	defer func() {
		if p.Steps == nil {
			p.Steps = []Step{}
		}
	}()

	prof, ok := profile.Get(req.Name)
	if !ok {
		p.add(LevelFail, "профиль", "нет профиля %q; доступны: %s", req.Name, strings.Join(profile.Names(), ", "))
		p.ProfileAfter = f.State.Profile
		return p
	}
	values := profile.Effective(prof, req.Set)
	status := prof.Status
	overrides := map[string]string{}
	for k, v := range req.Set {
		if prof.Params[k] != v {
			overrides[k] = v
		}
	}
	if len(overrides) > 0 {
		status = profile.StatusModified
	}
	info := &ProfilePlan{
		Name: prof.Name, Title: prof.Title, Description: prof.Description, Status: status, BaseStatus: prof.Status,
		Evidence: prof.Evidence, Overrides: overrides, Experimental: prof.Status == profile.StatusExperimental,
	}
	p.Profile = info

	switch prof.Status {
	case profile.StatusExperimental:
		p.add(LevelWarn, "статус", "профиль экспериментальный: не подтверждён scripts/bench.sh на реальном SSD — применяется только с явным подтверждением (в скриптах: --experimental)")
	case profile.StatusStandard:
		p.add(LevelInfo, "статус", "стандарт Ubuntu: применение возвращает стоковые значения")
	case profile.StatusVerified:
		p.add(LevelOK, "статус", "проверенный: %s", prof.Evidence)
	}
	if len(overrides) > 0 {
		var parts []string
		for _, k := range profile.Keys() {
			if v, ok := overrides[k]; ok {
				parts = append(parts, fmt.Sprintf("%s=%s (в профиле: %s)", k, v, orDash(prof.Params[k])))
			}
		}
		p.add(LevelInfo, "--set", "профиль изменён пользователем: %s", strings.Join(parts, ", "))
	}

	skip := map[string]bool{}
	zswapKeys := profile.ZswapKeys(values)
	if len(zswapKeys) > 0 && !pf.ZswapPresent {
		p.add(LevelFail, "zswap", "zswap нет в этом ядре (%s отсутствует), а профиль задаёт %s", profile.ZswapParamDir, strings.Join(zswapKeys, ", "))
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
			p.add(LevelInfo, "zswap.zpool", "параметра zpool нет в этом ядре: используется zsmalloc — пропускается")
		} else {
			p.add(LevelFail, k, "параметра нет в этом ядре — значение %s применить нельзя", values[k])
		}
	}
	if c, ok := values["zswap.compressor"]; ok && pf.ZswapPresent && !pf.CompressorOK[c] {
		p.add(LevelFail, "компрессор", "%s недоступен в ядре: нет в %s и нет модуля crypto/%s.ko — выберите другой (--set zswap.compressor=...)", c, CryptoPath, c)
	}
	if z, ok := values["zswap.zpool"]; ok && !skip["zswap.zpool"] && !pf.ZpoolOK[z] {
		p.add(LevelFail, "zpool", "аллокатор %s недоступен: не встроен, не загружен и нет модуля mm/%s.ko", z, z)
	}
	if values["zswap.enabled"] == "Y" && (pf.ZramActive || pf.ZramGenerator != "") {
		why := "активен zram"
		if !pf.ZramActive {
			why = "установлен генератор zram (" + pf.ZramGenerator + ")"
		}
		if req.Force {
			p.add(LevelWarn, "zram", "%s: zswap поверх zram бессмыслен (сжатые страницы сжимаются повторно) — продолжаем из-за --force", why)
		} else {
			p.add(LevelFail, "zram", "%s: zswap поверх zram бессмыслен (сжатые страницы сжимаются повторно). Отключите zram или добавьте --force", why)
		}
	}
	if cl := cmdlineParams(f.Cmdline, "zswap."); len(cl) > 0 && len(zswapKeys) > 0 {
		var parts []string
		for k, v := range cl {
			parts = append(parts, k+"="+v)
		}
		sort.Strings(parts)
		p.add(LevelWarn, "cmdline", "в /proc/cmdline задано %s: ядро применяет это при загрузке, затем %s перезаписывает значения профиля — действует профиль. Kernel cmdline и GRUB RamPlus не меняет; чтобы не путаться, уберите zswap.* из GRUB вручную",
			strings.Join(parts, " "), profile.ZswapUnitName)
	}

	// sysctl: порядок файлов при загрузке.
	sysctlVals := map[string]string{}
	for _, k := range profile.SysctlKeys(values) {
		sysctlVals[k] = values[k]
	}
	if len(sysctlVals) > 0 {
		order := profile.WithOurs(pf.SysctlFiles, sysctlVals)
		var chain []string
		for _, cf := range order {
			var set []string
			for _, k := range profile.SysctlKeys(sysctlVals) {
				if v, ok := cf.Values[k]; ok && !cf.Ours {
					set = append(set, k+"="+v)
				}
			}
			switch {
			case cf.Ours:
				chain = append(chain, profile.SysctlFileName)
			case len(set) > 0:
				chain = append(chain, fmt.Sprintf("%s (%s)", cf.Path, strings.Join(set, ", ")))
			}
		}
		if len(chain) > 1 {
			info.SysctlOrder = strings.Join(chain, " → ")
		}
		for _, k := range profile.SysctlKeys(sysctlVals) {
			for _, o := range profile.Overriders(order, k) {
				p.add(LevelWarn, "sysctl", "%s задаёт %s=%s и применяется после %s — при загрузке перебьёт значение профиля (%s). Уберите ключ из этого файла",
					o.Path, k, o.Values[k], profile.SysctlFileName, sysctlVals[k])
			}
			if v, ok := cmdlineParams(f.Cmdline, "sysctl.")["sysctl."+k]; ok {
				p.add(LevelInfo, "cmdline", "sysctl.%s=%s в cmdline применяется ядром раньше, %s — позже: действует профиль", k, v, profile.SysctlFileName)
			}
		}
	}

	// Строки таблицы.
	for _, pr := range profile.Params {
		nv, ok := values[pr.Key]
		if !ok {
			continue
		}
		cur := pf.Values[pr.Key]
		row := ProfileRow{Key: pr.Key, Title: pr.Title, Current: cur, New: nv, Source: pf.SourceOf(pr.Key), FromSet: overrides[pr.Key] != ""}
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
	for _, k := range profile.ZswapWriteOrder(enabling) {
		p.addSetValue(pf, values, skip, k)
	}
	for _, k := range profile.SysctlKeys(values) {
		p.addSetValue(pf, values, skip, k)
	}

	// Шаги: постоянство.
	if len(sysctlVals) > 0 {
		content := profile.SysctlFileContent(prof.Name, status, sysctlVals)
		if pf.OurSysctl == nil || *pf.OurSysctl != content {
			p.Steps = append(p.Steps, Step{Kind: KindWriteFile, Path: profile.SysctlFilePath, Content: content, Mode: 0o644})
		}
	} else if pf.OurSysctl != nil {
		p.Steps = append(p.Steps, Step{Kind: KindDeleteConfig, Path: profile.SysctlFilePath})
	}
	zswapVals := map[string]string{}
	for _, k := range zswapKeys {
		if !skip[k] {
			zswapVals[k] = values[k]
		}
	}
	p.unitSteps(pf, prof.Name, status, zswapVals, skip)

	if len(p.Steps) == 0 && !p.Blocked() {
		p.add(LevelOK, "профиль", "система уже соответствует профилю %s — менять нечего", prof.Name)
	}

	after := map[string]string{}
	for k, v := range values {
		if !skip[k] {
			after[k] = v
		}
	}
	p.ProfileAfter = &AppliedProfile{Name: prof.Name, Status: status, BaseStatus: prof.Status, Overrides: overrides, Values: after}
	if f.State.Baseline == nil {
		base := map[string]string{}
		for k, v := range pf.Values {
			base[k] = v
		}
		p.BaselineCapture = &Baseline{Values: base}
		p.add(LevelInfo, "исходные значения", "первый apply профиля: текущие значения системы сохраняются в %s для ramplus profile reset", StatePath)
	}
	p.summarizeProfile(pf)
	return p
}

func (p *Plan) addSetValue(pf ProfileFacts, values map[string]string, skip map[string]bool, key string) {
	nv, ok := values[key]
	if !ok || skip[key] {
		return
	}
	cur, present := pf.Values[key]
	if !present || cur == nv {
		return
	}
	pr, _ := profile.Lookup(key)
	p.Steps = append(p.Steps, Step{Kind: KindSetValue, Path: pr.Path, Content: nv})
}

// unitSteps — ramplus-zswap.service: создать/обновить/удалить.
func (p *Plan) unitSteps(pf ProfileFacts, name, status string, zswapVals map[string]string, skip map[string]bool) {
	if len(zswapVals) > 0 {
		content := profile.ZswapUnitContent(name, status, zswapVals, skip)
		if pf.OurUnit == nil || *pf.OurUnit != content {
			p.Steps = append(p.Steps,
				Step{Kind: KindWriteFile, Path: ZswapUnitPath, Content: content, Mode: 0o644, Reload: true},
				daemonReload())
		}
		if !pf.UnitEnabled {
			p.Steps = append(p.Steps, Step{Kind: KindSystemctl, Args: []string{"enable", profile.ZswapUnitName}})
		}
		return
	}
	p.removeUnitSteps(pf)
}

func (p *Plan) removeUnitSteps(pf ProfileFacts) {
	if pf.OurUnit == nil {
		return
	}
	if pf.UnitEnabled {
		p.Steps = append(p.Steps, Step{Kind: KindSystemctl, Args: []string{"disable", profile.ZswapUnitName}})
	}
	p.Steps = append(p.Steps, Step{Kind: KindDeleteConfig, Path: ZswapUnitPath, Reload: true}, daemonReload())
}

// BuildProfileReset — вернуть значения системы до первого apply и удалить
// файлы RamPlus.
func BuildProfileReset(pf ProfileFacts) Plan {
	f := pf.Facts
	p := Plan{Action: ActionProfileReset, ManagedBefore: f.State.Managed, ManagedAfter: f.State.Managed, facts: f,
		CommandLine: profileCommand(ProfileRequest{}, true)}
	p.commonChecks(f)
	info := &ProfilePlan{Name: "исходные значения", Reset: true}
	p.Profile = info
	base := f.State.Baseline
	if base == nil && pf.OurSysctl == nil && pf.OurUnit == nil {
		p.add(LevelFail, "reset", "профиль RamPlus не применялся — сбрасывать нечего")
		p.Steps = []Step{}
		p.ProfileAfter = f.State.Profile
		return p
	}
	if base == nil {
		p.add(LevelWarn, "reset", "исходные значения не сохранены в %s — удаляются только файлы RamPlus, runtime-значения не меняются", StatePath)
	} else {
		p.add(LevelInfo, "reset", "возврат к значениям, снятым %s перед первым apply (операция %s)", base.CapturedAt.Local().Format("2006-01-02 15:04:05"), base.OpID)
		values := base.Values
		for _, pr := range profile.Params {
			bv, ok := values[pr.Key]
			if !ok {
				continue
			}
			cur, present := pf.Values[pr.Key]
			row := ProfileRow{Key: pr.Key, Title: pr.Title, Current: cur, New: bv, Source: pf.SourceOf(pr.Key), Change: present && cur != bv}
			if !present {
				row.Skip = "нет в этом ядре"
			}
			info.Rows = append(info.Rows, row)
		}
		for _, k := range profile.ZswapWriteOrder(values["zswap.enabled"] == "Y") {
			p.addSetValue(pf, values, nil, k)
		}
		for _, k := range profile.SysctlKeys(values) {
			p.addSetValue(pf, values, nil, k)
		}
	}
	if pf.OurSysctl != nil {
		p.Steps = append(p.Steps, Step{Kind: KindDeleteConfig, Path: profile.SysctlFilePath})
	}
	p.removeUnitSteps(pf)
	if p.Steps == nil {
		p.Steps = []Step{}
	}
	p.ProfileAfter = nil
	p.summarizeProfile(pf)
	return p
}

// summarizeProfile — созданные/удалённые файлы для блока «Итог».
func (p *Plan) summarizeProfile(pf ProfileFacts) {
	created, deleted := map[string]bool{}, map[string]bool{}
	for _, s := range p.Steps {
		switch s.Kind {
		case KindWriteFile:
			exists := (s.Path == profile.SysctlFilePath && pf.OurSysctl != nil) || (s.Path == ZswapUnitPath && pf.OurUnit != nil)
			if !exists {
				created[s.Path] = true
			}
		case KindDeleteConfig:
			deleted[s.Path] = true
		}
	}
	p.Created, p.Deleted, p.Disabled = sortedKeys(created), sortedKeys(deleted), []string{}
}

// ProfileDrift — расхождения runtime-значений с применённым профилем.
func ProfileDrift(applied *AppliedProfile, values map[string]string) []ProfileRow {
	if applied == nil {
		return nil
	}
	var out []ProfileRow
	for _, pr := range profile.Params {
		want, ok := applied.Values[pr.Key]
		if !ok {
			continue
		}
		if got := values[pr.Key]; got != want {
			out = append(out, ProfileRow{Key: pr.Key, Current: got, New: want})
		}
	}
	return out
}

// PlanProfile собирает факты и строит план применения профиля (только чтение).
func PlanProfile(ctx context.Context, sys System, req ProfileRequest) Plan {
	f := Gather(ctx, sys, "")
	want := req.Set
	if prof, ok := profile.Get(req.Name); ok {
		want = profile.Effective(prof, req.Set)
	}
	return BuildProfilePlan(GatherProfile(ctx, sys, f, want), req)
}

// PlanProfileReset собирает факты и строит план сброса (только чтение).
func PlanProfileReset(ctx context.Context, sys System) Plan {
	f := Gather(ctx, sys, "")
	var want map[string]string
	if f.State.Baseline != nil {
		want = f.State.Baseline.Values
	}
	return BuildProfileReset(GatherProfile(ctx, sys, f, want))
}
