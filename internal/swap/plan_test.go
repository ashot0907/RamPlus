package swap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/monitor"
)

// baseFacts — здоровая система: ext4 на NVMe, много места, swap нет.
func baseFacts() Facts {
	return Facts{
		EUID: 0,
		Root: detect.RootDisk{MountPoint: "/", Source: "/dev/nvme0n1p2", FSType: "ext4", Kind: detect.KindNVMe,
			Disks: []detect.BlockDevice{{Name: "nvme0n1", Kind: detect.KindNVMe, Transport: "nvme"}}},
		DiskTotal:     500 * GiB,
		DiskAvail:     300 * GiB,
		Mem:           monitor.Meminfo{"MemTotal": 16 * GiB, "MemAvailable": 8 * GiB},
		Swaps:         []monitor.SwapDevice{},
		Fstab:         defaultFstab,
		FstabResolved: map[string]string{},
		Systemd:       true,
		Exists:        map[string]bool{},
	}
}

func withOldSwap(f Facts, used uint64) Facts {
	f.Swaps = append(f.Swaps, monitor.SwapDevice{Path: "/swap.img", Type: "file", SizeBytes: 8 * GiB, UsedBytes: used, Priority: -2})
	f.Fstab += "/swap.img\tnone\tswap\tsw\t0\t0\n"
	return f
}

func applyOpts(size uint64) Options { return Options{Action: ActionApply, Size: size} }

func findCheck(p Plan, title string) (Check, bool) {
	for _, c := range p.Checks {
		if c.Title == title || strings.HasPrefix(c.Title, title) {
			return c, true
		}
	}
	return Check{}, false
}

func wantLevel(t *testing.T, p Plan, title string, level Level) Check {
	t.Helper()
	c, ok := findCheck(p, title)
	if !ok {
		t.Fatalf("check %q not found in %+v", title, p.Checks)
	}
	if c.Level != level {
		t.Errorf("check %q: level %s, want %s (%s)", title, c.Level, level, c.Message)
	}
	return c
}

func kinds(steps []Step) []StepKind {
	out := make([]StepKind, len(steps))
	for i, s := range steps {
		out[i] = s.Kind
	}
	return out
}

func hasStep(p Plan, k StepKind, path string) bool {
	for _, s := range p.Steps {
		if s.Kind == k && (path == "" || s.Path == path) {
			return true
		}
	}
	return false
}

func TestPlanHappyPath(t *testing.T) {
	p := BuildPlan(baseFacts(), applyOpts(8*GiB))
	if p.Blocked() {
		t.Fatalf("blocked: %+v", p.Failures())
	}
	want := []StepKind{KindAllocate, KindChmod, KindMkswap, KindSwapon, KindWriteFile, KindSystemctl, KindSystemctl}
	if got := kinds(p.Steps); strings.Join(toStrings(got), ",") != strings.Join(toStrings(want), ",") {
		t.Errorf("steps = %v", got)
	}
	if p.SwapBefore != 0 || p.SwapAfter != 8*GiB || p.ManagedAfter == nil || p.ManagedAfter.Path != ManagedPath {
		t.Errorf("summary: %d → %d, %+v", p.SwapBefore, p.SwapAfter, p.ManagedAfter)
	}
	if p.Steps[3].Priority != 10 {
		t.Errorf("priority = %d", p.Steps[3].Priority)
	}
	var out bytes.Buffer
	if err := p.Render(&out); err != nil || !strings.Contains(out.String(), "план выполним") {
		t.Errorf("render: %v\n%s", err, out.String())
	}
}

func toStrings(k []StepKind) []string {
	out := make([]string, len(k))
	for i, v := range k {
		out[i] = string(v)
	}
	return out
}

func TestPlanNonRootIsInfoOnly(t *testing.T) {
	f := baseFacts()
	f.EUID = 1000
	p := BuildPlan(f, applyOpts(4*GiB))
	wantLevel(t, p, "root", LevelInfo)
	if p.Blocked() {
		t.Errorf("non-root plan must not be blocked: %+v", p.Failures())
	}
}

