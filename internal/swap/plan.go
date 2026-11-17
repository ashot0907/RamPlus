package swap

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
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

func (p *Plan) add(l Level, title, format string, a ...any) { p.Plan.Add(l, title, format, a...) }

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

func requiredFreeText() string {
	return fmt.Sprintf("%d%% диска, но не больше %s и не меньше %s", minFreeAfterPct, FormatSize(maxFreeAfterBytes), FormatSize(minFreeAfterBytes))
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
	return p
}

func buildPlan(f Facts, o Options) Plan {
	p := Plan{Options: o, ManagedBefore: f.Managed, facts: f, Path: f.Target.Path}
	p.Plan.Action = o.Action
	journal.CommonChecks(&p.Plan, f.Basics)
	switch o.Action {
	case ActionApply:
		p.planApply(f)
	case ActionResize:
		p.planResize(f)
	case ActionRemove:
		p.planRemove(f)
	case ActionRollback:
		journal.PlanRollback(&p.Plan, f.Basics)
	default:
		p.add(LevelFail, "операция", "неизвестная операция %q", o.Action)
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
	p.Plan.Swap = view(f, p.Options)
	return p
}

var actionTitle = map[Action]string{
	ActionApply:    "создание swapfile",
	ActionResize:   "изменение размера swapfile",
	ActionRemove:   "удаление swapfile",
	ActionRollback: "откат последней операции",
}

// view — блок «Сейчас» для плана: что есть в системе.
func view(f Facts, o Options) *plan.SwapView {
	v := &plan.SwapView{Swaps: []plan.SwapRow{}, ApplyCommand: o.Command(), Title: actionTitle[o.Action]}
	for _, s := range f.Swaps {
		row := plan.SwapRow{Path: s.Path, Type: s.Type, Size: s.SizeBytes, Used: s.UsedBytes, Priority: s.Priority}
		switch {
		case isZram(s.Path):
			row.Tag = "zram — не затрагивается"
		case f.Managed != nil && s.Path == f.Managed.Path:
			row.Tag = "RamPlus"
		case len(f.fstabEntriesFor(s.Path)) > 0:
			row.Tag = "в /etc/fstab"
		}
		v.Swaps = append(v.Swaps, row)
	}
	fi := f.Target.FS
	t := &plan.FSView{FSType: fi.FSType, Source: fi.Source, MountPoint: fi.MountPoint, Disk: "диск n/a",
		Total: fi.TotalBytes, Avail: fi.AvailableBytes}
	if len(fi.Disks) > 0 {
		d := fi.Disks[0]
		t.Disk = fmt.Sprintf("%s, %s, %s", d.Name, kindText(fi.Kind), d.Transport)
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
		p.add(LevelFail, "размер", "укажите размер: --size, например --size 8G")
		return false
	case size < MinSize:
		p.add(LevelFail, "размер", "%s меньше минимума %s", FormatSize(size), FormatSize(MinSize))
		return false
	}
	return true
}

// Оценка ФС.

var unsupportedFS = map[string]string{
	"zfs":     "ZFS не поддерживает swapfile (возможны зависания под нагрузкой); используйте zvol или zram",
	"tmpfs":   "tmpfs лежит в RAM — swap в нём бессмыслен",
	"ramfs":   "ramfs лежит в RAM — swap в нём бессмыслен",
	"overlay": "overlayfs (контейнер или live-система) не поддерживает swapfile",
}

// foreignFS — ФС Windows и флешек: swapfile на них Linux не поддерживает.
var foreignFS = map[string]bool{"ntfs": true, "ntfs3": true, "vfat": true, "msdos": true, "exfat": true, "fuseblk": true}

const foreignFSMsg = "swapfile на этой ФС не поддерживается Linux; создайте раздел ext4/xfs"

// AssessFS — можно ли создать swapfile на ФС. short — пометка для списка.
func AssessFS(fi detect.FSInfo) (ok bool, reason, short string) {
	t := fi.FSType
	switch {
	case fi.Error != nil:
		return false, "не удалось определить ФС: " + fi.Error.Detail, "ФС не определена"
	case fi.ReadOnly:
		return false, t + " смонтирована только для чтения", "только чтение — не подходит"
	case fi.Network:
		return false, t + ": сетевая ФС — swap поверх сети ненадёжен и не поддерживается", "сетевая ФС — не подходит"
	case foreignFS[t]:
		return false, t + ": " + foreignFSMsg, "не подходит"
	case unsupportedFS[t] != "":
		return false, t + ": " + unsupportedFS[t], "не подходит"
	case strings.HasPrefix(t, "fuse"):
		return false, t + ": FUSE не поддерживает swapfile", "не подходит"
	case t == "ext4" || t == "xfs" || t == "btrfs":
		return true, "", ""
	case t == "":
		return false, "не удалось определить ФС", "ФС не определена"
	default:
		return false, t + ": не поддерживается RamPlus (поддерживаются ext4, xfs, btrfs)", "не подходит"
	}
}

func kindText(k string) string {
	switch k {
	case detect.KindNVMe:
		return "NVMe"
	case detect.KindSSD:
		return "SSD"
	case detect.KindHDD:
		return "HDD"
	case detect.KindVirtual:
		return "виртуальный диск"
	default:
		return "тип не определён"
	}
}

func onUSB(fi detect.FSInfo) bool { return detect.HasTransport(fi.Disks, "usb") }

// AssessDisk — оценка диска под ФС: уровень проверки, сообщение и пометка.
func AssessDisk(fi detect.FSInfo, force bool) (Level, string, string) {
	switch {
	case onUSB(fi) && force:
		return LevelWarn, "swap на USB-диске медленный и опасный при отключении — продолжаем из-за --force", "USB"
	case onUSB(fi):
		return LevelFail, "swap на USB-диске медленный и опасный при отключении (выдернули диск — процессы со страницами в swap упадут). Чтобы всё равно продолжить: --force", "USB — нужен --force"
	case fi.Kind == detect.KindVirtual:
		return LevelInfo, "виртуальный диск: скорость зависит от хоста", "виртуальный диск"
	case fi.Kind == detect.KindHDD:
		return LevelWarn, "HDD: swap будет медленным", "HDD — медленно"
	case len(fi.Disks) == 0 || fi.Kind == detect.KindUnknown:
		return LevelWarn, "тип диска не определён", "тип диска не определён"
	default:
		return LevelOK, fmt.Sprintf("%s (%s)", kindText(fi.Kind), fi.Disks[0].Transport), ""
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
		p.add(LevelFail, "ФС", "%s", reason)
		return "", false
	}
	where := fmt.Sprintf("%s на %s (точка монтирования %s)", fi.FSType, fi.Source, fi.MountPoint)
	if fi.FSType != "btrfs" {
		p.add(LevelOK, "ФС", "%s: swapfile через fallocate", where)
		return methodFallocate, true
	}
	if !f.HasBtrfs || !f.HasChattr {
		p.add(LevelFail, "ФС", "btrfs: нужны утилиты btrfs (btrfs-progs) и chattr (e2fsprogs)")
		return "", false
	}
	m := methodFor(f, "btrfs")
	how := "chattr +C (NOCOW) + fallocate + mkswap"
	if m == methodBtrfsTool {
		how = "btrfs filesystem mkswapfile"
	}
	if f.Target.Path == BtrfsPath {
		p.add(LevelOK, "ФС", "%s: отдельный NOCOW-подтом %s, %s", where, BtrfsDir, how)
	} else {
		p.add(LevelOK, "ФС", "%s: %s", where, how)
		p.add(LevelWarn, "btrfs", "снапшоты подтома с активным swapfile не создаются (snapper/timeshift будут падать); безопаснее путь по умолчанию — отдельный подтом %s", BtrfsDir)
	}
	return m, true
}

func (p *Plan) checkDisk(fi detect.FSInfo) {
	level, msg, _ := AssessDisk(fi, p.Options.Force)
	p.add(level, "диск", "%s", msg)
}

var forbiddenDirs = []string{"/proc", "/sys", "/dev", "/run", "/tmp", "/var/tmp", "/boot"}

// checkPath — проверки самого пути swapfile (apply).
func (p *Plan) checkPath(f Facts) bool {
	t := f.Target
	if !filepath.IsAbs(t.Path) {
		p.add(LevelFail, "путь", "--path должен быть абсолютным: %q", t.Requested)
		return false
	}
	for _, d := range forbiddenDirs {
		if t.Path == d || strings.HasPrefix(t.Path, d+"/") {
			p.add(LevelFail, "путь", "%s: в %s swapfile создавать нельзя (виртуальная ФС или очищается при загрузке)", t.Path, d)
			return false
		}
	}
	if t.ParentErr != nil {
		p.add(LevelFail, "путь", "каталог %s недоступен: %v — создайте его или выберите другой путь", filepath.Dir(t.Path), t.ParentErr)
		return false
	}
	how := "указан --path"
	if t.Defaulted {
		how = "по умолчанию"
	}
	p.add(LevelInfo, "путь", "%s (%s)", t.Path, how)
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
func choosePriority(f Facts) (prio int, note string, conflict bool) {
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
		note = fmt.Sprintf("выше существующего swap (%d), ниже zram (%d)", maxDisk, zramMin)
	case hasDisk:
		note = fmt.Sprintf("выше существующего swap (%d): новый используется первым", maxDisk)
	case hasZram:
		note = fmt.Sprintf("ниже zram (%d): zram используется первым", zramMin)
	default:
		note = "других swap нет"
	}
	return prio, note, conflict
}

func (p *Plan) planApply(f Facts) {
	o := &p.Options
	if m := f.Managed; m != nil {
		p.add(LevelFail, "swap RamPlus", "уже создан: %s, %s. Изменить размер: ramplus swap resize --size N; удалить: ramplus swap remove",
			m.Path, FormatSize(m.SizeBytes))
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
		p.add(LevelWarn, "существующий swap", "%s: флаг --existing не указан → выбран безопасный вариант keep (старый swap остаётся, новый добавляется с более высоким приоритетом). Заменить: --existing=replace",
			swapList(others))
	case len(others) > 0:
		p.add(LevelInfo, "существующий swap", "%s: выбрано --existing=%s", swapList(others), o.Existing)
	case o.Existing == ExistingReplace:
		p.add(LevelInfo, "существующий swap", "другого swap нет — заменять нечего")
	}
	replace := o.Existing == ExistingReplace && len(others) > 0
	if o.DeleteOld && !replace {
		p.add(LevelInfo, "--delete-old", "игнорируется: действует только вместе с --existing=replace")
	}
	if replace {
		p.checkResume(f, "замена")
		if f.FstabErr != nil {
			p.add(LevelFail, "fstab", "не удалось прочитать %s: %v", FstabPath, f.FstabErr)
		}
	}

	prio, note, conflict := choosePriority(f)
	if conflict {
		p.add(LevelWarn, "приоритет", "новый swap получит приоритет %d — выше zram; zram перестанет использоваться первым", prio)
	} else {
		p.add(LevelInfo, "приоритет", "новый swap получит приоритет %d (%s)", prio, note)
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
			p.add(LevelFail, "файлы", "%s уже существует, но RamPlus его не создавал — удалите или переименуйте вручную", ex)
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
				p.add(LevelWarn, "fstab", "%s не найден в %s: swapoff действует до перезагрузки, потом он может включиться снова (systemd-юнит или скрипт)", old.Path, FstabPath)
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
					p.add(LevelInfo, "--delete-old", "%s — раздел, а не файл: не удаляется", old.Path)
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

func (p *Plan) checkResume(f Facts, what string) {
	if r := ResumeParam(f.Cmdline); r != "" {
		p.add(LevelWarn, "гибернация", "в /proc/cmdline задан resume=%s: %s swap может сломать гибернацию (проверьте resume= и initramfs)", r, what)
	}
}

func (p *Plan) requireManaged(f Facts) *Managed {
	m := f.Managed
	if m == nil {
		p.add(LevelFail, "swap RamPlus", "RamPlus swap не найден — сначала: ramplus swap apply --size N")
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
		p.add(LevelFail, "размер", "swap RamPlus уже %s", FormatSize(m.SizeBytes))
		return
	}
	p.add(LevelInfo, "размер", "%s → %s", FormatSize(m.SizeBytes), FormatSize(o.Size))
	p.checkDisk(f.Target.FS)
	if m.FSType == "btrfs" && (!f.HasBtrfs || !f.HasChattr) {
		p.add(LevelFail, "ФС", "btrfs: нужны утилиты btrfs (btrfs-progs) и chattr (e2fsprogs)")
	}
	if _, active := f.swapByPath(m.Path); active {
		p.Steps = append(p.Steps, Step{Kind: KindSwapoff, Path: m.Path, Priority: m.Priority, Size: m.SizeBytes})
	} else {
		p.add(LevelWarn, "swap RamPlus", "%s сейчас не активен", m.Path)
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
	p.checkResume(f, "удаление")
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
		p.add(LevelWarn, "swap", "после удаления в системе не останется swap")
	}
	for _, op := range f.State.Operations {
		if op.Status != journal.OpCommitted {
			continue
		}
		for _, a := range op.Applied {
			if a.Step.Kind == KindFstabDisable {
				p.add(LevelInfo, "старый swap", "строка %q отключена операцией %s и останется отключённой; вернуть: ramplus rollback (повторно, после отката этого remove)",
					strings.TrimSpace(a.Step.Line), op.ID)
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
		p.add(LevelFail, "место", "не удалось узнать свободное место на %s", fi.MountPoint)
		return
	}
	total, avail := *fi.TotalBytes, *fi.AvailableBytes
	required := RequiredFree(total)
	remaining := int64(avail) - peak
	msg := fmt.Sprintf("на %s доступно %s из %s; после создания останется %s, нужно ≥ %s (%s)",
		fi.MountPoint, FormatSize(avail), FormatSize(total), formatSigned(remaining), FormatSize(required), requiredFreeText())
	switch {
	case remaining >= int64(required):
		p.add(LevelOK, "место", "%s", msg)
	case p.Action == ActionRollback && p.Options.Force:
		p.add(LevelWarn, "место", "%s — продолжаем из-за --force", msg)
	default:
		p.add(LevelFail, "место", "%s", msg)
	}
}

func formatSigned(v int64) string {
	if v < 0 {
		return "−" + FormatSize(uint64(-v)) + " (не хватает)"
	}
	return FormatSize(uint64(v))
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
			title := "память для swapoff " + s.Path
			if d.used == 0 {
				p.add(LevelOK, title, "%s не используется — swapoff безопасен", s.Path)
				continue
			}
			if !haveMem {
				p.add(LevelFail, title, "не удалось прочитать %s, а в %s занято %s", MeminfoPath, s.Path, FormatSize(d.used))
				continue
			}
			budget := memAvail + free
			msg := fmt.Sprintf("нужно перенести %s; доступно RAM %s + свободно в остающемся swap %s (запас %s)",
				FormatSize(d.used), FormatSize(memAvail), FormatSize(free), FormatSize(memReserve))
			if d.used+memReserve > budget {
				p.add(LevelFail, title, "%s — не хватает, swapoff может вызвать OOM", msg)
			} else {
				p.add(LevelOK, title, "%s", msg)
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
			disabled[fstabLabel(s.Line)] = true
		case KindFstabEnable:
			delete(disabled, fstabLabel(s.Line))
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

func fstabLabel(line string) string {
	return "строка fstab «" + strings.Join(strings.Fields(line), " ") + "»"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func swapList(devs []monitor.SwapDevice) string {
	parts := make([]string, len(devs))
	for i, d := range devs {
		parts[i] = fmt.Sprintf("%s (%s, %s, занято %s, приоритет %d)", d.Path, d.Type, FormatSize(d.SizeBytes), FormatSize(d.UsedBytes), d.Priority)
	}
	return strings.Join(parts, ", ")
}

// candidateSummary — описание варианта места для swapfile.
func candidateSummary(fi detect.FSInfo, size uint64, force bool) string {
	parts := []string{fi.FSType + " на " + fi.Source}
	if len(fi.Disks) > 0 {
		parts = append(parts, kindText(fi.Kind)+"/"+fi.Disks[0].Transport)
	}
	if fi.AvailableBytes != nil && fi.TotalBytes != nil {
		parts = append(parts, fmt.Sprintf("свободно %s из %s", FormatSize(*fi.AvailableBytes), FormatSize(*fi.TotalBytes)))
	}
	var marks []string
	if ok, _, short := AssessFS(fi); !ok {
		marks = append(marks, short)
	} else {
		if _, _, short := AssessDisk(fi, force); short != "" {
			marks = append(marks, short)
		}
		maxSize := MaxSwapSize(fi)
		switch {
		case maxSize < MinSize:
			marks = append(marks, "мало места")
		case size > maxSize:
			marks = append(marks, "мало места: можно до "+FormatSize(maxSize))
		default:
			marks = append(marks, "можно до "+FormatSize(maxSize))
		}
	}
	return strings.Join(parts, ", ") + " — " + strings.Join(marks, "; ")
}

// applyChoices — выбор пользователя и альтернативы с последствиями.
func applyChoices(f Facts, o Options, p Plan) []Choice {
	// Место.
	where := Choice{Title: "Место", Flag: "--path", Value: f.Target.Path, Defaulted: f.Target.Defaulted}
	selected := false
	for _, fi := range f.Candidates {
		alt := Alternative{Value: SuggestedPath(fi), Summary: candidateSummary(fi, o.Size, o.Force)}
		if fi.MountPoint == f.Target.FS.MountPoint && !selected {
			alt.Value, alt.Selected, selected = f.Target.Path, true, true
		}
		where.Alternatives = append(where.Alternatives, alt)
	}
	if !selected && f.Target.FS.MountPoint != "" {
		where.Alternatives = append([]Alternative{{Value: f.Target.Path, Selected: true,
			Summary: candidateSummary(f.Target.FS, o.Size, o.Force)}}, where.Alternatives...)
	}
	where.Note = "варианты — смонтированные ФС; другой путь: --path /точка/монтирования/" + DefaultFileName

	size := Choice{Title: "Размер", Flag: "--size", Value: "не указан"}
	if o.Size > 0 {
		size.Value = FormatSize(o.Size)
	}
	switch {
	case p.MaxSize >= MinSize:
		size.Note = fmt.Sprintf("на %s допустимо до %s", f.Target.FS.MountPoint, FormatSize(p.MaxSize))
	case f.Target.FS.TotalBytes != nil:
		size.Note = fmt.Sprintf("на %s не хватает места даже на минимальный swap (%s)", f.Target.FS.MountPoint, FormatSize(MinSize))
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
	existing := Choice{Title: "Существующий swap", Flag: "--existing", Value: string(p.Options.Existing), Defaulted: p.ExistingDefaulted}
	for _, mode := range []ExistingMode{ExistingKeep, ExistingReplace} {
		alt := o
		alt.Existing = mode
		ap := buildPlan(f, alt)
		var sum string
		if mode == ExistingKeep {
			sum = fmt.Sprintf("итого swap %s: %s остаётся, новый добавляется", FormatSize(ap.SwapAfter), oldList)
			if ap.ManagedAfter != nil {
				sum += fmt.Sprintf(" с приоритетом %d и используется первым", ap.ManagedAfter.Priority)
			}
		} else {
			sum = fmt.Sprintf("итого swap %s: %s — swapoff и комментарий в %s", FormatSize(ap.SwapAfter), oldList, FstabPath)
			if o.DeleteOld {
				sum += "; файл удаляется (--delete-old)"
			} else {
				sum += "; файл остаётся на диске (удалить: --delete-old)"
			}
		}
		if ap.Blocked() && !p.Blocked() {
			sum += " — сейчас заблокирован: " + ap.Failures()[0].Message
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
			del := Choice{Title: "Старые swap-файлы", Flag: "--delete-old", Value: "оставить", Defaulted: !o.DeleteOld}
			if o.DeleteOld {
				del.Value = "удалить"
			}
			del.Alternatives = []Alternative{
				{Value: "оставить", Selected: !o.DeleteOld, Summary: fmt.Sprintf("на диске остаётся %s (%s): вернуть можно через rollback", strings.Join(files, ", "), FormatSize(total))},
				{Value: "удалить", Selected: o.DeleteOld, Summary: fmt.Sprintf("освобождается %s; rollback пересоздаст пустые swap-файлы того же размера", FormatSize(total))},
			}
			choices = append(choices, del)
		}
	}
	return choices
}
