package render

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/profile"
)

// reasonText — человекочитаемые причины n/a; context переопределяет общие.
func reasonText(l i18n.Lang, u *detect.Unavailable, context map[string]string) string {
	if u == nil {
		return "n/a"
	}
	if t, ok := context[u.Reason]; ok {
		return "n/a (" + t + ")"
	}
	switch u.Reason {
	case detect.ReasonPermissionDenied:
		return t(l, "render.na.root")
	case detect.ReasonNotPresent:
		return t(l, "render.na.absent")
	case detect.ReasonUnsupported:
		return t(l, "render.na.unsupported")
	case detect.ReasonParseError:
		return t(l, "render.na.parse")
	default:
		return t(l, "render.na.unavailable")
	}
}

// humanBytes форматирует размер в IEC-единицах: 7.8 GiB.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func bytesOrNA(p *uint64) string {
	if p == nil {
		return "n/a"
	}
	return humanBytes(*p)
}

func intOrNA(p *int) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprint(*p)
}

func strOrNA(p *string, na string) string {
	if p == nil || *p == "" {
		return na
	}
	return *p
}

func yesNo(l i18n.Lang, p *bool) string {
	switch {
	case p == nil:
		return "n/a"
	case *p:
		return t(l, "render.yes")
	default:
		return t(l, "render.no")
	}
}

func percent(part, total *uint64) string {
	if part == nil || total == nil || *total == 0 {
		return ""
	}
	return fmt.Sprintf(" (%.0f%%)", float64(*part)*100/float64(*total))
}

// section пишет заголовок и пары "ключ  значение" через tabwriter.
type section struct {
	tw *tabwriter.Writer
}

func newSection(w io.Writer, title string) *section {
	fmt.Fprintln(w, title)
	return &section{tw: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)}
}

func (s *section) kv(k, v string)               { fmt.Fprintf(s.tw, "  %s\t%s\n", k, v) }
func (s *section) row(cols ...string)           { fmt.Fprintf(s.tw, "  %s\n", strings.Join(cols, "\t")) }
func (s *section) line(format string, a ...any) { fmt.Fprintf(s.tw, "  "+format+"\n", a...) }
func (s *section) end(w io.Writer) error {
	if err := s.tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

// Status — ramplus status для человека.
func Status(l i18n.Lang, w io.Writer, st monitor.Status) error {
	renders := []func(i18n.Lang, io.Writer, monitor.Status) error{
		renderSystem, renderMemory, renderSwaps, renderZswap,
		renderVM, renderPSI, renderRootDisk, renderZram, renderProfile,
	}
	for _, r := range renders {
		if err := r(l, w, st); err != nil {
			return err
		}
	}
	return nil
}

func renderSystem(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, t(l, "render.status.system"))
	sys := st.System
	s.kv(t(l, "render.status.host"), strOrNA(&sys.Hostname, "n/a"))
	kernel := strOrNA(&sys.Kernel, "n/a")
	if sys.Arch != "" {
		kernel += " (" + sys.Arch + ")"
	}
	s.kv(t(l, "render.status.kernel"), kernel)
	if sys.Virtualization != "" {
		s.kv(t(l, "render.status.virt"), sys.Virtualization+t(l, "render.status.virt_note"))
	}
	if sys.OS != nil {
		s.kv(t(l, "render.status.distro"), sys.OS.PrettyName)
	} else {
		s.kv(t(l, "render.status.distro"), reasonText(l, sys.OSError, nil))
	}
	return s.end(w)
}

func renderMemory(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, t(l, "render.status.memory"))
	m := st.Memory
	if m.Error != nil {
		s.line("%s", reasonText(l, m.Error, nil))
		return s.end(w)
	}
	s.kv(t(l, "render.status.total"), bytesOrNA(m.TotalBytes))
	s.kv(t(l, "render.status.available"), bytesOrNA(m.AvailableBytes)+percent(m.AvailableBytes, m.TotalBytes))
	s.kv(t(l, "render.status.used"), bytesOrNA(m.UsedBytes)+percent(m.UsedBytes, m.TotalBytes))
	s.kv(t(l, "render.status.cached"), bytesOrNA(m.CachedBytes))
	s.kv(t(l, "render.status.swap_total"), bytesOrNA(m.SwapTotalBytes))
	s.kv(t(l, "render.status.swap_free"), bytesOrNA(m.SwapFreeBytes))
	s.kv(t(l, "render.status.swap_used"), bytesOrNA(m.SwapUsedBytes)+percent(m.SwapUsedBytes, m.SwapTotalBytes))
	return s.end(w)
}

