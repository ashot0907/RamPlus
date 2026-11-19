package swap

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ashot0907/ramplus/internal/journal"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// Orphan — след swap RamPlus без записи в журнале: swapfile по пути
// RamPlus и/или юнит RamPlus (журнал удалён вручную, потерян, или swap
// создан и брошен).
type Orphan struct {
	Path     string `json:"path,omitempty"`      // swapfile ("" — только юнит без What=)
	Unit     string `json:"unit,omitempty"`      // имя юнита ("" — юнита нет)
	Exists   bool   `json:"exists"`              // файл Path существует
	Regular  bool   `json:"regular"`             // обычный файл
	Size     uint64 `json:"size_bytes"`          // размер файла
	Swap     bool   `json:"swap_signature"`      // сигнатура SWAPSPACE2
	Active   bool   `json:"active"`              // swap включён
	Priority int    `json:"priority"`            // если активен
	Marker   bool   `json:"unit_marker"`         // в юните «# Managed by RamPlus»
	Problem  string `json:"problem,omitempty"`   // почему это нельзя убрать автоматически
	FSType   string `json:"fs_type,omitempty"`   // ФС под Path
	Subvol   string `json:"btrfs_dir,omitempty"` // подтом RamPlus (/ramplus-swap) с этим файлом
}

// Label — описание для человека.
func (o Orphan) Label() string {
	var parts []string
	if o.Path != "" {
		parts = append(parts, o.Path)
	}
	if o.Unit != "" {
		parts = append(parts, "юнит "+o.Unit)
	}
	return strings.Join(parts, ", ")
}

