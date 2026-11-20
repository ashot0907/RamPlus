package core

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"context"
	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/swap"
)

// RecommendationSchemaVersion — версия JSON-схемы Recommendation.
const RecommendationSchemaVersion = 1

// Профили, которые рекомендует RamPlus.
const (
	RecommendedProfile = "zswap-lz4"
	FallbackProfile    = "default"
)

// Note — замечание к рекомендации: стабильный код и текст для человека.
type Note struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RecTarget — вариант места для swapfile (смонтированная ФС).
type RecTarget struct {
	Path       string  `json:"path"`
	MountPoint string  `json:"mount_point"`
	FSType     string  `json:"fs_type"`
	DiskKind   string  `json:"disk_kind"` // nvme | ssd | hdd | virtual | unknown
	Transport  string  `json:"transport,omitempty"`
	Total      *uint64 `json:"total_bytes"`
	Available  *uint64 `json:"available_bytes"`
	MaxBytes   uint64  `json:"max_bytes"` // сколько можно создать с запасом места
	Usable     bool    `json:"usable"`
	Problem    string  `json:"problem,omitempty"` // почему не подходит
	Selected   bool    `json:"selected"`
}

// Recommendation — что RamPlus советует для этой машины. Только чтение.
type Recommendation struct {
	SchemaVersion int `json:"schema_version"`
	// RAMBytes — MemTotal; RAMRoundedBytes — округлено вверх до GiB
	// (ядро резервирует часть установленной памяти).
	RAMBytes        uint64 `json:"ram_bytes"`
	RAMRoundedBytes uint64 `json:"ram_rounded_bytes"`

	// SwapSizeBytes — рекомендуемый размер swap RamPlus (null — swap не
	// рекомендуется, причина — SwapReason).
	SwapSizeBytes  *uint64 `json:"swap_size_bytes"`
	SwapReason     string  `json:"swap_reason"`
	SwapReasonCode string  `json:"swap_reason_code"`
	// SwapMaxBytes — максимум для выбранного места (с запасом свободного
	// места, как в плане); 0 — места нет.
	SwapMaxBytes uint64 `json:"swap_max_bytes"`
	// SwapBlocked — проверки, из-за которых swap сейчас создать нельзя.
	SwapBlocked []plan.Check `json:"swap_blocked"`
	Target      *RecTarget   `json:"target"`
	Targets     []RecTarget  `json:"targets"`
	// Managed — swap RamPlus уже создан: его размер.
	ManagedBytes *uint64 `json:"managed_bytes"`

	// Existing — что делать с существующим swap: replace | keep | null
	// (другого swap нет или свой уже создан).
	Existing       *string `json:"existing"`
	ExistingReason string  `json:"existing_reason,omitempty"`

	Profile       string `json:"profile"`
	ProfileStatus string `json:"profile_status"` // реальный статус профиля (experimental — «предварительно»)
	ProfileReason string `json:"profile_reason"`

	Notes []Note `json:"notes"`
}

// recommendedSize — размер по RAM: ≤ 8 GiB — как RAM, до 32 GiB —
// половина, больше — 16 GiB.
func recommendedSize(l i18n.Lang, ram uint64) (uint64, string, string) {
	switch {
	case ram <= 8*swap.GiB:
		return ram, "ram.small", i18n.T(l, "rec.ram.small", gbText(ram))
	case ram <= 32*swap.GiB:
		return ram / 2, "ram.medium", i18n.T(l, "rec.ram.medium", gbText(ram))
	default:
		return 16 * swap.GiB, "ram.large", i18n.T(l, "rec.ram.large", gbText(ram))
	}
}

// maxSwapFor — сколько можно занять на ФС с сохранением запаса места;
// reclaim — место, которое освободит текущий swap RamPlus на этой ФС
// (resize сначала удаляет старый файл).
func maxSwapFor(fi detect.FSInfo, reclaim uint64) uint64 {
	if fi.TotalBytes == nil || fi.AvailableBytes == nil {
		return 0
	}
	req := swap.RequiredFree(*fi.TotalBytes)
	have := *fi.AvailableBytes + reclaim
	if have <= req {
		return 0
	}
	return (have - req) / swap.MiB * swap.MiB
}

func targetOf(l i18n.Lang, fi detect.FSInfo, path string, reclaim uint64) RecTarget {
	t := RecTarget{Path: path, MountPoint: fi.MountPoint, FSType: fi.FSType, DiskKind: fi.Kind,
		Total: fi.TotalBytes, Available: fi.AvailableBytes, MaxBytes: maxSwapFor(fi, reclaim)}
	if len(fi.Disks) > 0 {
		t.Transport = fi.Disks[0].Transport
	}
	ok, _, short := swap.AssessFS(fi)
	if ok {
		if lvl, _, s := swap.AssessDisk(fi, false); lvl == plan.LevelFail {
			ok, short = false, s
		}
	}
	if ok && t.MaxBytes < swap.GiB {
		ok, short = false, i18n.Msg("swap.cand.no_space")
	}
	t.Usable = ok
	if short != nil {
		t.Problem = short.Text(l)
	}
	return t
}

