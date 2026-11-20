// Package fakesys — система целиком в памяти для тестов: FS, Executor и
// Kernel без реальных системных действий. Все вызовы попадают в World.
package fakesys

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/sys"
)

const (
	KiB = 1 << 10
	GiB = 1 << 30

	// Пути, которые фейк обслуживает особо (как ядро).
	swapsPath     = "/proc/swaps"
	meminfoPath   = "/proc/meminfo"
	cmdlinePath   = "/proc/cmdline"
	unitDir       = "/etc/systemd/system"
	stateDir      = "/var/lib/ramplus"
	zswapParamDir = "/sys/module/zswap/parameters"
	osReleasePath = "/proc/sys/kernel/osrelease"
	cryptoPath    = "/proc/crypto"

	// DefaultFstab — /etc/fstab новой системы.
	DefaultFstab = "# /etc/fstab\n/dev/disk/by-uuid/1111 / ext4 defaults 0 1\n"
	// Release — версия ядра фейковой системы.
	Release = "7.0.0-test"
)

// File — файл или каталог.
type File struct {
	Data  string
	Size  uint64
	Mode  fs.FileMode
	Dir   bool
	Swap  bool // после mkswap
	NoCOW bool
	// UID/GID — владелец (Chown).
	UID, GID int
}

// Mount — дополнительная ФС (для --path).
type Mount struct {
	FSType, Transport, Kind string
	Total, Avail            uint64
	ReadOnly, Network       bool
}

// Crashed — «процесс упал» (panic при Crash): тест перехватывает его.
type Crashed struct{ At string }

// World — состояние фейковой системы.
type World struct {
	Files   map[string]*File
	Links   map[string]string // EvalSymlinks
	Active  map[string]int    // активный swap → приоритет
	Used    map[string]uint64 // занято в swap
	Enabled map[string]bool   // systemctl enable

	EUID              int
	FSType, Transport string
	Total, Avail      uint64
	MemAvail          uint64
	MemTotal          uint64 // 0 — 16 GiB
	Cmdline           string
	BtrfsTool         bool
	Mkswapfile        bool
	FallocUnsupported bool

	FailAt int  // упасть на N-м изменяющем вызове (0 — никогда)
	Crash  bool // вместо ошибки — panic(Crashed{})
	// FrozenClock — часы стоят: все операции получают одно и то же время.
	FrozenClock bool
	// Compressors/Zpools — что ядро примет при записи в параметры zswap.
	Compressors, Zpools map[string]bool
	Mutations           int
	Mounts              map[string]Mount
	Log                 []string
	// OnMutation вызывается перед каждым изменяющим вызовом (тесты
	// параллельной работы: остановить операцию посреди шагов).
	OnMutation func(op string)
	// Locked — блокировка журнала занята (Lock возвращает ErrLocked).
	Locked bool
	// ReadOnly — каталоги, смонтированные только для чтения (как /home у
	// демона с ProtectHome=read-only).
	ReadOnly []string
}

// lockMu — защищает World.Locked (Lock вызывается из разных горутин).
var lockMu sync.Mutex

// New — чистая система: ext4 на NVMe, 500 GiB, 300 GiB свободно, RAM 16 GiB
// (8 доступно), systemd, root.
func New() *World {
	w := &World{
		Files: map[string]*File{}, Links: map[string]string{},
		Active: map[string]int{}, Used: map[string]uint64{}, Enabled: map[string]bool{},
		Mounts: map[string]Mount{},
		EUID:   0, FSType: "ext4", Transport: "nvme",
		Total: 500 * GiB, Avail: 300 * GiB, MemAvail: 8 * GiB,
		Cmdline: "BOOT_IMAGE=/vmlinuz root=UUID=1111 ro quiet",
	}
	for _, d := range []string{"/", "/etc", unitDir, "/run", "/run/lock", "/run/systemd/system", "/var", "/var/lib"} {
		w.Files[d] = &File{Dir: true, Mode: 0o755}
	}
	w.Files["/etc/fstab"] = &File{Data: DefaultFstab, Mode: 0o644}
	return w
}

