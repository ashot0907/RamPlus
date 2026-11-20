package swap

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/i18n"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/plan"
)

// Операции swap (rollback — общий, plan.ActionRollback).
const (
	ActionApply  Action = "apply"
	ActionResize Action = "resize"
	ActionRemove Action = "remove"
)

// ExistingMode — что делать с существующим (не нашим) swap.
type ExistingMode string

const (
	ExistingKeep    ExistingMode = "keep"
	ExistingReplace ExistingMode = "replace"
)

// Options — выбор пользователя.
type Options struct {
	Action    Action       `json:"action"`
	Size      uint64       `json:"size,omitempty"`
	Path      string       `json:"path,omitempty"`     // "" — по умолчанию
	Existing  ExistingMode `json:"existing,omitempty"` // "" — не указан
	DeleteOld bool         `json:"delete_old,omitempty"`
	Force     bool         `json:"force,omitempty"`
	// Orphan — remove: убрать следы RamPlus без записи в журнале.
	Orphan bool `json:"orphan,omitempty"`
	// Lang — язык текстов плана.
	Lang i18n.Lang `json:"-"`
}

// Command — командная строка, соответствующая опциям.
func (o Options) Command() string {
	var b strings.Builder
	if o.Action == ActionRollback {
		b.WriteString("ramplus rollback")
	} else {
		b.WriteString("ramplus swap " + string(o.Action))
	}
	if o.Size > 0 && (o.Action == ActionApply || o.Action == ActionResize) {
		b.WriteString(" --size " + sizeFlag(o.Size))
	}
	if o.Path != "" && o.Action == ActionApply {
		b.WriteString(" --path " + o.Path)
	}
	if o.Action == ActionApply && o.Existing != "" {
		b.WriteString(" --existing=" + string(o.Existing))
	}
	if o.DeleteOld {
		b.WriteString(" --delete-old")
	}
	if o.Orphan && o.Action == ActionRemove {
		b.WriteString(" --orphan")
	}
	if o.Force {
		b.WriteString(" --force")
	}
	return b.String()
}

// Plan — план swap: общая модель (plan.Plan) плюс то, что знает только
// swap-планировщик.
type Plan struct {
	plan.Plan
	Options           Options // с подставленными значениями по умолчанию
	Path              string  // путь swapfile
	ExistingDefaulted bool
	MaxSize           uint64 // сколько можно создать на ФС цели
	ManagedBefore     *Managed
	ManagedAfter      *Managed
	facts             Facts
}

func (p *Plan) add(l Level, code string, a ...any) { p.Plan.Add(l, code, a...) }

// Пороговые значения.
const (
	// После создания swap на ФС должно остаться max(5 GiB, min(10%, 20 GiB)).
	minFreeAfterBytes = 5 * GiB
	maxFreeAfterBytes = 20 * GiB
	minFreeAfterPct   = 10
	memReserve        = 256 * MiB // запас RAM при swapoff
)

// RequiredFree — сколько места должно остаться на ФС размером total.
func RequiredFree(total uint64) uint64 {
	return max(minFreeAfterBytes, min(total/100*minFreeAfterPct, maxFreeAfterBytes))
}

func requiredFreeText() i18n.Text {
	return i18n.Msg("swap.required_free", minFreeAfterPct, FormatSize(maxFreeAfterBytes), FormatSize(minFreeAfterBytes))
}

// MaxSwapSize — какой swapfile поместится на ФС с соблюдением порога.
func MaxSwapSize(fi detect.FSInfo) uint64 {
	if fi.TotalBytes == nil || fi.AvailableBytes == nil {
		return 0
	}
	req := RequiredFree(*fi.TotalBytes)
	if *fi.AvailableBytes <= req {
		return 0
	}
	return (*fi.AvailableBytes - req) / MiB * MiB
}

// BuildPlan строит план. Чистая функция: ничего не читает и не меняет.
func BuildPlan(f Facts, o Options) Plan {
	p := buildPlan(f, o)
	if o.Action == ActionApply {
		p.Choices = applyChoices(f, o, p)
	}
	p.Plan.Seal(f.Basics.FingerprintPart(), f.fingerprintPart())
	return p
}

