package swap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/plan"
	system "github.com/ashot0907/ramplus/internal/sys"
)

// Модель плана — internal/plan (здесь — короткие имена для планировщика).
type (
	Step        = plan.Step
	StepKind    = plan.StepKind
	Level       = plan.Level
	Check       = plan.Check
	Choice      = plan.Choice
	Alternative = plan.Alternative
	Action      = plan.Action
)

const (
	LevelOK   = plan.LevelOK
	LevelInfo = plan.LevelInfo
	LevelWarn = plan.LevelWarn
	LevelFail = plan.LevelFail

	ActionRollback = plan.ActionRollback

	KindWriteFile    = plan.KindWriteFile
	KindDeleteConfig = plan.KindDeleteConfig
	KindSystemctl    = plan.KindSystemctl
	KindMkdir        = plan.KindMkdir
	KindRmdir        = plan.KindRmdir
	KindChmod        = plan.KindChmod
)

// Шаги swap.
const (
	KindAllocate          StepKind = "allocate"            // новый файл Path размером Size: fallocate, иначе нули
	KindBtrfsMkswapfile   StepKind = "btrfs_mkswapfile"    // btrfs filesystem mkswapfile --size Size Path
	KindMkswap            StepKind = "mkswap"              // mkswap Path
	KindSwapon            StepKind = "swapon"              // swapon Path с Priority
	KindSwapoff           StepKind = "swapoff"             // swapoff Path
	KindDeleteSwapfile    StepKind = "delete_swapfile"     // удалить swapfile; откат пересоздаёт его
	KindFstabDisable      StepKind = "fstab_disable"       // закомментировать Line в fstab
	KindFstabEnable       StepKind = "fstab_enable"        // раскомментировать Line
	KindChattrNoCOW       StepKind = "chattr_nocow"        // chattr +C Path
	KindBtrfsSubvolCreate StepKind = "btrfs_subvol_create" //
	KindBtrfsSubvolDelete StepKind = "btrfs_subvol_delete" //
	// KindRemoveFile — только из журналов v1 (отмена создания файла).
	// Выполняется обратимо: маленький файл восстанавливается по содержимому,
	// большой (swapfile) — пересозданием.
	KindRemoveFile StepKind = "remove_file"
)

func daemonReload() Step { return plan.DaemonReload() }

// legacyConfigMax — remove_file из журнала v1: файлы до этого размера
// считаются конфигами (юнит), больше — swapfile.
const legacyConfigMax = 1 << 20

// ErrNotOurs — файл не совпадает с журналом: удалять его нельзя.
var ErrNotOurs = journal.ErrNotOurs

// swapSizeSlack — /proc/swaps показывает размер без заголовка (страница)
// и в KiB; допускаем такое расхождение с размером файла.
const swapSizeSlack = 1 << 20

// CheckSwapfile — перед удалением swapfile: обычный файл, размер как в
// журнале, в заголовке сигнатура swap или нули (файл ещё до mkswap).
func CheckSwapfile(sys system.System, s Step) (fs.FileInfo, error) {
	st, err := sys.FS.Stat(s.Path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: не обычный файл: %w", s.Path, ErrNotOurs)
	}
	size := uint64(st.Size())
	switch {
	case s.Size == 0: // размер не записан (журнал v1, старый swap без наблюдения)
	case s.SizeAtMost && size <= s.Size:
	case size != s.Size:
		return nil, fmt.Errorf("%s: размер %s, по журналу %s: %w", s.Path, FormatSize(size), FormatSize(s.Size), ErrNotOurs)
	}
	head, err := sys.FS.ReadPrefix(s.Path, os.Getpagesize())
	if err != nil {
		return nil, err
	}
	if !IsSwapHeader(head) && !allZero(head) {
		return nil, fmt.Errorf("%s: нет сигнатуры swap (SWAPSPACE2) — это не swapfile: %w", s.Path, ErrNotOurs)
	}
	return st, nil
}