func TestPlanSizeRequired(t *testing.T) {
	p := BuildPlan(baseFacts(), applyOpts(0))
	wantLevel(t, p, "размер", LevelFail)
	if len(p.Steps) != 0 {
		t.Error("no steps without size")
	}
	p = BuildPlan(baseFacts(), applyOpts(128*MiB))
	wantLevel(t, p, "размер", LevelFail)
}

func TestPlanLowSpace(t *testing.T) {
	tests := []struct {
		total, avail, size uint64
		want               Level
		maxSize            uint64
	}{
		// нужно оставить max(10% = 10 GiB, 5 GiB) = 10 GiB
		{100 * GiB, 15 * GiB, 12 * GiB, LevelFail, 5 * GiB},
		{100 * GiB, 15 * GiB, 5 * GiB, LevelOK, 5 * GiB},
		// маленький диск: 10% = 3 GiB, но минимум 5 GiB
		{30 * GiB, 10 * GiB, 4 * GiB, LevelOK, 5 * GiB},
		{30 * GiB, 10 * GiB, 6 * GiB, LevelFail, 5 * GiB},
		// места нет вообще
		{100 * GiB, 4 * GiB, 1 * GiB, LevelFail, 0},
	}
	for _, tt := range tests {
		f := baseFacts()
		f.DiskTotal, f.DiskAvail = tt.total, tt.avail
		p := BuildPlan(f, applyOpts(tt.size))
		c := wantLevel(t, p, "место", tt.want)
		if p.MaxSize != tt.maxSize {
			t.Errorf("total=%d avail=%d: MaxSize = %d, want %d", tt.total/GiB, tt.avail/GiB, p.MaxSize/GiB, tt.maxSize/GiB)
		}
		if tt.want == LevelFail && !strings.Contains(c.Message, "нужно ≥") {
			t.Errorf("message must explain: %s", c.Message)
		}
	}
}

func TestPlanFilesystems(t *testing.T) {
	tests := []struct {
		fs        string
		btrfs     bool
		mkswap    bool
		chattr    bool
		wantLevel Level
		wantPath  string
		wantFirst StepKind
	}{
		{"ext4", false, false, false, LevelOK, ManagedPath, KindAllocate},
		{"xfs", false, false, false, LevelOK, ManagedPath, KindAllocate},
		{"btrfs", true, true, true, LevelOK, BtrfsPath, KindBtrfsSubvolCreate},
		{"btrfs", true, false, true, LevelOK, BtrfsPath, KindBtrfsSubvolCreate},
		{"btrfs", false, false, true, LevelFail, "", ""},
		{"btrfs", true, true, false, LevelFail, "", ""},
		{"zfs", false, false, false, LevelFail, "", ""},
		{"tmpfs", false, false, false, LevelFail, "", ""},
		{"overlay", false, false, false, LevelFail, "", ""},
		{"nfs", false, false, false, LevelFail, "", ""},
		{"nfs4", false, false, false, LevelFail, "", ""},
		{"fuse.sshfs", false, false, false, LevelFail, "", ""},
		{"vfat", false, false, false, LevelFail, "", ""},
		{"", false, false, false, LevelFail, "", ""},
	}
	for _, tt := range tests {
		f := baseFacts()
		f.Root.FSType, f.HasBtrfs, f.HasMkswapfile, f.HasChattr = tt.fs, tt.btrfs, tt.mkswap, tt.chattr
		p := BuildPlan(f, applyOpts(4*GiB))
		c := wantLevel(t, p, "ФС", tt.wantLevel)
		if tt.wantLevel == LevelFail {
			if len(p.Steps) != 0 || c.Message == "" {
				t.Errorf("%s: must refuse with explanation, steps=%v", tt.fs, kinds(p.Steps))
			}
			continue
		}
		if p.ManagedAfter.Path != tt.wantPath || p.Steps[0].Kind != tt.wantFirst {
			t.Errorf("%s: path %s, first %s", tt.fs, p.ManagedAfter.Path, p.Steps[0].Kind)
		}
		if tt.fs == "btrfs" {
			if !hasStep(p, KindChattrNoCOW, BtrfsDir) {
				t.Errorf("btrfs: subvolume must be NOCOW: %v", kinds(p.Steps))
			}
			if tt.mkswap != hasStep(p, KindBtrfsMkswapfile, BtrfsPath) {
				t.Errorf("btrfs mkswapfile=%v: steps %v", tt.mkswap, kinds(p.Steps))
			}
			if p.ManagedAfter.Dir != BtrfsDir {
				t.Errorf("btrfs: dir = %q", p.ManagedAfter.Dir)
			}
		}
	}
}