func buildPlan(f Facts, o Options) Plan {
	p := Plan{Options: o, ManagedBefore: f.Managed, facts: f, Path: f.Target.Path}
	p.Plan.Action = o.Action
	p.Plan.Lang = o.Lang
	journal.CommonChecks(&p.Plan, f.Basics)
	switch o.Action {
	case ActionApply:
		p.planApply(f)
	case ActionResize:
		p.planResize(f)
	case ActionRemove:
		if o.Orphan {
			p.planOrphan(f)
		} else {
			p.planRemove(f)
		}
	case ActionRollback:
		journal.PlanRollback(&p.Plan, f.Basics)
	default:
		p.add(LevelFail, "operation.unknown", o.Action)
	}
	if o.Action != ActionRollback {
		// swap-операции меняют только раздел managed; профиль — как был.
		p.Effects.Managed = EncodeManaged(p.ManagedAfter)
		p.Effects.Profile = f.State.Profile
		p.Effects.Finalize = finalize
	}
	p.diskCheck(f)
	p.memoryCheck(f)
	p.summarize(f)
	if p.Steps == nil {
		p.Steps = []Step{}
	}
	// p.Options — с подставленными значениями по умолчанию (--existing=keep).
	p.Plan.Command = p.Options.Command()
	p.Plan.Swap = view(f, p.Options, o.Lang)
	return p
}

// view — блок «Сейчас» для плана: что есть в системе.
func view(f Facts, o Options, l i18n.Lang) *plan.SwapView {
	title := ""
	if o.Action != "" {
		title = i18n.T(l, "swap.view_title."+string(o.Action))
	}
	v := &plan.SwapView{Swaps: []plan.SwapRow{}, ApplyCommand: o.Command(), Title: title}
	for _, s := range f.Swaps {
		row := plan.SwapRow{Path: s.Path, Type: s.Type, Size: s.SizeBytes, Used: s.UsedBytes, Priority: s.Priority}
		switch {
		case isZram(s.Path):
			row.Tag = i18n.T(l, "swap.tag.zram")
		case f.Managed != nil && s.Path == f.Managed.Path:
			row.Tag = "RamPlus"
		case len(f.fstabEntriesFor(s.Path)) > 0:
			row.Tag = i18n.T(l, "swap.tag.fstab")
		}
		v.Swaps = append(v.Swaps, row)
	}
	fi := f.Target.FS
	t := &plan.FSView{FSType: fi.FSType, Source: fi.Source, MountPoint: fi.MountPoint, Disk: i18n.T(l, "swap.disk_na"),
		Total: fi.TotalBytes, Avail: fi.AvailableBytes}
	if len(fi.Disks) > 0 {
		d := fi.Disks[0]
		t.Disk = fmt.Sprintf("%s, %s, %s", d.Name, KindText(fi.Kind).Text(l), d.Transport)
	}
	if fi.FSType != "" || (fi.TotalBytes != nil && fi.AvailableBytes != nil) {
		v.Target = t
	}
	if a, ok := f.Mem["MemAvailable"]; ok {
		v.MemAvailable = &a
		v.MemTotal = f.Mem["MemTotal"]
	}
	if m := f.Managed; m != nil {
		v.Managed = &plan.ManagedView{Path: m.Path, Size: m.SizeBytes, Priority: m.Priority, Unit: m.Unit}
	}
	return v
}

func (p *Plan) checkSize(size uint64) bool {
	switch {
	case size == 0:
		p.add(LevelFail, "size.missing")
		return false
	case size < MinSize:
		p.add(LevelFail, "size.too_small", FormatSize(size), FormatSize(MinSize))
		return false
	}
	return true
}

// Оценка ФС.

// unsupportedFS — ФС, на которых swapfile невозможен (причина — ключ каталога).
var unsupportedFS = map[string]string{
	"zfs":     "fs.reason.zfs",
	"tmpfs":   "fs.reason.tmpfs",
	"ramfs":   "fs.reason.ramfs",
	"overlay": "fs.reason.overlay",
}

// foreignFS — ФС Windows и флешек: swapfile на них Linux не поддерживает.
var foreignFS = map[string]bool{"ntfs": true, "ntfs3": true, "vfat": true, "msdos": true, "exfat": true, "fuseblk": true}

// AssessFS — можно ли создать swapfile на ФС. short — пометка для списка.
func AssessFS(fi detect.FSInfo) (ok bool, reason, short i18n.Text) {
	t := fi.FSType
	unfit := i18n.Msg("fs.short.unfit")
	switch {
	case fi.Error != nil:
		return false, i18n.Msg("fs.reason.unknown_detail", fi.Error.Detail), i18n.Msg("fs.short.unknown")
	case fi.ReadOnly:
		return false, i18n.Msg("fs.reason.read_only", t), i18n.Msg("fs.short.read_only")
	case fi.Network:
		return false, i18n.Msg("fs.reason.network", t), i18n.Msg("fs.short.network")
	case foreignFS[t]:
		return false, i18n.Msg("fs.reason.foreign", t), unfit
	case unsupportedFS[t] != "":
		return false, i18n.Msg(unsupportedFS[t], t), unfit
	case strings.HasPrefix(t, "fuse"):
		return false, i18n.Msg("fs.reason.fuse", t), unfit
	case t == "ext4" || t == "xfs" || t == "btrfs":
		return true, nil, nil
	case t == "":
		return false, i18n.Msg("fs.reason.unknown"), i18n.Msg("fs.short.unknown")
	default:
		return false, i18n.Msg("fs.reason.unsupported", t), unfit
	}
}