// WithOldSwap добавляет /swap.img (8 GiB, активен, приоритет -2, в fstab).
func (w *World) WithOldSwap(used uint64) *World {
	w.Files["/swap.img"] = &File{Size: 8 * GiB, Mode: 0o600, Swap: true}
	w.Active["/swap.img"] = -2
	w.Used["/swap.img"] = used
	w.Files["/etc/fstab"].Data += "/swap.img\tnone\tswap\tsw\t0\t0\n"
	return w
}

// WithKernelParams — параметры zswap и vm.* как на стоковой Ubuntu.
// withZpool=false — ядро 7.0+, где параметра zpool нет.
func (w *World) WithKernelParams(withZpool bool) *World {
	dir := func(p string) { w.Files[p] = &File{Dir: true, Mode: 0o755} }
	val := func(p, v string) { w.Files[p] = &File{Data: v + "\n", Mode: 0o644} }
	for _, d := range []string{"/sys", "/sys/module", "/sys/module/zswap", zswapParamDir, "/proc/sys", "/proc/sys/vm",
		"/proc/sys/kernel", "/sys/module/zsmalloc", "/etc/sysctl.d", "/usr/lib/sysctl.d", "/lib/modules", "/lib/modules/" + Release,
		"/lib/modules/" + Release + "/kernel", "/lib/modules/" + Release + "/kernel/crypto"} {
		dir(d)
	}
	val(zswapParamDir+"/enabled", "N")
	val(zswapParamDir+"/compressor", "lzo")
	val(zswapParamDir+"/max_pool_percent", "20")
	val(zswapParamDir+"/shrinker_enabled", "Y")
	w.Zpools = map[string]bool{"zsmalloc": true}
	if withZpool {
		val(zswapParamDir+"/zpool", "zbud")
		w.Zpools["zbud"] = true
		dir("/sys/module/zbud")
	}
	val("/proc/sys/vm/swappiness", "60")
	val("/proc/sys/vm/page-cluster", "3")
	val("/proc/sys/vm/watermark_scale_factor", "10")
	val("/proc/sys/vm/vfs_cache_pressure", "100")
	val(osReleasePath, Release)
	w.Files[cryptoPath] = &File{Data: "name         : lzo\ndriver       : lzo-generic\n\nname         : deflate\n", Mode: 0o444}
	w.Files["/lib/modules/"+Release+"/modules.builtin"] = &File{Data: "kernel/mm/zsmalloc.ko\nkernel/crypto/lzo.ko\n"}
	w.Files["/lib/modules/"+Release+"/kernel/crypto/lz4.ko.zst"] = &File{Data: "\x28\xb5"}
	w.Files["/lib/modules/"+Release+"/kernel/crypto/zstd.ko.zst"] = &File{Data: "\x28\xb5"}
	w.Compressors = map[string]bool{"lzo": true, "lz4": true, "zstd": true}
	// Ubuntu: /etc/sysctl.d/99-sysctl.conf → ../sysctl.conf
	w.Files["/etc/sysctl.conf"] = &File{Data: "# /etc/sysctl.conf\n", Mode: 0o644}
	w.Files["/etc/sysctl.d/99-sysctl.conf"] = &File{Data: "# /etc/sysctl.conf\n", Mode: 0o644}
	w.Links["/etc/sysctl.d/99-sysctl.conf"] = "/etc/sysctl.conf"
	w.Files["/etc/sysctl.d/10-console-messages.conf"] = &File{Data: "kernel.printk = 4 4 1 7\n", Mode: 0o644}
	return w
}

// System — система поверх фейка.
func (w *World) System() sys.System {
	return sys.System{
		FS: &FS{w}, Exec: &Exec{w}, Kernel: &Kernel{w},
		DescribeFS: w.DescribeFS,
		ListFS: func() []detect.FSInfo {
			out := []detect.FSInfo{w.DescribeFS("/")}
			mps := make([]string, 0, len(w.Mounts))
			for mp := range w.Mounts {
				mps = append(mps, mp)
			}
			sort.Strings(mps)
			for _, mp := range mps {
				out = append(out, w.DescribeFS(mp))
			}
			return out
		},
		Now: w.Now,
	}
}

