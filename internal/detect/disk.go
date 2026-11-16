package detect

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Типы накопителей.
const (
	KindNVMe = "nvme"
	KindSSD  = "ssd"
	KindHDD  = "hdd"
	// KindVirtual — диск виртуальной машины: rotational у него
	// недостоверен, скорость зависит от хоста.
	KindVirtual = "virtual"
	KindUnknown = "unknown"
)

// BlockDevice — физический диск (не раздел и не dm/md-устройство).
type BlockDevice struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	Rotational *bool   `json:"rotational"`
	Transport  string  `json:"transport"`
	Model      string  `json:"model,omitempty"`
	Vendor     string  `json:"vendor,omitempty"`
	SizeBytes  *uint64 `json:"size_bytes"`
}

// FSInfo — файловая система, которой принадлежит путь, и диски под ней.
type FSInfo struct {
	MountPoint string `json:"mount_point"`
	Source     string `json:"source"`
	Device     string `json:"device"` // имя в sysfs: sda2, nvme0n1p2, dm-0
	FSType     string `json:"fs_type"`
	ReadOnly   bool   `json:"read_only"`
	Network    bool   `json:"network"`
	// Kind — сводный тип по всем физическим дискам (худший из них).
	Kind           string        `json:"kind"`
	Disks          []BlockDevice `json:"disks"`
	TotalBytes     *uint64       `json:"total_bytes"`
	FreeBytes      *uint64       `json:"free_bytes"`
	AvailableBytes *uint64       `json:"available_bytes"`
	DiskError      *Unavailable  `json:"disk_error,omitempty"`
	SpaceError     *Unavailable  `json:"space_error,omitempty"`
	Error          *Unavailable  `json:"error,omitempty"`
}

// RootDisk — ФС корня (в status — root_disk).
type RootDisk = FSInfo

// DiskHints — то, по чему определяется тип накопителя.
type DiskHints struct {
	Name       string
	Transport  string
	Rotational *bool
	Model      string
	Vendor     string
	// Virt — тип виртуализации системы ("" — железо).
	Virt string
}

// virtualModels — модели и производители виртуальных дисков.
var virtualModels = []string{"qemu", "virtual disk", "vmware", "vbox", "virtio", "msft", "xen", "bochs"}

// IsVirtualDisk: диск виртуальный, если об этом говорит шина (virtio, xen,
// hyperv), имя (vd*, xvd*), модель/производитель, или система работает в VM
// (кроме проброшенных USB-дисков).
func IsVirtualDisk(h DiskHints) bool {
	switch h.Transport {
	case "virtio", "xen", "hyperv":
		return true
	case "usb":
		return false
	}
	if strings.HasPrefix(h.Name, "vd") || strings.HasPrefix(h.Name, "xvd") {
		return true
	}
	mv := strings.ToLower(h.Model + " " + h.Vendor)
	for _, v := range virtualModels {
		if strings.Contains(mv, v) {
			return true
		}
	}
	return h.Virt != ""
}

// ClassifyDisk определяет тип накопителя.
func ClassifyDisk(h DiskHints) string {
	switch {
	case IsVirtualDisk(h):
		return KindVirtual
	case strings.HasPrefix(h.Name, "nvme") || h.Transport == "nvme":
		return KindNVMe
	case h.Rotational == nil:
		return KindUnknown
	case *h.Rotational:
		return KindHDD
	default:
		return KindSSD
	}
}

// SummarizeKind сводит типы нескольких дисков (LVM, RAID) к одному —
// самому медленному.
func SummarizeKind(kinds []string) string {
	if len(kinds) == 0 {
		return KindUnknown
	}
	rank := map[string]int{KindHDD: 4, KindUnknown: 3, KindVirtual: 2, KindSSD: 1, KindNVMe: 0}
	worst := kinds[0]
	for _, k := range kinds[1:] {
		if rank[k] > rank[worst] {
			worst = k
		}
	}
	return worst
}