// IsSwapHeader — первая страница файла содержит сигнатуру mkswap.
func IsSwapHeader(page []byte) bool {
	if len(page) < 10 {
		return false
	}
	sig := string(page[len(page)-10:])
	return sig == "SWAPSPACE2" || sig == "SWAP-SPACE"
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func fstabEdit(s Step) func(content, line string) (string, error) {
	if s.Kind == KindFstabEnable {
		return EnableFstabLine
	}
	return DisableFstabLine
}

func hasLine(content, line string) bool {
	for _, l := range strings.Split(content, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

func activeSwap(sys system.System, path string) (monitor.SwapDevice, bool) {
	b, err := sys.FS.ReadFile(system.SwapsPath)
	if err != nil {
		return monitor.SwapDevice{}, false
	}
	devs, _ := monitor.ParseSwaps(bytes.NewReader(b))
	for _, d := range devs {
		if d.Path == path {
			return d, true
		}
	}
	return monitor.SwapDevice{}, false
}

// swapExpect — swap (не) активен; при активном — приоритет и размер.
func swapExpect(path string, active bool, priority int, size uint64) journal.Expectation {
	return journal.Expectation{Key: "swap:" + path, Path: path, Check: func(sys system.System) string {
		got, on := activeSwap(sys, path)
		switch {
		case active && !on:
			return fmt.Sprintf("swap %s не активен, а по журналу включён", path)
		case !active && on:
			return fmt.Sprintf("swap %s активен, а по журналу отключён", path)
		case active && priority >= 0 && got.Priority != priority:
			return fmt.Sprintf("swap %s: приоритет %d, по журналу %d", path, got.Priority, priority)
		case active && size > 0 && (got.SizeBytes > size || size-got.SizeBytes > swapSizeSlack):
			return fmt.Sprintf("swap %s: размер %s, по журналу %s", path, FormatSize(got.SizeBytes), FormatSize(size))
		}
		return ""
	}}
}

// fstabExpect — строка fstab закомментирована RamPlus (marked) или активна.
func fstabExpect(line string, marked bool) journal.Expectation {
	return journal.Expectation{Key: "fstab:" + line, Check: func(sys system.System) string {
		b, err := sys.FS.ReadFile(system.FstabPath)
		content := string(b)
		has, plain := hasLine(content, fstabDisabledBy+line), hasLine(content, line)
		switch {
		case err != nil:
			return fmt.Sprintf("%s недоступен: %v", FstabPath, err)
		case marked && (!has || plain):
			return fmt.Sprintf("%s: строка «%s» должна быть закомментирована RamPlus", FstabPath, strings.TrimSpace(line))
		case !marked && (has || !plain):
			return fmt.Sprintf("%s: строка «%s» должна быть активна", FstabPath, strings.TrimSpace(line))
		}
		return ""
	}}
}

func recreateSwapfile(path string, size uint64, nocow bool) []Step {
	return []Step{
		{Kind: KindAllocate, Path: path, Size: size, NoCOW: nocow},
		{Kind: KindChmod, Path: path, Mode: 0o600},
		{Kind: KindMkswap, Path: path},
	}
}

func init() {
	plan.RegisterAction(ActionApply, plan.ActionInfo{Domain: plan.DomainSwap, Title: "создание swap"})
	plan.RegisterAction(ActionResize, plan.ActionInfo{Domain: plan.DomainSwap, Title: "изменение размера swap"})
	plan.RegisterAction(ActionRemove, plan.ActionInfo{Domain: plan.DomainSwap, Title: "удаление swap"})
	createSwapfileVerify := func(e *journal.Env, a journal.Applied) (journal.Verdict, string) {
		s := a.Step
		st, err := e.Sys.FS.Stat(s.Path)
		switch {
		case err != nil:
			return journal.VerdictAbsent, s.Path + " не создан"
		case st.Mode().IsRegular() && st.Mode().Perm() == 0o600 && uint64(st.Size()) <= s.Size:
			return journal.VerdictPresent, fmt.Sprintf("%s: путь, права 600 и размер (%s ≤ %s) совпадают с записью",
				s.Path, FormatSize(uint64(st.Size())), FormatSize(s.Size))
		default:
			return journal.VerdictMismatch, fmt.Sprintf("%s существует, но не похож на файл RamPlus (права %o, размер %s, ожидалось ≤ %s)",
				s.Path, st.Mode().Perm(), FormatSize(uint64(st.Size())), FormatSize(s.Size))
		}
	}
	createSwapfilePrepare := func(e *journal.Env, s Step) ([]Step, string, error) {
		if _, err := e.Sys.FS.Stat(s.Path); err == nil {
			return nil, "", &fs.PathError{Op: "create", Path: s.Path, Err: fs.ErrExist} // чужой файл не трогаем
		}
		return []Step{{Kind: KindDeleteSwapfile, Path: s.Path, Size: s.Size, NoCOW: s.NoCOW}}, "", nil
	}
	createdFile := func(s Step) []journal.Expectation {
		return []journal.Expectation{journal.FileExpect(s.Path, true, false, s.Size, nil)}
	}

	journal.Register(KindAllocate, journal.Handler{
		Describe: func(s Step) string {
			nocow := ""
			if s.NoCOW {
				nocow = ", chattr +C (NOCOW)"
			}
			return fmt.Sprintf("создать %s, %s (fallocate; если ФС не умеет — запись нулями), права 600%s", s.Path, FormatSize(s.Size), nocow)
		},
		Prepare: createSwapfilePrepare,
		Do: func(ctx context.Context, e *journal.Env, s Step) error {
			fsys := e.Sys.FS
			if err := fsys.Create(s.Path, 0o600); err != nil {
				return err // в т.ч. fs.ErrExist: чужой файл не трогаем
			}
			err := func() error {
				if s.NoCOW {
					// +C можно поставить только пустому файлу.
					if err := e.Run(ctx, "chattr", "+C", s.Path); err != nil {
						return err
					}
				}
				err := fsys.Fallocate(s.Path, s.Size)
				if errors.Is(err, system.ErrFallocateUnsupported) {
					err = fsys.WriteZeros(s.Path, s.Size)
				}
				return err
			}()
			if err != nil {
				_ = fsys.Remove(s.Path) // файл создан нами — убираем недописанный
			}
			return err
		},
		Verify: createSwapfileVerify,
		Expect: createdFile,
	})

	journal.Register(KindBtrfsMkswapfile, journal.Handler{
		Describe: func(s Step) string {
			return fmt.Sprintf("btrfs filesystem mkswapfile --size %s %s", sizeFlag(s.Size), s.Path)
		},
		Prepare: createSwapfilePrepare,
		Do: func(ctx context.Context, e *journal.Env, s Step) error {
			err := e.Run(ctx, "btrfs", "filesystem", "mkswapfile", "--size", strconv.FormatUint(s.Size, 10), s.Path)
			if err != nil {
				if _, serr := e.Sys.FS.Stat(s.Path); serr == nil {
					_ = e.Sys.FS.Remove(s.Path)
				}
			}
			return err
		},
		Verify: createSwapfileVerify,
		Expect: createdFile,
	})

	journal.Register(KindMkswap, journal.Handler{
		Describe: func(s Step) string { return "mkswap " + s.Path },
		Do:       func(ctx context.Context, e *journal.Env, s Step) error { return e.Run(ctx, "mkswap", s.Path) },
	})

	journal.Register(KindSwapon, journal.Handler{
		Describe: func(s Step) string {
			if s.Priority < 0 {
				return fmt.Sprintf("swapon %s (приоритет — автоматически)", s.Path)
			}
			return fmt.Sprintf("swapon %s (приоритет %d)", s.Path, s.Priority)
		},
		Prepare: func(_ *journal.Env, s Step) ([]Step, string, error) {
			return []Step{{Kind: KindSwapoff, Path: s.Path, Priority: s.Priority, Size: s.Size}}, "", nil
		},
		Do: func(_ context.Context, e *journal.Env, s Step) error {
			if err := e.Sys.Kernel.Swapon(s.Path, s.Priority); err != nil {
				return fmt.Errorf("swapon %s: %w", s.Path, err)
			}
			return nil
		},
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, string) {
			if _, on := activeSwap(e.Sys, a.Step.Path); on {
				return journal.VerdictPresent, a.Step.Path + " активен"
			}
			return journal.VerdictAbsent, a.Step.Path + " не активен"
		},
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{swapExpect(s.Path, true, s.Priority, s.Size)}
		},
	})

	journal.Register(KindSwapoff, journal.Handler{
		Describe: func(s Step) string { return "swapoff " + s.Path },
		Prepare: func(_ *journal.Env, s Step) ([]Step, string, error) {
			return []Step{{Kind: KindSwapon, Path: s.Path, Priority: s.Priority, Size: s.Size}}, "", nil
		},
		Do: func(_ context.Context, e *journal.Env, s Step) error {
			if err := e.Sys.Kernel.Swapoff(s.Path); err != nil {
				return fmt.Errorf("swapoff %s: %w", s.Path, err)
			}
			return nil
		},
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, string) {
			if _, on := activeSwap(e.Sys, a.Step.Path); on {
				return journal.VerdictAbsent, a.Step.Path + " всё ещё активен"
			}
			return journal.VerdictPresent, a.Step.Path + " отключён"
		},
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{swapExpect(s.Path, false, 0, 0)}
		},
	})

	journal.Register(KindDeleteSwapfile, journal.Handler{
		Describe: func(s Step) string { return fmt.Sprintf("удалить файл %s (%s)", s.Path, FormatSize(s.Size)) },
		Prepare: func(e *journal.Env, s Step) ([]Step, string, error) {
			st, err := CheckSwapfile(e.Sys, s)
			if err != nil {
				return nil, "", err
			}
			// Содержимое swap после swapoff не нужно — пустой swapfile того же
			// размера эквивалентен исходному.
			return recreateSwapfile(s.Path, uint64(st.Size()), s.NoCOW), "", nil
		},
		Do: func(_ context.Context, e *journal.Env, s Step) error { return e.Sys.FS.Remove(s.Path) },
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, string) {
			s := a.Step
			st, err := e.Sys.FS.Stat(s.Path)
			switch {
			case err != nil:
				return journal.VerdictPresent, s.Path + " удалён"
			case s.Size == 0 || uint64(st.Size()) == s.Size:
				return journal.VerdictAbsent, s.Path + " на месте"
			default:
				return journal.VerdictMismatch, fmt.Sprintf("%s: размер %s, в записи %s", s.Path, FormatSize(uint64(st.Size())), FormatSize(s.Size))
			}
		},
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{journal.FileExpect(s.Path, false, false, 0, nil)}
		},
		Recovered: func(s Step) Step {
			s.SizeAtMost = true // файл мог остаться недописанным
			return s
		},
	})

	fstabHandler := journal.Handler{
		Describe: func(s Step) string {
			if s.Kind == KindFstabEnable {
				return fmt.Sprintf("вернуть в %s строку %q", s.Path, strings.TrimSpace(s.Line))
			}
			return fmt.Sprintf("закомментировать в %s строку %q (бэкап — в %s)", s.Path, strings.TrimSpace(s.Line), BackupDir)
		},
		Prepare: func(e *journal.Env, s Step) ([]Step, string, error) {
			content, err := e.Sys.FS.ReadFile(s.Path)
			if err != nil {
				return nil, "", err
			}
			if _, err := fstabEdit(s)(string(content), s.Line); err != nil {
				return nil, "", err
			}
			backup, err := e.Backup(s.Path, content)
			if err != nil {
				return nil, "", err
			}
			inverse := KindFstabEnable
			if s.Kind == KindFstabEnable {
				inverse = KindFstabDisable
			}
			return []Step{{Kind: inverse, Path: s.Path, Line: s.Line, Reload: s.Reload}}, backup, nil
		},
		Do: func(_ context.Context, e *journal.Env, s Step) error {
			content, err := e.Sys.FS.ReadFile(s.Path)
			if err != nil {
				return err
			}
			updated, err := fstabEdit(s)(string(content), s.Line)
			if err != nil {
				return err
			}
			return e.Sys.FS.WriteFileAtomic(s.Path, []byte(updated), e.Perm(s.Path, 0o644))
		},
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, string) {
			s := a.Step
			b, err := e.Sys.FS.ReadFile(s.Path)
			if err != nil {
				return journal.VerdictMismatch, s.Path + " недоступен"
			}
			cur := string(b)
			marked, plain := hasLine(cur, fstabDisabledBy+s.Line), hasLine(cur, s.Line)
			disabled, enabled := marked && !plain, plain && !marked
			switch {
			case (s.Kind == KindFstabDisable && disabled) || (s.Kind == KindFstabEnable && enabled):
				return journal.VerdictPresent, "строка в " + s.Path + " изменена"
			case (s.Kind == KindFstabDisable && enabled) || (s.Kind == KindFstabEnable && disabled):
				return journal.VerdictAbsent, "строка в " + s.Path + " не изменена"
			default:
				return journal.VerdictMismatch, "строка «" + strings.TrimSpace(s.Line) + "» в " + s.Path + " не в ожидаемом виде"
			}
		},
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{fstabExpect(s.Line, s.Kind == KindFstabDisable)}
		},
	}
	journal.Register(KindFstabDisable, fstabHandler)
	journal.Register(KindFstabEnable, fstabHandler)

	journal.Register(KindChattrNoCOW, journal.Handler{
		Describe: func(s Step) string {
			return "chattr +C " + s.Path + " (NOCOW: обязательно для swap на btrfs)"
		},
		Do: func(ctx context.Context, e *journal.Env, s Step) error { return e.Run(ctx, "chattr", "+C", s.Path) },
	})

	journal.Register(KindBtrfsSubvolCreate, journal.Handler{
		Describe: func(s Step) string {
			return "btrfs subvolume create " + s.Path + " (отдельный подтом: снапшоты корня не ломаются)"
		},
		Prepare: func(_ *journal.Env, s Step) ([]Step, string, error) {
			return []Step{{Kind: KindBtrfsSubvolDelete, Path: s.Path}}, "", nil
		},
		Do: func(ctx context.Context, e *journal.Env, s Step) error {
			return e.Run(ctx, "btrfs", "subvolume", "create", s.Path)
		},
		Verify: journal.VerifyCreated,
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{journal.FileExpect(s.Path, true, true, 0, nil)}
		},
	})

	journal.Register(KindBtrfsSubvolDelete, journal.Handler{
		Describe: func(s Step) string { return "btrfs subvolume delete " + s.Path },
		Prepare: func(e *journal.Env, s Step) ([]Step, string, error) {
			// btrfs subvolume delete удаляет вместе с содержимым — только пустой.
			if rest, err := e.Sys.FS.Glob(filepath.Join(s.Path, "*")); err != nil {
				return nil, "", err
			} else if len(rest) > 0 {
				return nil, "", fmt.Errorf("%s не пуст (%s) — подтом не удаляется", s.Path, strings.Join(rest, ", "))
			}
			return []Step{{Kind: KindBtrfsSubvolCreate, Path: s.Path}, {Kind: KindChattrNoCOW, Path: s.Path}}, "", nil
		},
		Do: func(ctx context.Context, e *journal.Env, s Step) error {
			return e.Run(ctx, "btrfs", "subvolume", "delete", s.Path)
		},
		Verify: journal.VerifyRemoved,
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{journal.FileExpect(s.Path, false, false, 0, nil)}
		},
	})

	journal.Register(KindRemoveFile, journal.Handler{ // журнал v1
		Describe: func(s Step) string { return "удалить " + s.Path },
		Prepare: func(e *journal.Env, s Step) ([]Step, string, error) {
			st, err := e.Sys.FS.Stat(s.Path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return nil, "", nil // уже удалён — шаг ничего не сделает
			case err != nil:
				return nil, "", err
			case st.Size() > legacyConfigMax:
				if _, err := CheckSwapfile(e.Sys, s); err != nil {
					return nil, "", err
				}
				return recreateSwapfile(s.Path, uint64(st.Size()), false), "", nil
			default:
				prev, err := e.Sys.FS.ReadFile(s.Path)
				if err != nil {
					return nil, "", err
				}
				if err := journal.CheckConfig(s, string(prev)); err != nil {
					return nil, "", err
				}
				backup, err := e.Backup(s.Path, prev)
				if err != nil {
					return nil, "", err
				}
				return []Step{{Kind: KindWriteFile, Path: s.Path, Content: string(prev), Mode: uint32(st.Mode().Perm())}}, backup, nil
			}
		},
		Do: func(_ context.Context, e *journal.Env, s Step) error {
			if err := e.Sys.FS.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		},
		Verify: journal.VerifyRemoved,
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{journal.FileExpect(s.Path, false, false, 0, nil)}
		},
	})
}