// Now — часы фейка: детерминированы числом изменяющих вызовов.
func (w *World) Now() time.Time {
	if w.FrozenClock {
		return time.Date(2026, 11, 17, 11, 33, 8, 0, time.UTC) // все операции — в одну секунду
	}
	return time.Date(2026, 11, 16, 12, 0, w.Mutations%60, 0, time.UTC)
}

func (w *World) mount(path string) (string, Mount) {
	best, m := "/", Mount{FSType: w.FSType, Transport: w.Transport, Total: w.Total, Avail: w.Avail}
	for mp, fm := range w.Mounts {
		if (path == mp || strings.HasPrefix(path, mp+"/")) && len(mp) > len(best) {
			best, m = mp, fm
		}
	}
	return best, m
}

// FSOf — тип ФС пути.
func (w *World) FSOf(path string) string { _, m := w.mount(path); return m.FSType }

// DescribeFS — описание ФС пути.
func (w *World) DescribeFS(path string) detect.FSInfo {
	mp, m := w.mount(path)
	kind := m.Kind
	if kind == "" {
		kind = detect.KindNVMe
		if m.Transport == "usb" {
			kind = detect.KindHDD
		}
	}
	total, avail := m.Total, m.Avail
	fi := detect.FSInfo{
		MountPoint: mp, Source: "/dev/fake" + strings.ReplaceAll(mp, "/", "_"), FSType: m.FSType,
		ReadOnly: m.ReadOnly, Network: m.Network, Kind: kind,
		Disks:      []detect.BlockDevice{{Name: "sda", Kind: kind, Transport: m.Transport}},
		TotalBytes: &total, AvailableBytes: &avail,
	}
	if m.Network {
		fi.Disks, fi.Kind = []detect.BlockDevice{}, detect.KindUnknown
	}
	return fi
}

func (w *World) mutate(op string) error {
	if w.OnMutation != nil {
		w.OnMutation(op)
	}
	w.Mutations++
	w.Log = append(w.Log, op)
	if w.FailAt != 0 && w.Mutations == w.FailAt {
		if w.Crash {
			panic(Crashed{op})
		}
		return fmt.Errorf("injected failure #%d at %s", w.Mutations, op)
	}
	return nil
}

func notExist(op, path string) error { return &fs.PathError{Op: op, Path: path, Err: fs.ErrNotExist} }

// HasKey — есть ли ключ в карте.
func HasKey[V any](m map[string]V, k string) bool { _, ok := m[k]; return ok }

// Snapshot — состояние системы без каталога RamPlus (журнал и бэкапы
// меняются ожидаемо).
type Snapshot struct {
	Files   map[string]File
	Active  map[string]int
	Enabled map[string]bool
}

// Snap — снимок состояния.
func (w *World) Snap() Snapshot {
	s := Snapshot{Files: map[string]File{}, Active: map[string]int{}, Enabled: map[string]bool{}}
	for p, f := range w.Files {
		if p == stateDir || strings.HasPrefix(p, stateDir+"/") {
			continue
		}
		c := *f
		if !c.Dir && c.Data != "" {
			c.Size = uint64(len(c.Data)) // размер текстовых файлов = длина содержимого
		}
		s.Files[p] = c
	}
	for k, v := range w.Active {
		s.Active[k] = v
	}
	for k, v := range w.Enabled {
		s.Enabled[k] = v
	}
	return s
}

