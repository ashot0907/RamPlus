package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ashot0907/ramplus/internal/monitor"
	"github.com/ashot0907/ramplus/internal/swap"
)

// Тесты CLI профилей не вызывают apply/reset: даже под root ничего не меняется.

func TestProfileCommandsRegistered(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{{"profile", "list"}, {"profile", "show"}, {"profile", "plan"}, {"profile", "apply"}, {"profile", "current"}, {"profile", "reset"}} {
		if cmd, _, err := root.Find(path); err != nil || cmd.Name() != path[1] {
			t.Errorf("%v: %v", path, err)
		}
	}
	apply, _, _ := root.Find([]string{"profile", "apply"})
	for _, flag := range []string{"set", "force", "experimental", "yes", "dry-run", "verbose"} {
		if apply.Flags().Lookup(flag) == nil {
			t.Errorf("apply lacks --%s", flag)
		}
	}
}

func TestProfileFlagsValidation(t *testing.T) {
	if _, err := (&profileFlags{set: []string{"vm.swappiness=300"}}).request("default"); err == nil {
		t.Error("swappiness 300 must be refused")
	}
	if _, err := (&profileFlags{set: []string{"vm.page-cluster=11"}}).request("default"); err == nil {
		t.Error("page-cluster 11 must be refused")
	}
	r, err := (&profileFlags{set: []string{"vm.swappiness=80"}, experimental: true}).request("zswap-lz4")
	if err != nil || r.Set["vm.swappiness"] != "80" || !r.Experimental {
		t.Errorf("request = %+v, %v", r, err)
	}
}

func TestExperimentalConfirmation(t *testing.T) {
	exp := swap.Plan{Profile: &swap.ProfilePlan{Experimental: true}}
	std := swap.Plan{Profile: &swap.ProfilePlan{}}
	var out bytes.Buffer
	if _, err := profileConfirmer(true, false, strings.NewReader(""), &out)(exp); !errors.Is(err, ErrExperimentalNotConfirmed) {
		t.Errorf("--yes without --experimental: %v", err)
	}
	if ok, err := profileConfirmer(true, true, strings.NewReader(""), &out)(exp); !ok || err != nil {
		t.Errorf("--yes --experimental: %v %v", ok, err)
	}
	if _, err := profileConfirmer(false, false, strings.NewReader("yes\n"), &out)(exp); !errors.Is(err, ErrExperimentalNotConfirmed) {
		t.Errorf("no terminal: %v", err)
	}
	if ok, err := profileConfirmer(true, false, strings.NewReader(""), &out)(std); !ok || err != nil {
		t.Errorf("standard profile with --yes: %v %v", ok, err)
	}
}

func TestStatusProfileSection(t *testing.T) {
	var out bytes.Buffer
	st := monitor.Status{
		Profile: &monitor.ProfileInfo{Name: "zswap-lz4", Status: "experimental", AppliedAt: time.Date(2026, 11, 17, 12, 0, 0, 0, time.Local)},
		Drift:   []monitor.DriftItem{{Key: "vm.swappiness", Expected: "60", Actual: "80"}},
	}
	if err := renderProfile(&out, st); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"zswap-lz4", "экспериментальный", "ВНИМАНИЕ: значения изменены вручную", "vm.swappiness = 80 (профиль: 60)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	renderProfile(&out, monitor.Status{})
	if !strings.Contains(out.String(), "не применялся") {
		t.Errorf("no profile:\n%s", out.String())
	}
}

func TestRenderCurrent(t *testing.T) {
	pf := swap.ProfileFacts{Values: map[string]string{"vm.swappiness": "80", "zswap.enabled": "Y"}}
	pf.State.Profile = &swap.AppliedProfile{Name: "default", Status: "modified", Values: map[string]string{"vm.swappiness": "80", "zswap.enabled": "N"}}
	pf.State.Baseline = &swap.Baseline{Values: map[string]string{"vm.swappiness": "60", "zswap.enabled": "N"}}
	var out bytes.Buffer
	if err := renderCurrent(&out, pf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"изменённый пользователем", "Отличия от исходных значений", "vm.swappiness: 60 → 80", "zswap.enabled: N → Y",
		"ВНИМАНИЕ: значения изменены вручную после apply: zswap.enabled = Y (профиль: N)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
}
