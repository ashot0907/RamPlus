package detect

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Типы накопителей.
const (
	KindNVMe    = "nvme"
	KindSSD     = "ssd"
	KindHDD     = "hdd"
	KindUnknown = "unknown"
)

// BlockDevice — физический диск (не раздел и не dm/md-устройство).
type BlockDevice struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	Rotational *bool   `json:"rotational"`
	Transport  string  `json:"transport"`
	Model      string  `json:"model,omitempty"`
	SizeBytes  *uint64 `json:"size_bytes"`
}

// RootDisk — что лежит под корневой ФС.
type RootDisk struct {
	MountPoint string `json:"mount_point"`
	Source     string `json:"source"`
	Device     string `json:"device"` // имя в sysfs: sda2, nvme0n1p2, dm-0
	FSType     string `json:"fs_type"`
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

// ClassifyDisk определяет тип накопителя по имени, транспорту и флагу
// queue/rotational.
func ClassifyDisk(name, transport string, rotational *bool) string {
	switch {
	case strings.HasPrefix(name, "nvme") || transport == "nvme":
		return KindNVMe
	case rotational == nil:
		return KindUnknown
	case *rotational:
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
	rank := map[string]int{KindHDD: 3, KindUnknown: 2, KindSSD: 1, KindNVMe: 0}
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

// ReadRootDisk описывает диск под "/". Не возвращает ошибку: всё, что не
// удалось узнать, отражено в полях *Error.
func ReadRootDisk() RootDisk {
	return readRootDisk("/proc/self/mountinfo", "/sys", "/")
}

func readRootDisk(mountinfoPath, sysRoot, mountPoint string) RootDisk {
	rd := RootDisk{MountPoint: mountPoint, Kind: KindUnknown, Disks: []BlockDevice{}}

	if total, free, avail, err := statfs(mountPoint); err != nil {
		rd.SpaceError = NewUnavailable(err)
	} else {
		rd.TotalBytes, rd.FreeBytes, rd.AvailableBytes = &total, &free, &avail
	}

	f, err := os.Open(mountinfoPath)
	if err != nil {
		rd.Error = NewUnavailable(err)
		return rd
	}
	mounts, err := ParseMountinfo(f)
	f.Close()
	if err != nil {
		rd.Error = NewUnavailable(err)
		return rd
	}
	m, ok := FindMount(mounts, mountPoint)
	if !ok {
		rd.Error = NewUnavailable(fmt.Errorf("mount %s: %w", mountPoint, os.ErrNotExist))
		return rd
	}
	rd.Source, rd.FSType = m.Source, m.FSType

	maj, mnr := mountDevNum(m)
	if maj == 0 {
		// tmpfs, overlay, zfs и т.п. — блочного устройства нет.
		rd.DiskError = &Unavailable{Reason: ReasonNotPresent, Detail: "no block device behind " + m.Source}
		return rd
	}

	dev, disks, err := resolveBlockDevices(sysRoot, maj, mnr)
	rd.Device = dev
	if err != nil {
		rd.DiskError = NewUnavailable(err)
	}
	if len(disks) > 0 {
		rd.Disks = disks
		rd.Kind = summarizeDisks(disks)
	}
	return rd
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
	dev, disks, err := resolveBlockDevices(sysRoot, maj, mnr)
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
func resolveBlockDevices(sysRoot string, major, minor uint32) (string, []BlockDevice, error) {
	link := filepath.Join(sysRoot, "dev", "block", fmt.Sprintf("%d:%d", major, minor))
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", nil, err
	}
	var disks []BlockDevice
	seen := map[string]bool{}
	err = collectDisks(sysRoot, real, seen, &disks, 0)
	return filepath.Base(real), disks, err
}

const maxStackDepth = 8

func collectDisks(sysRoot, real string, seen map[string]bool, out *[]BlockDevice, depth int) error {
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
				err = collectDisks(sysRoot, sr, seen, out, depth+1)
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	*out = append(*out, describeDisk(real))
	return nil
}

func describeDisk(real string) BlockDevice {
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
	d.Kind = ClassifyDisk(name, d.Transport, d.Rotational)
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