func TestPlanUSB(t *testing.T) {
	f := baseFacts()
	f.Root.Disks[0].Transport = "usb"
	p := BuildPlan(f, applyOpts(4*GiB))
	c := wantLevel(t, p, "диск", LevelFail)
	if !strings.Contains(c.Message, "USB") || !strings.Contains(c.Message, "--force") {
		t.Errorf("message: %s", c.Message)
	}
	o := applyOpts(4 * GiB)
	o.Force = true
	p = BuildPlan(f, o)
	wantLevel(t, p, "диск", LevelWarn)
	if p.Blocked() {
		t.Errorf("--force must unblock: %+v", p.Failures())
	}
}

func TestPlanExistingDefaultsToKeep(t *testing.T) {
	p := BuildPlan(withOldSwap(baseFacts(), 0), applyOpts(12*GiB))
	if p.Options.Existing != ExistingKeep || !p.ExistingDefaulted {
		t.Fatalf("existing = %q defaulted=%v", p.Options.Existing, p.ExistingDefaulted)
	}
	c := wantLevel(t, p, "существующий swap", LevelWarn)
	if !strings.Contains(c.Message, "--existing не указан") || !strings.Contains(c.Message, "keep") {
		t.Errorf("must say explicitly: %s", c.Message)
	}
	if hasStep(p, KindSwapoff, "") || hasStep(p, KindFstabDisable, "") {
		t.Errorf("keep must not touch old swap: %v", kinds(p.Steps))
	}
	if p.SwapAfter != 20*GiB {
		t.Errorf("after = %d GiB", p.SwapAfter/GiB)
	}
	if p.Steps[3].Kind != KindSwapon || p.Steps[3].Priority <= -2 {
		t.Errorf("new swap must have higher priority: %+v", p.Steps[3])
	}
	// Выбор показан явно, с альтернативами и последствиями.
	if len(p.Choices) != 2 {
		t.Fatalf("choices = %+v", p.Choices)
	}
	ex := p.Choices[1]
	if ex.Flag != "--existing" || !ex.Defaulted || len(ex.Alternatives) != 2 ||
		!ex.Alternatives[0].Selected || !strings.Contains(ex.Alternatives[0].Summary, "20.0 GiB") ||
		!strings.Contains(ex.Alternatives[1].Summary, "12.0 GiB") {
		t.Errorf("existing choice = %+v", ex)
	}
}

func TestPlanReplace(t *testing.T) {
	f := withOldSwap(baseFacts(), 0)
	o := Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace}
	p := BuildPlan(f, o)
	if p.Blocked() {
		t.Fatalf("blocked: %+v", p.Failures())
	}
	tail := toStrings(kinds(p.Steps[len(p.Steps)-3:]))
	if strings.Join(tail, ",") != "swapoff,fstab_disable,systemctl" {
		t.Errorf("tail = %v", tail)
	}
	if hasStep(p, KindDeleteSwapfile, "/swap.img") {
		t.Error("old file must not be deleted without --delete-old")
	}
	if p.SwapAfter != 12*GiB || strings.Join(p.Disabled, ";") != "/swap.img;строка fstab «/swap.img none swap sw 0 0»" {
		t.Errorf("after=%d disabled=%q", p.SwapAfter/GiB, p.Disabled)
	}
	// Новый swap включается раньше, чем отключается старый.
	var onIdx, offIdx int
	for i, s := range p.Steps {
		if s.Kind == KindSwapon {
			onIdx = i
		}
		if s.Kind == KindSwapoff {
			offIdx = i
		}
	}
	if onIdx > offIdx {
		t.Error("swapon new must precede swapoff old")
	}

	o.DeleteOld = true
	p = BuildPlan(f, o)
	if last := p.Steps[len(p.Steps)-1]; last.Kind != KindDeleteSwapfile || last.Path != "/swap.img" {
		t.Errorf("delete-old must be the last step: %+v", last)
	}
	if len(p.Choices) != 3 || p.Choices[2].Flag != "--delete-old" || p.Choices[2].Value != "удалить" {
		t.Errorf("delete-old choice = %+v", p.Choices)
	}
}

