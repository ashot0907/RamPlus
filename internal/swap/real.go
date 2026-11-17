package swap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ashot0907/ramplus/internal/detect"
)

// DefaultSystem — реальная система.
func DefaultSystem() System {
	return System{FS: OSFS{}, Exec: OSExec{}, Kernel: OSKernel{},
		DescribeFS: detect.DescribeFS, ListFS: detect.ListLocalFS, Now: time.Now}
}

// OSFS — FS поверх os и unix.
type OSFS struct{}

func (OSFS) ReadFile(name string) ([]byte, error)         { return os.ReadFile(name) }
func (OSFS) Stat(name string) (fs.FileInfo, error)        { return os.Stat(name) }
func (OSFS) Glob(pattern string) ([]string, error)        { return filepath.Glob(pattern) }
func (OSFS) EvalSymlinks(path string) (string, error)     { return filepath.EvalSymlinks(path) }
func (OSFS) Remove(name string) error                     { return os.Remove(name) }
func (OSFS) Mkdir(name string, perm fs.FileMode) error    { return os.Mkdir(name, perm) }
func (OSFS) MkdirAll(name string, perm fs.FileMode) error { return os.MkdirAll(name, perm) }
func (OSFS) Chmod(name string, perm fs.FileMode) error    { return os.Chmod(name, perm) }

func (OSFS) WriteFileAtomic(name string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(name)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(name)+".ramplus-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func (OSFS) Create(name string, perm fs.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	return f.Close()
}

func (OSFS) Fallocate(name string, size uint64) error {
	f, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Fallocate(int(f.Fd()), 0, 0, int64(size)); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) {
			return fmt.Errorf("%s: %w", name, ErrFallocateUnsupported)
		}
		return &os.PathError{Op: "fallocate", Path: name, Err: err}
	}
	return f.Sync()
}

func (OSFS) WriteZeros(name string, size uint64) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 4*MiB)
	for left := size; left > 0; {
		n := min(left, uint64(len(buf)))
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		left -= n
	}
	return f.Sync()
}

func (OSFS) Lock(name string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("другой экземпляр ramplus уже выполняет операцию (%s)", name)
		}
		return nil, err
	}
	return func() error {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return f.Close()
	}, nil
}

// OSExec запускает команды с LC_ALL=C (стабильный вывод ошибок).
type OSExec struct{}

func (OSExec) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (OSExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// OSKernel — системные вызовы. В golang.org/x/sys/unix нет обёрток
// swapon/swapoff, поэтому вызываем syscall напрямую.
type OSKernel struct{}

// Флаги swapon(2).
const (
	swapFlagPrefer   = 0x8000
	swapFlagPrioMask = 0x7fff
)

func (OSKernel) Geteuid() int { return unix.Geteuid() }

func (OSKernel) Swapon(path string, priority int) error {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	flags := 0
	if priority >= 0 {
		flags = swapFlagPrefer | (priority & swapFlagPrioMask)
	}
	if _, _, e := unix.Syscall(unix.SYS_SWAPON, uintptr(unsafe.Pointer(p)), uintptr(flags), 0); e != 0 {
		return e
	}
	return nil
}

func (OSKernel) Swapoff(path string) error {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	if _, _, e := unix.Syscall(unix.SYS_SWAPOFF, uintptr(unsafe.Pointer(p)), 0, 0); e != 0 {
		return e
	}
	return nil
}
