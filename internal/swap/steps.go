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
	// KindRemoveFile — только из журналов v1 (отмена создания файла).
	// Выполняется обратимо: маленький файл восстанавливается по содержимому,
	// большой (swapfile) — пересозданием.
	KindRemoveFile StepKind = "remove_file"
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
	// NoCOW — swapfile на btrfs: перед выделением места файлу ставится
	// chattr +C (иначе ядро откажет в swapon).
	NoCOW bool `json:"nocow,omitempty"`
	// Reload — после отмены шага нужен systemctl daemon-reload (шаг меняет
	// конфигурацию systemd: юнит или fstab).
	Reload bool `json:"reload,omitempty"`
}

func daemonReload() Step { return Step{Kind: KindSystemctl, Args: []string{"daemon-reload"}} }

// Describe — описание шага для человека.
func (s Step) Describe() string {
	switch s.Kind {
	case KindAllocate:
		nocow := ""
		if s.NoCOW {
			nocow = ", chattr +C (NOCOW)"
		}
		return fmt.Sprintf("создать %s, %s (fallocate; если ФС не умеет — запись нулями), права 600%s", s.Path, FormatSize(s.Size), nocow)
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
	case KindDeleteSwapfile:
		return fmt.Sprintf("удалить файл %s (%s)", s.Path, FormatSize(s.Size))
	case KindWriteFile:
		return "записать " + s.Path
	case KindDeleteConfig, KindRemoveFile:
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

// Статусы записи журнала (write-ahead).
const (
	StepPending = "pending" // записано до выполнения шага
	StepDone    = "done"    // шаг выполнен
)

// Applied — запись журнала о шаге и о том, как его отменить. Undo
// вычисляется ДО выполнения (например, сохраняется прежнее содержимое
// файла) и пишется вместе со статусом pending: если процесс упадёт посреди
// шага, rollback знает, что проверять и как откатывать.
type Applied struct {
	Step   Step   `json:"step"`
	Status string `json:"status"`
	Undo   []Step `json:"undo"`
	Backup string `json:"backup,omitempty"` // копия файла до изменения
}

// UndoSteps — шаги отмены выполненных (done) шагов, в обратном порядке.
func UndoSteps(applied []Applied) []Step {
	var out []Step
	for i := len(applied) - 1; i >= 0; i-- {
		if applied[i].Status == StepDone {
			out = append(out, applied[i].Undo...)
		}
	}
	return out
}
