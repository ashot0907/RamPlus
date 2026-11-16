package swap

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
)

// fakeWorld — система целиком в памяти. Тесты не выполняют реальных
// системных действий: все вызовы FS/Executor/Kernel попадают сюда.
type fakeFile struct {
	data  string
	size  uint64
	mode  fs.FileMode
	dir   bool
	swap  bool // после mkswap
	nocow bool
}

type fakeWorld struct {
	t       *testing.T
	files   map[string]*fakeFile
	links   map[string]string // EvalSymlinks
	active  map[string]int    // активный swap → приоритет
	used    map[string]uint64 // занято в swap
	enabled map[string]bool   // systemctl enable

	euid              int
	fsType, transport string
	total, avail      uint64
	memAvail          uint64
	cmdline           string
	btrfsTool         bool
	mkswapfile        bool
	fallocUnsupported bool

	failAt    int  // упасть на N-м изменяющем вызове (0 — никогда)
	crash     bool // вместо ошибки — «падение процесса» (panic crashed{})
	mutations int
	// mounts — дополнительные ФС для --path (точка монтирования → описание).
	mounts map[string]fakeMount
	log    []string
}

// fakeMount — ФС в фейковой системе.
type fakeMount struct {
	fsType, transport, kind string
	total, avail            uint64
	readOnly, network       bool
}

// crashed — «процесс упал»: тест перехватывает panic.
type crashed struct{ at string }

const defaultFstab = "# /etc/fstab\n/dev/disk/by-uuid/1111 / ext4 defaults 0 1\n"

func newWorld(t *testing.T) *fakeWorld {
	w := &fakeWorld{
		t: t, files: map[string]*fakeFile{}, links: map[string]string{},
		active: map[string]int{}, used: map[string]uint64{}, enabled: map[string]bool{},
		mounts: map[string]fakeMount{},
		euid:   0, fsType: "ext4", transport: "nvme",
		total: 500 * GiB, avail: 300 * GiB, memAvail: 8 * GiB,
		cmdline: "BOOT_IMAGE=/vmlinuz root=UUID=1111 ro quiet",
	}
	for _, d := range []string{"/", "/etc", UnitDir, "/run", "/run/lock", SystemdRunDir, "/var", "/var/lib"} {
		w.files[d] = &fakeFile{dir: true, mode: 0o755}
	}
	w.files[FstabPath] = &fakeFile{data: defaultFstab, mode: 0o644}
	return w
}

// withOldSwap добавляет /swap.img (8 GiB, активен, описан в fstab).
func (w *fakeWorld) withOldSwap(used uint64) *fakeWorld {
	w.files["/swap.img"] = &fakeFile{size: 8 * GiB, mode: 0o600, swap: true}
	w.active["/swap.img"] = -2
	w.used["/swap.img"] = used
	w.files[FstabPath].data += "/swap.img\tnone\tswap\tsw\t0\t0\n"
	return w
}

func (w *fakeWorld) sys() System {
	return System{
		FS: &fakeFS{w}, Exec: &fakeExec{w}, Kernel: &fakeKernel{w},
		DescribeFS: w.describeFS,
		ListFS: func() []detect.FSInfo {
			out := []detect.FSInfo{w.describeFS("/")}
			mps := make([]string, 0, len(w.mounts))
			for mp := range w.mounts {
				mps = append(mps, mp)
			}
			sort.Strings(mps)
			for _, mp := range mps {
				out = append(out, w.describeFS(mp))
			}
			return out
		},
		Now: func() time.Time { return time.Date(2026, 11, 16, 12, 0, w.mutations%60, 0, time.UTC) },
	}
}

func (w *fakeWorld) mount(path string) (string, fakeMount) {
	best, m := "/", fakeMount{fsType: w.fsType, transport: w.transport, total: w.total, avail: w.avail}
	for mp, fm := range w.mounts {
		if (path == mp || strings.HasPrefix(path, mp+"/")) && len(mp) > len(best) {
			best, m = mp, fm
		}
	}
	return best, m
}