func renderSwaps(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, t(l, "render.status.swaps"))
	switch {
	case st.Swaps.Error != nil:
		s.line("%s", reasonText(l, st.Swaps.Error, nil))
	case len(st.Swaps.Devices) == 0:
		s.line(t(l, "render.status.no_swap"))
	default:
		s.row(t(l, "render.col.path"), t(l, "render.col.type"), t(l, "render.col.size"), t(l, "render.col.used"), t(l, "render.col.priority"), t(l, "render.col.disk"), "RAMPLUS")
		for _, d := range st.Swaps.Devices {
			disk := "—"
			if d.DiskKind != "" {
				disk = kindText(l, d.DiskKind) + ", " + d.Transport
			}
			managed := t(l, "render.no")
			if d.Managed {
				managed = t(l, "render.yes")
			}
			s.row(d.Path, d.Type, humanBytes(d.SizeBytes), humanBytes(d.UsedBytes), fmt.Sprint(d.Priority), disk, managed)
		}
	}
	if err := s.end(w); err != nil {
		return err
	}
	for _, wr := range st.Swaps.Warnings {
		switch wr.Code {
		case monitor.WarnSwapOnUSB:
			fmt.Fprintf(w, t(l, "render.status.warn_usb"), wr.Path)
		case monitor.WarnInterrupted:
			fmt.Fprintf(w, t(l, "render.status.warn_interrupted"), wr.Path)
		default:
			fmt.Fprintf(w, t(l, "render.status.warn_other"), wr.Path, wr.Code)
		}
	}
	if len(st.Swaps.Warnings) > 0 {
		fmt.Fprintln(w)
	}
	return nil
}

func renderZswap(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, "zswap")
	z := st.Zswap
	if !z.Present {
		s.line("%s", reasonText(l, z.Error, map[string]string{detect.ReasonNotPresent: t(l, "render.status.no_zswap")}))
		return s.end(w)
	}
	if z.Error != nil {
		s.kv(t(l, "render.status.error"), reasonText(l, z.Error, nil))
	}
	p := z.Params
	s.kv(t(l, "render.status.enabled"), yesNo(l, p.Enabled))
	s.kv(t(l, "render.status.compressor"), strOrNA(p.Compressor, "n/a"))
	s.kv("Zpool", strOrNA(p.Zpool, t(l, "render.na.no_param")))
	if p.MaxPoolPercent != nil {
		s.kv("max_pool_percent", fmt.Sprintf("%d%%", *p.MaxPoolPercent))
	} else {
		s.kv("max_pool_percent", "n/a")
	}
	if z.PoolBytes != nil || z.StoredBytes != nil {
		s.kv(t(l, "render.status.pool_data"), bytesOrNA(z.PoolBytes)+" / "+bytesOrNA(z.StoredBytes)+" (meminfo)")
	}
	if z.Stats == nil {
		s.kv(t(l, "render.status.stats"), reasonText(l, z.StatsError, map[string]string{
			detect.ReasonNotPresent: t(l, "render.status.no_debugfs"),
		}))
		return s.end(w)
	}
	s.kv(t(l, "render.status.pool_size"), bytesOrNA(z.Stats.PoolTotalSizeBytes))
	s.kv(t(l, "render.status.stored_pages"), uintOrNA(z.Stats.StoredPages))
	s.kv(t(l, "render.status.written_back"), uintOrNA(z.Stats.WrittenBackPages)+t(l, "render.status.pages_unit"))
	s.kv(t(l, "render.status.pool_limit"), uintOrNA(z.Stats.PoolLimitHit)+t(l, "render.status.times_unit"))
	if z.Stats.CompressionRatio != nil {
		s.kv(t(l, "render.status.ratio"), fmt.Sprintf("%.2fx", *z.Stats.CompressionRatio))
	}
	return s.end(w)
}

func uintOrNA(p *uint64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprint(*p)
}

func renderVM(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, "VM (sysctl)")
	vm := st.VM
	val := func(name string, p *int) string {
		if p != nil {
			return fmt.Sprint(*p)
		}
		return reasonText(l, vm.Errors[name], nil)
	}
	s.kv("swappiness", val("swappiness", vm.Swappiness))
	s.kv("vfs_cache_pressure", val("vfs_cache_pressure", vm.VFSCachePressure))
	s.kv("watermark_scale_factor", val("watermark_scale_factor", vm.WatermarkScaleFactor))
	return s.end(w)
}

func renderPSI(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, t(l, "render.status.psi"))
	p := st.PSI
	if p.Error != nil {
		s.line("%s", reasonText(l, p.Error, map[string]string{
			detect.ReasonNotPresent:  t(l, "render.status.no_psi"),
			detect.ReasonUnsupported: t(l, "render.status.psi_off"),
		}))
		return s.end(w)
	}
	s.row("", "avg10", "avg60", "avg300")
	for _, l := range []struct {
		name string
		v    *monitor.PSILine
	}{{"some", p.Some}, {"full", p.Full}} {
		if l.v == nil {
			s.row(l.name, "n/a", "n/a", "n/a")
			continue
		}
		s.row(l.name, fmt.Sprintf("%.2f", l.v.Avg10), fmt.Sprintf("%.2f", l.v.Avg60), fmt.Sprintf("%.2f", l.v.Avg300))
	}
	if p.PressureLevel != "" {
		s.row(t(l, "render.status.assessment"), pressureText(l, p.PressureLevel)+t(l, "render.status.by_some"))
	}
	return s.end(w)
}