// TransportFromSysPath угадывает шину по реальному пути устройства в sysfs,
// например /sys/devices/pci0000:00/0000:00:14.0/usb2/.../block/sda.
func TransportFromSysPath(p string) string {
	switch {
	case strings.Contains(p, "/nvme/") || strings.HasPrefix(filepath.Base(p), "nvme"):
		return "nvme"
	case strings.Contains(p, "/usb"):
		return "usb"
	case strings.Contains(p, "/virtio"):
		return "virtio"
	case strings.Contains(p, "/xen/") || strings.Contains(p, "/vbd-") || strings.HasPrefix(filepath.Base(p), "xvd"):
		return "xen"
	case strings.Contains(strings.ToLower(p), "/vmbus"):
		return "hyperv"
	case strings.Contains(p, "/mmc_host/") || strings.HasPrefix(filepath.Base(p), "mmcblk"):
		return "mmc"
	case strings.Contains(p, "/ata"):
		return "sata"
	case strings.Contains(p, "/devices/virtual/"):
		return "virtual"
	case strings.Contains(p, "/host"):
		return "scsi"
	default:
		return "unknown"
	}
}

// ReadRootDisk описывает ФС корня.
func ReadRootDisk() RootDisk { return DescribeFS("/") }

// DescribeFS описывает ФС, которой принадлежит путь (путь может ещё не
// существовать — ищется самая длинная точка монтирования-префикс).
// Не возвращает ошибку: недоступное отражено в полях *Error.
func DescribeFS(path string) FSInfo {
	return describeFS("/proc/self/mountinfo", "/sys", Virtualization(), path)
}

func describeFS(mountinfoPath, sysRoot, virt, path string) FSInfo {
	fi := FSInfo{Kind: KindUnknown, Disks: []BlockDevice{}}
	data, err := os.ReadFile(mountinfoPath)
	if err != nil {
		fi.Error = NewUnavailable(err)
		return fi
	}
	mounts, err := ParseMountinfo(bytes.NewReader(data))
	if err != nil {
		fi.Error = NewUnavailable(err)
		return fi
	}
	m, ok := FindMountForPath(mounts, path)
	if !ok {
		fi.Error = NewUnavailable(fmt.Errorf("mount for %s: %w", path, os.ErrNotExist))
		return fi
	}
	return fsFromMount(sysRoot, virt, m)
}

func fsFromMount(sysRoot, virt string, m Mount) FSInfo {
	fi := FSInfo{
		MountPoint: m.MountPoint, Source: m.Source, FSType: m.FSType,
		ReadOnly: m.ReadOnly(), Network: IsNetworkFS(m.FSType),
		Kind: KindUnknown, Disks: []BlockDevice{},
	}
	if total, free, avail, err := statfs(m.MountPoint); err != nil {
		fi.SpaceError = NewUnavailable(err)
	} else {
		fi.TotalBytes, fi.FreeBytes, fi.AvailableBytes = &total, &free, &avail
	}
	maj, mnr := mountDevNum(m)
	if maj == 0 {
		// tmpfs, overlay, zfs, сетевые ФС — блочного устройства нет.
		fi.DiskError = &Unavailable{Reason: ReasonNotPresent, Detail: "no block device behind " + m.Source}
		return fi
	}
	dev, disks, err := resolveBlockDevices(sysRoot, virt, maj, mnr)
	fi.Device = dev
	if err != nil {
		fi.DiskError = NewUnavailable(err)
	}
	if len(disks) > 0 {
		fi.Disks = disks
		fi.Kind = summarizeDisks(disks)
	}
	return fi
}

// networkFS — сетевые ФС.
var networkFS = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "9p": true,
	"ceph": true, "glusterfs": true, "fuse.sshfs": true, "fuse.glusterfs": true, "afs": true,
}

// IsNetworkFS сообщает, сетевая ли ФС.
func IsNetworkFS(fsType string) bool { return networkFS[fsType] }

