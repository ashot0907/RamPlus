package detect

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		file string
		want OSRelease
	}{
		{"os-release/ubuntu", OSRelease{ID: "ubuntu", Name: "Ubuntu", PrettyName: "Ubuntu 24.04.5 LTS", VersionID: "24.04", VersionCodename: "noble"}},
		{"os-release/fedora", OSRelease{ID: "fedora", Name: "Fedora Linux", PrettyName: `Fedora Linux 41 "Workstation" Edition`, VersionID: "41"}},
		{"os-release/minimal", OSRelease{ID: "alpine", Name: "Linux", PrettyName: "Linux"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := ParseOSRelease(openFixture(t, tt.file))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseOSReleaseEmpty(t *testing.T) {
	_, err := ParseOSRelease(openFixture(t, "os-release/empty"))
	if !errors.Is(err, ErrParse) {
		t.Fatalf("want ErrParse, got %v", err)
	}
}

func TestParseMountinfo(t *testing.T) {
	tests := []struct {
		file             string
		mountPoint       string
		wantMaj, wantMin uint32
		wantFS, wantSrc  string
	}{
		{"mountinfo/ext4", "/", 8, 2, "ext4", "/dev/sda2"},
		{"mountinfo/ext4", "/boot/efi", 259, 1, "vfat", "/dev/nvme0n1p1"},
		{"mountinfo/btrfs", "/", 0, 24, "btrfs", "/dev/nvme0n1p2"},
		{"mountinfo/lvm-escaped", "/", 253, 1, "xfs", "/dev/mapper/vg0-root"},
		{"mountinfo/lvm-escaped", "/mnt/my disk", 8, 17, "ext4", "/dev/sdb1"},
		{"mountinfo/overmount", "/", 0, 50, "overlay", "overlay"},
	}
	for _, tt := range tests {
		t.Run(tt.file+":"+tt.mountPoint, func(t *testing.T) {
			mounts, err := ParseMountinfo(openFixture(t, tt.file))
			if err != nil {
				t.Fatal(err)
			}
			m, ok := FindMount(mounts, tt.mountPoint)
			if !ok {
				t.Fatalf("mount %q not found", tt.mountPoint)
			}
			if m.Major != tt.wantMaj || m.Minor != tt.wantMin || m.FSType != tt.wantFS || m.Source != tt.wantSrc {
				t.Errorf("got %+v", m)
			}
		})
	}
}

func TestParseMountinfoMalformed(t *testing.T) {
	for _, in := range []string{"", "33 1 8:2 / / rw,relatime shared:1 ext4 /dev/sda2 rw\n"} {
		if _, err := ParseMountinfo(strings.NewReader(in)); !errors.Is(err, ErrParse) {
			t.Errorf("%q: want ErrParse, got %v", in, err)
		}
	}
	if _, err := ParseMountinfo(openFixture(t, "mountinfo/malformed")); !errors.Is(err, ErrParse) {
		t.Errorf("fixture: want ErrParse, got %v", err)
	}
}

func TestUnescapeOctal(t *testing.T) {
	tests := map[string]string{
		`/mnt/my\040disk`:        "/mnt/my disk",
		`/a\011b\012c\134d`:      "/a\tb\nc\\d",
		`/plain`:                 "/plain",
		`/trailing\04`:           `/trailing\04`,
		`/swapfile\040(deleted)`: "/swapfile (deleted)",
		`\999`:                   `\999`,
	}
	for in, want := range tests {
		if got := UnescapeOctal(in); got != want {
			t.Errorf("UnescapeOctal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyDisk(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name string
		h    DiskHints
		want string
	}{
		{"nvme", DiskHints{Name: "nvme0n1", Transport: "nvme", Rotational: &no}, KindNVMe},
		{"sata ssd", DiskHints{Name: "sda", Transport: "sata", Rotational: &no}, KindSSD},
		{"sata hdd", DiskHints{Name: "sda", Transport: "sata", Rotational: &yes}, KindHDD},
		{"usb hdd", DiskHints{Name: "sdb", Transport: "usb", Rotational: &yes}, KindHDD},
		{"no rotational", DiskHints{Name: "sdc", Transport: "scsi"}, KindUnknown},
		// Виртуальные диски сообщают rotational недостоверно.
		{"virtio", DiskHints{Name: "vda", Transport: "virtio", Rotational: &yes}, KindVirtual},
		{"vd* name", DiskHints{Name: "vdb", Transport: "unknown", Rotational: &yes}, KindVirtual},
		{"xen", DiskHints{Name: "xvda", Transport: "xen", Rotational: &yes}, KindVirtual},
		{"hyperv", DiskHints{Name: "sda", Transport: "hyperv", Rotational: &yes}, KindVirtual},
		{"QEMU HARDDISK", DiskHints{Name: "sda", Transport: "sata", Rotational: &yes, Model: "QEMU HARDDISK", Vendor: "ATA"}, KindVirtual},
		{"VMware", DiskHints{Name: "sda", Transport: "scsi", Rotational: &yes, Model: "Virtual disk", Vendor: "VMware"}, KindVirtual},
		{"Hyper-V Msft", DiskHints{Name: "sda", Transport: "scsi", Rotational: &yes, Model: "Virtual Disk", Vendor: "Msft"}, KindVirtual},
		{"QEMU NVMe", DiskHints{Name: "nvme0n1", Transport: "nvme", Rotational: &no, Model: "QEMU NVMe Ctrl"}, KindVirtual},
		{"any disk in VM", DiskHints{Name: "sda", Transport: "scsi", Rotational: &yes, Virt: "kvm"}, KindVirtual},
		{"usb passthrough in VM", DiskHints{Name: "sdb", Transport: "usb", Rotational: &no, Virt: "kvm"}, KindSSD},
	}
	for _, tt := range tests {
		if got := ClassifyDisk(tt.h); got != tt.want {
			t.Errorf("%s: ClassifyDisk = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestDetectVirt(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	metal, vm := read("cpuinfo/metal"), read("cpuinfo/vm")
	tests := []struct {
		dmi     map[string]string
		cpuinfo string
		want    string
	}{
		{map[string]string{"sys_vendor": "ASUS", "product_name": "System Product Name"}, metal, ""},
		{map[string]string{"sys_vendor": "QEMU", "product_name": "Standard PC (Q35 + ICH9, 2009)"}, vm, "kvm"},
		{map[string]string{"sys_vendor": "VMware, Inc.", "product_name": "VMware Virtual Platform"}, vm, "vmware"},
		{map[string]string{"sys_vendor": "innotek GmbH", "product_name": "VirtualBox"}, vm, "oracle"},
		{map[string]string{"sys_vendor": "Microsoft Corporation", "product_name": "Virtual Machine"}, vm, "microsoft"},
		{map[string]string{"sys_vendor": "Microsoft Corporation", "product_name": "Surface Laptop 5"}, metal, ""},
		{map[string]string{"sys_vendor": "Amazon EC2"}, vm, "amazon"},
		{nil, vm, "vm"}, // DMI недоступен, но CPU сообщает hypervisor
		{nil, metal, ""},
	}
	for _, tt := range tests {
		if got := DetectVirt(tt.dmi, tt.cpuinfo); got != tt.want {
			t.Errorf("DetectVirt(%v) = %q, want %q", tt.dmi, got, tt.want)
		}
	}
}

func TestSelectCandidateMounts(t *testing.T) {
	mounts, err := ParseMountinfo(openFixture(t, "mountinfo/desktop"))
	if err != nil {
		t.Fatal(err)
	}
	devNum := func(m Mount) (uint32, uint32) {
		if m.Major == 0 && strings.HasPrefix(m.Source, "/dev/") {
			return 259, 2 // btrfs: номер из устройства-источника
		}
		return m.Major, m.Minor
	}
	var got []string
	for _, m := range SelectCandidateMounts(mounts, devNum) {
		got = append(got, m.MountPoint)
	}
	// /boot/efi, snap, docker, tmpfs, proc/sys — отброшены; btrfs-подтом
	// /mnt/data/media — дубликат устройства; корень первый.
	want := "/,/mnt/data,/mnt/nas,/mnt/win"
	if strings.Join(got, ",") != want {
		t.Errorf("candidates = %v, want %s", got, want)
	}
	win, _ := FindMount(mounts, "/mnt/win")
	if !win.ReadOnly() || IsNetworkFS(win.FSType) || !IsNetworkFS("nfs4") {
		t.Errorf("ro/network detection: %+v", win)
	}
}

func TestSummarizeKind(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{nil, KindUnknown},
		{[]string{KindNVMe}, KindNVMe},
		{[]string{KindNVMe, KindSSD}, KindSSD},
		{[]string{KindSSD, KindHDD, KindNVMe}, KindHDD},
		{[]string{KindNVMe, KindUnknown}, KindUnknown},
	}
	for _, tt := range tests {
		if got := SummarizeKind(tt.in); got != tt.want {
			t.Errorf("SummarizeKind(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestTransportFromSysPath(t *testing.T) {
	tests := map[string]string{
		"/sys/devices/pci0000:00/0000:00:06.0/0000:02:00.0/nvme/nvme0/nvme0n1":                              "nvme",
		"/sys/devices/pci0000:00/0000:00:14.0/usb2/2-2/2-2:1.0/host8/target8:0:0/8:0:0:0/block/sda":         "usb",
		"/sys/devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda":                     "sata",
		"/sys/devices/pci0000:00/0000:00:04.0/virtio1/block/vda":                                            "virtio",
		"/sys/devices/platform/fe320000.mmc/mmc_host/mmc0/mmc0:aaaa/block/mmcblk0":                          "mmc",
		"/sys/devices/vbd-51712/block/xvda":                                                                 "xen",
		"/sys/devices/LNXSYSTM:00/LNXSYBUS:00/ACPI0004:00/VMBUS:00/abc/host0/target0:0:0/0:0:0:0/block/sda": "hyperv",
		"/sys/devices/virtual/block/loop0":                                                                  "virtual",
		"/sys/devices/pci0000:00/0000:00:10.0/host2/target2:0:0/2:0:0:0/block/sda":                          "scsi",
		"/somewhere/else": "unknown",
	}
	for in, want := range tests {
		if got := TransportFromSysPath(in); got != want {
			t.Errorf("TransportFromSysPath(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestReason(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{&fs.PathError{Op: "open", Path: "/sys/kernel/debug/zswap", Err: syscall.EACCES}, ReasonPermissionDenied},
		{&fs.PathError{Op: "open", Path: "/proc/pressure/memory", Err: syscall.ENOENT}, ReasonNotPresent},
		{&fs.PathError{Op: "read", Path: "/proc/pressure/memory", Err: syscall.EOPNOTSUPP}, ReasonUnsupported},
		{fmt.Errorf("%w: bad", ErrParse), ReasonParseError},
		{errors.New("boom"), ReasonUnavailable},
	}
	for _, tt := range tests {
		if got := Reason(tt.err); got != tt.want {
			t.Errorf("Reason(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
	if NewUnavailable(nil) != nil {
		t.Error("NewUnavailable(nil) must be nil")
	}
}

// fakeSysfs строит минимальное дерево sysfs с симлинками, как у ядра:
// nvme0n1p3 → dm-0 (LUKS) → dm-1 (LVM), и sda2 на USB-HDD.
func fakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(p, content string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ln := func(target, link string) {
		full := filepath.Join(root, link)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, target), full); err != nil {
			t.Fatal(err)
		}
	}
	nvme := "devices/pci0000:00/0000:00:06.0/nvme/nvme0/nvme0n1"
	mk(nvme+"/queue/rotational", "0\n")
	mk(nvme+"/size", "1000215216\n")
	mk(nvme+"/device/model", "Fast SSD 1TB   \n")
	mk(nvme+"/nvme0n1p3/partition", "3\n")
	mk("devices/virtual/block/dm-0/queue/rotational", "0\n")
	mk("devices/virtual/block/dm-1/queue/rotational", "0\n")
	ln(nvme+"/nvme0n1p3", "devices/virtual/block/dm-0/slaves/nvme0n1p3")
	ln("devices/virtual/block/dm-0", "devices/virtual/block/dm-1/slaves/dm-0")
	ln(nvme+"/nvme0n1p3", "class/block/nvme0n1p3")
	ln("devices/virtual/block/dm-0", "class/block/dm-0")
	ln("devices/virtual/block/dm-1", "dev/block/253:1")

	usb := "devices/pci0000:00/0000:00:14.0/usb2/2-2/2-2:1.0/host8/target8:0:0/8:0:0:0/block/sda"
	mk(usb+"/queue/rotational", "1\n")
	mk(usb+"/size", "100\n")
	mk(usb+"/sda2/partition", "2\n")
	ln(usb+"/sda2", "dev/block/8:2")
	return root
}

func TestResolveBlockDevices(t *testing.T) {
	root := fakeSysfs(t)

	dev, disks, err := resolveBlockDevices(root, "", 253, 1)
	if err != nil {
		t.Fatal(err)
	}
	if dev != "dm-1" || len(disks) != 1 {
		t.Fatalf("dev=%s disks=%+v", dev, disks)
	}
	d := disks[0]
	if d.Name != "nvme0n1" || d.Kind != KindNVMe || d.Transport != "nvme" || d.Model != "Fast SSD 1TB" ||
		d.SizeBytes == nil || *d.SizeBytes != 1000215216*512 {
		t.Errorf("unexpected disk %+v", d)
	}

	dev, disks, err = resolveBlockDevices(root, "", 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	if dev != "sda2" || len(disks) != 1 || disks[0].Name != "sda" || disks[0].Kind != KindHDD || disks[0].Transport != "usb" {
		t.Errorf("dev=%s disks=%+v", dev, disks)
	}

	if _, _, err := resolveBlockDevices(root, "", 9, 9); Reason(err) != ReasonNotPresent {
		t.Errorf("missing device: want not_present, got %v", err)
	}
}

func TestReadRootDiskMissingMountinfo(t *testing.T) {
	rd := describeFS(filepath.Join("testdata", "nope"), t.TempDir(), "", "/")
	if rd.Error == nil || rd.Error.Reason != ReasonNotPresent {
		t.Fatalf("want not_present error, got %+v", rd.Error)
	}
	if rd.Kind != KindUnknown || rd.Disks == nil {
		t.Errorf("want unknown kind and empty (non-nil) disks, got %+v", rd)
	}
}

func TestReadRootDiskFromFixtures(t *testing.T) {
	root := fakeSysfs(t)
	rd := describeFS(filepath.Join("testdata", "mountinfo", "lvm-escaped"), root, "", "/")
	if rd.Error != nil || rd.DiskError != nil {
		t.Fatalf("errors: %+v %+v", rd.Error, rd.DiskError)
	}
	if rd.Device != "dm-1" || rd.FSType != "xfs" || rd.Kind != KindNVMe {
		t.Errorf("got %+v", rd)
	}

	rd = describeFS(filepath.Join("testdata", "mountinfo", "overmount"), root, "", "/")
	if rd.DiskError == nil || rd.DiskError.Reason != ReasonNotPresent || rd.FSType != "overlay" {
		t.Errorf("overlay root: got %+v", rd)
	}
}

func TestFindMountForPath(t *testing.T) {
	mounts, err := ParseMountinfo(openFixture(t, "mountinfo/ext4"))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"/ramplus.swap":   "/",
		"/boot/efi/EFI/x": "/boot/efi",
		"/boot/efi":       "/boot/efi",
		"/boot/efix":      "/",
		"/proc/meminfo":   "/proc",
		"/":               "/",
	}
	for path, want := range tests {
		m, ok := FindMountForPath(mounts, path)
		if !ok || m.MountPoint != want {
			t.Errorf("FindMountForPath(%s) = %q, %v; want %q", path, m.MountPoint, ok, want)
		}
	}
	if _, ok := FindMountForPath(mounts, "relative"); ok {
		t.Error("relative path must not match")
	}
}

func TestResolveBlockDevicesInVM(t *testing.T) {
	root := fakeSysfs(t)
	_, disks, err := resolveBlockDevices(root, "kvm", 8, 2)
	if err != nil || disks[0].Kind != KindHDD {
		// USB-диск, проброшенный в VM, остаётся физическим.
		t.Errorf("usb in VM: %+v %v", disks, err)
	}
	_, disks, _ = resolveBlockDevices(root, "kvm", 253, 1)
	if disks[0].Kind != KindVirtual {
		t.Errorf("disk in VM: %+v", disks)
	}
}
