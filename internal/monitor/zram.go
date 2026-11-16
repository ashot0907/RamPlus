package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
)

// ZramMMStat — /sys/block/zramN/mm_stat (Documentation/admin-guide/blockdev/zram.rst).
type ZramMMStat struct {
	OrigDataBytes  uint64  `json:"orig_data_bytes"`
	ComprDataBytes uint64  `json:"compr_data_bytes"`
	MemUsedBytes   uint64  `json:"mem_used_bytes"`
	MemLimitBytes  uint64  `json:"mem_limit_bytes"`
	MemUsedMax     uint64  `json:"mem_used_max_bytes"`
	SamePages      *uint64 `json:"same_pages"`
	PagesCompacted *uint64 `json:"pages_compacted"`
	HugePages      *uint64 `json:"huge_pages"`
}

// ZramDevice — одно zram-устройство.
type ZramDevice struct {
	Name          string      `json:"name"`
	DiskSizeBytes *uint64     `json:"disksize_bytes"`
	Algorithm     *string     `json:"algorithm"`
	Algorithms    []string    `json:"algorithms"`
	MMStat        *ZramMMStat `json:"mm_stat"`
	// CompressionRatio = orig_data_size / compr_data_size.
	CompressionRatio *float64            `json:"compression_ratio"`
	Error            *detect.Unavailable `json:"error,omitempty"`
}

// Zram — все zram-устройства. Devices никогда не nil.
type Zram struct {
	Devices []ZramDevice        `json:"devices"`
	Error   *detect.Unavailable `json:"error,omitempty"`
}

// ParseZramCompAlgorithm разбирает comp_algorithm: "lzo lzo-rle [lz4] zstd".
// Выбранный алгоритм — в квадратных скобках.
func ParseZramCompAlgorithm(b []byte) (selected string, available []string, err error) {
	for _, a := range strings.Fields(string(b)) {
		if strings.HasPrefix(a, "[") && strings.HasSuffix(a, "]") {
			a = strings.Trim(a, "[]")
			selected = a
		}
		available = append(available, a)
	}
	if selected == "" {
		return "", available, fmt.Errorf("%w: zram comp_algorithm: no [selected] entry", detect.ErrParse)
	}
	return selected, available, nil
}

// ParseZramMMStat разбирает mm_stat. Старые ядра отдают меньше колонок —
// хвостовые поля тогда nil.
func ParseZramMMStat(b []byte) (ZramMMStat, error) {
	f := strings.Fields(string(b))
	if len(f) < 5 {
		return ZramMMStat{}, fmt.Errorf("%w: zram mm_stat: want >=5 columns, got %d", detect.ErrParse, len(f))
	}
	vals := make([]uint64, len(f))
	for i, s := range f {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return ZramMMStat{}, fmt.Errorf("%w: zram mm_stat column %d: %v", detect.ErrParse, i, err)
		}
		vals[i] = v
	}
	opt := func(i int) *uint64 {
		if i < len(vals) {
			return ptr(vals[i])
		}
		return nil
	}
	return ZramMMStat{
		OrigDataBytes:  vals[0],
		ComprDataBytes: vals[1],
		MemUsedBytes:   vals[2],
		MemLimitBytes:  vals[3],
		MemUsedMax:     vals[4],
		SamePages:      opt(5),
		PagesCompacted: opt(6),
		HugePages:      opt(7),
	}, nil
}

// ZramDeviceFromFiles строит описание устройства из содержимого его
// каталога в sysfs (имя файла → содержимое).
func ZramDeviceFromFiles(name string, files map[string]string) ZramDevice {
	d := ZramDevice{Name: name, Algorithms: []string{}}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s, ok := files["disksize"]; ok {
		v, err := ParseUintValue([]byte(s))
		note(err)
		if err == nil {
			d.DiskSizeBytes = &v
		}
	}
	if s, ok := files["comp_algorithm"]; ok {
		sel, all, err := ParseZramCompAlgorithm([]byte(s))
		note(err)
		if sel != "" {
			d.Algorithm = &sel
		}
		if all != nil {
			d.Algorithms = all
		}
	}
	if s, ok := files["mm_stat"]; ok {
		st, err := ParseZramMMStat([]byte(s))
		note(err)
		if err == nil {
			d.MMStat = &st
			if st.ComprDataBytes > 0 {
				d.CompressionRatio = ptr(float64(st.OrigDataBytes) / float64(st.ComprDataBytes))
			}
		}
	}
	d.Error = detect.NewUnavailable(firstErr)
	return d
}

// ReadZram ищет устройства zram* в sysBlock (обычно /sys/block).
func ReadZram(sysBlock string) Zram {
	z := Zram{Devices: []ZramDevice{}}
	paths, err := filepath.Glob(filepath.Join(sysBlock, "zram*"))
	if err != nil {
		z.Error = detect.NewUnavailable(err)
		return z
	}
	if _, err := os.Stat(sysBlock); err != nil {
		z.Error = detect.NewUnavailable(err)
		return z
	}
	sort.Strings(paths)
	for _, p := range paths {
		name := filepath.Base(p)
		if strings.HasPrefix(name, "zram-") { // не устройство
			continue
		}
		// Читаем только нужные файлы: в каталоге есть write-only (reset,
		// compact) и тяжёлые для чтения атрибуты.
		files := map[string]string{}
		var readErr error
		for _, f := range []string{"disksize", "comp_algorithm", "mm_stat"} {
			b, err := os.ReadFile(filepath.Join(p, f))
			if err != nil {
				if readErr == nil {
					readErr = err
				}
				continue
			}
			files[f] = string(b)
		}
		d := ZramDeviceFromFiles(name, files)
		if d.Error == nil {
			d.Error = detect.NewUnavailable(readErr)
		}
		z.Devices = append(z.Devices, d)
	}
	return z
}
