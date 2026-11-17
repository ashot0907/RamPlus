package main

import (
	"testing"

	"github.com/ashot0907/ramplus/internal/swap"
)

// Тесты CLI не вызывают apply/resize/remove/rollback: даже под root
// (sudo go test) они не должны менять систему.
func TestSwapFlagsOptions(t *testing.T) {
	f := swapFlags{size: "12G", existing: "replace", deleteOld: true}
	o, err := f.options(swap.ActionApply)
	if err != nil {
		t.Fatal(err)
	}
	if o.Size != 12*swap.GiB || o.Existing != swap.ExistingReplace || !o.DeleteOld {
		t.Errorf("options = %+v", o)
	}
	for _, bad := range []swapFlags{{size: "12X"}, {existing: "maybe"}} {
		if _, err := bad.options(swap.ActionApply); err == nil {
			t.Errorf("%+v: want error", bad)
		}
	}
	if o, _ := (&swapFlags{}).options(swap.ActionApply); o.Existing != "" {
		t.Error("unspecified --existing must stay empty (planner reports the default)")
	}
}

func TestSwapCommandsRegistered(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{{"swap", "plan"}, {"swap", "apply"}, {"swap", "resize"}, {"swap", "remove"}, {"swap", "check-clean"}, {"rollback"}} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Errorf("%v: %v", path, err)
		}
	}
	apply, _, _ := root.Find([]string{"swap", "apply"})
	for _, flag := range []string{"size", "existing", "delete-old", "force", "yes", "dry-run"} {
		if apply.Flags().Lookup(flag) == nil {
			t.Errorf("apply lacks --%s", flag)
		}
	}
}

func TestPlanFlags(t *testing.T) {
	root := newRootCmd()
	plan, _, _ := root.Find([]string{"swap", "plan"})
	for _, flag := range []string{"size", "path", "existing", "delete-old", "force", "verbose"} {
		if plan.Flags().Lookup(flag) == nil {
			t.Errorf("plan lacks --%s", flag)
		}
	}
	resize, _, _ := root.Find([]string{"swap", "resize"})
	if resize.Flags().Lookup("path") != nil {
		t.Error("--path is only for apply/plan")
	}
	o, _ := (&swapFlags{size: "4G", path: "/mnt/nvme/ramplus.swapfile"}).options(swap.ActionApply)
	if o.Path != "/mnt/nvme/ramplus.swapfile" {
		t.Errorf("path = %q", o.Path)
	}
}