// skipFS — ФС, которые не бывают местом для swapfile и только засоряют
// список вариантов (образы snap, ISO).
var skipFS = map[string]bool{"squashfs": true, "iso9660": true, "udf": true}

// ListLocalFS — смонтированные ФС, пригодные для выбора места swapfile:
// на блочных устройствах (по одной точке на устройство) и сетевые.
// Корень всегда первый.
func ListLocalFS() []FSInfo {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	mounts, err := ParseMountinfo(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var out []FSInfo
	for _, m := range SelectCandidateMounts(mounts, mountDevNum) {
		out = append(out, fsFromMount("/sys", Virtualization(), m))
	}
	return out
}

// SelectCandidateMounts выбирает точки монтирования для ListLocalFS.
// devNum возвращает номер устройства (0 — нет блочного устройства).
func SelectCandidateMounts(mounts []Mount, devNum func(Mount) (uint32, uint32)) []Mount {
	type key struct{ maj, min uint32 }
	best := map[key]int{}
	var order []key
	var network []Mount
	for i, m := range mounts {
		mp := m.MountPoint
		if skipFS[m.FSType] || isSystemMount(mp) {
			continue
		}
		if IsNetworkFS(m.FSType) {
			network = append(network, m)
			continue
		}
		maj, mnr := devNum(m)
		if maj == 0 {
			continue
		}
		k := key{maj, mnr}
		j, seen := best[k]
		switch {
		case !seen:
			best[k] = i
			order = append(order, k)
		case len(mp) < len(mounts[j].MountPoint): // btrfs-подтома: берём самую короткую точку
			best[k] = i
		}
	}
	var out []Mount
	for _, k := range order {
		out = append(out, mounts[best[k]])
	}
	out = append(out, network...)
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].MountPoint == "/" || out[b].MountPoint == "/" {
			return out[a].MountPoint == "/"
		}
		return out[a].MountPoint < out[b].MountPoint
	})
	return out
}

func isSystemMount(mp string) bool {
	if mp == "/" {
		return false
	}
	for _, p := range []string{"/proc", "/sys", "/dev", "/run", "/boot", "/snap", "/var/lib/docker", "/var/lib/containers", "/var/snap"} {
		if mp == p || strings.HasPrefix(mp, p+"/") {
			return true
		}
	}
	return false
}

// mountDevNum возвращает номер блочного устройства монтирования. btrfs и
// некоторые другие ФС отдают анонимный 0:N — тогда номер берётся из самого
// устройства-источника. 0:0 — блочного устройства нет.
func mountDevNum(m Mount) (major, minor uint32) {
	major, minor = m.Major, m.Minor
	if major == 0 && strings.HasPrefix(m.Source, "/dev/") {
		var st unix.Stat_t
		if err := unix.Stat(m.Source, &st); err == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK {
			return unix.Major(st.Rdev), unix.Minor(st.Rdev)
		}
		return 0, 0
	}
	return major, minor
}

func summarizeDisks(disks []BlockDevice) string {
	kinds := make([]string, len(disks))
	for i, d := range disks {
		kinds[i] = d.Kind
	}
	return SummarizeKind(kinds)
}

// HasTransport сообщает, подключён ли хотя бы один из дисков через transport.
func HasTransport(disks []BlockDevice, transport string) bool {
	for _, d := range disks {
		if d.Transport == transport {
			return true
		}
	}
	return false
}

// PathDisks — на каких физических дисках лежит путь.
type PathDisks struct {
	Device string        `json:"device"`
	Kind   string        `json:"kind"`
	Disks  []BlockDevice `json:"disks"`
}

// DisksForPath находит физические диски под путём: для блочного
// устройства — само устройство, для файла — устройство его ФС.
func DisksForPath(path string) (PathDisks, error) {
	return disksForPath("/proc/self/mountinfo", "/sys", path)
}