func (w *fakeWorld) describeFS(path string) detect.FSInfo {
	mp, m := w.mount(path)
	kind := m.kind
	if kind == "" {
		kind = detect.KindNVMe
		if m.transport == "usb" {
			kind = detect.KindHDD
		}
	}
	total, avail := m.total, m.avail
	fi := detect.FSInfo{
		MountPoint: mp, Source: "/dev/fake" + strings.ReplaceAll(mp, "/", "_"), FSType: m.fsType,
		ReadOnly: m.readOnly, Network: m.network, Kind: kind,
		Disks:      []detect.BlockDevice{{Name: "sda", Kind: kind, Transport: m.transport}},
		TotalBytes: &total, AvailableBytes: &avail,
	}
	if m.network {
		fi.Disks, fi.Kind = []detect.BlockDevice{}, detect.KindUnknown
	}
	return fi
}

func (w *fakeWorld) mutate(op string) error {
	w.mutations++
	w.log = append(w.log, op)
	if w.failAt != 0 && w.mutations == w.failAt {
		if w.crash {
			panic(crashed{op})
		}
		return fmt.Errorf("injected failure #%d at %s", w.mutations, op)
	}
	return nil
}

func notExist(op, path string) error { return &fs.PathError{Op: op, Path: path, Err: fs.ErrNotExist} }

// snapshot — состояние системы без каталога RamPlus (журнал и бэкапы
// меняются ожидаемо).
type snapshot struct {
	Files   map[string]fakeFile
	Active  map[string]int
	Enabled map[string]bool
}

func (w *fakeWorld) snap() snapshot {
	s := snapshot{Files: map[string]fakeFile{}, Active: map[string]int{}, Enabled: map[string]bool{}}
	for p, f := range w.files {
		if p == StateDir || strings.HasPrefix(p, StateDir+"/") {
			continue
		}
		c := *f
		if !c.dir && c.data != "" {
			c.size = uint64(len(c.data)) // размер текстовых файлов = длина содержимого
		}
		s.Files[p] = c
	}
	for k, v := range w.active {
		s.Active[k] = v
	}
	for k, v := range w.enabled {
		s.Enabled[k] = v
	}
	return s
}

func (s snapshot) diff(o snapshot) string {
	var out []string
	keys := map[string]bool{}
	for k := range s.Files {
		keys[k] = true
	}
	for k := range o.Files {
		keys[k] = true
	}
	for k := range keys {
		a, aok := s.Files[k]
		b, bok := o.Files[k]
		if aok != bok || a != b {
			out = append(out, fmt.Sprintf("file %s: %+v (%v) vs %+v (%v)", k, a, aok, b, bok))
		}
	}
	if fmt.Sprint(s.Active) != fmt.Sprint(o.Active) {
		out = append(out, fmt.Sprintf("active: %v vs %v", s.Active, o.Active))
	}
	if fmt.Sprint(s.Enabled) != fmt.Sprint(o.Enabled) {
		out = append(out, fmt.Sprintf("enabled: %v vs %v", s.Enabled, o.Enabled))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func (w *fakeWorld) state() State {
	f, ok := w.files[StatePath]
	if !ok {
		return State{}
	}
	s, err := ParseState([]byte(f.data))
	if err != nil {
		w.t.Fatalf("state.json: %v", err)
	}
	return s
}

// --- FS ---

type fakeFS struct{ w *fakeWorld }

type fakeInfo struct {
	name string
	f    fakeFile
}

func (i fakeInfo) Name() string { return i.name }
func (i fakeInfo) Size() int64  { return int64(i.f.size) }
func (i fakeInfo) Mode() fs.FileMode {
	if i.f.dir {
		return fs.ModeDir | i.f.mode
	}
	return i.f.mode
}
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.f.dir }
func (i fakeInfo) Sys() any           { return nil }

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	w := f.w
	switch name {
	case SwapsPath:
		var b bytes.Buffer
		b.WriteString("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n")
		paths := make([]string, 0, len(w.active))
		for p := range w.active {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			typ := "file"
			if strings.HasPrefix(p, "/dev/") {
				typ = "partition"
			}
			fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%d\n", p, typ, w.files[p].size/KiB, w.used[p]/KiB, w.active[p])
		}
		return b.Bytes(), nil
	case MeminfoPath:
		return []byte(fmt.Sprintf("MemTotal: %d kB\nMemFree: %d kB\nMemAvailable: %d kB\n",
			16*GiB/KiB, w.memAvail/KiB, w.memAvail/KiB)), nil
	case CmdlinePath:
		return []byte(w.cmdline + "\n"), nil
	}
	file, ok := w.files[name]
	if !ok {
		return nil, notExist("open", name)
	}
	if file.dir {
		return nil, &fs.PathError{Op: "read", Path: name, Err: syscall.EISDIR}
	}
	return []byte(file.data), nil
}

