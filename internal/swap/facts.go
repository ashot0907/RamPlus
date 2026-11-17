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

// Target — куда ляжет swapfile.
type Target struct {
	Path      string // абсолютный путь (каталог с разрешёнными симлинками)
	Requested string // как указал пользователь ("" — по умолчанию)
	Defaulted bool
	ParentErr error // каталог не существует или недоступен
	FS        detect.FSInfo
}

// Recovery — незавершённая операция, найденная в журнале.
type Recovery struct {
	OpID    string
	Pending *Applied // запись pending (процесс упал посреди шага), может быть nil
	Verdict Verdict
	Detail  string
}

// Facts — всё, что планировщик знает о системе. Собирается Gather (только
// чтение); в тестах заполняется вручную.
type Facts struct {
	EUID int

	Target     Target
	RootFS     detect.FSInfo
	Candidates []detect.FSInfo

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
	Recovery *Recovery

	Systemd       bool
	HasBtrfs      bool
	HasMkswapfile bool
	HasChattr     bool

	// Exists — какие из интересующих путей уже существуют.
	Exists map[string]bool
	// Units — юниты RamPlus в UnitDir (*ramplus*.swap).
	Units []string
	// Observed — что сейчас лежит по путям из журнала отменяемой операции и
	// по путям активных swap-файлов.
	Observed map[string]ObservedFile
}

// ObservedFile — фактическое состояние пути.
type ObservedFile struct {
	Exists     bool
	IsDir      bool
	Size       uint64
	Content    string // для небольших конфигов (юнит)
	HasContent bool
}

// DefaultPathFor — путь swapfile по умолчанию для ФС корня.
func DefaultPathFor(rootFSType string) string {
	if rootFSType == "btrfs" {
		return BtrfsPath
	}
	return ManagedPath
}

// SuggestedPath — путь swapfile на точке монтирования.
func SuggestedPath(fi detect.FSInfo) string {
	if fi.MountPoint == "/" {
		return DefaultPathFor(fi.FSType)
	}
	return filepath.Join(fi.MountPoint, DefaultFileName)
}

// Gather собирает факты. Ничего не меняет и не требует root. requested —
// значение --path ("" — путь по умолчанию или путь уже созданного swap).
func Gather(ctx context.Context, sys System, requested string) Facts {
	f := Facts{EUID: sys.Kernel.Geteuid(), FstabResolved: map[string]string{}, Exists: map[string]bool{}}

	if b, err := sys.FS.ReadFile(StatePath); err == nil {
		f.State, f.StateErr = ParseState(b)
	} else if !errors.Is(err, fs.ErrNotExist) {
		f.StateErr = err
	}

	f.RootFS = sys.DescribeFS("/")
	f.Target = resolveTarget(sys, requested, f.State.Managed, f.RootFS)
	f.Candidates = sys.ListFS()

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

	_, err := sys.FS.Stat(SystemdRunDir)
	f.Systemd = err == nil

	if f.Target.FS.FSType == "btrfs" {
		if _, err := sys.Exec.LookPath("btrfs"); err == nil {
			f.HasBtrfs = true
			// btrfs-progs >= 6.1; старые версии отвечают ошибкой на неизвестную команду.
			_, err := sys.Exec.Run(ctx, "btrfs", "filesystem", "mkswapfile", "--help")
			f.HasMkswapfile = err == nil
		}
		_, err := sys.Exec.LookPath("chattr")
		f.HasChattr = err == nil
	}

	check := []string{f.Target.Path, filepath.Join(UnitDir, UnitName(f.Target.Path)),
		ManagedPath, BtrfsPath, BtrfsDir, LegacyPath}
	if m := f.State.Managed; m != nil {
		check = append(check, m.Path, filepath.Join(UnitDir, m.Unit))
	}
	for _, p := range check {
		if _, err := sys.FS.Stat(p); err == nil {
			f.Exists[p] = true
		}
	}

	f.Units, _ = sys.FS.Glob(filepath.Join(UnitDir, "*ramplus*.swap"))

	f.Observed = map[string]ObservedFile{}
	observe := func(path string, content bool) {
		if path == "" || path == FstabPath {
			return
		}
		st, err := sys.FS.Stat(path)
		if err != nil {
			f.Observed[path] = ObservedFile{}
			return
		}
		o := ObservedFile{Exists: true, IsDir: st.IsDir(), Size: uint64(st.Size())}
		if content && st.Mode().IsRegular() && st.Size() <= legacyConfigMax {
			if b, err := sys.FS.ReadFile(path); err == nil {
				o.Content, o.HasContent = string(b), true
			}
		}
		f.Observed[path] = o
	}
	for _, sw := range f.Swaps {
		if sw.Type == "file" {
			observe(sw.Path, false)
		}
	}
	if idx, ok := f.State.LastUndoable(); ok {
		for _, a := range f.State.Operations[idx].Applied {
			k := a.Step.Kind
			observe(a.Step.Path, k == KindWriteFile || k == KindDeleteConfig || k == KindRemoveFile)
		}
	}

	if op := f.State.Pending(); op != nil {
		rec := &Recovery{OpID: op.ID}
		if n := len(op.Applied); n > 0 && op.Applied[n-1].Status == StepPending {
			a := op.Applied[n-1]
			rec.Pending = &a
			rec.Verdict, rec.Detail = VerifyPending(sys, a)
		}
		f.Recovery = rec
	}
	return f
}

func resolveTarget(sys System, requested string, managed *Managed, root detect.FSInfo) Target {
	t := Target{Requested: requested}
	path := requested
	switch {
	case path != "":
	case managed != nil:
		path = managed.Path
	default:
		path = DefaultPathFor(root.FSType)
		t.Defaulted = true
	}
	path = filepath.Clean(path)
	if filepath.IsAbs(path) {
		// Каталог разрешаем: в юнит и журнал попадает реальный путь.
		dir := filepath.Dir(path)
		if path == BtrfsPath && t.Defaulted {
			dir = "/" // каталог /ramplus-swap создаст RamPlus
		} else if real, err := sys.FS.EvalSymlinks(dir); err != nil {
			t.ParentErr = err
		} else if st, err := sys.FS.Stat(real); err != nil || !st.IsDir() {
			t.ParentErr = &fs.PathError{Op: "stat", Path: real, Err: errors.New("не каталог")}
		} else if path != BtrfsPath {
			path = filepath.Join(real, filepath.Base(path))
		}
	}
	t.Path = path
	t.FS = sys.DescribeFS(path)
	return t
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

// isOurPath — путь swap RamPlus: из журнала или один из путей по умолчанию.
func (f Facts) isOurPath(path string) bool {
	if m := f.State.Managed; m != nil && m.Path == path {
		return true
	}
	return path == ManagedPath || path == BtrfsPath || path == LegacyPath || strings.HasPrefix(path, BtrfsDir+"/")
}

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