// KindText — тип диска для человека: NVMe, SSD, HDD, виртуальный диск.
func KindText(k string) i18n.Text {
	switch k {
	case detect.KindNVMe, detect.KindSSD, detect.KindHDD, detect.KindVirtual:
		return i18n.Msg("disk.kind." + k)
	default:
		return i18n.Msg("disk.kind.unknown")
	}
}

func onUSB(fi detect.FSInfo) bool { return detect.HasTransport(fi.Disks, "usb") }

// AssessDisk — оценка диска под ФС: уровень проверки, сообщение и пометка.
func AssessDisk(fi detect.FSInfo, force bool) (Level, i18n.Text, i18n.Text) {
	switch {
	case onUSB(fi) && force:
		return LevelWarn, i18n.Msg("disk.usb_force"), i18n.Raw("USB")
	case onUSB(fi):
		return LevelFail, i18n.Msg("disk.usb"), i18n.Msg("disk.short.usb")
	case fi.Kind == detect.KindVirtual:
		return LevelInfo, i18n.Msg("disk.virtual"), i18n.Msg("disk.kind.virtual")
	case fi.Kind == detect.KindHDD:
		return LevelWarn, i18n.Msg("disk.hdd"), i18n.Msg("disk.short.hdd")
	case len(fi.Disks) == 0 || fi.Kind == detect.KindUnknown:
		return LevelWarn, i18n.Msg("disk.unknown"), i18n.Msg("disk.unknown")
	default:
		return LevelOK, i18n.Msg("disk.ok", KindText(fi.Kind), fi.Disks[0].Transport), nil
	}
}

// fsMethod — способ создания swapfile.
type fsMethod string

const (
	methodFallocate   fsMethod = "fallocate"
	methodBtrfsTool   fsMethod = "btrfs-mkswapfile"
	methodBtrfsChattr fsMethod = "btrfs-chattr"
)

func methodFor(f Facts, fsType string) fsMethod {
	switch {
	case fsType != "btrfs":
		return methodFallocate
	case f.HasMkswapfile:
		return methodBtrfsTool
	default:
		return methodBtrfsChattr
	}
}

// checkTargetFS — проверки ФС под путём swapfile.
func (p *Plan) checkTargetFS(f Facts) (fsMethod, bool) {
	fi := f.Target.FS
	ok, reason, _ := AssessFS(fi)
	if !ok {
		p.add(LevelFail, "fs.unsupported", reason)
		return "", false
	}
	where := i18n.Msg("swap.fs_where", fi.FSType, fi.Source, fi.MountPoint)
	if fi.FSType != "btrfs" {
		p.add(LevelOK, "fs.fallocate", where)
		return methodFallocate, true
	}
	if !f.HasBtrfs || !f.HasChattr {
		p.add(LevelFail, "fs.btrfs_tools_missing")
		return "", false
	}
	m := methodFor(f, "btrfs")
	how := "chattr +C (NOCOW) + fallocate + mkswap"
	if m == methodBtrfsTool {
		how = "btrfs filesystem mkswapfile"
	}
	if f.Target.Path == BtrfsPath {
		p.add(LevelOK, "fs.btrfs_subvolume", where, BtrfsDir, how)
	} else {
		p.add(LevelOK, "fs.btrfs_path", where, how)
		p.add(LevelWarn, "btrfs.snapshots", BtrfsDir)
	}
	return m, true
}

func (p *Plan) checkDisk(fi detect.FSInfo) {
	level, msg, _ := AssessDisk(fi, p.Options.Force)
	p.AddMsg(level, diskCode(fi), i18n.Msg("check.disk.title"), msg)
}

// diskCode — машинный код проверки диска (условия как в AssessDisk;
// уровень зависит от --force, код — нет).
func diskCode(fi detect.FSInfo) string {
	switch {
	case onUSB(fi):
		return "disk.usb"
	case fi.Kind == detect.KindVirtual:
		return "disk.virtual"
	case fi.Kind == detect.KindHDD:
		return "disk.hdd"
	case len(fi.Disks) == 0 || fi.Kind == detect.KindUnknown:
		return "disk.unknown"
	default:
		return "disk.ok"
	}
}