// Diff — различия снимков ("" — совпадают).
func (s Snapshot) Diff(o Snapshot) string {
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

// Clone — независимая копия.
func (w *World) Clone() *World {
	c := *w
	c.Files, c.Links, c.Active, c.Used, c.Enabled, c.Mounts =
		map[string]*File{}, map[string]string{}, map[string]int{}, map[string]uint64{}, map[string]bool{}, map[string]Mount{}
	for k, v := range w.Files {
		f := *v
		c.Files[k] = &f
	}
	for k, v := range w.Links {
		c.Links[k] = v
	}
	for k, v := range w.Active {
		c.Active[k] = v
	}
	for k, v := range w.Used {
		c.Used[k] = v
	}
	for k, v := range w.Enabled {
		c.Enabled[k] = v
	}
	for k, v := range w.Mounts {
		c.Mounts[k] = v
	}
	c.Log = nil
	return &c
}

// --- FS ---

// FS — файловая система фейка.
type FS struct{ w *World }

type info struct {
	name string
	f    File
}

func (i info) Name() string { return i.name }
func (i info) Size() int64  { return int64(i.f.Size) }
func (i info) Mode() fs.FileMode {
	if i.f.Dir {
		return fs.ModeDir | i.f.Mode
	}
	return i.f.Mode
}
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.f.Dir }
func (i info) Sys() any           { return nil }

func (f *FS) ReadFile(name string) ([]byte, error) {
	w := f.w
	switch name {
	case swapsPath:
		var b bytes.Buffer
		b.WriteString("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n")
		paths := make([]string, 0, len(w.Active))
		for p := range w.Active {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			typ := "file"
			if strings.HasPrefix(p, "/dev/") {
				typ = "partition"
			}
			fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%d\n", p, typ, w.Files[p].Size/KiB, w.Used[p]/KiB, w.Active[p])
		}
		return b.Bytes(), nil
	case meminfoPath:
		total := w.MemTotal
		if total == 0 {
			total = 16 * GiB
		}
		return []byte(fmt.Sprintf("MemTotal: %d kB\nMemFree: %d kB\nMemAvailable: %d kB\n",
			total/KiB, w.MemAvail/KiB, w.MemAvail/KiB)), nil
	case cmdlinePath:
		return []byte(w.Cmdline + "\n"), nil
	}
	file, ok := w.Files[name]
	if !ok {
		return nil, notExist("open", name)
	}
	if file.Dir {
		return nil, &fs.PathError{Op: "read", Path: name, Err: syscall.EISDIR}
	}
	return []byte(file.Data), nil
}

func (f *FS) WriteFileAtomic(name string, data []byte, perm fs.FileMode) error {
	if err := f.w.mutate("write " + name); err != nil {
		return err
	}
	if p, ok := f.w.Files[filepath.Dir(name)]; !ok || !p.Dir {
		return notExist("write", name)
	}
	f.w.Files[name] = &File{Data: string(data), Size: uint64(len(data)), Mode: perm}
	return nil
}

func (f *FS) Stat(name string) (fs.FileInfo, error) {
	file, ok := f.w.Files[name]
	if !ok {
		return nil, notExist("stat", name)
	}
	return info{filepath.Base(name), *file}, nil
}

func (f *FS) WriteValue(name, value string) error {
	if err := f.w.mutate("write-value " + name); err != nil {
		return err
	}
	file, ok := f.w.Files[name]
	if !ok || file.Dir {
		return notExist("open", name)
	}
	v := strings.TrimSpace(value)
	switch name {
	case zswapParamDir + "/compressor":
		if !f.w.Compressors[v] {
			return &fs.PathError{Op: "write", Path: name, Err: syscall.EINVAL}
		}
	case zswapParamDir + "/zpool":
		if !f.w.Zpools[v] {
			return &fs.PathError{Op: "write", Path: name, Err: syscall.EINVAL}
		}
	}
	file.Data = v + "\n"
	return nil
}

func (f *FS) ReadPrefix(name string, n int) ([]byte, error) {
	file, ok := f.w.Files[name]
	if !ok {
		return nil, notExist("open", name)
	}
	size := int(min(uint64(n), file.Size))
	buf := make([]byte, size)
	switch {
	case file.Data != "":
		buf = []byte(file.Data)[:min(len(file.Data), n)]
	case file.Swap && size >= 10:
		copy(buf[size-10:], "SWAPSPACE2") // как после mkswap
	}
	return buf, nil
}