func (f *fakeFS) WriteFileAtomic(name string, data []byte, perm fs.FileMode) error {
	if err := f.w.mutate("write " + name); err != nil {
		return err
	}
	if p, ok := f.w.files[filepath.Dir(name)]; !ok || !p.dir {
		return notExist("write", name)
	}
	f.w.files[name] = &fakeFile{data: string(data), size: uint64(len(data)), mode: perm}
	return nil
}

func (f *fakeFS) Stat(name string) (fs.FileInfo, error) {
	file, ok := f.w.files[name]
	if !ok {
		return nil, notExist("stat", name)
	}
	return fakeInfo{filepath.Base(name), *file}, nil
}

func (f *fakeFS) EvalSymlinks(path string) (string, error) {
	if t, ok := f.w.links[path]; ok {
		return t, nil
	}
	if _, ok := f.w.files[path]; ok {
		return path, nil
	}
	return "", notExist("lstat", path)
}

func (f *fakeFS) hasChildren(dir string) bool {
	for p := range f.w.files {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func (f *fakeFS) Remove(name string) error {
	if err := f.w.mutate("remove " + name); err != nil {
		return err
	}
	file, ok := f.w.files[name]
	switch {
	case !ok:
		return notExist("remove", name)
	case file.dir && f.hasChildren(name):
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.ENOTEMPTY}
	case f.w.active[name] != 0 || hasKey(f.w.active, name):
		// Строже реального ядра: ловим удаление активного swap.
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.EBUSY}
	}
	delete(f.w.files, name)
	return nil
}

func hasKey[V any](m map[string]V, k string) bool { _, ok := m[k]; return ok }

