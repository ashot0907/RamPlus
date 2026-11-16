package swap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/monitor"
)

func fsInfo(mp, fsType, kind, transport string, total, avail uint64) detect.FSInfo {
	return detect.FSInfo{
		MountPoint: mp, Source: "/dev/x" + strings.ReplaceAll(mp, "/", "_"), FSType: fsType, Kind: kind,
		Disks:      []detect.BlockDevice{{Name: "x", Kind: kind, Transport: transport}},
		TotalBytes: ptr(total), AvailableBytes: ptr(avail),
	}
}

func ptr[T any](v T) *T { return &v }

// baseFacts — здоровая система: ext4 на NVMe, много места, swap нет.
func baseFacts() Facts {
	root := fsInfo("/", "ext4", detect.KindNVMe, "nvme", 500*GiB, 300*GiB)
	return Facts{
		EUID:          0,
		RootFS:        root,
		Target:        Target{Path: ManagedPath, Defaulted: true, FS: root},
		Candidates:    []detect.FSInfo{root},
		Mem:           monitor.Meminfo{"MemTotal": 16 * GiB, "MemAvailable": 8 * GiB},
		Swaps:         []monitor.SwapDevice{},
		Fstab:         defaultFstab,
		FstabResolved: map[string]string{},
		Systemd:       true,
		Exists:        map[string]bool{},
	}
}

// setFS меняет ФС цели (и корня, если цель на корне).
func setFS(f *Facts, fi detect.FSInfo) {
	f.Target.FS = fi
	if fi.MountPoint == "/" {
		f.RootFS = fi
		f.Candidates = []detect.FSInfo{fi}
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

func kinds(steps []Step) string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = string(s.Kind)
	}
	return strings.Join(out, ",")
}

func hasStep(p Plan, k StepKind, path string) bool {
	for _, s := range p.Steps {
		if s.Kind == k && (path == "" || s.Path == path) {
			return true
		}
	}
	return false
}

