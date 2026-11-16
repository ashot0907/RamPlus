package monitor

import (
	"os"
	"path/filepath"

	"github.com/ashot0907/ramplus/internal/detect"
)

// VM — параметры /proc/sys/vm, влияющие на swap и кэш.
type VM struct {
	Swappiness           *int `json:"swappiness"`
	VFSCachePressure     *int `json:"vfs_cache_pressure"`
	WatermarkScaleFactor *int `json:"watermark_scale_factor"`
	// Errors — причины n/a по именам sysctl (только для недоступных).
	Errors map[string]*detect.Unavailable `json:"errors,omitempty"`
}

// ReadVM читает sysctl из dir (обычно /proc/sys/vm).
func ReadVM(dir string) VM {
	var vm VM
	read := func(name string) *int {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			var v int
			if v, err = ParseIntValue(b); err == nil {
				return &v
			}
		}
		if vm.Errors == nil {
			vm.Errors = map[string]*detect.Unavailable{}
		}
		vm.Errors[name] = detect.NewUnavailable(err)
		return nil
	}
	vm.Swappiness = read("swappiness")
	vm.VFSCachePressure = read("vfs_cache_pressure")
	vm.WatermarkScaleFactor = read("watermark_scale_factor")
	return vm
}