var forbiddenDirs = []string{"/proc", "/sys", "/dev", "/run", "/tmp", "/var/tmp", "/boot"}

// checkPath — проверки самого пути swapfile (apply).
func (p *Plan) checkPath(f Facts) bool {
	t := f.Target
	if !filepath.IsAbs(t.Path) {
		p.add(LevelFail, "path.relative", t.Requested)
		return false
	}
	for _, d := range forbiddenDirs {
		if t.Path == d || strings.HasPrefix(t.Path, d+"/") {
			p.add(LevelFail, "path.forbidden", t.Path, d)
			return false
		}
	}
	if t.ParentErr != nil {
		p.add(LevelFail, "path.parent", filepath.Dir(t.Path), t.ParentErr)
		return false
	}
	how := i18n.Msg("swap.path_how.flag")
	if t.Defaulted {
		how = i18n.Msg("swap.path_how.default")
	}
	p.add(LevelInfo, "path.selected", t.Path, how)
	return true
}

// fileSteps — создание swapfile (без каталога, swapon и юнита).
func fileSteps(path string, size uint64, m fsMethod) []Step {
	if m == methodBtrfsTool {
		return []Step{
			{Kind: KindBtrfsMkswapfile, Path: path, Size: size},
			{Kind: KindChmod, Path: path, Mode: 0o600},
		}
	}
	return []Step{
		{Kind: KindAllocate, Path: path, Size: size, NoCOW: m == methodBtrfsChattr},
		{Kind: KindChmod, Path: path, Mode: 0o600},
		{Kind: KindMkswap, Path: path},
	}
}

// otherSwaps — активный swap, не принадлежащий RamPlus и не zram.
func otherSwaps(f Facts) []monitor.SwapDevice {
	var out []monitor.SwapDevice
	for _, s := range f.Swaps {
		if !isZram(s.Path) && !f.isOurPath(s.Path) {
			out = append(out, s)
		}
	}
	return out
}

// choosePriority: выше существующего дискового swap, но ниже zram (zram
// быстрее и должен использоваться первым).
func choosePriority(f Facts) (prio int, note i18n.Text, conflict bool) {
	maxDisk, zramMin, hasDisk, hasZram := -1, 0, false, false
	for _, s := range f.Swaps {
		switch {
		case isZram(s.Path):
			if !hasZram || s.Priority < zramMin {
				zramMin = s.Priority
			}
			hasZram = true
		case !f.isOurPath(s.Path):
			// Начальное -1 не годится: приоритеты бывают -2, -3…
			if !hasDisk || s.Priority > maxDisk {
				maxDisk = s.Priority
			}
			hasDisk = true
		}
	}
	prio = max(10, maxDisk+10)
	if hasZram && prio >= zramMin {
		if zramMin-1 > maxDisk && zramMin-1 >= 0 {
			prio = zramMin - 1
		} else {
			conflict = true
		}
	}
	prio = min(prio, 32767)
	switch {
	case hasDisk && hasZram:
		note = i18n.Msg("swap.prio.disk_zram", maxDisk, zramMin)
	case hasDisk:
		note = i18n.Msg("swap.prio.disk", maxDisk)
	case hasZram:
		note = i18n.Msg("swap.prio.zram", zramMin)
	default:
		note = i18n.Msg("swap.prio.none")
	}
	return prio, note, conflict
}

