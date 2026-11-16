package swap

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/monitor"
)

// Facts — всё, что планировщик знает о системе. Собирается Gather (только
// чтение); в тестах заполняется вручную.
type Facts struct {
	EUID int

	Root      detect.RootDisk
	DiskTotal uint64 // statfs("/")
	DiskAvail uint64
	DiskErr   error

	Swaps    []monitor.SwapDevice
	SwapsErr error
	Mem      monitor.Meminfo
	MemErr   error

	Cmdline  string
	Fstab    string
	FstabErr error
	// FstabResolved — во что разрешаются spec из fstab (UUID=... → /dev/sda3).
	FstabResolved map[string]string

	State    State
	StateErr error

	Systemd       bool
	HasBtrfs      bool
	HasMkswapfile bool
	HasChattr     bool

	// Exists — какие из наших путей уже существуют.
	Exists map[string]bool
}

// Gather собирает факты. Ничего не меняет и не требует root.
func Gather(ctx context.Context, sys System) Facts {
	f := Facts{EUID: sys.Kernel.Geteuid(), FstabResolved: map[string]string{}, Exists: map[string]bool{}}
	f.Root = sys.RootDisk()
	f.DiskTotal, f.DiskAvail, f.DiskErr = sys.FS.Statfs("/")

	if b, err := sys.FS.ReadFile(SwapsPath); err != nil {
		f.SwapsErr = err
	} else {
		f.Swaps, f.SwapsErr = monitor.ParseSwaps(bytes.NewReader(b))
	}
	if b, err := sys.FS.ReadFile(MeminfoPath); err != nil {
		f.MemErr = err
	} else {
		f.Mem, f.MemErr = monitor.ParseMeminfo(bytes.NewReader(b))
	}
	if b, err := sys.FS.ReadFile(CmdlinePath); err == nil {
		f.Cmdline = strings.TrimSpace(string(b))
	}
	if b, err := sys.FS.ReadFile(FstabPath); err != nil {
		f.FstabErr = err
	} else {
		f.Fstab = string(b)
		for _, e := range SwapEntries(ParseFstab(f.Fstab)) {
			for _, c := range SpecCandidates(e.Spec) {
				if real, err := sys.FS.EvalSymlinks(c); err == nil {
					f.FstabResolved[e.Spec] = real
				}
			}
		}
	}
	if b, err := sys.FS.ReadFile(StatePath); err == nil {
		f.State, f.StateErr = ParseState(b)
	} else if !errors.Is(err, fs.ErrNotExist) {
		f.StateErr = err
	}

	_, err := sys.FS.Stat(SystemdRunDir)
	f.Systemd = err == nil

	if f.Root.FSType == "btrfs" {
		if _, err := sys.Exec.LookPath("btrfs"); err == nil {
			f.HasBtrfs = true
			// btrfs-progs >= 6.1; старые версии отвечают ошибкой на неизвестную команду.
			_, err := sys.Exec.Run(ctx, "btrfs", "filesystem", "mkswapfile", "--help")
			f.HasMkswapfile = err == nil
		}
		_, err := sys.Exec.LookPath("chattr")
		f.HasChattr = err == nil
	}

	for _, p := range []string{ManagedPath, BtrfsPath, BtrfsDir,
		filepath.Join(UnitDir, UnitName(ManagedPath)), filepath.Join(UnitDir, UnitName(BtrfsPath))} {
		if _, err := sys.FS.Stat(p); err == nil {
			f.Exists[p] = true
		}
	}
	if m := f.State.Managed; m != nil {
		if _, err := sys.FS.Stat(filepath.Join(UnitDir, m.Unit)); err == nil {
			f.Exists[filepath.Join(UnitDir, m.Unit)] = true
		}
	}
	return f
}

// swapByPath ищет активный swap.
func (f Facts) swapByPath(path string) (monitor.SwapDevice, bool) {
	for _, s := range f.Swaps {
		if s.Path == path {
			return s, true
		}
	}
	return monitor.SwapDevice{}, false
}

func isZram(path string) bool { return strings.HasPrefix(path, "/dev/zram") }

func isOurPath(path string) bool { return path == ManagedPath || path == BtrfsPath }

// fstabEntriesFor — строки fstab, описывающие активный swap path.
func (f Facts) fstabEntriesFor(path string) []FstabEntry {
	var out []FstabEntry
	for _, e := range SwapEntries(ParseFstab(f.Fstab)) {
		if e.Spec == path || f.FstabResolved[e.Spec] == path {
			out = append(out, e)
		}
	}
	return out
}

func (f Facts) onUSB() bool { return detect.HasTransport(f.Root.Disks, "usb") }
