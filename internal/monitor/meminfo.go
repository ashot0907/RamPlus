// Package monitor читает состояние памяти: /proc/meminfo, /proc/swaps, PSI,
// zswap, zram, sysctl vm.*. Только чтение.
//
// Парсеры — чистые функции ([]byte/io.Reader → структура), функции Read*
// занимаются вводом-выводом и никогда не возвращают ошибку: недоступный
// источник отражается в поле Error (в выводе — "n/a").
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

// Meminfo — значения /proc/meminfo; поля в kB переведены в байты.
type Meminfo map[string]uint64

// Get возвращает указатель на значение или nil, если поля нет.
func (m Meminfo) Get(key string) *uint64 {
	v, ok := m[key]
	if !ok {
		return nil
	}
	return &v
}

// ParseMeminfo разбирает /proc/meminfo.
func ParseMeminfo(r io.Reader) (Meminfo, error) {
	m := Meminfo{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: meminfo %s: %v", detect.ErrParse, key, err)
		}
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		m[strings.TrimSpace(key)] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("%w: meminfo: empty", detect.ErrParse)
	}
	return m, nil
}

// Memory — сводка по RAM и swap. nil-поле означает "n/a".
type Memory struct {
	TotalBytes     *uint64 `json:"total_bytes"`
	AvailableBytes *uint64 `json:"available_bytes"`
	// UsedBytes = MemTotal - MemAvailable (то, что реально нельзя быстро
	// отдать; так же считает современный free(1)).
	UsedBytes       *uint64             `json:"used_bytes"`
	FreeBytes       *uint64             `json:"free_bytes"`
	CachedBytes     *uint64             `json:"cached_bytes"`
	BuffersBytes    *uint64             `json:"buffers_bytes"`
	SwapTotalBytes  *uint64             `json:"swap_total_bytes"`
	SwapFreeBytes   *uint64             `json:"swap_free_bytes"`
	SwapUsedBytes   *uint64             `json:"swap_used_bytes"`
	SwapCachedBytes *uint64             `json:"swap_cached_bytes"`
	Error           *detect.Unavailable `json:"error,omitempty"`
}

// MemoryFromMeminfo строит сводку из разобранного meminfo.
func MemoryFromMeminfo(m Meminfo) Memory {
	mem := Memory{
		TotalBytes:      m.Get("MemTotal"),
		AvailableBytes:  m.Get("MemAvailable"),
		FreeBytes:       m.Get("MemFree"),
		CachedBytes:     m.Get("Cached"),
		BuffersBytes:    m.Get("Buffers"),
		SwapTotalBytes:  m.Get("SwapTotal"),
		SwapFreeBytes:   m.Get("SwapFree"),
		SwapCachedBytes: m.Get("SwapCached"),
	}
	mem.UsedBytes = subSat(mem.TotalBytes, mem.AvailableBytes)
	mem.SwapUsedBytes = subSat(mem.SwapTotalBytes, mem.SwapFreeBytes)
	return mem
}

// subSat возвращает a-b (не меньше нуля) или nil, если операнда нет.
func subSat(a, b *uint64) *uint64 {
	if a == nil || b == nil {
		return nil
	}
	var v uint64
	if *a > *b {
		v = *a - *b
	}
	return &v
}

// ReadMemory читает path (обычно /proc/meminfo). Сырой Meminfo возвращается
// для повторного использования (zswap берёт из него Zswap/Zswapped) и может
// быть nil.
func ReadMemory(path string) (Memory, Meminfo) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Memory{Error: detect.NewUnavailable(err)}, nil
	}
	m, err := ParseMeminfo(bytes.NewReader(data))
	if err != nil {
		return Memory{Error: detect.NewUnavailable(err)}, nil
	}
	return MemoryFromMeminfo(m), m
}