func TestPlanReplaceNotInFstab(t *testing.T) {
	f := withOldSwap(baseFacts(), 0)
	f.Fstab = defaultFstab
	p := BuildPlan(f, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace})
	wantLevel(t, p, "fstab", LevelWarn)
	if hasStep(p, KindFstabDisable, "") {
		t.Error("nothing to comment out")
	}
}

func TestPlanReplaceUUIDPartition(t *testing.T) {
	f := baseFacts()
	f.Swaps = []monitor.SwapDevice{{Path: "/dev/sda3", Type: "partition", SizeBytes: 4 * GiB, Priority: 5}}
	f.Fstab = readFixture(t, "fstab/uuid-partition")
	f.FstabResolved = map[string]string{"UUID=aaaa-bbbb": "/dev/sda3"}
	p := BuildPlan(f, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace, DeleteOld: true})
	found := false
	for _, s := range p.Steps {
		if s.Kind == KindFstabDisable && s.Line == "UUID=aaaa-bbbb none swap sw,pri=5 0 0" {
			found = true
		}
	}
	if !found || hasStep(p, KindDeleteSwapfile, "/dev/sda3") {
		t.Errorf("steps = %+v", p.Steps)
	}
	c := wantLevel(t, p, "--delete-old", LevelInfo)
	if !strings.Contains(c.Message, "раздел") {
		t.Errorf("message: %s", c.Message)
	}
	if p.ManagedAfter.Priority != 15 {
		t.Errorf("priority = %d, want 15 (above 5)", p.ManagedAfter.Priority)
	}
}

func TestPlanResume(t *testing.T) {
	f := withOldSwap(baseFacts(), 0)
	f.Cmdline = readFixture(t, "cmdline/resume")
	p := BuildPlan(f, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace})
	c := wantLevel(t, p, "гибернация", LevelWarn)
	if !strings.Contains(c.Message, "resume=UUID=aaaa-bbbb") {
		t.Errorf("message: %s", c.Message)
	}
	if _, ok := findCheck(BuildPlan(f, applyOpts(4*GiB)), "гибернация"); ok {
		t.Error("keep does not touch existing swap — no hibernation warning")
	}
	f.State.Managed = &Managed{Path: ManagedPath, SizeBytes: 4 * GiB, Unit: "ramplus.swap.swap"}
	wantLevel(t, BuildPlan(f, Options{Action: ActionRemove}), "гибернация", LevelWarn)
}

func TestPlanMemoryForSwapoff(t *testing.T) {
	// В /swap.img занято 10 GiB; RAM доступно 2 GiB; новый swap 4 GiB.
	f := withOldSwap(baseFacts(), 0)
	f.Swaps[0].SizeBytes, f.Swaps[0].UsedBytes = 16*GiB, 10*GiB
	f.Mem["MemAvailable"] = 2 * GiB
	p := BuildPlan(f, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace})
	c := wantLevel(t, p, "память для swapoff /swap.img", LevelFail)
	if !strings.Contains(c.Message, "10.0 GiB") {
		t.Errorf("message: %s", c.Message)
	}
	// С новым swap 12 GiB данные помещаются: 2 GiB RAM + 12 GiB swap.
	p = BuildPlan(f, Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace})
	wantLevel(t, p, "память для swapoff", LevelOK)
	// В альтернативах keep показан причину блокировки replace.
	p = BuildPlan(f, applyOpts(4*GiB))
	if !strings.Contains(p.Choices[1].Alternatives[1].Summary, "заблокирован") {
		t.Errorf("replace alternative must show it is blocked: %+v", p.Choices[1].Alternatives[1])
	}
}

