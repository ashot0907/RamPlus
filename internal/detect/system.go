package detect

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// OSRelease — нужные поля из os-release(5).
type OSRelease struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	PrettyName      string `json:"pretty_name"`
	VersionID       string `json:"version_id"`
	VersionCodename string `json:"version_codename,omitempty"`
}

// System — общая информация о машине.
type System struct {
	Hostname string       `json:"hostname"`
	Kernel   string       `json:"kernel"`
	Arch     string       `json:"arch"`
	OS       *OSRelease   `json:"os"`
	OSError  *Unavailable `json:"os_error,omitempty"`
}

// ParseOSRelease разбирает формат os-release: KEY=value, значения могут быть
// в двойных или одинарных кавычках, # — комментарий.
func ParseOSRelease(r io.Reader) (OSRelease, error) {
	vals := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(key)] = unquoteOSRelease(strings.TrimSpace(val))
	}
	if err := sc.Err(); err != nil {
		return OSRelease{}, err
	}
	if len(vals) == 0 {
		return OSRelease{}, fmt.Errorf("%w: os-release: no KEY=value pairs", ErrParse)
	}
	rel := OSRelease{
		ID:              vals["ID"],
		Name:            vals["NAME"],
		PrettyName:      vals["PRETTY_NAME"],
		VersionID:       vals["VERSION_ID"],
		VersionCodename: vals["VERSION_CODENAME"],
	}
	// Значения по умолчанию из os-release(5).
	if rel.ID == "" {
		rel.ID = "linux"
	}
	if rel.Name == "" {
		rel.Name = "Linux"
	}
	if rel.PrettyName == "" {
		rel.PrettyName = rel.Name
	}
	return rel, nil
}

func unquoteOSRelease(v string) string {
	if len(v) >= 2 {
		switch {
		case v[0] == '\'' && v[len(v)-1] == '\'':
			return v[1 : len(v)-1]
		case v[0] == '"' && v[len(v)-1] == '"':
			v = v[1 : len(v)-1]
			var b strings.Builder
			for i := 0; i < len(v); i++ {
				if v[i] == '\\' && i+1 < len(v) && strings.IndexByte("\"\\$`", v[i+1]) >= 0 {
					i++
				}
				b.WriteByte(v[i])
			}
			return b.String()
		}
	}
	return v
}

// osReleasePaths — порядок поиска по os-release(5).
var osReleasePaths = []string{"/etc/os-release", "/usr/lib/os-release"}

// ReadSystem собирает информацию о системе. Никогда не возвращает ошибку:
// недоступные поля помечаются через OSError / пустые строки.
func ReadSystem() System {
	var s System
	s.Hostname, _ = os.Hostname()

	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		s.Kernel = unix.ByteSliceToString(uts.Release[:])
		s.Arch = unix.ByteSliceToString(uts.Machine[:])
	}

	var lastErr error
	for _, p := range osReleasePaths {
		data, err := os.ReadFile(p)
		if err != nil {
			lastErr = err
			continue
		}
		rel, err := ParseOSRelease(bytes.NewReader(data))
		if err != nil {
			lastErr = err
			continue
		}
		s.OS = &rel
		lastErr = nil
		break
	}
	s.OSError = NewUnavailable(lastErr)
	return s
}
