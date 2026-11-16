package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ashot0907/ramplus/internal/detect"
)

// readDirFiles читает все обычные файлы каталога (sysfs/debugfs) в карту
// имя → содержимое. Нечитаемые отдельные файлы пропускаются; ошибка
// возвращается, только если не удалось открыть сам каталог или ни одного
// файла прочитать не вышло.
func readDirFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string, len(entries))
	var firstErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		files[e.Name()] = string(b)
	}
	if len(files) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return files, nil
}

// ParseIntValue разбирает однострочное целое значение (sysctl, sysfs).
func ParseIntValue(b []byte) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", detect.ErrParse, err)
	}
	return v, nil
}

// ParseUintValue разбирает однострочное беззнаковое значение.
func ParseUintValue(b []byte) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", detect.ErrParse, err)
	}
	return v, nil
}

// ParseBoolValue разбирает булев параметр модуля: Y/N, 1/0.
func ParseBoolValue(b []byte) (bool, error) {
	switch strings.TrimSpace(string(b)) {
	case "Y", "y", "1":
		return true, nil
	case "N", "n", "0":
		return false, nil
	}
	return false, fmt.Errorf("%w: bad bool %q", detect.ErrParse, strings.TrimSpace(string(b)))
}

func ptr[T any](v T) *T { return &v }