func kindText(l i18n.Lang, k string) string {
	switch k {
	case detect.KindNVMe:
		return "NVMe"
	case detect.KindSSD:
		return "SSD"
	case detect.KindHDD:
		return "HDD"
	case detect.KindVirtual:
		return t(l, "render.kind.virtual")
	default:
		return t(l, "render.unknown")
	}
}

// pressureText — интерпретация уровня давления на память.
func pressureText(l i18n.Lang, level string) string {
	switch level {
	case monitor.PressureNormal:
		return t(l, "render.pressure.normal")
	case monitor.PressureElevated:
		return t(l, "render.pressure.elevated")
	case monitor.PressureHigh:
		return t(l, "render.pressure.high")
	case monitor.PressureSevere:
		return t(l, "render.pressure.severe")
	default:
		return "n/a"
	}
}

func renderRootDisk(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, t(l, "render.status.root_disk"))
	rd := st.RootDisk
	if rd.Error != nil {
		s.kv(t(l, "render.status.mount"), reasonText(l, rd.Error, nil))
	} else {
		dev := rd.Source
		if rd.Device != "" && !strings.HasSuffix(rd.Source, "/"+rd.Device) {
			dev += " (" + rd.Device + ")"
		}
		s.kv(t(l, "render.status.device"), dev)
		s.kv(t(l, "render.status.fs"), rd.FSType)
	}
	switch {
	case len(rd.Disks) > 0:
		s.kv(t(l, "render.status.type"), kindText(l, rd.Kind))
		usbRotational := false
		for _, d := range rd.Disks {
			info := []string{kindText(l, d.Kind), d.Transport}
			if d.Model != "" {
				info = append(info, d.Model)
			}
			if d.SizeBytes != nil {
				info = append(info, humanBytes(*d.SizeBytes))
			}
			s.kv(t(l, "render.status.disk_prefix")+d.Name, strings.Join(info, ", "))
			if d.Transport == "usb" && d.Rotational != nil && *d.Rotational {
				usbRotational = true
			}
		}
		if usbRotational {
			s.kv("", t(l, "render.status.usb_note"))
		}
	case rd.DiskError != nil:
		s.kv(t(l, "render.status.type"), reasonText(l, rd.DiskError, map[string]string{detect.ReasonNotPresent: t(l, "render.status.no_block")}))
	default:
		s.kv(t(l, "render.status.type"), "n/a")
	}
	if rd.SpaceError != nil {
		s.kv(t(l, "render.status.free"), reasonText(l, rd.SpaceError, nil))
	} else {
		s.kv(t(l, "render.status.free"), fmt.Sprintf(t(l, "render.status.free_value"),
			bytesOrNA(rd.FreeBytes), bytesOrNA(rd.TotalBytes), bytesOrNA(rd.AvailableBytes)))
	}
	return s.end(w)
}

func renderZram(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, "zram")
	z := st.Zram
	switch {
	case z.Error != nil:
		s.line("%s", reasonText(l, z.Error, nil))
	case len(z.Devices) == 0:
		s.line(t(l, "render.status.no_devices"))
	default:
		s.row(t(l, "render.col.device"), t(l, "render.col.size"), t(l, "render.col.algorithm"), t(l, "render.col.data"), t(l, "render.col.compressed"), t(l, "render.col.memory"), t(l, "render.col.ratio"))
		for _, d := range z.Devices {
			orig, compr, used, ratio := "n/a", "n/a", "n/a", "n/a"
			if d.MMStat != nil {
				orig = humanBytes(d.MMStat.OrigDataBytes)
				compr = humanBytes(d.MMStat.ComprDataBytes)
				used = humanBytes(d.MMStat.MemUsedBytes)
			}
			if d.CompressionRatio != nil {
				ratio = fmt.Sprintf("%.2fx", *d.CompressionRatio)
			}
			s.row(d.Name, bytesOrNA(d.DiskSizeBytes), strOrNA(d.Algorithm, "n/a"), orig, compr, used, ratio)
			if d.Error != nil {
				s.row("", reasonText(l, d.Error, nil))
			}
		}
	}
	return s.end(w)
}

func renderProfile(l i18n.Lang, w io.Writer, st monitor.Status) error {
	s := newSection(w, t(l, "render.status.profile"))
	if st.Profile == nil {
		s.line(t(l, "render.status.profile_none"))
		return s.end(w)
	}
	s.kv(t(l, "render.status.profile_name"), st.Profile.Name)
	s.kv(t(l, "render.status.status"), profile.StatusText(l, st.Profile.Status))
	for k, v := range st.Profile.Overrides {
		s.kv("--set", k+"="+v)
	}
	if !st.Profile.AppliedAt.IsZero() {
		s.kv(t(l, "render.status.applied"), st.Profile.AppliedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if err := s.end(w); err != nil {
		return err
	}
	if len(st.Drift) > 0 {
		var parts []string
		for _, d := range st.Drift {
			parts = append(parts, fmt.Sprintf(t(l, "render.profile.drift_item"), d.Key, d.Actual, d.Expected))
		}
		fmt.Fprintf(w, t(l, "render.status.drift"), strings.Join(parts, "; "))
	}
	return nil
}