func disksForPath(mountinfoPath, sysRoot, path string) (PathDisks, error) {
	pd := PathDisks{Kind: KindUnknown, Disks: []BlockDevice{}}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return pd, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	var maj, mnr uint32
	if st.Mode&unix.S_IFMT == unix.S_IFBLK {
		maj, mnr = unix.Major(st.Rdev), unix.Minor(st.Rdev)
	} else {
		data, err := os.ReadFile(mountinfoPath)
		if err != nil {
			return pd, err
		}
		mounts, err := ParseMountinfo(strings.NewReader(string(data)))
		if err != nil {
			return pd, err
		}
		m, ok := FindMountForPath(mounts, path)
		if !ok {
			return pd, fmt.Errorf("mount for %s: %w", path, os.ErrNotExist)
		}
		if maj, mnr = mountDevNum(m); maj == 0 {
			return pd, fmt.Errorf("no block device behind %s: %w", m.Source, os.ErrNotExist)
		}
	}
	dev, disks, err := resolveBlockDevices(sysRoot, Virtualization(), maj, mnr)
	pd.Device = dev
	if len(disks) > 0 {
		pd.Disks = disks
		pd.Kind = summarizeDisks(disks)
	}
	return pd, err
}

func statfs(path string) (total, free, avail uint64, err error) {
	var st unix.Statfs_t
	if err = unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bfree * bs, st.Bavail * bs, nil
}

// resolveBlockDevices по номеру устройства находит его имя в sysfs и все
// физические диски под ним: раздел → диск, dm/md → slaves (рекурсивно).
func resolveBlockDevices(sysRoot, virt string, major, minor uint32) (string, []BlockDevice, error) {
	link := filepath.Join(sysRoot, "dev", "block", fmt.Sprintf("%d:%d", major, minor))
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", nil, err
	}
	var disks []BlockDevice
	seen := map[string]bool{}
	err = collectDisks(sysRoot, virt, real, seen, &disks, 0)
	return filepath.Base(real), disks, err
}

const maxStackDepth = 8

func collectDisks(sysRoot, virt, real string, seen map[string]bool, out *[]BlockDevice, depth int) error {
	if depth > maxStackDepth {
		return errors.New("block device stack too deep")
	}
	if fileExists(filepath.Join(real, "partition")) {
		real = filepath.Dir(real)
	}
	if seen[real] {
		return nil
	}
	seen[real] = true

	slaves, _ := os.ReadDir(filepath.Join(real, "slaves"))
	if len(slaves) > 0 {
		var firstErr error
		for _, s := range slaves {
			sr, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "class", "block", s.Name()))
			if err == nil {
				err = collectDisks(sysRoot, virt, sr, seen, out, depth+1)
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	*out = append(*out, describeDisk(real, virt))
	return nil
}

func describeDisk(real, virt string) BlockDevice {
	name := filepath.Base(real)
	d := BlockDevice{Name: name, Transport: TransportFromSysPath(real)}
	if v, err := readTrimmed(filepath.Join(real, "queue", "rotational")); err == nil {
		switch v {
		case "1":
			t := true
			d.Rotational = &t
		case "0":
			f := false
			d.Rotational = &f
		}
	}
	if v, err := readTrimmed(filepath.Join(real, "size")); err == nil {
		if sectors, err := strconv.ParseUint(v, 10, 64); err == nil {
			b := sectors * 512 // sysfs всегда считает в 512-байтных секторах
			d.SizeBytes = &b
		}
	}
	if v, err := readTrimmed(filepath.Join(real, "device", "model")); err == nil {
		d.Model = v
	}
	if v, err := readTrimmed(filepath.Join(real, "device", "vendor")); err == nil {
		d.Vendor = v
	}
	d.Kind = ClassifyDisk(DiskHints{Name: name, Transport: d.Transport, Rotational: d.Rotational, Model: d.Model, Vendor: d.Vendor, Virt: virt})
	return d
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(b)), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
