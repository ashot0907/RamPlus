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
}

// Swaps — активные swap-устройства. Devices никогда не nil (в JSON — []).
type Swaps struct {
	Devices []SwapDevice        `json:"devices"`
	Error   *detect.Unavailable `json:"error,omitempty"`
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
		return Swaps{Devices: []SwapDevice{}, Error: detect.NewUnavailable(err)}
	}
	devs, err := ParseSwaps(bytes.NewReader(data))
	if err != nil {
		return Swaps{Devices: []SwapDevice{}, Error: detect.NewUnavailable(err)}
	}
	return Swaps{Devices: devs}
}
