// Package swap управляет swapfile RamPlus: факты, планировщик (apply,
// resize, remove и rollback со сверкой swap/места/памяти) и шаги swap,
// которые выполняет журнал (internal/journal).
package swap

import (
	"github.com/ashot0907/ramplus/internal/journal"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// Общие пути — internal/sys и internal/journal; здесь — только swap.
const (
	UnitDir     = system.UnitDir
	FstabPath   = system.FstabPath
	CmdlinePath = system.CmdlinePath
	MeminfoPath = system.MeminfoPath
	SwapsPath   = system.SwapsPath
	StatePath   = journal.StatePath
	BackupDir   = journal.BackupDir
)

// Пути, которые использует RamPlus.
const (
	DefaultFileName = "ramplus.swapfile"
	ManagedPath     = "/" + DefaultFileName // по умолчанию на ext4/xfs
	BtrfsDir        = "/ramplus-swap"       // NOCOW-подтом на btrfs (путь по умолчанию)
	BtrfsPath       = BtrfsDir + "/" + DefaultFileName
	LegacyPath      = "/ramplus.swap" // путь по умолчанию в state.json v1
	fstabDisabledBy = "# disabled by ramplus: "
	unitHeader      = "# Managed by RamPlus: do not edit, use `ramplus swap`.\n"
)
