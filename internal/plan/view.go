package plan

import "fmt"

const (
	KiB = 1 << 10
	MiB = 1 << 20
	GiB = 1 << 30
	TiB = 1 << 40
)

// FormatSize — человекочитаемый размер в IEC-единицах.
func FormatSize(b uint64) string {
	if b < KiB {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(KiB), 0
	for n := b / KiB; n >= KiB && exp < 4; n /= KiB {
		div *= KiB
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGT"[exp])
}

// SwapView — блок «Сейчас» плана swap (и rollback): что есть в системе.
type SwapView struct {
	Swaps        []SwapRow    `json:"swaps"`
	Target       *FSView      `json:"target_fs,omitempty"`
	MemAvailable *uint64      `json:"mem_available_bytes,omitempty"`
	MemTotal     uint64       `json:"mem_total_bytes"`
	Managed      *ManagedView `json:"managed,omitempty"`
	// ApplyCommand — команда, которой применить план.
	ApplyCommand string `json:"apply_command"`
	// Title — название операции ("создание swapfile").
	Title string `json:"title"`
}

// SwapRow — активный swap.
type SwapRow struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	Size     uint64 `json:"size_bytes"`
	Used     uint64 `json:"used_bytes"`
	Priority int    `json:"priority"`
	Tag      string `json:"tag,omitempty"` // "RamPlus", "в /etc/fstab", "zram — не затрагивается"
}

// FSView — ФС, на которой будет swapfile.
type FSView struct {
	FSType     string  `json:"fs_type"`
	Source     string  `json:"source"`
	MountPoint string  `json:"mount_point"`
	Disk       string  `json:"disk"` // "sda, NVMe, nvme" или "диск n/a"
	Total      *uint64 `json:"total_bytes,omitempty"`
	Avail      *uint64 `json:"available_bytes,omitempty"`
}

// ManagedView — swap RamPlus.
type ManagedView struct {
	Path     string `json:"path"`
	Size     uint64 `json:"size_bytes"`
	Priority int    `json:"priority"`
	Unit     string `json:"unit"`
}

// ProfileView — план профиля.
type ProfileView struct {
	Name         string            `json:"name"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Status       string            `json:"status"`
	BaseStatus   string            `json:"base_status"`
	Evidence     string            `json:"evidence"`
	Overrides    map[string]string `json:"overrides,omitempty"`
	Experimental bool              `json:"experimental"`
	Reset        bool              `json:"reset"`
	Rows         []ProfileRow      `json:"rows"`
	SysctlOrder  string            `json:"sysctl_order,omitempty"`
	// Current — применённый сейчас профиль ("" — нет) и его статус.
	Current       string `json:"current,omitempty"`
	CurrentStatus string `json:"current_status,omitempty"`
	// Тексты статусов для человека.
	StatusText        string `json:"status_text"`
	CurrentStatusText string `json:"current_status_text,omitempty"`
}

// ProfileRow — строка таблицы параметров в плане профиля.
type ProfileRow struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Current string `json:"current"` // "" — нет в ядре
	New     string `json:"new"`
	Source  string `json:"source"` // откуда текущее
	Change  bool   `json:"change"`
	Skip    string `json:"skip,omitempty"` // почему не применяется
	FromSet bool   `json:"from_set,omitempty"`
}
