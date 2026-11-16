package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/detect"
	"github.com/ashot0907/ramplus/internal/monitor"
)

// reasonText — человекочитаемые причины n/a; context переопределяет общие.
func reasonText(u *detect.Unavailable, context map[string]string) string {
	if u == nil {
		return "n/a"
	}
	if t, ok := context[u.Reason]; ok {
		return "n/a (" + t + ")"
	}
	switch u.Reason {
	case detect.ReasonPermissionDenied:
		return "n/a (нужен root)"
	case detect.ReasonNotPresent:
		return "n/a (нет в системе)"
	case detect.ReasonUnsupported:
		return "n/a (не поддерживается ядром)"
	case detect.ReasonParseError:
		return "n/a (неожиданный формат)"
	default:
		return "n/a (недоступно)"
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

func yesNo(p *bool) string {
	switch {
	case p == nil:
		return "n/a"
	case *p:
		return "да"
	default:
		return "нет"
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

func renderStatus(w io.Writer, st monitor.Status) error {
	renders := []func(io.Writer, monitor.Status) error{
		renderSystem, renderMemory, renderSwaps, renderZswap,
		renderVM, renderPSI, renderRootDisk, renderZram,
	}
	for _, r := range renders {
		if err := r(w, st); err != nil {
			return err
		}
	}
	return nil
}

func renderSystem(w io.Writer, st monitor.Status) error {
	s := newSection(w, "Система")
	sys := st.System
	s.kv("Хост", strOrNA(&sys.Hostname, "n/a"))
	kernel := strOrNA(&sys.Kernel, "n/a")
	if sys.Arch != "" {
		kernel += " (" + sys.Arch + ")"
	}
	s.kv("Ядро", kernel)
	if sys.OS != nil {
		s.kv("Дистрибутив", sys.OS.PrettyName)
	} else {
		s.kv("Дистрибутив", reasonText(sys.OSError, nil))
	}
	return s.end(w)
}

func renderMemory(w io.Writer, st monitor.Status) error {
	s := newSection(w, "Память")
	m := st.Memory
	if m.Error != nil {
		s.line("%s", reasonText(m.Error, nil))
		return s.end(w)
	}
	s.kv("Всего", bytesOrNA(m.TotalBytes))
	s.kv("Доступно", bytesOrNA(m.AvailableBytes)+percent(m.AvailableBytes, m.TotalBytes))
	s.kv("Используется", bytesOrNA(m.UsedBytes)+percent(m.UsedBytes, m.TotalBytes))
	s.kv("Кэш (Cached)", bytesOrNA(m.CachedBytes))
	s.kv("Swap всего", bytesOrNA(m.SwapTotalBytes))
	s.kv("Swap свободно", bytesOrNA(m.SwapFreeBytes))
	s.kv("Swap занято", bytesOrNA(m.SwapUsedBytes)+percent(m.SwapUsedBytes, m.SwapTotalBytes))
	return s.end(w)
}

func renderSwaps(w io.Writer, st monitor.Status) error {
	s := newSection(w, "Swap-устройства")
	switch {
	case st.Swaps.Error != nil:
		s.line("%s", reasonText(st.Swaps.Error, nil))
	case len(st.Swaps.Devices) == 0:
		s.line("swap не включён")
	default:
		s.row("ПУТЬ", "ТИП", "РАЗМЕР", "ИСПОЛЬЗОВАНО", "ПРИОРИТЕТ", "ДИСК", "RAMPLUS")
		for _, d := range st.Swaps.Devices {
			disk := "—"
			if d.DiskKind != "" {
				disk = kindText(d.DiskKind) + ", " + d.Transport
			}
			managed := "нет"
			if d.Managed {
				managed = "да"
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
			fmt.Fprintf(w, "  ВНИМАНИЕ: %s на USB-диске — swap медленный и опасный при отключении диска\n", wr.Path)
		default:
			fmt.Fprintf(w, "  ВНИМАНИЕ: %s: %s\n", wr.Path, wr.Code)
		}
	}
	if len(st.Swaps.Warnings) > 0 {
		fmt.Fprintln(w)
	}
	return nil
}

func renderZswap(w io.Writer, st monitor.Status) error {
	s := newSection(w, "zswap")
	z := st.Zswap
	if !z.Present {
		s.line("%s", reasonText(z.Error, map[string]string{detect.ReasonNotPresent: "zswap нет в ядре"}))
		return s.end(w)
	}
	if z.Error != nil {
		s.kv("Ошибка", reasonText(z.Error, nil))
	}
	p := z.Params
	s.kv("Включён", yesNo(p.Enabled))
	s.kv("Компрессор", strOrNA(p.Compressor, "n/a"))
	s.kv("Zpool", strOrNA(p.Zpool, "n/a (параметра нет в этом ядре)"))
	if p.MaxPoolPercent != nil {
		s.kv("max_pool_percent", fmt.Sprintf("%d%%", *p.MaxPoolPercent))
	} else {
		s.kv("max_pool_percent", "n/a")
	}
	if z.PoolBytes != nil || z.StoredBytes != nil {
		s.kv("Пул / данные", bytesOrNA(z.PoolBytes)+" / "+bytesOrNA(z.StoredBytes)+" (meminfo)")
	}
	if z.Stats == nil {
		s.kv("Статистика", reasonText(z.StatsError, map[string]string{
			detect.ReasonNotPresent: "debugfs не смонтирован",
		}))
		return s.end(w)
	}
	s.kv("Размер пула", bytesOrNA(z.Stats.PoolTotalSizeBytes))
	s.kv("Сохранено страниц", uintOrNA(z.Stats.StoredPages))
	s.kv("Вытеснено на диск", uintOrNA(z.Stats.WrittenBackPages)+" стр.")
	s.kv("Лимит пула достигнут", uintOrNA(z.Stats.PoolLimitHit)+" раз")
	if z.Stats.CompressionRatio != nil {
		s.kv("Степень сжатия", fmt.Sprintf("%.2fx", *z.Stats.CompressionRatio))
	}
	return s.end(w)
}

func uintOrNA(p *uint64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprint(*p)
}

func renderVM(w io.Writer, st monitor.Status) error {
	s := newSection(w, "VM (sysctl)")
	vm := st.VM
	val := func(name string, p *int) string {
		if p != nil {
			return fmt.Sprint(*p)
		}
		return reasonText(vm.Errors[name], nil)
	}
	s.kv("swappiness", val("swappiness", vm.Swappiness))
	s.kv("vfs_cache_pressure", val("vfs_cache_pressure", vm.VFSCachePressure))
	s.kv("watermark_scale_factor", val("watermark_scale_factor", vm.WatermarkScaleFactor))
	return s.end(w)
}

func renderPSI(w io.Writer, st monitor.Status) error {
	s := newSection(w, "PSI memory (% времени в ожидании памяти)")
	p := st.PSI
	if p.Error != nil {
		s.line("%s", reasonText(p.Error, map[string]string{
			detect.ReasonNotPresent:  "ядро без PSI",
			detect.ReasonUnsupported: "PSI отключён (psi=0)",
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
	return s.end(w)
}

func kindText(k string) string {
	switch k {
	case detect.KindNVMe:
		return "NVMe"
	case detect.KindSSD:
		return "SSD"
	case detect.KindHDD:
		return "HDD"
	default:
		return "неизвестно"
	}
}

func renderRootDisk(w io.Writer, st monitor.Status) error {
	s := newSection(w, "Корневой диск")
	rd := st.RootDisk
	if rd.Error != nil {
		s.kv("Монтирование", reasonText(rd.Error, nil))
	} else {
		dev := rd.Source
		if rd.Device != "" && !strings.HasSuffix(rd.Source, "/"+rd.Device) {
			dev += " (" + rd.Device + ")"
		}
		s.kv("Устройство", dev)
		s.kv("ФС", rd.FSType)
	}
	switch {
	case len(rd.Disks) > 0:
		s.kv("Тип", kindText(rd.Kind))
		usbRotational := false
		for _, d := range rd.Disks {
			info := []string{kindText(d.Kind), d.Transport}
			if d.Model != "" {
				info = append(info, d.Model)
			}
			if d.SizeBytes != nil {
				info = append(info, humanBytes(*d.SizeBytes))
			}
			s.kv("Диск "+d.Name, strings.Join(info, ", "))
			if d.Transport == "usb" && d.Rotational != nil && *d.Rotational {
				usbRotational = true
			}
		}
		if usbRotational {
			s.kv("", "примечание: USB-мосты часто сообщают rotational=1 и для SSD")
		}
	case rd.DiskError != nil:
		s.kv("Тип", reasonText(rd.DiskError, map[string]string{detect.ReasonNotPresent: "нет блочного устройства"}))
	default:
		s.kv("Тип", "n/a")
	}
	if rd.SpaceError != nil {
		s.kv("Свободно", reasonText(rd.SpaceError, nil))
	} else {
		s.kv("Свободно", fmt.Sprintf("%s из %s (доступно без root: %s)",
			bytesOrNA(rd.FreeBytes), bytesOrNA(rd.TotalBytes), bytesOrNA(rd.AvailableBytes)))
	}
	return s.end(w)
}

func renderZram(w io.Writer, st monitor.Status) error {
	s := newSection(w, "zram")
	z := st.Zram
	switch {
	case z.Error != nil:
		s.line("%s", reasonText(z.Error, nil))
	case len(z.Devices) == 0:
		s.line("устройств нет")
	default:
		s.row("УСТРОЙСТВО", "РАЗМЕР", "АЛГОРИТМ", "ДАННЫЕ", "СЖАТО", "ПАМЯТЬ", "СЖАТИЕ")
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
				s.row("", reasonText(d.Error, nil))
			}
		}
	}
	return s.end(w)
}