func (f *fakeFS) Mkdir(name string, perm fs.FileMode) error {
	if err := f.w.mutate("mkdir " + name); err != nil {
		return err
	}
	if _, ok := f.w.files[name]; ok {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	f.w.files[name] = &fakeFile{dir: true, mode: perm}
	return nil
}

func (f *fakeFS) MkdirAll(name string, perm fs.FileMode) error {
	if err := f.w.mutate("mkdirall " + name); err != nil {
		return err
	}
	for p := name; p != "/" && p != "."; p = filepath.Dir(p) {
		if _, ok := f.w.files[p]; !ok {
			f.w.files[p] = &fakeFile{dir: true, mode: perm}
		}
	}
	return nil
}

func (f *fakeFS) Chmod(name string, perm fs.FileMode) error {
	if err := f.w.mutate("chmod " + name); err != nil {
		return err
	}
	file, ok := f.w.files[name]
	if !ok {
		return notExist("chmod", name)
	}
	file.mode = perm
	return nil
}

func (f *fakeFS) Create(name string, perm fs.FileMode) error {
	if err := f.w.mutate("create " + name); err != nil {
		return err
	}
	if _, ok := f.w.files[name]; ok {
		return &fs.PathError{Op: "create", Path: name, Err: fs.ErrExist}
	}
	parent, ok := f.w.files[filepath.Dir(name)]
	if !ok || !parent.dir {
		return notExist("create", name)
	}
	f.w.files[name] = &fakeFile{mode: perm, nocow: parent.nocow}
	return nil
}

func (f *fakeFS) Fallocate(name string, size uint64) error {
	if err := f.w.mutate("fallocate " + name); err != nil {
		return err
	}
	file, ok := f.w.files[name]
	if !ok {
		return notExist("fallocate", name)
	}
	if f.w.fallocUnsupported {
		return fmt.Errorf("%s: %w", name, ErrFallocateUnsupported)
	}
	file.size = size
	return nil
}

func (f *fakeFS) WriteZeros(name string, size uint64) error {
	if err := f.w.mutate("zeros " + name); err != nil {
		return err
	}
	file, ok := f.w.files[name]
	if !ok {
		return notExist("zeros", name)
	}
	file.size = size
	return nil
}

func (f *fakeFS) Lock(string) (func() error, error) { return func() error { return nil }, nil }

// --- Executor ---

type fakeExec struct{ w *fakeWorld }

func (e *fakeExec) LookPath(file string) (string, error) {
	if file == "btrfs" && !e.w.btrfsTool {
		return "", notExist("lookpath", file)
	}
	return "/usr/bin/" + file, nil
}

func (e *fakeExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	w := e.w
	cmd := name + " " + strings.Join(args, " ")
	if cmd == "btrfs filesystem mkswapfile --help" { // только чтение
		if w.btrfsTool && w.mkswapfile {
			return nil, nil
		}
		return nil, fmt.Errorf("btrfs: unknown token 'mkswapfile'")
	}
	if err := w.mutate(cmd); err != nil {
		return nil, err
	}
	last := args[len(args)-1]
	switch {
	case name == "mkswap":
		f, ok := w.files[last]
		if !ok || f.dir {
			return nil, notExist("mkswap", last)
		}
		f.swap = true
	case cmd == "systemctl daemon-reload":
	case name == "systemctl" && args[0] == "enable":
		if !hasKey(w.files, filepath.Join(UnitDir, last)) {
			return nil, fmt.Errorf("Unit %s does not exist", last)
		}
		w.enabled[last] = true
	case name == "systemctl" && args[0] == "disable":
		delete(w.enabled, last)
	case strings.HasPrefix(cmd, "btrfs subvolume create "):
		if hasKey(w.files, last) {
			return nil, fmt.Errorf("target path already exists: %s", last)
		}
		w.files[last] = &fakeFile{dir: true, mode: 0o755}
	case strings.HasPrefix(cmd, "btrfs subvolume delete "):
		f, ok := w.files[last]
		if !ok || !f.dir || (&fakeFS{w}).hasChildren(last) {
			return nil, fmt.Errorf("cannot delete subvolume %s", last)
		}
		delete(w.files, last)
	case strings.HasPrefix(cmd, "btrfs filesystem mkswapfile --size "):
		size, _ := strconv.ParseUint(args[3], 10, 64)
		if hasKey(w.files, last) {
			return nil, fmt.Errorf("file exists: %s", last)
		}
		w.files[last] = &fakeFile{size: size, mode: 0o600, swap: true, nocow: true}
	case cmd == "chattr +C "+last:
		f, ok := w.files[last]
		switch {
		case !ok:
			return nil, notExist("chattr", last)
		case !f.dir && f.size > 0:
			return nil, fmt.Errorf("chattr: +C на непустом файле не действует: %s", last)
		}
		f.nocow = true
	default:
		return nil, fmt.Errorf("fakeExec: неожиданная команда %q", cmd)
	}
	return nil, nil
}

// --- Kernel ---

type fakeKernel struct{ w *fakeWorld }

func (k *fakeKernel) Geteuid() int { return k.w.euid }

func (k *fakeKernel) Swapon(path string, prio int) error {
	w := k.w
	if err := w.mutate("swapon " + path); err != nil {
		return err
	}
	f, ok := w.files[path]
	switch {
	case !ok:
		return syscall.ENOENT
	case !f.swap:
		return syscall.EINVAL // нет сигнатуры mkswap
	case f.mode&0o077 != 0:
		return fmt.Errorf("insecure permissions %o", f.mode)
	case w.fsOf(path) == "btrfs" && !f.nocow:
		return syscall.EINVAL // btrfs отвергает COW-swapfile
	case hasKey(w.active, path):
		return syscall.EBUSY
	}
	w.active[path] = prio
	return nil
}

func (k *fakeKernel) Swapoff(path string) error {
	w := k.w
	if err := w.mutate("swapoff " + path); err != nil {
		return err
	}
	if !hasKey(w.active, path) {
		return syscall.EINVAL
	}
	delete(w.active, path)
	delete(w.used, path)
	return nil
}

func (w *fakeWorld) fsOf(path string) string { _, m := w.mount(path); return m.fsType }