// unitWhat — значение What= из юнита.
func unitWhat(content string) string {
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "What="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// findOrphans — следы RamPlus, которых нет в журнале. Только чтение.
// Кандидаты: юниты *ramplus*.swap в UnitDir и пути RamPlus по умолчанию;
// путь из юнита (What=) — только если в юните есть маркер RamPlus.
func (f *Facts) findOrphans(sys system.System) {
	byPath := map[string]*Orphan{}
	var list []*Orphan
	add := func(path string) *Orphan {
		if o, ok := byPath[path]; ok && path != "" {
			return o
		}
		o := &Orphan{Path: path}
		if path != "" {
			byPath[path] = o
		}
		list = append(list, o)
		return o
	}
	var managedUnit string
	if m := f.Managed; m != nil {
		managedUnit = m.Unit
	}
	for _, u := range f.Units {
		name := filepath.Base(u)
		if name == managedUnit {
			continue
		}
		b, err := sys.FS.ReadFile(u)
		content := string(b)
		marker := err == nil && strings.Contains(content, journal.Marker)
		what := ""
		if marker {
			what = unitWhat(content)
		}
		if f.Managed != nil && what == f.Managed.Path {
			continue
		}
		o := add(what)
		o.Unit, o.Marker = name, marker
		switch {
		case err != nil:
			o.Problem = fmt.Sprintf("юнит %s не прочитать: %v", u, err)
		case !marker:
			o.Problem = fmt.Sprintf("в юните %s нет маркера RamPlus (%q)", u, journal.Marker)
		case what == "":
			o.Problem = fmt.Sprintf("в юните %s нет What=", u)
		}
	}
	for _, p := range []string{ManagedPath, BtrfsPath, LegacyPath} {
		if f.Managed != nil && f.Managed.Path == p {
			continue
		}
		_, active := f.swapByPath(p)
		if _, err := sys.FS.Stat(p); err == nil || active {
			add(p)
		}
	}
	for _, o := range list {
		if o.Path == "" {
			continue
		}
		if s, ok := f.swapByPath(o.Path); ok {
			o.Active, o.Priority = true, s.Priority
		}
		st, err := sys.FS.Stat(o.Path)
		if err == nil {
			o.Exists, o.Regular, o.Size = true, st.Mode().IsRegular(), uint64(st.Size())
			if o.Regular {
				if head, err := sys.FS.ReadPrefix(o.Path, os.Getpagesize()); err == nil {
					o.Swap = IsSwapHeader(head)
				}
			}
			o.FSType = sys.DescribeFS(o.Path).FSType
		}
		if o.Path == BtrfsPath {
			if _, err := sys.FS.Stat(BtrfsDir); err == nil {
				o.Subvol = BtrfsDir
			}
		}
		if o.Problem != "" {
			continue
		}
		switch {
		case !filepath.IsAbs(o.Path):
			o.Problem = fmt.Sprintf("путь %q не абсолютный", o.Path)
		case o.Exists && !o.Regular:
			o.Problem = o.Path + ": не обычный файл"
		case o.Exists && !o.Swap:
			o.Problem = o.Path + ": нет сигнатуры swap (SWAPSPACE2) — это не swapfile"
		}
	}
	f.Orphans = f.Orphans[:0]
	for _, o := range list {
		if o.Exists || o.Active || o.Unit != "" {
			f.Orphans = append(f.Orphans, *o)
		}
	}
	sort.Slice(f.Orphans, func(i, j int) bool { return f.Orphans[i].Label() < f.Orphans[j].Label() })
}

// planOrphan — ramplus swap remove --orphan: убрать следы RamPlus без
// записи в журнале. Каждый след проверяется по маркерам; что-то не так —
// план блокируется, ничего не трогается.
func (p *Plan) planOrphan(f Facts) {
	p.ManagedAfter = f.Managed // запись в журнале (если есть) не трогаем
	if len(f.Orphans) == 0 {
		p.add(LevelFail, "orphan.none", "следы", "следов RamPlus без записи в журнале нет — убирать нечего")
		return
	}
	for _, o := range f.Orphans {
		if o.Problem != "" {
			p.add(LevelFail, "orphan.not_ours", "следы", "%s: %s — RamPlus это не трогает, разберитесь вручную", o.Label(), o.Problem)
			continue
		}
		var facts []string
		if o.Active {
			facts = append(facts, fmt.Sprintf("активен, приоритет %d", o.Priority))
		}
		if o.Exists {
			facts = append(facts, FormatSize(o.Size), "сигнатура SWAPSPACE2")
		}
		if o.Marker {
			facts = append(facts, "юнит с маркером RamPlus")
		}
		p.add(LevelInfo, "orphan.found", "следы", "%s (%s) — нет в журнале %s", o.Label(), strings.Join(facts, ", "), journal.StatePath)
	}
	if p.Blocked() {
		return
	}
	p.checkResume(f, "удаление")
	for _, o := range f.Orphans {
		if o.Active {
			p.Steps = append(p.Steps, Step{Kind: KindSwapoff, Path: o.Path, Priority: o.Priority, Size: o.Size})
		}
		if o.Unit != "" {
			p.Steps = append(p.Steps,
				Step{Kind: KindSystemctl, Args: []string{"disable", o.Unit}},
				Step{Kind: KindDeleteConfig, Path: filepath.Join(UnitDir, o.Unit), Reload: true},
				daemonReload(),
			)
		}
		if o.Exists {
			p.Steps = append(p.Steps, Step{Kind: KindDeleteSwapfile, Path: o.Path, Size: o.Size, NoCOW: o.FSType == "btrfs"})
		}
		if o.Subvol != "" {
			p.Steps = append(p.Steps, Step{Kind: KindBtrfsSubvolDelete, Path: o.Subvol})
		}
	}
	remaining := 0
	for _, s := range f.Swaps {
		orphan := false
		for _, o := range f.Orphans {
			orphan = orphan || (o.Active && o.Path == s.Path)
		}
		if !orphan {
			remaining++
		}
	}
	if remaining == 0 {
		p.add(LevelWarn, "swap.none_left", "swap", "после удаления в системе не останется swap")
	}
}

// isOrphan — путь (swapfile или юнит) — след RamPlus без записи в журнале,
// который можно убрать remove --orphan.
func (f Facts) isOrphan(path string) bool {
	for _, o := range f.Orphans {
		if o.Problem == "" && (o.Path == path || (o.Unit != "" && filepath.Join(UnitDir, o.Unit) == path) || o.Subvol == path) {
			return true
		}
	}
	return false
}
