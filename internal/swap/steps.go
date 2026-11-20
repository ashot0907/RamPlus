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

	"github.com/ashot0907/ramplus/internal/i18n"
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
		return nil, i18n.Errorf("swap.err.not_regular", s.Path, ErrNotOurs)
	}
	size := uint64(st.Size())
	switch {
	case s.Size == 0: // размер не записан (журнал v1, старый swap без наблюдения)
	case s.SizeAtMost && size <= s.Size:
	case size != s.Size:
		return nil, i18n.Errorf("swap.err.size_mismatch", s.Path, FormatSize(size), FormatSize(s.Size), ErrNotOurs)
	}
	head, err := sys.FS.ReadPrefix(s.Path, os.Getpagesize())
	if err != nil {
		return nil, err
	}
	if !IsSwapHeader(head) && !allZero(head) {
		return nil, i18n.Errorf("swap.err.no_signature", s.Path, ErrNotOurs)
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
	return journal.Expectation{Key: "swap:" + path, Path: path, Check: func(sys system.System) i18n.Text {
		got, on := activeSwap(sys, path)
		switch {
		case active && !on:
			return i18n.Msg("expect.swap_inactive", path)
		case !active && on:
			return i18n.Msg("expect.swap_active", path)
		case active && priority >= 0 && got.Priority != priority:
			return i18n.Msg("expect.swap_priority", path, got.Priority, priority)
		case active && size > 0 && (got.SizeBytes > size || size-got.SizeBytes > swapSizeSlack):
			return i18n.Msg("expect.swap_size", path, FormatSize(got.SizeBytes), FormatSize(size))
		}
		return nil
	}}
}