func (p *Plan) planApply(f Facts) {
	o := &p.Options
	if m := f.Managed; m != nil {
		p.add(LevelFail, "managed.exists", m.Path, FormatSize(m.SizeBytes))
		return
	}
	sizeOK := p.checkSize(o.Size)
	pathOK := p.checkPath(f)
	method, fsOK := p.checkTargetFS(f)
	p.checkDisk(f.Target.FS)

	others := otherSwaps(f)
	switch {
	case len(others) > 0 && o.Existing == "":
		o.Existing = ExistingKeep
		p.ExistingDefaulted = true
		p.add(LevelWarn, "existing.defaulted_keep", swapList(others))
	case len(others) > 0:
		p.add(LevelInfo, "existing.selected", swapList(others), o.Existing)
	case o.Existing == ExistingReplace:
		p.add(LevelInfo, "existing.none")
	}
	replace := o.Existing == ExistingReplace && len(others) > 0
	if o.DeleteOld && !replace {
		p.add(LevelInfo, "delete_old.ignored")
	}
	if replace {
		p.checkResume(f, i18n.Msg("swap.op.replace"))
		if f.FstabErr != nil {
			p.add(LevelFail, "fstab.unreadable", FstabPath, f.FstabErr)
		}
	}

	prio, note, conflict := choosePriority(f)
	if conflict {
		p.add(LevelWarn, "priority.above_zram", prio)
	} else {
		p.add(LevelInfo, "priority.selected", prio, note)
	}

	if !pathOK || !fsOK {
		return
	}
	path := f.Target.Path
	dir := ""
	if path == BtrfsPath && f.Target.FS.FSType == "btrfs" {
		dir = BtrfsDir
	}
	unit := UnitName(path)
	unitPath := filepath.Join(UnitDir, unit)
	for _, ex := range []string{path, dir, unitPath} {
		if ex != "" && f.Exists[ex] {
			if f.isOrphan(ex) {
				p.add(LevelFail, "files.orphan", ex)
				continue
			}
			p.add(LevelFail, "files.foreign", ex)
		}
	}
	if !sizeOK {
		return
	}

	if dir != "" {
		p.Steps = append(p.Steps,
			Step{Kind: KindBtrfsSubvolCreate, Path: dir},
			Step{Kind: KindChattrNoCOW, Path: dir})
	}
	p.Steps = append(p.Steps, fileSteps(path, o.Size, method)...)
	p.Steps = append(p.Steps,
		Step{Kind: KindSwapon, Path: path, Priority: prio, Size: o.Size},
		Step{Kind: KindWriteFile, Path: unitPath, Content: UnitContent(path, prio), Mode: 0o644, Reload: true},
		daemonReload(),
		Step{Kind: KindSystemctl, Args: []string{"enable", unit}},
	)

	if replace {
		fstabChanged := false
		var deletes []Step
		for _, old := range others {
			p.Steps = append(p.Steps, Step{Kind: KindSwapoff, Path: old.Path, Priority: old.Priority, Size: old.SizeBytes})
			entries := f.fstabEntriesFor(old.Path)
			if len(entries) == 0 {
				p.add(LevelWarn, "fstab.not_listed", old.Path, FstabPath)
			}
			for _, e := range entries {
				p.Steps = append(p.Steps, Step{Kind: KindFstabDisable, Path: FstabPath, Line: e.Line, Reload: true})
				fstabChanged = true
			}
			if o.DeleteOld {
				if old.Type == "file" {
					// Размер файла (а не из /proc/swaps — там без заголовка):
					// delete_swapfile сверяет его перед удалением.
					deletes = append(deletes, Step{Kind: KindDeleteSwapfile, Path: old.Path, Size: f.Observed[old.Path].Size})
				} else {
					p.add(LevelInfo, "delete_old.partition", old.Path)
				}
			}
		}
		if fstabChanged {
			p.Steps = append(p.Steps, daemonReload())
		}
		p.Steps = append(p.Steps, deletes...)
	}

	p.ManagedAfter = &Managed{Path: path, Dir: dir, SizeBytes: o.Size, Priority: prio, Unit: unit, FSType: f.Target.FS.FSType}
}

func (p *Plan) checkResume(f Facts, what i18n.Text) {
	if r := ResumeParam(f.Cmdline); r != "" {
		p.add(LevelWarn, "hibernation.resume", r, what)
	}
}

func (p *Plan) requireManaged(f Facts) *Managed {
	m := f.Managed
	if m == nil {
		p.add(LevelFail, "managed.missing")
	}
	return m
}

func (p *Plan) planResize(f Facts) {
	o := p.Options
	m := p.requireManaged(f)
	sizeOK := p.checkSize(o.Size)
	if m == nil || !sizeOK {
		return
	}
	if o.Size == m.SizeBytes {
		p.add(LevelFail, "size.unchanged", FormatSize(m.SizeBytes))
		return
	}
	p.add(LevelInfo, "size.change", FormatSize(m.SizeBytes), FormatSize(o.Size))
	p.checkDisk(f.Target.FS)
	if m.FSType == "btrfs" && (!f.HasBtrfs || !f.HasChattr) {
		p.add(LevelFail, "fs.btrfs_tools_missing")
	}
	if _, active := f.swapByPath(m.Path); active {
		p.Steps = append(p.Steps, Step{Kind: KindSwapoff, Path: m.Path, Priority: m.Priority, Size: m.SizeBytes})
	} else {
		p.add(LevelWarn, "managed.inactive", m.Path)
	}
	if f.Exists[m.Path] {
		p.Steps = append(p.Steps, Step{Kind: KindDeleteSwapfile, Path: m.Path, Size: m.SizeBytes, NoCOW: m.FSType == "btrfs"})
	}
	p.Steps = append(p.Steps, fileSteps(m.Path, o.Size, methodFor(f, m.FSType))...)
	p.Steps = append(p.Steps, Step{Kind: KindSwapon, Path: m.Path, Priority: m.Priority, Size: o.Size})
	after := *m
	after.SizeBytes = o.Size
	p.ManagedAfter = &after
}

