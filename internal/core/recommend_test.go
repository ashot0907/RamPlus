package core

import (
	"context"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/fakesys"
	"github.com/ashot0907/ramplus/internal/swap"
)

// Таблица: RAM, место, тип существующего swap, USB → рекомендация.
func TestRecommend(t *testing.T) {
	type want struct {
		size     uint64 // 0 — swap не рекомендуется
		code     string
		existing string // "" — null
		profile  string
	}
	cases := []struct {
		name string
		prep func(w *fakesys.World)
		want want
	}{
		{"ram 4 → как RAM", func(w *fakesys.World) { w.MemTotal = 4*GiB - 180*swap.MiB }, want{4 * GiB, "ram.small", "", "zswap-lz4"}},
		{"ram 8 → как RAM", func(w *fakesys.World) { w.MemTotal = 8*GiB - 300*swap.MiB }, want{8 * GiB, "ram.small", "", "zswap-lz4"}},
		{"ram 16 → половина", func(w *fakesys.World) { w.MemTotal = 16 * GiB }, want{8 * GiB, "ram.medium", "", "zswap-lz4"}},
		{"ram 32 (31.1) → 16", func(w *fakesys.World) { w.MemTotal = 31*GiB + 100*swap.MiB }, want{16 * GiB, "ram.medium", "", "zswap-lz4"}},
		{"ram 64 → 16", func(w *fakesys.World) { w.MemTotal = 64 * GiB }, want{16 * GiB, "ram.large", "", "zswap-lz4"}},
		{"место ограничивает", func(w *fakesys.World) {
			w.MemTotal, w.Total, w.Avail = 32*GiB, 100*GiB, 15*GiB+512*swap.MiB // запас 10 GiB → до 5.5 GiB
		}, want{5 * GiB, "disk.limited", "", "zswap-lz4"}},
		{"мало места", func(w *fakesys.World) { w.Total, w.Avail = 100*GiB, 10*GiB+512*swap.MiB }, want{0, "disk.too_small", "", "zswap-lz4"}},
		{"swap-файл пуст → replace", func(w *fakesys.World) { w.WithOldSwap(0) }, want{8 * GiB, "ram.medium", "replace", "zswap-lz4"}},
		{"swap-файл занят, RAM мало → keep", func(w *fakesys.World) {
			w.WithOldSwap(7 * GiB)
			w.MemAvail = 1 * GiB
			w.MemTotal = 2 * GiB // новый swap 2 GiB + RAM 1 GiB < 7 GiB
		}, want{2 * GiB, "ram.small", "keep", "zswap-lz4"}},
		{"раздел → keep", func(w *fakesys.World) {
			w.Files["/dev/sda3"] = &fakesys.File{Size: 4 * GiB}
			w.Active["/dev/sda3"] = -2
		}, want{8 * GiB, "ram.medium", "keep", "zswap-lz4"}},
		{"zram → keep, профиль default", func(w *fakesys.World) {
			w.Files["/dev/zram0"] = &fakesys.File{Size: 4 * GiB}
			w.Active["/dev/zram0"] = 100
		}, want{8 * GiB, "ram.medium", "keep", "default"}},
		{"USB → нет swap", func(w *fakesys.World) { w.Transport = "usb" }, want{0, "blocked.usb", "", "zswap-lz4"}},
		{"нет zswap → default", func(w *fakesys.World) {
			for p := range w.Files {
				if strings.HasPrefix(p, "/sys/module/zswap") {
					delete(w.Files, p)
				}
			}
		}, want{8 * GiB, "ram.medium", "", "default"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := fakesys.New().WithKernelParams(false)
			tc.prep(w)
			r := New(w.System()).Recommend(context.Background(), "")
			var size uint64
			if r.SwapSizeBytes != nil {
				size = *r.SwapSizeBytes
			}
			existing := ""
			if r.Existing != nil {
				existing = *r.Existing
			}
			got := want{size, r.SwapReasonCode, existing, r.Profile}
			if got != tc.want {
				t.Errorf("got %+v, want %+v\nreason: %s\nexisting: %s\nprofile: %s", got, tc.want, r.SwapReason, r.ExistingReason, r.ProfileReason)
			}
			if r.SwapSizeBytes == nil && r.SwapReason == "" {
				t.Error("no swap without a reason")
			}
			if tc.want.code == "blocked.usb" && (len(r.SwapBlocked) == 0 || r.SwapBlocked[0].Code != "disk.usb") {
				t.Errorf("blocked: %+v", r.SwapBlocked)
			}
			wantStatus := map[string]string{"zswap-lz4": "experimental", "default": "standard"}[r.Profile]
			if r.ProfileStatus != wantStatus {
				t.Errorf("profile_status %q", r.ProfileStatus)
			}
		})
	}
}

// Рекомендация — то, что план примет: setup с рекомендованными значениями
// не заблокирован.
func TestRecommendIsApplicable(t *testing.T) {
	w := fakesys.New().WithKernelParams(false).WithOldSwap(0)
	c := New(w.System())
	r := c.Recommend(context.Background(), "")
	if r.SwapSizeBytes == nil || r.Existing == nil {
		t.Fatalf("%+v", r)
	}
	notes := map[string]bool{}
	for _, n := range r.Notes {
		notes[n.Code] = true
	}
	if !notes["profile.preliminary"] {
		t.Errorf("experimental profile must be marked preliminary: %+v", r.Notes)
	}
	p := c.SetupPlan(context.Background(), setupReq(*r.SwapSizeBytes, swap.ExistingMode(*r.Existing), r.Profile))
	if p.Blocked() {
		t.Fatalf("recommended setup blocked: %+v", p.Failures())
	}
	if res := c.Apply(context.Background(), p.ID, journalHooksYes()); !res.OK() {
		t.Fatal(res.Status, res.Error)
	}
	// Свой swap уже есть: existing не нужен, размер — для resize.
	r = c.Recommend(context.Background(), "")
	if r.Existing != nil || r.ManagedBytes == nil || *r.ManagedBytes != 8*GiB || r.SwapSizeBytes == nil || *r.SwapSizeBytes != 8*GiB {
		t.Errorf("after setup: %+v", r)
	}
	p = c.SetupPlan(context.Background(), setupReq(*r.SwapSizeBytes, "", r.Profile))
	if len(p.Steps) != 0 {
		t.Errorf("recommended state reached, plan must be empty: %s", kinds(p.Steps))
	}
}

func TestRecommendTargets(t *testing.T) {
	w := fakesys.New().WithKernelParams(false)
	w.Mounts["/mnt/usb"] = fakesys.Mount{FSType: "ext4", Transport: "usb", Kind: "ssd", Total: 100 * GiB, Avail: 90 * GiB}
	w.Files["/mnt"] = &fakesys.File{Dir: true}
	w.Files["/mnt/usb"] = &fakesys.File{Dir: true}
	r := New(w.System()).Recommend(context.Background(), "")
	if len(r.Targets) != 2 || !r.Targets[0].Selected || !r.Targets[0].Usable || r.Targets[1].Usable || r.Targets[1].Problem == "" {
		t.Fatalf("targets %+v", r.Targets)
	}
	r = New(w.System()).Recommend(context.Background(), "/mnt/usb/ramplus.swapfile")
	if r.SwapSizeBytes != nil || r.SwapReasonCode != "blocked.usb" || r.Target.MountPoint != "/mnt/usb" {
		t.Errorf("usb target: %+v %s", r.Target, r.SwapReasonCode)
	}
}
