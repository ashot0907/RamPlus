package detect

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Mount — одна строка /proc/self/mountinfo.
type Mount struct {
	ID         int
	ParentID   int
	Major      uint32
	Minor      uint32
	Root       string
	MountPoint string
	FSType     string
	Source     string
}

// ParseMountinfo разбирает /proc/<pid>/mountinfo (см. proc(5)):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
func ParseMountinfo(r io.Reader) ([]Mount, error) {
	var mounts []Mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(f) < sep+3 {
			return nil, fmt.Errorf("%w: mountinfo line %d: malformed", ErrParse, n)
		}
		id, err1 := strconv.Atoi(f[0])
		pid, err2 := strconv.Atoi(f[1])
		majS, minS, ok := strings.Cut(f[2], ":")
		maj, err3 := strconv.ParseUint(majS, 10, 32)
		mnr, err4 := strconv.ParseUint(minS, 10, 32)
		if err1 != nil || err2 != nil || !ok || err3 != nil || err4 != nil {
			return nil, fmt.Errorf("%w: mountinfo line %d: bad ids", ErrParse, n)
		}
		mounts = append(mounts, Mount{
			ID:         id,
			ParentID:   pid,
			Major:      uint32(maj),
			Minor:      uint32(mnr),
			Root:       UnescapeOctal(f[3]),
			MountPoint: UnescapeOctal(f[4]),
			FSType:     f[sep+1],
			Source:     UnescapeOctal(f[sep+2]),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(mounts) == 0 {
		return nil, fmt.Errorf("%w: mountinfo: empty", ErrParse)
	}
	return mounts, nil
}

// FindMount возвращает монтирование, видимое по пути mountPoint. Если точка
// перемонтирована несколько раз, видимо последнее.
func FindMount(mounts []Mount, mountPoint string) (Mount, bool) {
	for i := len(mounts) - 1; i >= 0; i-- {
		if mounts[i].MountPoint == mountPoint {
			return mounts[i], true
		}
	}
	return Mount{}, false
}

// FindMountForPath возвращает монтирование, которому принадлежит абсолютный
// путь (самая длинная точка монтирования-префикс по границе компонентов).
func FindMountForPath(mounts []Mount, path string) (Mount, bool) {
	best, found := -1, false
	for i, m := range mounts {
		mp := m.MountPoint
		if path != mp && mp != "/" && !strings.HasPrefix(path, mp+"/") {
			continue
		}
		if !strings.HasPrefix(path, "/") {
			continue
		}
		// При равной длине побеждает более позднее (перемонтирование).
		if !found || len(mp) >= len(mounts[best].MountPoint) {
			best, found = i, true
		}
	}
	if !found {
		return Mount{}, false
	}
	return mounts[best], true
}
