package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/plan"
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
	exp := &plan.Plan{Profile: &plan.ProfileView{Experimental: true}}
	std := &plan.Plan{Profile: &plan.ProfileView{}}
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
