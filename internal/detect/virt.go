package detect

import (
	"os"
	"strings"
	"sync"
)

// DetectVirt определяет виртуализацию по DMI (/sys/class/dmi/id/*) и флагам
// CPU (/proc/cpuinfo). Чистая функция; "" — похоже на железо.
func DetectVirt(dmi map[string]string, cpuinfo string) string {
	all := strings.ToLower(dmi["sys_vendor"] + "|" + dmi["product_name"] + "|" + dmi["bios_vendor"] + "|" + dmi["board_vendor"])
	switch {
	case strings.Contains(all, "qemu") || strings.Contains(all, "kvm") || strings.Contains(all, "bochs"):
		return "kvm"
	case strings.Contains(all, "vmware"):
		return "vmware"
	case strings.Contains(all, "innotek") || strings.Contains(all, "virtualbox"):
		return "oracle"
	case strings.Contains(all, "microsoft corporation") && strings.Contains(all, "virtual machine"):
		return "microsoft"
	case strings.Contains(all, "xen"):
		return "xen"
	case strings.Contains(all, "amazon ec2"):
		return "amazon"
	case strings.Contains(all, "google compute engine"):
		return "google"
	case strings.Contains(all, "parallels"):
		return "parallels"
	}
	for _, line := range strings.Split(cpuinfo, "\n") {
		if strings.HasPrefix(line, "flags") {
			for _, f := range strings.Fields(line) {
				if f == "hypervisor" {
					return "vm"
				}
			}
			break
		}
	}
	return ""
}

// Virtualization — тип виртуализации этой системы ("" — железо).
// Читается один раз.
var Virtualization = sync.OnceValue(func() string {
	dmi := map[string]string{}
	for _, k := range []string{"sys_vendor", "product_name", "bios_vendor", "board_vendor"} {
		if b, err := os.ReadFile("/sys/class/dmi/id/" + k); err == nil {
			dmi[k] = strings.TrimSpace(string(b))
		}
	}
	cpuinfo, _ := os.ReadFile("/proc/cpuinfo")
	return DetectVirt(dmi, string(cpuinfo))
})
