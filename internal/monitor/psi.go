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

// PSILine — строка some/full из /proc/pressure/*: доля времени (%), когда
// задачи простаивали из-за нехватки ресурса, за 10/60/300 с.
type PSILine struct {
	Avg10   float64 `json:"avg10"`
	Avg60   float64 `json:"avg60"`
	Avg300  float64 `json:"avg300"`
	TotalUs uint64  `json:"total_us"`
}

// PSI — давление на память. Без PSI в ядре (CONFIG_PSI=n или psi=0)
// оба поля nil, а Error объясняет причину.
type PSI struct {
	Some  *PSILine            `json:"some"`
	Full  *PSILine            `json:"full"`
	Error *detect.Unavailable `json:"error,omitempty"`
}

// ParsePSI разбирает /proc/pressure/memory:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
func ParsePSI(r io.Reader) (PSI, error) {
	var p PSI
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		var line PSILine
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return PSI{}, fmt.Errorf("%w: psi: bad field %q", detect.ErrParse, kv)
			}
			var err error
			switch k {
			case "avg10":
				line.Avg10, err = strconv.ParseFloat(v, 64)
			case "avg60":
				line.Avg60, err = strconv.ParseFloat(v, 64)
			case "avg300":
				line.Avg300, err = strconv.ParseFloat(v, 64)
			case "total":
				line.TotalUs, err = strconv.ParseUint(v, 10, 64)
			}
			if err != nil {
				return PSI{}, fmt.Errorf("%w: psi %s: %v", detect.ErrParse, k, err)
			}
		}
		switch f[0] {
		case "some":
			p.Some = &line
		case "full":
			p.Full = &line
		}
	}
	if err := sc.Err(); err != nil {
		return PSI{}, err
	}
	if p.Some == nil && p.Full == nil {
		return PSI{}, fmt.Errorf("%w: psi: no some/full lines", detect.ErrParse)
	}
	return p, nil
}

// ReadPSI читает path (обычно /proc/pressure/memory).
func ReadPSI(path string) PSI {
	data, err := os.ReadFile(path)
	if err != nil {
		return PSI{Error: detect.NewUnavailable(err)}
	}
	p, err := ParsePSI(bytes.NewReader(data))
	if err != nil {
		return PSI{Error: detect.NewUnavailable(err)}
	}
	return p
}
