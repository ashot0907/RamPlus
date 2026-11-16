package swap

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/monitor"
)

// Action — операция.
type Action string

const (
	ActionApply    Action = "apply"
	ActionResize   Action = "resize"
	ActionRemove   Action = "remove"
	ActionRollback Action = "rollback"
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

// Level — серьёзность проверки.
type Level string

const (
	LevelOK   Level = "ok"
	LevelInfo Level = "info"
	LevelWarn Level = "warn"
	LevelFail Level = "fail" // блокирует выполнение
)

// Check — результат preflight-проверки.
type Check struct {
	Level   Level  `json:"level"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// Alternative — один из вариантов выбора и его последствия.
type Alternative struct {
	Value    string `json:"value"`
	Selected bool   `json:"selected"`
	Summary  string `json:"summary"`
}

// Choice — решение пользователя, которое показывается явно.
type Choice struct {
	Title        string        `json:"title"`
	Flag         string        `json:"flag"`
	Value        string        `json:"value"`
	Defaulted    bool          `json:"defaulted"` // выбрано по умолчанию, флаг не указан
	Note         string        `json:"note,omitempty"`
	Alternatives []Alternative `json:"alternatives,omitempty"`
}

// Plan — что будет сделано и почему.
type Plan struct {
	Action            Action     `json:"action"`
	Options           Options    `json:"options"` // с подставленными значениями по умолчанию
	ExistingDefaulted bool       `json:"existing_defaulted"`
	Choices           []Choice   `json:"choices"`
	Checks            []Check    `json:"checks"`
	Steps             []Step     `json:"steps"`
	SwapBefore        uint64     `json:"swap_before_bytes"`
	SwapAfter         uint64     `json:"swap_after_bytes"`
	Created           []string   `json:"created"`
	Disabled          []string   `json:"disabled"`
	Deleted           []string   `json:"deleted"`
	MaxSize           uint64     `json:"max_size_bytes,omitempty"` // сколько можно создать при текущем месте
	ManagedBefore     *Managed   `json:"managed_before"`
	ManagedAfter      *Managed   `json:"managed_after"`
	Target            *Operation `json:"-"` // для rollback
	facts             Facts
}

// Blocked — есть проваленные проверки.
func (p Plan) Blocked() bool { return len(p.Failures()) > 0 }

// Failures — проваленные проверки.
func (p Plan) Failures() []Check {
	var out []Check
	for _, c := range p.Checks {
		if c.Level == LevelFail {
			out = append(out, c)
		}
	}
	return out
}

func (p *Plan) add(l Level, title, format string, a ...any) {
	p.Checks = append(p.Checks, Check{Level: l, Title: title, Message: fmt.Sprintf(format, a...)})
}

// Пороговые значения.
const (
	minFreeAfterBytes = 5 * GiB   // после создания swap должно остаться ≥ 5 GiB
	minFreeAfterPct   = 10        // и ≥ 10% диска
	memReserve        = 256 * MiB // запас RAM при swapoff
)

// BuildPlan строит план. Чистая функция: ничего не читает и не меняет.
func BuildPlan(f Facts, o Options) Plan {
	p := buildPlan(f, o)
	if o.Action == ActionApply {
		p.Choices = applyChoices(f, o, p)
	}
	return p
}

func buildPlan(f Facts, o Options) Plan {
	p := Plan{Action: o.Action, Options: o, ManagedBefore: f.State.Managed, facts: f}
	p.commonChecks(f)
	switch o.Action {
	case ActionApply:
		p.planApply(f)
	case ActionResize:
		p.planResize(f)
	case ActionRemove:
		p.planRemove(f)
	case ActionRollback:
		p.planRollback(f)
	default:
		p.add(LevelFail, "операция", "неизвестная операция %q", o.Action)
	}
	p.diskCheck(f)
	p.memoryCheck(f)
	p.summarize(f)
	if p.Steps == nil {
		p.Steps = []Step{}
	}
	return p
}

func (p *Plan) commonChecks(f Facts) {
	if f.EUID == 0 {
		p.add(LevelOK, "root", "запущено от root")
	} else {
		p.add(LevelInfo, "root", "план строится без root; для выполнения нужен sudo")
	}
	if !f.Systemd {
		p.add(LevelFail, "systemd", "systemd не обнаружен (%s): RamPlus настраивает swap через systemd .swap-юниты", SystemdRunDir)
	}
	if f.StateErr != nil {
		p.add(LevelFail, "журнал", "не удалось прочитать %s: %v", StatePath, f.StateErr)
	}
	if op := f.State.Pending(); op != nil && p.Action != ActionRollback {
		p.add(LevelFail, "журнал", "операция %s (%s) не завершена — сначала выполните: sudo ramplus rollback", op.ID, op.Command)
	}
	if f.SwapsErr != nil {
		p.add(LevelFail, "swap", "не удалось прочитать %s: %v", SwapsPath, f.SwapsErr)
	}
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

// fsMethod определяет способ создания swapfile на ФС корня.
type fsMethod string

const (
	methodFallocate   fsMethod = "fallocate"
	methodBtrfsTool   fsMethod = "btrfs-mkswapfile"
	methodBtrfsChattr fsMethod = "btrfs-chattr"
)

var unsupportedFS = map[string]string{
	"zfs":     "ZFS не поддерживает swapfile (возможны зависания под нагрузкой); используйте zvol или zram",
	"tmpfs":   "tmpfs лежит в RAM — swap в нём бессмыслен",
	"ramfs":   "ramfs лежит в RAM — swap в нём бессмыслен",
	"overlay": "overlayfs (контейнер или live-система) не поддерживает swapfile",
	"nfs":     "сетевая ФС: swap поверх NFS ненадёжен и не поддерживается",
	"nfs4":    "сетевая ФС: swap поверх NFS ненадёжен и не поддерживается",
	"cifs":    "сетевая ФС: swap поверх SMB не поддерживается",
	"fuse":    "FUSE не поддерживает swapfile",
}

// checkFS проверяет ФС корня и возвращает путь, каталог (для btrfs) и метод.
func (p *Plan) checkFS(f Facts) (path, dir string, m fsMethod, ok bool) {
	fsType := f.Root.FSType
	switch {
	case fsType == "ext4" || fsType == "xfs":
		p.add(LevelOK, "ФС", "%s на %s: swapfile через fallocate", fsType, f.Root.Source)
		return ManagedPath, "", methodFallocate, true
	case fsType == "btrfs":
		if !f.HasBtrfs || !f.HasChattr {
			p.add(LevelFail, "ФС", "btrfs: нужны утилиты btrfs (btrfs-progs) и chattr (e2fsprogs)")
			return "", "", "", false
		}
		// Каталог всегда NOCOW: тогда и пересоздание файла при откате или
		// resize даёт корректный swapfile.
		if f.HasMkswapfile {
			p.add(LevelOK, "ФС", "btrfs: отдельный подтом %s с chattr +C (NOCOW) + btrfs filesystem mkswapfile", BtrfsDir)
			return BtrfsPath, BtrfsDir, methodBtrfsTool, true
		}
		p.add(LevelOK, "ФС", "btrfs: отдельный подтом %s с chattr +C (NOCOW) + fallocate + mkswap (btrfs-progs < 6.1)", BtrfsDir)
		return BtrfsPath, BtrfsDir, methodBtrfsChattr, true
	case fsType == "":
		p.add(LevelFail, "ФС", "не удалось определить ФС корня")
	case strings.HasPrefix(fsType, "fuse"):
		p.add(LevelFail, "ФС", "%s: %s", fsType, unsupportedFS["fuse"])
	case unsupportedFS[fsType] != "":
		p.add(LevelFail, "ФС", "%s: %s", fsType, unsupportedFS[fsType])
	default:
		p.add(LevelFail, "ФС", "%s: не поддерживается (поддерживаются ext4, xfs, btrfs)", fsType)
	}
	return "", "", "", false
}

func (p *Plan) checkDiskType(f Facts) {
	switch {
	case f.onUSB() && p.Options.Force:
		p.add(LevelWarn, "диск", "swap на USB-диске медленный и опасный при отключении — продолжаем из-за --force")
	case f.onUSB():
		p.add(LevelFail, "диск", "swap на USB-диске медленный и опасный при отключении (выдернули диск — процессы со страницами в swap упадут). Чтобы всё равно продолжить: --force")
	case len(f.Root.Disks) == 0:
		p.add(LevelWarn, "диск", "тип диска под / не определён")
	case f.Root.Kind == "hdd":
		p.add(LevelWarn, "диск", "корень на HDD: swap будет медленным")
	default:
		p.add(LevelOK, "диск", "корень на %s (%s)", strings.ToUpper(f.Root.Kind), f.Root.Disks[0].Transport)
	}
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
		{Kind: KindAllocate, Path: path, Size: size},
		{Kind: KindChmod, Path: path, Mode: 0o600},
		{Kind: KindMkswap, Path: path},
	}
}

func methodFor(f Facts, fsType string) fsMethod {
	if fsType != "btrfs" {
		return methodFallocate
	}
	if f.HasMkswapfile {
		return methodBtrfsTool
	}
	return methodBtrfsChattr
}

// otherSwaps — активный swap, не принадлежащий RamPlus и не zram.
func otherSwaps(f Facts) []monitor.SwapDevice {
	var out []monitor.SwapDevice
	for _, s := range f.Swaps {
		if !isZram(s.Path) && !isOurPath(s.Path) && (f.State.Managed == nil || s.Path != f.State.Managed.Path) {
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
		case !isOurPath(s.Path):
			if s.Priority > maxDisk {
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
	if m := f.State.Managed; m != nil {
		p.add(LevelFail, "swap RamPlus", "уже создан: %s, %s. Изменить размер: ramplus swap resize --size N; удалить: ramplus swap remove",
			m.Path, FormatSize(m.SizeBytes))
		return
	}
	sizeOK := p.checkSize(o.Size)
	path, dir, method, fsOK := p.checkFS(f)
	p.checkDiskType(f)

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

	if !fsOK {
		return
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
					deletes = append(deletes, Step{Kind: KindDeleteSwapfile, Path: old.Path, Size: old.SizeBytes})
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

	p.ManagedAfter = &Managed{Path: path, Dir: dir, SizeBytes: o.Size, Priority: prio, Unit: unit, FSType: f.Root.FSType}
}

func (p *Plan) checkResume(f Facts, what string) {
	if r := ResumeParam(f.Cmdline); r != "" {
		p.add(LevelWarn, "гибернация", "в /proc/cmdline задан resume=%s: %s swap может сломать гибернацию (проверьте resume= и initramfs)", r, what)
	}
}

func (p *Plan) requireManaged(f Facts) *Managed {
	m := f.State.Managed
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
	p.checkDiskType(f)
	method := methodFor(f, m.FSType)
	if m.FSType == "btrfs" && !f.HasBtrfs {
		p.add(LevelFail, "ФС", "btrfs: нужна утилита btrfs (btrfs-progs)")
	}
	if _, active := f.swapByPath(m.Path); active {
		p.Steps = append(p.Steps, Step{Kind: KindSwapoff, Path: m.Path, Priority: m.Priority, Size: m.SizeBytes})
	} else {
		p.add(LevelWarn, "swap RamPlus", "%s сейчас не активен", m.Path)
	}
	if f.Exists[m.Path] {
		p.Steps = append(p.Steps, Step{Kind: KindDeleteSwapfile, Path: m.Path, Size: m.SizeBytes})
	}
	p.Steps = append(p.Steps, fileSteps(m.Path, o.Size, method)...)
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
		p.Steps = append(p.Steps, Step{Kind: KindDeleteSwapfile, Path: m.Path, Size: m.SizeBytes})
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
		if op.Status != OpCommitted {
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

func (p *Plan) planRollback(f Facts) {
	for i := len(f.State.Operations) - 1; i >= 0; i-- {
		op := f.State.Operations[i]
		if op.Status == OpRollbackFailed {
			p.add(LevelFail, "rollback", "операция %s (%s) откатилась не полностью — нужна ручная проверка по %s (поле applied) и бэкапам в %s",
				op.ID, op.Command, StatePath, BackupDir)
			return
		}
		if op.Status == OpCommitted || op.Status == OpInProgress {
			break
		}
	}
	idx, ok := f.State.LastUndoable()
	if !ok {
		p.add(LevelFail, "rollback", "нечего откатывать: в %s нет выполненных операций", StatePath)
		return
	}
	op := f.State.Operations[idx]
	p.Target = &op
	p.add(LevelInfo, "rollback", "отменяется операция %s от %s: %s", op.ID, op.StartedAt.Local().Format("2006-01-02 15:04:05"), op.Command)
	if op.Status == OpInProgress {
		p.add(LevelWarn, "rollback", "операция была прервана посреди выполнения — откатываются только записанные в журнал шаги")
	}
	p.Steps = UndoSteps(op.Applied)
	p.ManagedAfter = op.ManagedBefore
}

// diskCheck: после создания должно остаться ≥ 10% диска и ≥ 5 GiB.
func (p *Plan) diskCheck(f Facts) {
	var running, peak int64
	for _, s := range p.Steps {
		switch s.Kind {
		case KindAllocate, KindBtrfsMkswapfile:
			running += int64(s.Size)
		case KindDeleteSwapfile:
			if isOurPath(s.Path) {
				running -= int64(s.Size)
			}
		}
		peak = max(peak, running)
	}
	required := max(f.DiskTotal/100*minFreeAfterPct, minFreeAfterBytes)
	if p.Action == ActionApply && f.DiskErr == nil && f.DiskAvail > required {
		p.MaxSize = (f.DiskAvail - required) / MiB * MiB
	}
	if peak == 0 {
		return
	}
	if f.DiskErr != nil {
		p.add(LevelFail, "место", "не удалось узнать свободное место на /: %v", f.DiskErr)
		return
	}
	remaining := int64(f.DiskAvail) - peak
	msg := fmt.Sprintf("на / доступно %s из %s; после создания останется %s, нужно ≥ %s (≥ %d%% диска и ≥ %s)",
		FormatSize(f.DiskAvail), FormatSize(f.DiskTotal), formatSigned(remaining), FormatSize(required),
		minFreeAfterPct, FormatSize(minFreeAfterBytes))
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
		case KindDeleteSwapfile, KindDeleteConfig, KindRemoveFile, KindRmdir, KindBtrfsSubvolDelete:
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
	// swapoff+swapon того же пути (resize) — не «отключение».
	for path := range disabled {
		if _, on := active[path]; on {
			delete(disabled, path)
		}
	}
	for path := range deleted {
		if created[path] {
			delete(deleted, path)
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

// applyChoices — выбор пользователя и альтернативы с последствиями.
func applyChoices(f Facts, o Options, p Plan) []Choice {
	size := Choice{Title: "Размер", Flag: "--size", Value: "не указан"}
	if o.Size > 0 {
		size.Value = FormatSize(o.Size)
	}
	switch {
	case p.MaxSize >= MinSize:
		size.Note = "при текущем свободном месте допустимо до " + FormatSize(p.MaxSize)
	case f.DiskErr == nil && f.DiskTotal > 0:
		size.Note = "свободного места не хватает даже на минимальный swap (" + FormatSize(MinSize) + ")"
	}
	choices := []Choice{size}

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