func TestPlanRemoveMemory(t *testing.T) {
	f := baseFacts()
	f.State.Managed = &Managed{Path: ManagedPath, SizeBytes: 8 * GiB, Priority: 10, Unit: "ramplus.swap.swap"}
	f.Swaps = []monitor.SwapDevice{{Path: ManagedPath, Type: "file", SizeBytes: 8 * GiB, UsedBytes: 7 * GiB, Priority: 10}}
	f.Mem["MemAvailable"] = 3 * GiB
	f.Exists = map[string]bool{ManagedPath: true, UnitDir + "/ramplus.swap.swap": true}
	p := BuildPlan(f, Options{Action: ActionRemove})
	wantLevel(t, p, "память для swapoff", LevelFail)
	wantLevel(t, p, "swap", LevelWarn) // не останется swap
}

func TestPlanPriority(t *testing.T) {
	tests := []struct {
		swaps    []monitor.SwapDevice
		want     int
		conflict bool
	}{
		{nil, 10, false},
		{[]monitor.SwapDevice{{Path: "/swap.img", Priority: -2}}, 10, false},
		{[]monitor.SwapDevice{{Path: "/swap.img", Priority: 50}}, 60, false},
		{[]monitor.SwapDevice{{Path: "/dev/zram0", Priority: 100}}, 10, false},
		{[]monitor.SwapDevice{{Path: "/swap.img", Priority: -2}, {Path: "/dev/zram0", Priority: 100}}, 10, false},
		{[]monitor.SwapDevice{{Path: "/swap.img", Priority: 95}, {Path: "/dev/zram0", Priority: 100}}, 99, false},
		{[]monitor.SwapDevice{{Path: "/swap.img", Priority: 100}, {Path: "/dev/zram0", Priority: 100}}, 110, true},
	}
	for _, tt := range tests {
		f := baseFacts()
		f.Swaps = tt.swaps
		prio, _, conflict := choosePriority(f)
		if prio != tt.want || conflict != tt.conflict {
			t.Errorf("%+v: prio=%d conflict=%v, want %d %v", tt.swaps, prio, conflict, tt.want, tt.conflict)
		}
	}
}

func TestPlanZramUntouched(t *testing.T) {
	f := withOldSwap(baseFacts(), 0)
	f.Swaps = append(f.Swaps, monitor.SwapDevice{Path: "/dev/zram0", Type: "partition", SizeBytes: 4 * GiB, Priority: 100})
	p := BuildPlan(f, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace})
	if hasStep(p, KindSwapoff, "/dev/zram0") || !hasStep(p, KindSwapoff, "/swap.img") {
		t.Errorf("zram must not be touched: %+v", p.Steps)
	}
	if p.SwapAfter != 8*GiB {
		t.Errorf("after = %d GiB (zram 4 + new 4)", p.SwapAfter/GiB)
	}
}

func TestPlanBlockers(t *testing.T) {
	managed := &Managed{Path: ManagedPath, SizeBytes: 4 * GiB, Priority: 10, Unit: "ramplus.swap.swap"}
	tests := []struct {
		name  string
		mod   func(*Facts)
		title string
	}{
		{"no systemd", func(f *Facts) { f.Systemd = false }, "systemd"},
		{"already managed", func(f *Facts) { f.State.Managed = managed }, "swap RamPlus"},
		{"foreign file", func(f *Facts) { f.Exists[ManagedPath] = true }, "файлы"},
		{"foreign unit", func(f *Facts) { f.Exists[UnitDir+"/ramplus.swap.swap"] = true }, "файлы"},
		{"pending op", func(f *Facts) {
			f.State.Operations = []Operation{{ID: "x", Action: ActionApply, Status: OpInProgress}}
		}, "журнал"},
		{"bad swaps", func(f *Facts) { f.SwapsErr = detect.ErrParse }, "swap"},
	}
	for _, tt := range tests {
		f := baseFacts()
		tt.mod(&f)
		p := BuildPlan(f, applyOpts(4*GiB))
		if c, ok := findCheck(p, tt.title); !ok || c.Level != LevelFail || !p.Blocked() {
			t.Errorf("%s: want fail %q, got %+v", tt.name, tt.title, p.Checks)
		}
	}
}

