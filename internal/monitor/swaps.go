package monitor

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
)

// SwapDevice — строка /proc/swaps.
type SwapDevice struct {
	Path      string `json:"path"`
	Type      string `json:"type"` // file | partition
	SizeBytes uint64 `json:"size_bytes"`
	UsedBytes uint64 `json:"used_bytes"`
	Priority  int    `json:"priority"`
	// Managed — swap создан и управляется RamPlus.
	Managed bool `json:"managed"`
	// DiskKind / Transport — физический диск под swap (nvme|ssd|hdd,
	// usb|sata|...). Пусто, если определить не удалось (или это zram).
	DiskKind  string `json:"disk_kind,omitempty"`
	Transport string `json:"transport,omitempty"`
}

// Коды предупреждений о swap.
const (
	WarnSwapOnUSB = "swap_on_usb" // swap на USB-диске: медленно и опасно при отключении
)

// SwapWarning — предупреждение, относящееся к конкретному swap-устройству.
type SwapWarning struct {
	Code string `json:"code"`
	Path string `json:"path"`
}

// Swaps — активные swap-устройства. Devices и Warnings никогда не nil.
type Swaps struct {
	Devices  []SwapDevice        `json:"devices"`
	Warnings []SwapWarning       `json:"warnings"`
	Error    *detect.Unavailable `json:"error,omitempty"`
}

// ParseSwaps разбирает /proc/swaps. Если swap не включён, файл содержит
// только заголовок — результат пустой срез без ошибки.
func ParseSwaps(r io.Reader) ([]SwapDevice, error) {
	devs := []SwapDevice{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if n == 1 && strings.HasPrefix(line, "Filename") {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 5 {
			return nil, fmt.Errorf("%w: swaps line %d: want 5 fields, got %d", detect.ErrParse, n, len(f))
		}
		size, err1 := strconv.ParseUint(f[2], 10, 64)
		used, err2 := strconv.ParseUint(f[3], 10, 64)
		prio, err3 := strconv.Atoi(f[4])
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("%w: swaps line %d: bad number", detect.ErrParse, n)
		}
		devs = append(devs, SwapDevice{
			Path:      detect.UnescapeOctal(f[0]),
			Type:      f[1],
			SizeBytes: size * 1024,
			UsedBytes: used * 1024,
			Priority:  prio,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return devs, nil
}

// ReadSwaps читает path (обычно /proc/swaps).
func ReadSwaps(path string) Swaps {
	data, err := os.ReadFile(path)
	if err != nil {
		return Swaps{Devices: []SwapDevice{}, Warnings: []SwapWarning{}, Error: detect.NewUnavailable(err)}
	}
	devs, err := ParseSwaps(bytes.NewReader(data))
	if err != nil {
		return Swaps{Devices: []SwapDevice{}, Warnings: []SwapWarning{}, Error: detect.NewUnavailable(err)}
	}
	return Swaps{Devices: devs, Warnings: []SwapWarning{}}
}

// AnnotateSwapDisks дополняет устройства сведениями о физическом диске и
// добавляет предупреждения (swap на USB). disksFor обычно detect.DisksForPath.
func AnnotateSwapDisks(s *Swaps, disksFor func(path string) (detect.PathDisks, error)) {
	for i := range s.Devices {
		d := &s.Devices[i]
		if strings.HasPrefix(d.Path, "/dev/zram") {
			continue
		}
		pd, err := disksFor(d.Path)
		if err != nil && len(pd.Disks) == 0 {
			continue
		}
		d.DiskKind = pd.Kind
		if len(pd.Disks) > 0 {
			d.Transport = pd.Disks[0].Transport
		}
		if detect.HasTransport(pd.Disks, "usb") {
			d.Transport = "usb"
			s.Warnings = append(s.Warnings, SwapWarning{Code: WarnSwapOnUSB, Path: d.Path})
		}
	}
}