func (f *FS) Glob(pattern string) ([]string, error) {
	var out []string
	for p := range f.w.Files {
		if ok, err := filepath.Match(pattern, p); err != nil {
			return nil, err
		} else if ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *FS) EvalSymlinks(path string) (string, error) {
	if t, ok := f.w.Links[path]; ok {
		return t, nil
	}
	if _, ok := f.w.Files[path]; ok {
		return path, nil
	}
	return "", notExist("lstat", path)
}

func (f *FS) hasChildren(dir string) bool {
	for p := range f.w.Files {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func (f *FS) Remove(name string) error {
	if err := f.w.mutate("remove " + name); err != nil {
		return err
	}
	file, ok := f.w.Files[name]
	switch {
	case !ok:
		return notExist("remove", name)
	case file.Dir && f.hasChildren(name):
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.ENOTEMPTY}
	case HasKey(f.w.Active, name):
		// Строже реального ядра: ловим удаление активного swap.
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.EBUSY}
	}
	delete(f.w.Files, name)
	return nil
}

func (f *FS) Mkdir(name string, perm fs.FileMode) error {
	if err := f.w.mutate("mkdir " + name); err != nil {
		return err
	}
	if _, ok := f.w.Files[name]; ok {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	f.w.Files[name] = &File{Dir: true, Mode: perm}
	return nil
}

func (f *FS) MkdirAll(name string, perm fs.FileMode) error {
	if err := f.w.mutate("mkdirall " + name); err != nil {
		return err
	}
	for p := name; p != "/" && p != "."; p = filepath.Dir(p) {
		if _, ok := f.w.Files[p]; !ok {
			f.w.Files[p] = &File{Dir: true, Mode: perm}
		}
	}
	return nil
}

func (f *FS) Chmod(name string, perm fs.FileMode) error {
	if err := f.w.mutate("chmod " + name); err != nil {
		return err
	}
	file, ok := f.w.Files[name]
	if !ok {
		return notExist("chmod", name)
	}
	file.Mode = perm
	return nil
}

func (f *FS) Chown(name string, uid, gid int) error {
	if err := f.w.mutate("chown " + name); err != nil {
		return err
	}
	file, ok := f.w.Files[name]
	if !ok {
		return notExist("chown", name)
	}
	if uid >= 0 {
		file.UID = uid
	}
	if gid >= 0 {
		file.GID = gid
	}
	return nil
}

func (f *FS) Create(name string, perm fs.FileMode) error {
	if err := f.w.mutate("create " + name); err != nil {
		return err
	}
	if _, ok := f.w.Files[name]; ok {
		return &fs.PathError{Op: "create", Path: name, Err: fs.ErrExist}
	}
	parent, ok := f.w.Files[filepath.Dir(name)]
	if !ok || !parent.Dir {
		return notExist("create", name)
	}
	f.w.Files[name] = &File{Mode: perm, NoCOW: parent.NoCOW}
	return nil
}

func (f *FS) Fallocate(name string, size uint64) error {
	if err := f.w.mutate("fallocate " + name); err != nil {
		return err
	}
	file, ok := f.w.Files[name]
	if !ok {
		return notExist("fallocate", name)
	}
	if f.w.FallocUnsupported {
		return fmt.Errorf("%s: %w", name, sys.ErrFallocateUnsupported)
	}
	file.Size = size
	return nil
}

func (f *FS) WriteZeros(name string, size uint64) error {
	if err := f.w.mutate("zeros " + name); err != nil {
		return err
	}
	file, ok := f.w.Files[name]
	if !ok {
		return notExist("zeros", name)
	}
	file.Size = size
	return nil
}

func (f *FS) Writable(path string) error {
	for _, ro := range f.w.ReadOnly {
		if path == ro || strings.HasPrefix(path, ro+"/") {
			return &fs.PathError{Op: "access", Path: path, Err: syscall.EROFS}
		}
	}
	if _, ok := f.w.Files[path]; !ok && path != "/" {
		return notExist("access", path)
	}
	return nil
}

func (f *FS) Lock(name string) (func() error, error) {
	lockMu.Lock()
	defer lockMu.Unlock()
	if f.w.Locked {
		return nil, i18n.Errorf("sys.locked_path", sys.ErrLocked, name)
	}
	f.w.Locked = true
	return func() error {
		lockMu.Lock()
		defer lockMu.Unlock()
		f.w.Locked = false
		return nil
	}, nil
}

// --- Executor ---

// Exec — внешние команды фейка.
type Exec struct{ w *World }

func (e *Exec) LookPath(file string) (string, error) {
	if file == "btrfs" && !e.w.BtrfsTool {
		return "", notExist("lookpath", file)
	}
	return "/usr/bin/" + file, nil
}

func (e *Exec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	w := e.w
	cmd := name + " " + strings.Join(args, " ")
	if strings.HasPrefix(cmd, "systemctl is-enabled ") { // только чтение
		if w.Enabled[args[len(args)-1]] {
			return []byte("enabled\n"), nil
		}
		return []byte("disabled\n"), fmt.Errorf("exit status 1")
	}
	if cmd == "btrfs filesystem mkswapfile --help" { // только чтение
		if w.BtrfsTool && w.Mkswapfile {
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
		f, ok := w.Files[last]
		if !ok || f.Dir {
			return nil, notExist("mkswap", last)
		}
		f.Swap = true
	case cmd == "systemctl daemon-reload":
	case name == "systemctl" && args[0] == "enable":
		if !HasKey(w.Files, filepath.Join(unitDir, last)) {
			return nil, fmt.Errorf("Unit %s does not exist", last)
		}
		w.Enabled[last] = true
	case name == "systemctl" && args[0] == "disable":
		delete(w.Enabled, last)
	case strings.HasPrefix(cmd, "btrfs subvolume create "):
		if HasKey(w.Files, last) {
			return nil, fmt.Errorf("target path already exists: %s", last)
		}
		w.Files[last] = &File{Dir: true, Mode: 0o755}
	case strings.HasPrefix(cmd, "btrfs subvolume delete "):
		f, ok := w.Files[last]
		if !ok || !f.Dir || (&FS{w}).hasChildren(last) {
			return nil, fmt.Errorf("cannot delete subvolume %s", last)
		}
		delete(w.Files, last)
	case strings.HasPrefix(cmd, "btrfs filesystem mkswapfile --size "):
		size, _ := strconv.ParseUint(args[3], 10, 64)
		if HasKey(w.Files, last) {
			return nil, fmt.Errorf("file exists: %s", last)
		}
		w.Files[last] = &File{Size: size, Mode: 0o600, Swap: true, NoCOW: true}
	case cmd == "chattr +C "+last:
		f, ok := w.Files[last]
		switch {
		case !ok:
			return nil, notExist("chattr", last)
		case !f.Dir && f.Size > 0:
			return nil, fmt.Errorf("chattr: +C has no effect on a non-empty file: %s", last)
		}
		f.NoCOW = true
	default:
		return nil, fmt.Errorf("fakeExec: unexpected command %q", cmd)
	}
	return nil, nil
}

// --- Kernel ---

// Kernel — системные вызовы фейка.
type Kernel struct{ w *World }

func (k *Kernel) Geteuid() int { return k.w.EUID }

func (k *Kernel) Swapon(path string, prio int) error {
	w := k.w
	if err := w.mutate("swapon " + path); err != nil {
		return err
	}
	f, ok := w.Files[path]
	switch {
	case !ok:
		return syscall.ENOENT
	case !f.Swap:
		return syscall.EINVAL // нет сигнатуры mkswap
	case f.Mode&0o077 != 0:
		return fmt.Errorf("insecure permissions %o", f.Mode)
	case w.FSOf(path) == "btrfs" && !f.NoCOW:
		return syscall.EINVAL // btrfs отвергает COW-swapfile
	case HasKey(w.Active, path):
		return syscall.EBUSY
	}
	w.Active[path] = prio
	return nil
}

func (k *Kernel) Swapoff(path string) error {
	w := k.w
	if err := w.mutate("swapoff " + path); err != nil {
		return err
	}
	if !HasKey(w.Active, path) {
		return syscall.EINVAL
	}
	delete(w.Active, path)
	delete(w.Used, path)
	return nil
}