func TestPlanResize(t *testing.T) {
	f := baseFacts()
	f.State.Managed = &Managed{Path: ManagedPath, SizeBytes: 4 * GiB, Priority: 10, Unit: "ramplus.swap.swap", FSType: "ext4"}
	f.Swaps = []monitor.SwapDevice{{Path: ManagedPath, Type: "file", SizeBytes: 4 * GiB, Priority: 10}}
	f.Exists = map[string]bool{ManagedPath: true}
	// Мало места: 12 GiB доступно, нужно оставить 50 GiB (10% от 500)... берём диск 100 GiB.
	f.DiskTotal, f.DiskAvail = 100*GiB, 12*GiB
	p := BuildPlan(f, Options{Action: ActionResize, Size: 6 * GiB})
	want := "swapoff,delete_swapfile,allocate,chmod,mkswap,swapon"
	if got := strings.Join(toStrings(kinds(p.Steps)), ","); got != want {
		t.Errorf("steps = %s", got)
	}
	// Старый файл удаляется до создания нового: пик +2 GiB, остаётся 10 GiB.
	wantLevel(t, p, "место", LevelOK)
	if p.SwapAfter != 6*GiB || p.ManagedAfter.SizeBytes != 6*GiB || len(p.Disabled) != 0 {
		t.Errorf("after=%d managed=%+v disabled=%v", p.SwapAfter/GiB, p.ManagedAfter, p.Disabled)
	}
	wantLevel(t, BuildPlan(f, Options{Action: ActionResize, Size: 4 * GiB}), "размер", LevelFail)
	f.State.Managed = nil
	wantLevel(t, BuildPlan(f, Options{Action: ActionResize, Size: 6 * GiB}), "swap RamPlus", LevelFail)
}

func TestPlanRollbackNothing(t *testing.T) {
	p := BuildPlan(baseFacts(), Options{Action: ActionRollback})
	wantLevel(t, p, "rollback", LevelFail)
}

func TestPlanRollbackAfterPartialUndo(t *testing.T) {
	f := baseFacts()
	f.State.Operations = []Operation{
		{ID: "1", Action: ActionApply, Status: OpCommitted},
		{ID: "2", Action: ActionResize, Status: OpRollbackFailed},
	}
	c := wantLevel(t, BuildPlan(f, Options{Action: ActionRollback}), "rollback", LevelFail)
	if !strings.Contains(c.Message, "ручная проверка") {
		t.Errorf("message: %s", c.Message)
	}
}

func TestOptionsCommand(t *testing.T) {
	o := Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace, DeleteOld: true, Force: true}
	if got := o.Command(); got != "ramplus swap apply --size 12G --existing=replace --delete-old --force" {
		t.Errorf("command = %q", got)
	}
	if got := (Options{Action: ActionRollback}).Command(); got != "ramplus rollback" {
		t.Errorf("command = %q", got)
	}
}

func TestAnnotateStatus(t *testing.T) {
	w := newWorld(t)
	st := State{Managed: &Managed{Path: "/custom/ramplus.swap"}}
	b, _ := st.Marshal()
	w.files[StateDir] = &fakeFile{dir: true}
	w.files[StatePath] = &fakeFile{data: string(b)}
	status := monitor.Status{Swaps: monitor.Swaps{Devices: []monitor.SwapDevice{
		{Path: "/swap.img"}, {Path: "/custom/ramplus.swap"}, {Path: ManagedPath},
	}}}
	AnnotateStatus(&status, &fakeFS{w})
	got := []bool{status.Swaps.Devices[0].Managed, status.Swaps.Devices[1].Managed, status.Swaps.Devices[2].Managed}
	if got[0] || !got[1] || !got[2] {
		t.Errorf("managed = %v", got)
	}
}