func (p *Plan) planRemove(f Facts) {
	m := p.requireManaged(f)
	if m == nil {
		return
	}
	p.checkResume(f, i18n.Msg("swap.op.remove"))
	if _, active := f.swapByPath(m.Path); active {
		p.Steps = append(p.Steps, Step{Kind: KindSwapoff, Path: m.Path, Priority: m.Priority, Size: m.SizeBytes})
	}
	unitPath := filepath.Join(UnitDir, m.Unit)
	if f.Exists[unitPath] {
		p.Steps = append(p.Steps,
			Step{Kind: KindSystemctl, Args: []string{"disable", m.Unit}},
			Step{Kind: KindDeleteConfig, Path: unitPath, Reload: true},
			daemonReload(),
		)
	}
	if f.Exists[m.Path] {
		p.Steps = append(p.Steps, Step{Kind: KindDeleteSwapfile, Path: m.Path, Size: m.SizeBytes, NoCOW: m.FSType == "btrfs"})
	}
	if m.Dir != "" {
		p.Steps = append(p.Steps, Step{Kind: KindBtrfsSubvolDelete, Path: m.Dir})
	}
	remaining := 0
	for _, s := range f.Swaps {
		if s.Path != m.Path {
			remaining++
		}
	}
	if remaining == 0 {
		p.add(LevelWarn, "swap.none_left")
	}
	for _, op := range f.State.Operations {
		if op.Status != journal.OpCommitted {
			continue
		}
		for _, a := range op.Applied {
			if a.Step.Kind == KindFstabDisable {
				p.add(LevelInfo, "old_swap.disabled", strings.TrimSpace(a.Step.Line), op.ID)
			}
		}
	}
	p.ManagedAfter = nil
}

// diskCheck: после создания на ФС цели должно остаться RequiredFree.
func (p *Plan) diskCheck(f Facts) {
	fi := f.Target.FS
	var running, peak int64
	for _, s := range p.Steps {
		switch s.Kind {
		case KindAllocate, KindBtrfsMkswapfile:
			running += int64(s.Size)
		case KindDeleteSwapfile:
			if f.isOurPath(s.Path) {
				running -= int64(s.Size)
			}
		}
		peak = max(peak, running)
	}
	if p.Action == ActionApply {
		p.MaxSize = MaxSwapSize(fi)
	}
	if peak == 0 {
		return
	}
	if fi.TotalBytes == nil || fi.AvailableBytes == nil {
		p.add(LevelFail, "disk_space.unknown", fi.MountPoint)
		return
	}
	total, avail := *fi.TotalBytes, *fi.AvailableBytes
	required := RequiredFree(total)
	remaining := int64(avail) - peak
	msg := i18n.Msg("swap.disk_space", fi.MountPoint, FormatSize(avail), FormatSize(total), formatSigned(remaining), FormatSize(required), requiredFreeText())
	switch {
	case remaining >= int64(required):
		p.add(LevelOK, "disk_space.ok", msg)
	case p.Action == ActionRollback && p.Options.Force:
		p.AddKey(LevelWarn, "disk_space.insufficient", "disk_space.insufficient_force", msg)
	default:
		p.add(LevelFail, "disk_space.insufficient", msg)
	}
}

func formatSigned(v int64) i18n.Text {
	if v < 0 {
		return i18n.Msg("swap.short_by", FormatSize(uint64(-v)))
	}
	return i18n.Raw(FormatSize(uint64(v)))
}

// memoryCheck: перед каждым swapoff данные из этого swap должны поместиться
// в свободную RAM + свободное место в остающемся swap.
func (p *Plan) memoryCheck(f Facts) {
	type dev struct{ size, used uint64 }
	active := map[string]dev{}
	for _, s := range f.Swaps {
		active[s.Path] = dev{s.SizeBytes, s.UsedBytes}
	}
	memAvail, haveMem := f.Mem["MemAvailable"]
	if !haveMem {
		memAvail, haveMem = f.Mem["MemFree"]
	}
	for _, s := range p.Steps {
		switch s.Kind {
		case KindSwapon:
			active[s.Path] = dev{size: s.Size}
		case KindSwapoff:
			d, ok := active[s.Path]
			delete(active, s.Path)
			if !ok {
				continue
			}
			var free uint64
			for _, o := range active {
				if o.size > o.used {
					free += o.size - o.used
				}
			}
			title := i18n.Msg("check.swapoff_memory.title", s.Path)
			check := func(l Level, code string, a ...any) { p.AddMsg(l, code, title, i18n.Msg("check."+code, a...)) }
			if d.used == 0 {
				check(LevelOK, "swapoff_memory.unused", s.Path)
				continue
			}
			if !haveMem {
				check(LevelFail, "swapoff_memory.unreadable", MeminfoPath, s.Path, FormatSize(d.used))
				continue
			}
			budget := memAvail + free
			msg := i18n.Msg("swap.swapoff_budget", FormatSize(d.used), FormatSize(memAvail), FormatSize(free), FormatSize(memReserve))
			if d.used+memReserve > budget {
				check(LevelFail, "swapoff_memory.insufficient", msg)
			} else {
				check(LevelOK, "swapoff_memory.ok", msg)
			}
			memAvail -= min(memAvail, d.used)
		}
	}
}

