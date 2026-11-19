// Package sys — интерфейсы доступа к системе (файлы, команды, системные
// вызовы) и их реальные реализации. Вся логика RamPlus работает через
// System, поэтому тестируется на фейке (internal/fakesys).
package sys

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/ashot0907/ramplus/internal/detect"
)

// Системные пути, общие для разных частей RamPlus.
const (
	UnitDir       = "/etc/systemd/system"
	FstabPath     = "/etc/fstab"
	CmdlinePath   = "/proc/cmdline"
	MeminfoPath   = "/proc/meminfo"
	SwapsPath     = "/proc/swaps"
	SystemdRunDir = "/run/systemd/system" // есть, если PID 1 — systemd
)

// ErrFallocateUnsupported — ФС не поддерживает fallocate.
var ErrFallocateUnsupported = errors.New("fallocate not supported")

// ErrLocked — блокировка занята: другой процесс RamPlus (CLI или демон)
// уже выполняет операцию.
var ErrLocked = errors.New("другой экземпляр ramplus уже выполняет операцию")

// FS — операции с файлами. Реализация по умолчанию — OSFS.
type FS interface {
	ReadFile(name string) ([]byte, error)
	// WriteFileAtomic пишет через временный файл + fsync + rename.
	WriteFileAtomic(name string, data []byte, perm fs.FileMode) error
	Stat(name string) (fs.FileInfo, error)
	EvalSymlinks(path string) (string, error)
	Remove(name string) error
	Mkdir(name string, perm fs.FileMode) error
	MkdirAll(name string, perm fs.FileMode) error
	Chmod(name string, perm fs.FileMode) error
	// Create создаёт новый пустой файл (O_EXCL). Существующий — ошибка fs.ErrExist.
	Create(name string, perm fs.FileMode) error
	// Fallocate выделяет size байт в существующем файле. Если ФС не умеет —
	// ошибка, совместимая с errors.Is(err, ErrFallocateUnsupported).
	Fallocate(name string, size uint64) error
	// WriteZeros заполняет существующий файл size нулями.
	WriteZeros(name string, size uint64) error
	// WriteValue пишет значение в существующий файл /proc или /sys (без
	// временного файла).
	WriteValue(name, value string) error
	// ReadPrefix читает до n первых байт файла (заголовок swap).
	ReadPrefix(name string, n int) ([]byte, error)
	// Glob — как filepath.Glob.
	Glob(pattern string) ([]string, error)
	// Lock берёт эксклюзивную блокировку (flock) без ожидания.
	Lock(name string) (unlock func() error, err error)
}

// Executor запускает внешние команды (mkswap, systemctl, btrfs, chattr).
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	LookPath(file string) (string, error)
}

// Kernel — системные вызовы.
type Kernel interface {
	Geteuid() int
	// Swapon включает swap; priority < 0 — приоритет выбирает ядро.
	Swapon(path string, priority int) error
	Swapoff(path string) error
}

// System — всё, через что пакет взаимодействует с ОС.
type System struct {
	FS     FS
	Exec   Executor
	Kernel Kernel
	// DescribeFS описывает ФС пути (обычно detect.DescribeFS).
	DescribeFS func(path string) detect.FSInfo
	// ListFS — смонтированные ФС-кандидаты (обычно detect.ListLocalFS).
	ListFS func() []detect.FSInfo
	Now    func() time.Time
}
