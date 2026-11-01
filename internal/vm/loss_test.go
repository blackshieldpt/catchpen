package vm

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/blackshieldpt/catchpen/internal/qemu"
)

// What each check reports, for a stopped VM with snapshots, one without, a
// running one, one whose state file is corrupt or missing, and one without
// a disk.
func TestLoss(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	offlineVM(t, m, "v1")
	for _, s := range []string{"s1", "s2"} {
		if _, err := m.Snapshot(ctx, "v1", s, false); err != nil {
			t.Fatal(err)
		}
	}
	offlineVM(t, m, "v2")
	check := func(what string, got Loss, err error, disk bool, snaps []string, running bool) {
		t.Helper()
		if err != nil || got.Disk != disk || !slices.Equal(got.Snapshots, snaps) || got.Running != running {
			t.Errorf("%s: %+v, %v", what, got, err)
		}
	}
	l, err := m.RebaseLoss("v1")
	check("rebase v1", l, err, false, []string{"s1", "s2"}, false)
	l, err = m.ResetLoss("v1")
	check("reset v1", l, err, true, []string{"s1", "s2"}, false)
	l, err = m.DestroyLoss("v1")
	check("destroy v1", l, err, true, []string{"s1", "s2"}, false)
	l, err = m.RebaseLoss("v2")
	check("rebase v2", l, err, false, nil, false)

	fakeQEMU(t, m, "v2", 512)
	l, err = m.RebaseLoss("v2")
	check("rebase running v2", l, err, false, nil, true)
	l, err = m.ResetLoss("v2")
	check("reset running v2", l, err, true, nil, true)
	l, err = m.DestroyLoss("v2")
	check("destroy running v2", l, err, true, nil, true)

	// A corrupt state: only DestroyLoss answers, as only Destroy works.
	if err := os.WriteFile(m.vm("v1").path(stateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for what, f := range map[string]func(string) (Loss, error){"rebase": m.RebaseLoss, "reset": m.ResetLoss} {
		if l, err := f("v1"); err == nil {
			t.Errorf("%s of a corrupt state: %+v", what, l)
		}
	}
	l, err = m.DestroyLoss("v1")
	check("destroy corrupt v1", l, err, true, []string{"s1", "s2"}, false)
	// No state file at all (as an interrupted create leaves): the same.
	if err := os.Remove(m.vm("v1").path(stateFile)); err != nil {
		t.Fatal(err)
	}
	for what, f := range map[string]func(string) (Loss, error){"rebase": m.RebaseLoss, "reset": m.ResetLoss} {
		if l, err := f("v1"); err == nil {
			t.Errorf("%s without a state: %+v", what, l)
		}
	}
	l, err = m.DestroyLoss("v1")
	check("destroy stateless v1", l, err, true, []string{"s1", "s2"}, false)

	// A state but no disk: reset and destroy go ahead, rebase can't.
	if err := os.Remove(m.vm("v2").path(diskFile)); err != nil {
		t.Fatal(err)
	}
	if l, err := m.RebaseLoss("v2"); err == nil {
		t.Errorf("rebase without a disk: %+v", l)
	}
	l, err = m.ResetLoss("v2")
	check("reset diskless v2", l, err, true, nil, true)
	l, err = m.DestroyLoss("v2")
	check("destroy diskless v2", l, err, true, nil, true)

	for _, n := range []string{"nosuch", "bad name", ".."} {
		for what, f := range map[string]func(string) (Loss, error){"rebase": m.RebaseLoss, "reset": m.ResetLoss, "destroy": m.DestroyLoss} {
			if l, err := f(n); err == nil {
				t.Errorf("%s %q: %+v", what, n, l)
			}
		}
	}
	if _, err := m.DestroyLoss("nosuch"); err == nil || err.Error() != `no VM named "nosuch" (see: catchpen ls)` {
		t.Errorf("destroy nosuch: %v", err)
	}
	// The checks delete nothing.
	if ss, err := qemu.Snapshots(m.vm("v1").path(diskFile)); err != nil || len(ss) != 2 {
		t.Errorf("v1's snapshots afterwards: %+v, %v", ss, err)
	}
}