// summarize считает итоговый объём swap и списки файлов.
func (p *Plan) summarize(f Facts) {
	active := map[string]uint64{}
	for _, s := range f.Swaps {
		active[s.Path] = s.SizeBytes
		p.SwapBefore += s.SizeBytes
	}
	created, disabled, deleted := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range p.Steps {
		switch s.Kind {
		case KindAllocate, KindBtrfsMkswapfile, KindWriteFile, KindMkdir, KindBtrfsSubvolCreate:
			if deleted[s.Path] {
				delete(deleted, s.Path)
			} else if !f.Exists[s.Path] || s.Kind != KindWriteFile {
				created[s.Path] = true
			}
		case KindSwapon:
			active[s.Path] = s.Size
			delete(disabled, s.Path)
		case KindSwapoff:
			delete(active, s.Path)
			disabled[s.Path] = true
		case KindDeleteSwapfile, KindDeleteConfig, KindRmdir, KindBtrfsSubvolDelete:
			if created[s.Path] {
				delete(created, s.Path)
			} else {
				deleted[s.Path] = true
			}
		case KindFstabDisable:
			disabled[fstabLabel(p.Lang, s.Line)] = true
		case KindFstabEnable:
			delete(disabled, fstabLabel(p.Lang, s.Line))
		}
	}
	for _, sz := range active {
		p.SwapAfter += sz
	}
	for path := range disabled { // swapoff+swapon того же пути (resize) — не «отключение»
		if _, on := active[path]; on {
			delete(disabled, path)
		}
	}
	p.Created, p.Disabled, p.Deleted = sortedKeys(created), sortedKeys(disabled), sortedKeys(deleted)
}

