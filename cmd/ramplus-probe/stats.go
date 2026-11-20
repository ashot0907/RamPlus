package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
)

const pageSize = 4096

// Percentile — p-й перцентиль (0..100) методом ближайшего ранга.
// Пустой вход → NaN.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	rank = min(max(rank, 1), len(s))
	return s[rank-1]
}

// Median — медиана (среднее двух центральных для чётного числа).
func Median(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// LatencySummary — сводка задержек в миллисекундах.
type LatencySummary struct {
	Samples int     `json:"samples"`
	P50Ms   float64 `json:"p50_ms"`
	P99Ms   float64 `json:"p99_ms"`
	MaxMs   float64 `json:"max_ms"`
	MeanMs  float64 `json:"mean_ms"`
}

// Summarize считает p50/p99/max/mean.
func Summarize(ms []float64) LatencySummary {
	s := LatencySummary{Samples: len(ms)}
	if len(ms) == 0 {
		return s
	}
	var sum float64
	for _, v := range ms {
		sum += v
		s.MaxMs = max(s.MaxMs, v)
	}
	s.MeanMs = sum / float64(len(ms))
	s.P50Ms, s.P99Ms = Percentile(ms, 50), Percentile(ms, 99)
	return s
}

// Типы данных нагрузки.
const (
	DataCompressible = "compressible"
	DataRandom       = "random"
)

// FillPages заполняет buf постранично. random — случайные байты (не
// сжимаются). compressible — примерно 3:1 как у типичной памяти процессов:
// треть страницы случайная, остальное — повторяющийся текстоподобный шаблон.
// Страницы не бывают "одинаково заполненными" (zswap хранит такие без
// сжатия, это исказило бы результат).
func FillPages(buf []byte, data string, seed uint64) error {
	rng := rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24)})
	switch data {
	case DataRandom:
		_, _ = rng.Read(buf)
	case DataCompressible:
		pattern := []byte("ramplus compressible page: the quick brown fox jumps over the lazy dog 0123456789; ")
		for off := 0; off < len(buf); off += pageSize {
			page := buf[off:min(off+pageSize, len(buf))]
			third := len(page) / 3
			_, _ = rng.Read(page[:third])
			for i := third; i < len(page); i++ {
				page[i] = pattern[(i+off/pageSize)%len(pattern)]
			}
		}
	default:
		return fmt.Errorf("unknown data type %q (compressible|random)", data)
	}
	return nil
}

// ParseSize: "50M", "5G", "512K", байты.
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := uint64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return uint64(v * float64(mult)), nil
}