// Recommend — рекомендация для этой машины. path — выбранное место
// swapfile ("" — по умолчанию). Проверяется тем же планировщиком, что и
// план: если план с рекомендуемым размером заблокирован, рекомендация
// говорит почему, а не предлагает невозможное.
func (c *Core) Recommend(ctx context.Context, path string, l i18n.Lang) Recommendation {
	f := swap.Gather(ctx, c.Sys, path)
	r := Recommendation{SchemaVersion: RecommendationSchemaVersion, SwapBlocked: []plan.Check{}, Targets: []RecTarget{}, Notes: []Note{}}
	note := func(code string, a ...any) {
		r.Notes = append(r.Notes, Note{Code: code, Message: i18n.T(l, "rec.note."+code, a...)})
	}

	var reclaim uint64
	if m := f.Managed; m != nil {
		r.ManagedBytes = &m.SizeBytes
		reclaim = m.SizeBytes // цель — ФС своего swap (resize)
	}
	t := targetOf(l, f.Target.FS, f.Target.Path, reclaim)
	t.Selected = true
	r.Target = &t
	r.SwapMaxBytes = t.MaxBytes
	for _, fi := range f.Candidates {
		ct := targetOf(l, fi, swap.SuggestedPath(fi), 0)
		if fi.MountPoint == f.Target.FS.MountPoint {
			ct = t
		}
		r.Targets = append(r.Targets, ct)
	}

	// Размер.
	r.RAMBytes = f.Mem["MemTotal"]
	r.RAMRoundedBytes = (r.RAMBytes + swap.GiB - 1) / swap.GiB * swap.GiB
	size, code, reason := recommendedSize(l, r.RAMRoundedBytes)
	if r.RAMBytes == 0 {
		code, reason, size = "ram.unknown", i18n.T(l, "rec.ram.unknown", swap.MeminfoPath), 0
	}
	if size > t.MaxBytes {
		size = t.MaxBytes
		code, reason = "disk.limited", i18n.T(l, "rec.disk.limited", reason, t.MountPoint, gbText(t.MaxBytes/swap.GiB*swap.GiB))
	}
	size = size / swap.GiB * swap.GiB
	r.SwapReasonCode, r.SwapReason = code, reason
	if r.RAMBytes != 0 && size < swap.GiB {
		r.SwapReasonCode = "disk.too_small"
		r.SwapReason = i18n.T(l, "rec.disk.too_small", t.MountPoint)
		size = 0
	}

	// Существующий swap.
	others := swap.OtherSwaps(f)
	hasZram := false
	for _, s := range f.Swaps {
		hasZram = hasZram || swap.IsZram(s.Path)
	}
	existing := ""
	switch {
	case f.Managed != nil:
	case len(others) > 0:
		existing = string(swap.ExistingReplace)
		r.ExistingReason = i18n.T(l, "rec.existing.replace")
		for _, o := range others {
			if o.Type != "file" {
				existing = string(swap.ExistingKeep)
				r.ExistingReason = i18n.T(l, "rec.existing.partition", o.Path)
			}
		}
	case hasZram:
		existing = string(swap.ExistingKeep)
		r.ExistingReason = i18n.T(l, "rec.existing.zram")
	}
	if size > 0 {
		o := swap.Options{Size: size, Path: path, Existing: swap.ExistingMode(existing), Lang: l}
		var sp *plan.Plan
		if existing == string(swap.ExistingReplace) {
			sp = c.recommendSwapPlan(f, o)
			if planHasCode(sp, "swapoff_memory.insufficient", "swapoff_memory.unreadable") {
				existing = string(swap.ExistingKeep)
				r.ExistingReason = i18n.T(l, "rec.existing.unsafe")
				o.Existing = swap.ExistingKeep
				sp = nil
			}
		}
		if sp == nil {
			sp = c.recommendSwapPlan(f, o)
		}
		if sp.Blocked() {
			r.SwapBlocked = sp.Failures()
			msgs := make([]string, 0, len(r.SwapBlocked))
			for _, ch := range r.SwapBlocked {
				msgs = append(msgs, ch.Message)
			}
			r.SwapReasonCode = "blocked"
			if planHasCode(sp, "disk.usb") {
				r.SwapReasonCode = "blocked.usb"
			}
			r.SwapReason = i18n.T(l, "rec.blocked", strings.Join(msgs, "; "))
			size = 0
		}
	}
	if size > 0 {
		r.SwapSizeBytes = &size
	}
	if existing != "" {
		r.Existing = &existing
	}

	// Профиль.
	r.Profile, r.ProfileReason = RecommendedProfile, i18n.T(l, "rec.profile.won")
	pp := profile.PlanProfile(ctx, c.Sys, profile.ProfileRequest{Name: RecommendedProfile, Lang: l})
	if fails := pp.Failures(); len(fails) > 0 {
		r.Profile = FallbackProfile
		r.ProfileReason = i18n.T(l, "rec.profile.no_zswap", fails[0].Message)
		note("profile.zswap_unavailable", r.ProfileReason)
	}
	if prof, ok := profile.Get(r.Profile); ok {
		r.ProfileStatus = prof.Status
		if prof.Status == profile.StatusExperimental {
			note("profile.preliminary", r.Profile)
		}
	}
	if r.SwapSizeBytes == nil {
		note("swap.none", r.SwapReason)
	}
	return r
}

// recommendSwapPlan — план swap для рекомендации (как часть setup).
func (c *Core) recommendSwapPlan(f swap.Facts, o swap.Options) *plan.Plan {
	p := &plan.Plan{Lang: o.Lang}
	so := setupSwapOptions(p, f, o)
	if so == nil {
		return p
	}
	sp := swap.BuildPlan(f, *so)
	return &sp.Plan
}

func planHasCode(p *plan.Plan, codes ...string) bool {
	for _, c := range p.Checks {
		for _, code := range codes {
			if c.Code == code && (c.Level == plan.LevelFail || c.Level == plan.LevelWarn) {
				return true
			}
		}
	}
	return false
}
