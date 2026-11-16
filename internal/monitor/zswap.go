package monitor

import (
	"fmt"
	"os"
	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
)

// ZswapParams — /sys/module/zswap/parameters/*. nil — параметра нет в этом
// ядре (например, zpool убран в новых ядрах, где остался только zsmalloc).
type ZswapParams struct {
	Enabled                *bool   `json:"enabled"`
	Compressor             *string `json:"compressor"`
	Zpool                  *string `json:"zpool"`
	MaxPoolPercent         *int    `json:"max_pool_percent"`
	AcceptThresholdPercent *int    `json:"accept_threshold_percent"`
	ShrinkerEnabled        *bool   `json:"shrinker_enabled"`
}

// ZswapStats — счётчики из debugfs (/sys/kernel/debug/zswap, только root).
type ZswapStats struct {
	PoolTotalSizeBytes *uint64 `json:"pool_total_size_bytes"`
	StoredPages        *uint64 `json:"stored_pages"`
	WrittenBackPages   *uint64 `json:"written_back_pages"`
	PoolLimitHit       *uint64 `json:"pool_limit_hit"`
	// CompressionRatio = stored_pages*page_size / pool_total_size.
	CompressionRatio *float64 `json:"compression_ratio"`
	// Counters — все числовые файлы как есть: набор зависит от версии ядра.
	Counters map[string]uint64 `json:"counters"`
}

// Zswap — состояние zswap.
type Zswap struct {
	// Present — zswap есть в ядре (встроен или модуль загружен).
	Present bool        `json:"present"`
	Params  ZswapParams `json:"params"`
	// PoolBytes / StoredBytes — строки Zswap/Zswapped из /proc/meminfo
	// (ядро 5.19+), доступны без root.
	PoolBytes   *uint64             `json:"pool_bytes"`
	StoredBytes *uint64             `json:"stored_bytes"`
	Stats       *ZswapStats         `json:"stats"`
	StatsError  *detect.Unavailable `json:"stats_error,omitempty"`
	Error       *detect.Unavailable `json:"error,omitempty"`
}

// ParseZswapParams разбирает содержимое каталога parameters (имя файла →
// содержимое). Отсутствующие параметры остаются nil.
func ParseZswapParams(files map[string]string) (ZswapParams, error) {
	var p ZswapParams
	var err error
	parseBool := func(name string) *bool {
		s, ok := files[name]
		if !ok || err != nil {
			return nil
		}
		v, e := ParseBoolValue([]byte(s))
		if e != nil {
			err = fmt.Errorf("zswap %s: %w", name, e)
			return nil
		}
		return &v
	}
	parseInt := func(name string) *int {
		s, ok := files[name]
		if !ok || err != nil {
			return nil
		}
		v, e := ParseIntValue([]byte(s))
		if e != nil {
			err = fmt.Errorf("zswap %s: %w", name, e)
			return nil
		}
		return &v
	}
	parseStr := func(name string) *string {
		s, ok := files[name]
		if !ok {
			return nil
		}
		return ptr(strings.TrimSpace(s))
	}

	p.Enabled = parseBool("enabled")
	p.Compressor = parseStr("compressor")
	p.Zpool = parseStr("zpool")
	p.MaxPoolPercent = parseInt("max_pool_percent")
	p.AcceptThresholdPercent = parseInt("accept_threshold_percent")
	p.ShrinkerEnabled = parseBool("shrinker_enabled")
	return p, err
}

// ParseZswapStats разбирает каталог debugfs zswap. Нечисловые файлы
// игнорируются.
func ParseZswapStats(files map[string]string, pageSize uint64) (ZswapStats, error) {
	s := ZswapStats{Counters: map[string]uint64{}}
	for name, content := range files {
		v, err := ParseUintValue([]byte(content))
		if err != nil {
			continue
		}
		s.Counters[name] = v
	}
	if len(s.Counters) == 0 {
		return ZswapStats{}, fmt.Errorf("%w: zswap debugfs: no counters", detect.ErrParse)
	}
	get := func(k string) *uint64 {
		if v, ok := s.Counters[k]; ok {
			return &v
		}
		return nil
	}
	s.PoolTotalSizeBytes = get("pool_total_size")
	s.StoredPages = get("stored_pages")
	s.WrittenBackPages = get("written_back_pages")
	s.PoolLimitHit = get("pool_limit_hit")
	if s.PoolTotalSizeBytes != nil && s.StoredPages != nil && *s.PoolTotalSizeBytes > 0 && pageSize > 0 {
		s.CompressionRatio = ptr(float64(*s.StoredPages*pageSize) / float64(*s.PoolTotalSizeBytes))
	}
	return s, nil
}

// ReadZswap читает параметры из paramsDir и статистику из debugDir.
// mi — уже разобранный /proc/meminfo (может быть nil).
func ReadZswap(paramsDir, debugDir string, mi Meminfo) Zswap {
	z := Zswap{PoolBytes: mi.Get("Zswap"), StoredBytes: mi.Get("Zswapped")}

	files, err := readDirFiles(paramsDir)
	if err != nil {
		z.Error = detect.NewUnavailable(err)
		return z
	}
	z.Present = true
	z.Params, err = ParseZswapParams(files)
	if err != nil {
		z.Error = detect.NewUnavailable(err)
	}

	stats, err := readDirFiles(debugDir)
	if err == nil {
		var s ZswapStats
		if s, err = ParseZswapStats(stats, uint64(os.Getpagesize())); err == nil {
			z.Stats = &s
		}
	}
	z.StatsError = detect.NewUnavailable(err)
	return z
}