func fstabLabel(l i18n.Lang, line string) string {
	return i18n.T(l, "swap.fstab_label", strings.Join(strings.Fields(line), " "))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func swapList(devs []monitor.SwapDevice) i18n.Text {
	parts := make([]i18n.Text, len(devs))
	for i, d := range devs {
		parts[i] = i18n.Msg("swap.list_item", d.Path, d.Type, FormatSize(d.SizeBytes), FormatSize(d.UsedBytes), d.Priority)
	}
	return i18n.Join{Sep: ", ", Parts: parts}
}

// candidateSummary — описание варианта места для swapfile.
func candidateSummary(l i18n.Lang, fi detect.FSInfo, size uint64, force bool) string {
	parts := []string{i18n.T(l, "swap.cand.fs_on", fi.FSType, fi.Source)}
	if len(fi.Disks) > 0 {
		parts = append(parts, KindText(fi.Kind).Text(l)+"/"+fi.Disks[0].Transport)
	}
	if fi.AvailableBytes != nil && fi.TotalBytes != nil {
		parts = append(parts, i18n.T(l, "swap.cand.free", FormatSize(*fi.AvailableBytes), FormatSize(*fi.TotalBytes)))
	}
	var marks []string
	if ok, _, short := AssessFS(fi); !ok {
		marks = append(marks, short.Text(l))
	} else {
		if _, _, short := AssessDisk(fi, force); short != nil {
			marks = append(marks, short.Text(l))
		}
		maxSize := MaxSwapSize(fi)
		switch {
		case maxSize < MinSize:
			marks = append(marks, i18n.T(l, "swap.cand.no_space"))
		case size > maxSize:
			marks = append(marks, i18n.T(l, "swap.cand.no_space_up_to", FormatSize(maxSize)))
		default:
			marks = append(marks, i18n.T(l, "swap.cand.up_to", FormatSize(maxSize)))
		}
	}
	return strings.Join(parts, ", ") + " — " + strings.Join(marks, "; ")
}

// applyChoices — выбор пользователя и альтернативы с последствиями.
func applyChoices(f Facts, o Options, p Plan) []Choice {
	// Место.
	l := o.Lang
	where := Choice{Title: i18n.T(l, "choice.path"), Flag: "--path", Value: f.Target.Path, Defaulted: f.Target.Defaulted}
	selected := false
	for _, fi := range f.Candidates {
		alt := Alternative{Value: SuggestedPath(fi), Summary: candidateSummary(l, fi, o.Size, o.Force)}
		if fi.MountPoint == f.Target.FS.MountPoint && !selected {
			alt.Value, alt.Selected, selected = f.Target.Path, true, true
		}
		where.Alternatives = append(where.Alternatives, alt)
	}
	if !selected && f.Target.FS.MountPoint != "" {
		where.Alternatives = append([]Alternative{{Value: f.Target.Path, Selected: true,
			Summary: candidateSummary(l, f.Target.FS, o.Size, o.Force)}}, where.Alternatives...)
	}
	where.Note = i18n.T(l, "choice.path_note", DefaultFileName)

	size := Choice{Title: i18n.T(l, "choice.size"), Flag: "--size", Value: i18n.T(l, "choice.size_unset")}
	if o.Size > 0 {
		size.Value = FormatSize(o.Size)
	}
	switch {
	case p.MaxSize >= MinSize:
		size.Note = i18n.T(l, "choice.size_note", f.Target.FS.MountPoint, FormatSize(p.MaxSize))
	case f.Target.FS.TotalBytes != nil:
		size.Note = i18n.T(l, "choice.size_note_none", f.Target.FS.MountPoint, FormatSize(MinSize))
	}
	choices := []Choice{where, size}

	others := otherSwaps(f)
	if len(others) == 0 {
		return choices
	}
	var names []string
	for _, d := range others {
		names = append(names, d.Path)
	}
	oldList := strings.Join(names, ", ")
	existing := Choice{Title: i18n.T(l, "choice.existing"), Flag: "--existing", Value: string(p.Options.Existing), Defaulted: p.ExistingDefaulted}
	for _, mode := range []ExistingMode{ExistingKeep, ExistingReplace} {
		alt := o
		alt.Existing = mode
		ap := buildPlan(f, alt)
		var sum string
		if mode == ExistingKeep {
			sum = i18n.T(l, "choice.keep", FormatSize(ap.SwapAfter), oldList)
			if ap.ManagedAfter != nil {
				sum += i18n.T(l, "choice.keep_prio", ap.ManagedAfter.Priority)
			}
		} else {
			sum = i18n.T(l, "choice.replace", FormatSize(ap.SwapAfter), oldList, FstabPath)
			if o.DeleteOld {
				sum += i18n.T(l, "choice.replace_delete")
			} else {
				sum += i18n.T(l, "choice.replace_keep_file")
			}
		}
		if ap.Blocked() && !p.Blocked() {
			sum += i18n.T(l, "choice.blocked_now", ap.Failures()[0].Message)
		}
		existing.Alternatives = append(existing.Alternatives, Alternative{Value: string(mode), Selected: p.Options.Existing == mode, Summary: sum})
	}
	choices = append(choices, existing)

	if p.Options.Existing == ExistingReplace {
		var files []string
		var total uint64
		for _, d := range others {
			if d.Type == "file" {
				files = append(files, d.Path)
				total += d.SizeBytes
			}
		}
		if len(files) > 0 {
			keep, remove := i18n.T(l, "choice.delete_old.keep"), i18n.T(l, "choice.delete_old.delete")
			del := Choice{Title: i18n.T(l, "choice.delete_old"), Flag: "--delete-old", Value: keep, Defaulted: !o.DeleteOld}
			if o.DeleteOld {
				del.Value = remove
			}
			del.Alternatives = []Alternative{
				{Value: keep, Selected: !o.DeleteOld, Summary: i18n.T(l, "choice.delete_old.keep_sum", strings.Join(files, ", "), FormatSize(total))},
				{Value: remove, Selected: o.DeleteOld, Summary: i18n.T(l, "choice.delete_old.delete_sum", FormatSize(total))},
			}
			choices = append(choices, del)
		}
	}
	return choices
}

// View — блок «Сейчас» (swap в системе, ФС цели, RAM) без операции:
// для плана setup, в котором swap не меняется.
func View(f Facts, l i18n.Lang) *plan.SwapView { return view(f, Options{}, l) }

// FingerprintPart — часть отпечатка плана от фактов swap (для составных
// планов: setup).
func (f Facts) FingerprintPart() any { return f.fingerprintPart() }

// OtherSwaps — активный swap не RamPlus и не zram (его можно заменить или
// оставить).
func OtherSwaps(f Facts) []monitor.SwapDevice { return otherSwaps(f) }

// IsZram — путь swap — устройство zram.
func IsZram(path string) bool { return isZram(path) }
