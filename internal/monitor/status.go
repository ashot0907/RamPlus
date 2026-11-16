package monitor

import (
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
)

// SchemaVersion — версия JSON-схемы Status. Увеличивается при несовместимых
// изменениях; добавление полей совместимо.
const SchemaVersion = 1

// Status — полный снимок состояния памяти. Эту же структуру будет отдавать
// веб-API (GET /api/status), поэтому JSON-имена — часть контракта.
//
// Соглашения: размеры в байтах (*_bytes), null — значение недоступно ("n/a"),
// причина — в соседнем поле error/*_error ({reason, detail}).
type Status struct {
	SchemaVersion int             `json:"schema_version"`
	CollectedAt   time.Time       `json:"collected_at"`
	System        detect.System   `json:"system"`
	Memory        Memory          `json:"memory"`
	Swaps         Swaps           `json:"swaps"`
	Zswap         Zswap           `json:"zswap"`
	VM            VM              `json:"vm"`
	PSI           PSI             `json:"psi"`
	RootDisk      detect.RootDisk `json:"root_disk"`
	Zram          Zram            `json:"zram"`
}

// Paths — откуда читать источники. Подменяется в тестах.
type Paths struct {
	Meminfo     string
	Swaps       string
	ZswapParams string
	ZswapDebug  string
	VMSysctl    string
	PSIMemory   string
	SysBlock    string
}

// DefaultPaths — реальные пути в системе.
var DefaultPaths = Paths{
	Meminfo:     "/proc/meminfo",
	Swaps:       "/proc/swaps",
	ZswapParams: "/sys/module/zswap/parameters",
	ZswapDebug:  "/sys/kernel/debug/zswap",
	VMSysctl:    "/proc/sys/vm",
	PSIMemory:   "/proc/pressure/memory",
	SysBlock:    "/sys/block",
}

// Collect собирает Status из реальной системы. Только чтение, без root.
func Collect() Status {
	s := CollectFrom(DefaultPaths)
	s.System = detect.ReadSystem()
	s.RootDisk = detect.ReadRootDisk()
	return s
}

// CollectFrom собирает часть Status, относящуюся к памяти, из заданных путей.
func CollectFrom(p Paths) Status {
	mem, mi := ReadMemory(p.Meminfo)
	return Status{
		SchemaVersion: SchemaVersion,
		CollectedAt:   time.Now().UTC(),
		Memory:        mem,
		Swaps:         ReadSwaps(p.Swaps),
		Zswap:         ReadZswap(p.ZswapParams, p.ZswapDebug, mi),
		VM:            ReadVM(p.VMSysctl),
		PSI:           ReadPSI(p.PSIMemory),
		Zram:          ReadZram(p.SysBlock),
		RootDisk:      detect.RootDisk{Kind: detect.KindUnknown, Disks: []detect.BlockDevice{}},
	}
}
