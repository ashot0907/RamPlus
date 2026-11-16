package swap

import (
	"fmt"
	"strings"
)

// StepKind — тип системного действия.
type StepKind string

const (
	KindAllocate          StepKind = "allocate"            // новый файл Path размером Size: fallocate, иначе нули
	KindBtrfsMkswapfile   StepKind = "btrfs_mkswapfile"    // btrfs filesystem mkswapfile --size Size Path
	KindChmod             StepKind = "chmod"               // chmod Mode Path
	KindMkswap            StepKind = "mkswap"              // mkswap Path
	KindSwapon            StepKind = "swapon"              // swapon Path с Priority
	KindSwapoff           StepKind = "swapoff"             // swapoff Path
	KindRemoveFile        StepKind = "remove_file"         // удалить свой файл (без восстановления)
	KindDeleteSwapfile    StepKind = "delete_swapfile"     // удалить swapfile; откат пересоздаёт его
	KindWriteFile         StepKind = "write_file"          // записать Content в Path
	KindDeleteConfig      StepKind = "delete_config"       // удалить конфиг; откат восстанавливает содержимое
	KindFstabDisable      StepKind = "fstab_disable"       // закомментировать Line в fstab
	KindFstabEnable       StepKind = "fstab_enable"        // раскомментировать Line
	KindSystemctl         StepKind = "systemctl"           // systemctl Args...
	KindMkdir             StepKind = "mkdir"               //
	KindRmdir             StepKind = "rmdir"               //
	KindChattrNoCOW       StepKind = "chattr_nocow"        // chattr +C Path
	KindBtrfsSubvolCreate StepKind = "btrfs_subvol_create" //
	KindBtrfsSubvolDelete StepKind = "btrfs_subvol_delete" //
)

// Step — одно системное действие. Сериализуется в state.json.
type Step struct {
	Kind     StepKind `json:"kind"`
	Path     string   `json:"path,omitempty"`
	Size     uint64   `json:"size,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Mode     uint32   `json:"mode,omitempty"`
	Content  string   `json:"content,omitempty"`
	Line     string   `json:"line,omitempty"`
	Args     []string `json:"args,omitempty"`
	// Reload — после отмены шага нужен systemctl daemon-reload (шаг меняет
	// конфигурацию systemd: юнит или fstab).
	Reload bool `json:"reload,omitempty"`
}

func daemonReload() Step { return Step{Kind: KindSystemctl, Args: []string{"daemon-reload"}} }

// Describe — описание шага для человека.
func (s Step) Describe() string {
	switch s.Kind {
	case KindAllocate:
		return fmt.Sprintf("создать %s, %s (fallocate; если ФС не умеет — запись нулями), права 600", s.Path, FormatSize(s.Size))
	case KindBtrfsMkswapfile:
		return fmt.Sprintf("btrfs filesystem mkswapfile --size %s %s", sizeFlag(s.Size), s.Path)
	case KindChmod:
		return fmt.Sprintf("chmod %o %s", s.Mode, s.Path)
	case KindMkswap:
		return "mkswap " + s.Path
	case KindSwapon:
		if s.Priority < 0 {
			return fmt.Sprintf("swapon %s (приоритет — автоматически)", s.Path)
		}
		return fmt.Sprintf("swapon %s (приоритет %d)", s.Path, s.Priority)
	case KindSwapoff:
		return "swapoff " + s.Path
	case KindRemoveFile:
		return "удалить " + s.Path
	case KindDeleteSwapfile:
		return fmt.Sprintf("удалить файл %s (%s)", s.Path, FormatSize(s.Size))
	case KindWriteFile:
		return "записать " + s.Path
	case KindDeleteConfig:
		return "удалить " + s.Path
	case KindFstabDisable:
		return fmt.Sprintf("закомментировать в %s строку %q (бэкап — в %s)", s.Path, strings.TrimSpace(s.Line), BackupDir)
	case KindFstabEnable:
		return fmt.Sprintf("вернуть в %s строку %q", s.Path, strings.TrimSpace(s.Line))
	case KindSystemctl:
		return "systemctl " + strings.Join(s.Args, " ")
	case KindMkdir:
		return "создать каталог " + s.Path
	case KindRmdir:
		return "удалить каталог " + s.Path
	case KindChattrNoCOW:
		return "chattr +C " + s.Path + " (NOCOW: обязательно для swap на btrfs)"
	case KindBtrfsSubvolCreate:
		return "btrfs subvolume create " + s.Path + " (отдельный подтом: снапшоты корня не ломаются)"
	case KindBtrfsSubvolDelete:
		return "btrfs subvolume delete " + s.Path
	}
	return string(s.Kind) + " " + s.Path
}

// Applied — выполненный шаг и действия, которые его отменяют. Undo
// вычисляется в момент выполнения (например, сохраняет прежнее содержимое
// файла), поэтому rollback работает по записанному, а не по догадкам.
type Applied struct {
	Step   Step   `json:"step"`
	Undo   []Step `json:"undo"`
	Backup string `json:"backup,omitempty"` // копия файла до изменения
}

// UndoSteps — шаги отмены списка выполненных шагов (в обратном порядке).
func UndoSteps(applied []Applied) []Step {
	var out []Step
	for i := len(applied) - 1; i >= 0; i-- {
		out = append(out, applied[i].Undo...)
	}
	return out
}