func render(t *testing.T, p Plan, verbose bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := p.RenderWith(&out, RenderOptions{Verbose: verbose}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestPlanHappyPath(t *testing.T) {
	p := BuildPlan(baseFacts(), applyOpts(8*GiB))
	if p.Blocked() {
		t.Fatalf("blocked: %+v", p.Failures())
	}
	if got := kinds(p.Steps); got != "allocate,chmod,mkswap,swapon,write_file,systemctl,systemctl" {
		t.Errorf("steps = %s", got)
	}
	if p.SwapBefore != 0 || p.SwapAfter != 8*GiB || p.ManagedAfter == nil || p.ManagedAfter.Path != "/ramplus.swapfile" {
		t.Errorf("summary: %d → %d, %+v", p.SwapBefore, p.SwapAfter, p.ManagedAfter)
	}
	if p.ManagedAfter.Unit != "ramplus.swapfile.swap" || p.Steps[3].Priority != 10 {
		t.Errorf("unit/priority: %+v", p.ManagedAfter)
	}
	out := render(t, p, false)
	if !strings.Contains(out, "план выполним") || strings.Contains(out, "[Swap]") || !strings.Contains(out, "--verbose") {
		t.Errorf("render:\n%s", out)
	}
}

func TestPlanVerboseShowsUnit(t *testing.T) {
	out := render(t, BuildPlan(baseFacts(), applyOpts(4*GiB)), true)
	for _, want := range []string{"│ [Swap]", "│ What=/ramplus.swapfile", "│ Priority=10", "│ WantedBy=swap.target", "Managed by RamPlus"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose lacks %q:\n%s", want, out)
		}
	}
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
	wantLevel(t, BuildPlan(baseFacts(), applyOpts(128*MiB)), "размер", LevelFail)
}

func TestRequiredFree(t *testing.T) {
	// max(5 GiB, min(10%, 20 GiB))
	tests := map[uint64]uint64{
		30 * GiB:   5 * GiB,  // 10% = 3 → минимум 5
		50 * GiB:   5 * GiB,  // ровно 5
		100 * GiB:  10 * GiB, // 10%
		200 * GiB:  20 * GiB, // ровно 20
		1000 * GiB: 20 * GiB, // 10% = 100 → потолок 20
		4096 * GiB: 20 * GiB,
	}
	for total, want := range tests {
		if got := RequiredFree(total); got != want {
			t.Errorf("RequiredFree(%d GiB) = %d GiB, want %d", total/GiB, got/GiB, want/GiB)
		}
	}
}

func TestPlanLowSpace(t *testing.T) {
	tests := []struct {
		total, avail, size uint64
		want               Level
		maxSize            uint64
	}{
		{100 * GiB, 15 * GiB, 12 * GiB, LevelFail, 5 * GiB}, // остаётся 3, нужно 10
		{100 * GiB, 15 * GiB, 5 * GiB, LevelOK, 5 * GiB},
		{30 * GiB, 10 * GiB, 4 * GiB, LevelOK, 5 * GiB}, // маленький диск: минимум 5 GiB
		{30 * GiB, 10 * GiB, 6 * GiB, LevelFail, 5 * GiB},
		// большой диск: 10% = 200 GiB, но порог ограничен 20 GiB
		{2000 * GiB, 60 * GiB, 32 * GiB, LevelOK, 40 * GiB},
		{2000 * GiB, 60 * GiB, 45 * GiB, LevelFail, 40 * GiB},
		{100 * GiB, 4 * GiB, 1 * GiB, LevelFail, 0},
	}
	for _, tt := range tests {
		f := baseFacts()
		setFS(&f, fsInfo("/", "ext4", detect.KindNVMe, "nvme", tt.total, tt.avail))
		p := BuildPlan(f, applyOpts(tt.size))
		c := wantLevel(t, p, "место", tt.want)
		if p.MaxSize != tt.maxSize {
			t.Errorf("total=%d avail=%d: MaxSize = %d, want %d", tt.total/GiB, tt.avail/GiB, p.MaxSize/GiB, tt.maxSize/GiB)
		}
		if !strings.Contains(c.Message, "не больше 20.0 GiB и не меньше 5.0 GiB") {
			t.Errorf("message must explain the rule: %s", c.Message)
		}
	}
}

func TestPlanFilesystems(t *testing.T) {
	tests := []struct {
		fs                    string
		btrfs, mkswap, chattr bool
		ok                    bool
		reason                string
	}{
		{"ext4", false, false, false, true, ""},
		{"xfs", false, false, false, true, ""},
		{"btrfs", true, true, true, true, ""},
		{"btrfs", true, false, true, true, ""},
		{"btrfs", false, false, true, false, "btrfs-progs"},
		{"btrfs", true, true, false, false, "chattr"},
		{"zfs", false, false, false, false, "ZFS"},
		{"tmpfs", false, false, false, false, "RAM"},
		{"overlay", false, false, false, false, "overlayfs"},
		{"fuse.sshfs", false, false, false, false, "FUSE"},
		{"f2fs", false, false, false, false, "поддерживаются ext4, xfs, btrfs"},
		{"", false, false, false, false, "не удалось"},
		// Разделы Windows и флешки.
		{"ntfs", false, false, false, false, foreignFSMsg},
		{"ntfs3", false, false, false, false, foreignFSMsg},
		{"vfat", false, false, false, false, foreignFSMsg},
		{"exfat", false, false, false, false, foreignFSMsg},
		{"fuseblk", false, false, false, false, foreignFSMsg},
	}
	for _, tt := range tests {
		f := baseFacts()
		setFS(&f, fsInfo("/", tt.fs, detect.KindNVMe, "nvme", 500*GiB, 300*GiB))
		f.Target.Path = DefaultPathFor(tt.fs)
		f.HasBtrfs, f.HasMkswapfile, f.HasChattr = tt.btrfs, tt.mkswap, tt.chattr
		p := BuildPlan(f, applyOpts(4*GiB))
		if !tt.ok {
			c := wantLevel(t, p, "ФС", LevelFail)
			if len(p.Steps) != 0 || !strings.Contains(c.Message, tt.reason) {
				t.Errorf("%s: must refuse with %q, got %q steps=%s", tt.fs, tt.reason, c.Message, kinds(p.Steps))
			}
			continue
		}
		wantLevel(t, p, "ФС", LevelOK)
		if tt.fs != "btrfs" {
			continue
		}
		if p.ManagedAfter.Path != BtrfsPath || p.ManagedAfter.Dir != BtrfsDir || !hasStep(p, KindChattrNoCOW, BtrfsDir) {
			t.Errorf("btrfs default: %+v steps=%s", p.ManagedAfter, kinds(p.Steps))
		}
		if tt.mkswap != hasStep(p, KindBtrfsMkswapfile, BtrfsPath) {
			t.Errorf("btrfs mkswapfile=%v: steps %s", tt.mkswap, kinds(p.Steps))
		}
		if !tt.mkswap && !p.Steps[2].NoCOW {
			t.Errorf("btrfs without mkswapfile: allocate must be NoCOW: %+v", p.Steps[2])
		}
	}
}

func TestPlanDiskKinds(t *testing.T) {
	tests := []struct {
		kind, transport string
		level           Level
		text            string
	}{
		{detect.KindNVMe, "nvme", LevelOK, "NVMe"},
		{detect.KindSSD, "sata", LevelOK, "SSD"},
		{detect.KindHDD, "sata", LevelWarn, "HDD: swap будет медленным"},
		{detect.KindVirtual, "virtio", LevelInfo, "виртуальный диск: скорость зависит от хоста"},
		{detect.KindUnknown, "scsi", LevelWarn, "не определён"},
		{detect.KindHDD, "usb", LevelFail, "USB"},
	}
	for _, tt := range tests {
		f := baseFacts()
		setFS(&f, fsInfo("/", "ext4", tt.kind, tt.transport, 500*GiB, 300*GiB))
		p := BuildPlan(f, applyOpts(4*GiB))
		c := wantLevel(t, p, "диск", tt.level)
		if !strings.Contains(c.Message, tt.text) {
			t.Errorf("%s/%s: %q lacks %q", tt.kind, tt.transport, c.Message, tt.text)
		}
		if tt.kind == detect.KindVirtual {
			for _, ch := range p.Checks {
				if ch.Level == LevelWarn && strings.Contains(ch.Message, "медлен") {
					t.Errorf("virtual disk must not warn about speed: %+v", ch)
				}
			}
		}
	}
}

func TestPlanUSB(t *testing.T) {
	f := baseFacts()
	setFS(&f, fsInfo("/", "ext4", detect.KindHDD, "usb", 500*GiB, 300*GiB))
	c := wantLevel(t, BuildPlan(f, applyOpts(4*GiB)), "диск", LevelFail)
	if !strings.Contains(c.Message, "--force") {
		t.Errorf("message: %s", c.Message)
	}
	o := applyOpts(4 * GiB)
	o.Force = true
	p := BuildPlan(f, o)
	wantLevel(t, p, "диск", LevelWarn)
	if p.Blocked() {
		t.Errorf("--force must unblock: %+v", p.Failures())
	}
}

func TestPlanCustomPath(t *testing.T) {
	f := baseFacts()
	data := fsInfo("/mnt/nvme", "ext4", detect.KindNVMe, "nvme", 1000*GiB, 800*GiB)
	f.Candidates = append(f.Candidates, data)
	f.Target = Target{Path: "/mnt/nvme/ramplus.swapfile", Requested: "/mnt/nvme/ramplus.swapfile", FS: data}
	o := Options{Action: ActionApply, Size: 16 * GiB, Path: "/mnt/nvme/ramplus.swapfile"}
	p := BuildPlan(f, o)
	if p.Blocked() {
		t.Fatalf("blocked: %+v", p.Failures())
	}
	if p.ManagedAfter.Path != "/mnt/nvme/ramplus.swapfile" || p.ManagedAfter.Unit != "mnt-nvme-ramplus.swapfile.swap" {
		t.Errorf("managed = %+v", p.ManagedAfter)
	}
	c := wantLevel(t, p, "место", LevelOK)
	if !strings.Contains(c.Message, "на /mnt/nvme доступно 800.0 GiB") {
		t.Errorf("disk check must be for the path's FS: %s", c.Message)
	}
	if !strings.Contains(o.Command(), "--path /mnt/nvme/ramplus.swapfile") {
		t.Errorf("command: %s", o.Command())
	}
	where := p.Choices[0]
	if where.Flag != "--path" || where.Defaulted || len(where.Alternatives) != 2 || !where.Alternatives[1].Selected ||
		where.Alternatives[0].Value != "/ramplus.swapfile" {
		t.Errorf("path choice = %+v", where)
	}
}

func TestPlanPathChecks(t *testing.T) {
	tests := []struct {
		name   string
		mod    func(*Target)
		reason string
	}{
		{"relative", func(t *Target) { t.Path, t.Requested = "swapfile", "swapfile" }, "абсолютным"},
		{"tmp", func(t *Target) { t.Path = "/tmp/ramplus.swapfile" }, "/tmp"},
		{"proc", func(t *Target) { t.Path = "/proc/x" }, "/proc"},
		{"no parent", func(t *Target) {
			t.Path, t.ParentErr = "/mnt/nope/ramplus.swapfile", detect.ErrParse
		}, "недоступен"},
	}
	for _, tt := range tests {
		f := baseFacts()
		tt.mod(&f.Target)
		c := wantLevel(t, BuildPlan(f, applyOpts(4*GiB)), "путь", LevelFail)
		if !strings.Contains(c.Message, tt.reason) {
			t.Errorf("%s: %s", tt.name, c.Message)
		}
	}
}

func TestPlanCandidates(t *testing.T) {
	f := baseFacts()
	f.Candidates = append(f.Candidates,
		fsInfo("/mnt/fast", "ext4", detect.KindNVMe, "nvme", 1000*GiB, 500*GiB),
		fsInfo("/mnt/small", "ext4", detect.KindSSD, "sata", 50*GiB, 6*GiB),
		fsInfo("/mnt/usb", "ext4", detect.KindSSD, "usb", 500*GiB, 400*GiB),
		fsInfo("/mnt/vm", "xfs", detect.KindVirtual, "virtio", 500*GiB, 400*GiB),
		fsInfo("/mnt/win", "ntfs3", detect.KindNVMe, "nvme", 900*GiB, 400*GiB),
		fsInfo("/mnt/stick", "exfat", detect.KindSSD, "usb", 64*GiB, 60*GiB),
	)
	nas := detect.FSInfo{MountPoint: "/mnt/nas", Source: "nas:/x", FSType: "nfs4", Network: true, Kind: detect.KindUnknown}
	ro := fsInfo("/mnt/ro", "ext4", detect.KindSSD, "sata", 500*GiB, 400*GiB)
	ro.ReadOnly = true
	f.Candidates = append(f.Candidates, nas, ro)

	p := BuildPlan(f, applyOpts(8*GiB))
	alts := map[string]Alternative{}
	for _, a := range p.Choices[0].Alternatives {
		alts[a.Value] = a
	}
	want := map[string]string{
		"/ramplus.swapfile":           "можно до",
		"/mnt/fast/ramplus.swapfile":  "можно до 480.0 GiB",
		"/mnt/small/ramplus.swapfile": "мало места",
		"/mnt/usb/ramplus.swapfile":   "USB — нужен --force",
		"/mnt/vm/ramplus.swapfile":    "виртуальный диск",
		"/mnt/win/ramplus.swapfile":   "не подходит",
		"/mnt/stick/ramplus.swapfile": "не подходит",
		"/mnt/nas/ramplus.swapfile":   "сетевая ФС — не подходит",
		"/mnt/ro/ramplus.swapfile":    "только чтение",
	}
	for path, mark := range want {
		a, ok := alts[path]
		if !ok || !strings.Contains(a.Summary, mark) {
			t.Errorf("%s: want mark %q, got %+v", path, mark, a)
		}
	}
	if !alts["/ramplus.swapfile"].Selected || !p.Choices[0].Defaulted {
		t.Error("root must be selected by default")
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
		t.Errorf("keep must not touch old swap: %s", kinds(p.Steps))
	}
	if p.SwapAfter != 20*GiB {
		t.Errorf("after = %d GiB", p.SwapAfter/GiB)
	}
	ex := p.Choices[2]
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
	if !strings.HasSuffix(kinds(p.Steps), "swapoff,fstab_disable,systemctl") || hasStep(p, KindDeleteSwapfile, "/swap.img") {
		t.Errorf("steps = %s", kinds(p.Steps))
	}
	if p.SwapAfter != 12*GiB || strings.Join(p.Disabled, ";") != "/swap.img;строка fstab «/swap.img none swap sw 0 0»" {
		t.Errorf("after=%d disabled=%q", p.SwapAfter/GiB, p.Disabled)
	}
	o.DeleteOld = true
	p = BuildPlan(f, o)
	if last := p.Steps[len(p.Steps)-1]; last.Kind != KindDeleteSwapfile || last.Path != "/swap.img" {
		t.Errorf("delete-old must be the last step: %+v", last)
	}
	if len(p.Choices) != 4 || p.Choices[3].Flag != "--delete-old" || p.Choices[3].Value != "удалить" {
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
	if c := wantLevel(t, p, "--delete-old", LevelInfo); !strings.Contains(c.Message, "раздел") {
		t.Errorf("message: %s", c.Message)
	}
	if p.ManagedAfter.Priority != 15 {
		t.Errorf("priority = %d, want 15 (above 5)", p.ManagedAfter.Priority)
	}
}

func TestPlanResume(t *testing.T) {
	f := withOldSwap(baseFacts(), 0)
	f.Cmdline = readFixture(t, "cmdline/resume")
	c := wantLevel(t, BuildPlan(f, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace}), "гибернация", LevelWarn)
	if !strings.Contains(c.Message, "resume=UUID=aaaa-bbbb") {
		t.Errorf("message: %s", c.Message)
	}
	if _, ok := findCheck(BuildPlan(f, applyOpts(4*GiB)), "гибернация"); ok {
		t.Error("keep does not touch existing swap — no hibernation warning")
	}
	f.State.Managed = &Managed{Path: ManagedPath, SizeBytes: 4 * GiB, Unit: "ramplus.swapfile.swap"}
	wantLevel(t, BuildPlan(f, Options{Action: ActionRemove}), "гибернация", LevelWarn)
}

func TestPlanMemoryForSwapoff(t *testing.T) {
	f := withOldSwap(baseFacts(), 0)
	f.Swaps[0].SizeBytes, f.Swaps[0].UsedBytes = 16*GiB, 10*GiB
	f.Mem["MemAvailable"] = 2 * GiB
	c := wantLevel(t, BuildPlan(f, Options{Action: ActionApply, Size: 4 * GiB, Existing: ExistingReplace}), "память для swapoff /swap.img", LevelFail)
	if !strings.Contains(c.Message, "10.0 GiB") {
		t.Errorf("message: %s", c.Message)
	}
	wantLevel(t, BuildPlan(f, Options{Action: ActionApply, Size: 12 * GiB, Existing: ExistingReplace}), "память для swapoff", LevelOK)
	p := BuildPlan(f, applyOpts(4*GiB))
	if !strings.Contains(p.Choices[2].Alternatives[1].Summary, "заблокирован") {
		t.Errorf("replace alternative must show it is blocked: %+v", p.Choices[2].Alternatives[1])
	}
}

func TestPlanRemoveMemory(t *testing.T) {
	f := baseFacts()
	f.State.Managed = &Managed{Path: ManagedPath, SizeBytes: 8 * GiB, Priority: 10, Unit: "ramplus.swapfile.swap"}
	f.Swaps = []monitor.SwapDevice{{Path: ManagedPath, Type: "file", SizeBytes: 8 * GiB, UsedBytes: 7 * GiB, Priority: 10}}
	f.Mem["MemAvailable"] = 3 * GiB
	f.Exists = map[string]bool{ManagedPath: true, UnitDir + "/ramplus.swapfile.swap": true}
	p := BuildPlan(f, Options{Action: ActionRemove})
	wantLevel(t, p, "память для swapoff", LevelFail)
	wantLevel(t, p, "swap", LevelWarn)
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
	if hasStep(p, KindSwapoff, "/dev/zram0") || !hasStep(p, KindSwapoff, "/swap.img") || p.SwapAfter != 8*GiB {
		t.Errorf("zram must not be touched: %s after=%d", kinds(p.Steps), p.SwapAfter/GiB)
	}
}

func TestPlanBlockers(t *testing.T) {
	managed := &Managed{Path: ManagedPath, SizeBytes: 4 * GiB, Priority: 10, Unit: "ramplus.swapfile.swap"}
	tests := []struct {
		name  string
		mod   func(*Facts)
		title string
	}{
		{"no systemd", func(f *Facts) { f.Systemd = false }, "systemd"},
		{"already managed", func(f *Facts) { f.State.Managed = managed }, "swap RamPlus"},
		{"foreign file", func(f *Facts) { f.Exists[ManagedPath] = true }, "файлы"},
		{"foreign unit", func(f *Facts) { f.Exists[UnitDir+"/ramplus.swapfile.swap"] = true }, "файлы"},
		{"interrupted op", func(f *Facts) { f.Recovery = &Recovery{OpID: "x"} }, "журнал"},
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
	f.State.Managed = &Managed{Path: ManagedPath, SizeBytes: 4 * GiB, Priority: 10, Unit: "ramplus.swapfile.swap", FSType: "ext4"}
	f.Swaps = []monitor.SwapDevice{{Path: ManagedPath, Type: "file", SizeBytes: 4 * GiB, Priority: 10}}
	f.Exists = map[string]bool{ManagedPath: true}
	setFS(&f, fsInfo("/", "ext4", detect.KindNVMe, "nvme", 100*GiB, 12*GiB))
	p := BuildPlan(f, Options{Action: ActionResize, Size: 6 * GiB})
	if got := kinds(p.Steps); got != "swapoff,delete_swapfile,allocate,chmod,mkswap,swapon" {
		t.Errorf("steps = %s", got)
	}
	wantLevel(t, p, "место", LevelOK) // старый удаляется до создания нового: пик +2 GiB
	if p.SwapAfter != 6*GiB || p.ManagedAfter.SizeBytes != 6*GiB || len(p.Disabled) != 0 {
		t.Errorf("after=%d managed=%+v disabled=%v", p.SwapAfter/GiB, p.ManagedAfter, p.Disabled)
	}
	wantLevel(t, BuildPlan(f, Options{Action: ActionResize, Size: 4 * GiB}), "размер", LevelFail)
	f.State.Managed = nil
	wantLevel(t, BuildPlan(f, Options{Action: ActionResize, Size: 6 * GiB}), "swap RamPlus", LevelFail)
}

func TestPlanRollbackNothing(t *testing.T) {
	wantLevel(t, BuildPlan(baseFacts(), Options{Action: ActionRollback}), "rollback", LevelFail)
}

func TestPlanRollbackAfterPartialUndo(t *testing.T) {
	f := baseFacts()
	f.State.Operations = []Operation{
		{ID: "1", Action: ActionApply, Status: OpCommitted},
		{ID: "2", Action: ActionResize, Status: OpRollbackFailed},
	}
	if c := wantLevel(t, BuildPlan(f, Options{Action: ActionRollback}), "rollback", LevelFail); !strings.Contains(c.Message, "ручная проверка") {
		t.Errorf("message: %s", c.Message)
	}
}

func TestPlanRollbackRecovery(t *testing.T) {
	pending := Applied{
		Step:   Step{Kind: KindAllocate, Path: ManagedPath, Size: 4 * GiB},
		Status: StepPending,
		Undo:   []Step{{Kind: KindDeleteSwapfile, Path: ManagedPath, Size: 4 * GiB}},
	}
	mk := func(v Verdict) Facts {
		f := baseFacts()
		f.State.Operations = []Operation{{ID: "op1", Action: ActionApply, Status: OpInProgress, Applied: []Applied{pending}}}
		f.Recovery = &Recovery{OpID: "op1", Pending: &pending, Verdict: v, Detail: "детали"}
		return f
	}
	p := BuildPlan(mk(VerdictPresent), Options{Action: ActionRollback})
	if p.Blocked() || kinds(p.Steps) != "delete_swapfile" {
		t.Errorf("present: steps=%s checks=%+v", kinds(p.Steps), p.Checks)
	}
	p = BuildPlan(mk(VerdictAbsent), Options{Action: ActionRollback})
	if p.Blocked() || len(p.Steps) != 0 {
		t.Errorf("absent: steps=%s", kinds(p.Steps))
	}
	wantLevel(t, BuildPlan(mk(VerdictMismatch), Options{Action: ActionRollback}), "прерванный шаг", LevelFail)
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
	st := State{Managed: &Managed{Path: "/custom/ramplus.swapfile"},
		Operations: []Operation{{ID: "1", Action: ActionApply, Status: OpInProgress}}}
	b, _ := st.Marshal()
	w.files[StateDir] = &fakeFile{dir: true}
	w.files[StatePath] = &fakeFile{data: string(b)}
	status := monitor.Status{Swaps: monitor.Swaps{Devices: []monitor.SwapDevice{
		{Path: "/swap.img"}, {Path: "/custom/ramplus.swapfile"}, {Path: ManagedPath},
	}}}
	AnnotateStatus(&status, &fakeFS{w})
	got := []bool{status.Swaps.Devices[0].Managed, status.Swaps.Devices[1].Managed, status.Swaps.Devices[2].Managed}
	if got[0] || !got[1] || !got[2] {
		t.Errorf("managed = %v", got)
	}
	if len(status.Swaps.Warnings) != 1 || status.Swaps.Warnings[0].Code != WarnInterrupted {
		t.Errorf("warnings = %+v", status.Swaps.Warnings)
	}
}