// fstabExpect — строка fstab закомментирована RamPlus (marked) или активна.
func fstabExpect(line string, marked bool) journal.Expectation {
	return journal.Expectation{Key: "fstab:" + line, Check: func(sys system.System) i18n.Text {
		b, err := sys.FS.ReadFile(system.FstabPath)
		content := string(b)
		has, plain := hasLine(content, fstabDisabledBy+line), hasLine(content, line)
		switch {
		case err != nil:
			return i18n.Msg("expect.fstab_unreadable", FstabPath, err)
		case marked && (!has || plain):
			return i18n.Msg("expect.fstab_should_be_disabled", FstabPath, strings.TrimSpace(line))
		case !marked && (has || !plain):
			return i18n.Msg("expect.fstab_should_be_active", FstabPath, strings.TrimSpace(line))
		}
		return nil
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
	plan.RegisterAction(ActionApply, plan.ActionInfo{Domain: plan.DomainSwap})
	plan.RegisterAction(ActionResize, plan.ActionInfo{Domain: plan.DomainSwap})
	plan.RegisterAction(ActionRemove, plan.ActionInfo{Domain: plan.DomainSwap})
	createSwapfileVerify := func(e *journal.Env, a journal.Applied) (journal.Verdict, i18n.Text) {
		s := a.Step
		st, err := e.Sys.FS.Stat(s.Path)
		switch {
		case err != nil:
			return journal.VerdictAbsent, i18n.Msg("verify.not_created", s.Path)
		case st.Mode().IsRegular() && st.Mode().Perm() == 0o600 && uint64(st.Size()) <= s.Size:
			return journal.VerdictPresent, i18n.Msg("verify.swapfile_matches", s.Path, FormatSize(uint64(st.Size())), FormatSize(s.Size))
		default:
			return journal.VerdictMismatch, i18n.Msg("verify.swapfile_foreign", s.Path, uint32(st.Mode().Perm()), FormatSize(uint64(st.Size())), FormatSize(s.Size))
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
		Describe: func(s Step) i18n.Text {
			nocow := ""
			if s.NoCOW {
				nocow = ", chattr +C (NOCOW)"
			}
			return i18n.Msg("step.allocate", s.Path, FormatSize(s.Size), nocow)
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
		Describe: func(s Step) i18n.Text {
			return i18n.Raw(fmt.Sprintf("btrfs filesystem mkswapfile --size %s %s", sizeFlag(s.Size), s.Path))
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
		Describe: func(s Step) i18n.Text { return i18n.Raw("mkswap " + s.Path) },
		Do:       func(ctx context.Context, e *journal.Env, s Step) error { return e.Run(ctx, "mkswap", s.Path) },
	})

	journal.Register(KindSwapon, journal.Handler{
		Describe: func(s Step) i18n.Text {
			if s.Priority < 0 {
				return i18n.Msg("step.swapon_auto", s.Path)
			}
			return i18n.Msg("step.swapon", s.Path, s.Priority)
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
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, i18n.Text) {
			if _, on := activeSwap(e.Sys, a.Step.Path); on {
				return journal.VerdictPresent, i18n.Msg("verify.swap_active", a.Step.Path)
			}
			return journal.VerdictAbsent, i18n.Msg("verify.swap_inactive", a.Step.Path)
		},
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{swapExpect(s.Path, true, s.Priority, s.Size)}
		},
	})

	journal.Register(KindSwapoff, journal.Handler{
		Describe: func(s Step) i18n.Text { return i18n.Raw("swapoff " + s.Path) },
		Prepare: func(_ *journal.Env, s Step) ([]Step, string, error) {
			return []Step{{Kind: KindSwapon, Path: s.Path, Priority: s.Priority, Size: s.Size}}, "", nil
		},
		Do: func(_ context.Context, e *journal.Env, s Step) error {
			if err := e.Sys.Kernel.Swapoff(s.Path); err != nil {
				return fmt.Errorf("swapoff %s: %w", s.Path, err)
			}
			return nil
		},
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, i18n.Text) {
			if _, on := activeSwap(e.Sys, a.Step.Path); on {
				return journal.VerdictAbsent, i18n.Msg("verify.swap_still_active", a.Step.Path)
			}
			return journal.VerdictPresent, i18n.Msg("verify.swap_off", a.Step.Path)
		},
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{swapExpect(s.Path, false, 0, 0)}
		},
	})

	journal.Register(KindDeleteSwapfile, journal.Handler{
		Describe: func(s Step) i18n.Text { return i18n.Msg("step.delete_swapfile", s.Path, FormatSize(s.Size)) },
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
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, i18n.Text) {
			s := a.Step
			st, err := e.Sys.FS.Stat(s.Path)
			switch {
			case err != nil:
				return journal.VerdictPresent, i18n.Msg("verify.removed", s.Path)
			case s.Size == 0 || uint64(st.Size()) == s.Size:
				return journal.VerdictAbsent, i18n.Msg("verify.in_place", s.Path)
			default:
				return journal.VerdictMismatch, i18n.Msg("verify.size_mismatch", s.Path, FormatSize(uint64(st.Size())), FormatSize(s.Size))
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
		Describe: func(s Step) i18n.Text {
			if s.Kind == KindFstabEnable {
				return i18n.Msg("step.fstab_enable", s.Path, strings.TrimSpace(s.Line))
			}
			return i18n.Msg("step.fstab_disable", s.Path, strings.TrimSpace(s.Line), BackupDir)
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
		Verify: func(e *journal.Env, a journal.Applied) (journal.Verdict, i18n.Text) {
			s := a.Step
			b, err := e.Sys.FS.ReadFile(s.Path)
			if err != nil {
				return journal.VerdictMismatch, i18n.Msg("verify.unavailable", s.Path)
			}
			cur := string(b)
			marked, plain := hasLine(cur, fstabDisabledBy+s.Line), hasLine(cur, s.Line)
			disabled, enabled := marked && !plain, plain && !marked
			switch {
			case (s.Kind == KindFstabDisable && disabled) || (s.Kind == KindFstabEnable && enabled):
				return journal.VerdictPresent, i18n.Msg("verify.line_changed", s.Path)
			case (s.Kind == KindFstabDisable && enabled) || (s.Kind == KindFstabEnable && disabled):
				return journal.VerdictAbsent, i18n.Msg("verify.line_unchanged", s.Path)
			default:
				return journal.VerdictMismatch, i18n.Msg("verify.line_unexpected", strings.TrimSpace(s.Line), s.Path)
			}
		},
		Expect: func(s Step) []journal.Expectation {
			return []journal.Expectation{fstabExpect(s.Line, s.Kind == KindFstabDisable)}
		},
	}
	journal.Register(KindFstabDisable, fstabHandler)
	journal.Register(KindFstabEnable, fstabHandler)

	journal.Register(KindChattrNoCOW, journal.Handler{
		Describe: func(s Step) i18n.Text { return i18n.Msg("step.chattr_nocow", s.Path) },
		Do:       func(ctx context.Context, e *journal.Env, s Step) error { return e.Run(ctx, "chattr", "+C", s.Path) },
	})

	journal.Register(KindBtrfsSubvolCreate, journal.Handler{
		Describe: func(s Step) i18n.Text { return i18n.Msg("step.btrfs_subvol_create", s.Path) },
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
		Describe: func(s Step) i18n.Text { return i18n.Raw("btrfs subvolume delete " + s.Path) },
		Prepare: func(e *journal.Env, s Step) ([]Step, string, error) {
			// btrfs subvolume delete удаляет вместе с содержимым — только пустой.
			if rest, err := e.Sys.FS.Glob(filepath.Join(s.Path, "*")); err != nil {
				return nil, "", err
			} else if len(rest) > 0 {
				return nil, "", i18n.Errorf("swap.err.subvol_not_empty", s.Path, strings.Join(rest, ", "))
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
		Describe: func(s Step) i18n.Text { return i18n.Msg("step.delete_config", s.Path) },
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
