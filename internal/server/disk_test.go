package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// A reset needs confirm, with the snapshots and canary hit it deletes; its
// job runs the Manager's Reset, onto the template's current base when Latest.
func TestResetVM(t *testing.T) {
	h := home(t, nil)
	base := filepath.Join(h.root, "old-base.qcow2") // missing: Reset fails there, past the API's checks
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created, Base: base})
	h.disk(t, "alpha", "s1")
	c := h.client(t)
	wait := waiter(t, c)
	ctx := context.Background()
	l, err := c.ResetLoss(ctx, "alpha")
	if err != nil || !reflect.DeepEqual(*l, api.VMLoss{Disk: true, Snapshots: []string{"s1"}}) {
		t.Fatalf("loss: %+v %v", l, err)
	}
	_, err = c.ResetVM(ctx, "alpha", api.ResetVMRequest{Snapshots: l.Snapshots})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConfirmRequired || e.Message != `resetting alpha deletes all data on its disk, `+
		`its snapshots (s1) included: send "confirm": true, with the snapshots it deletes (GET this path for all it loses)` {
		t.Fatalf("without confirm: %v", err)
	}
	for _, r := range []api.ResetVMRequest{{Confirm: true}, {Confirm: true, Snapshots: []string{"s1", "s2"}},
		{Confirm: true, Snapshots: l.Snapshots, CanaryHit: true}} {
		if _, err := c.ResetVM(ctx, "alpha", r); code(err) != api.CodeConflict {
			t.Errorf("confirmed %+v: %v", r, err)
		}
	}
	j, _ := wait(c.ResetVM(ctx, "alpha", api.ResetVMRequest{Confirm: true, Snapshots: l.Snapshots}))
	if j.State != api.JobFailed || j.Kind != api.JobVMReset || j.VM != "alpha" || !strings.HasPrefix(j.Error, "base "+base+" missing") {
		t.Errorf("reset: %+v", j)
	}
	j, _ = wait(c.ResetVM(ctx, "alpha", api.ResetVMRequest{Confirm: true, Latest: true, Snapshots: l.Snapshots}))
	if j.State != api.JobFailed || !strings.HasPrefix(j.Error, "alpha was created with the old-style template base") {
		t.Errorf("reset onto the latest base: %+v", j)
	}
	if _, err := c.ResetLoss(ctx, "nope"); code(err) != api.CodeNotFound {
		t.Errorf("nope's loss: %v", err)
	}
	if _, err := c.ResetVM(ctx, "nope", api.ResetVMRequest{Confirm: true}); code(err) != api.CodeNotFound {
		t.Errorf("nope: %v", err)
	}
}

// A rebase needs confirm, with the snapshots it deletes, only when the VM
// has some; its job runs the Manager's Rebase.
func TestRebaseVM(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	h.disk(t, "alpha")
	h.writeVM(t, vm.State{Name: "beta", Template: "base", Created: created})
	h.disk(t, "beta", "s1", "s2")
	c := h.client(t)
	wait := waiter(t, c)
	ctx := context.Background()
	if st, body := h.post(t, api.PathVMRebase("alpha"), ""); st != http.StatusAccepted {
		t.Errorf("no snapshots, no body: %d %s", st, body)
	}
	j, _ := wait(c.RebaseVM(ctx, "alpha", api.RebaseVMRequest{}))
	if j.State != api.JobFailed || j.Kind != api.JobVMRebase || !strings.HasPrefix(j.Error, "alpha was created with the old-style template base") {
		t.Errorf("rebase: %+v", j)
	}
	l, err := c.RebaseLoss(ctx, "beta")
	if err != nil || !reflect.DeepEqual(*l, api.VMLoss{Snapshots: []string{"s1", "s2"}}) {
		t.Fatalf("loss: %+v %v", l, err)
	}
	_, err = c.RebaseVM(ctx, "beta", api.RebaseVMRequest{})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConfirmRequired || e.Message != `rebasing beta deletes its snapshots (s1, s2): `+
		`send "confirm": true, with the snapshots it deletes (GET this path for all it loses)` {
		t.Fatalf("without confirm: %v", err)
	}
	for _, snaps := range [][]string{nil, {"s2", "s1"}} {
		if _, err := c.RebaseVM(ctx, "beta", api.RebaseVMRequest{Confirm: true, Snapshots: snaps}); code(err) != api.CodeConflict {
			t.Errorf("confirmed %v: %v", snaps, err)
		}
	}
	if j, _ := wait(c.RebaseVM(ctx, "beta", api.RebaseVMRequest{Confirm: true, Snapshots: l.Snapshots})); j.State != api.JobFailed ||
		!strings.HasPrefix(j.Error, "beta was created with the old-style template base") {
		t.Errorf("confirmed: %+v", j)
	}
	if _, err := c.RebaseLoss(ctx, "nope"); code(err) != api.CodeNotFound {
		t.Errorf("nope's loss: %v", err)
	}
}

// A snapshot is saved, restored and deleted by jobs, each audited with the
// client; restore and delete need confirm.
func TestSnapshotJobs(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	h.disk(t, "alpha")
	c := h.client(t)
	wait := waiter(t, c)
	ctx := context.Background()
	for _, n := range []string{"", "1", "-a", "a/b"} {
		if _, err := c.CreateSnapshot(ctx, "alpha", api.CreateSnapshotRequest{Name: n}); code(err) != api.CodeBadRequest {
			t.Errorf("name %q: %v", n, err)
		}
	}
	if j, _ := wait(c.CreateSnapshot(ctx, "alpha", api.CreateSnapshotRequest{Name: "s.1"})); j.State != api.JobDone || j.Kind != api.JobVMSnapshot {
		t.Errorf("snapshot: %+v", j)
	}
	if e := lastEvent(t, h.p, audit.VMSnapshot); e.VM != "alpha" || e.Detail["snapshot"] != "s.1" || e.Detail["client"] != api.ClientLocal {
		t.Errorf("snapshot's audit: %+v", e)
	}
	if _, err := c.CreateSnapshot(ctx, "alpha", api.CreateSnapshotRequest{Name: "s.1"}); code(err) != api.CodeConflict {
		t.Errorf("again: %v", err)
	}

	_, err := c.RestoreSnapshot(ctx, "alpha", "s.1", api.RestoreSnapshotRequest{})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConfirmRequired || e.Message != `restoring alpha to s.1 loses what changed since it was saved: send "confirm": true` {
		t.Errorf("restore without confirm: %v", err)
	}
	if j, _ := wait(c.RestoreSnapshot(ctx, "alpha", "s.1", api.RestoreSnapshotRequest{Confirm: true})); j.State != api.JobDone || j.Kind != api.JobVMRestore {
		t.Errorf("restore: %+v", j)
	}
	if e := lastEvent(t, h.p, audit.VMRestore); e.Detail["snapshot"] != "s.1" || e.Detail["client"] != api.ClientLocal {
		t.Errorf("restore's audit: %+v", e)
	}

	_, err = c.DeleteSnapshot(ctx, "alpha", "s.1", api.DeleteSnapshotRequest{})
	if !errors.As(err, &e) || e.Code != api.CodeConfirmRequired || e.Message != `deleting alpha's snapshot s.1 can't be undone: send "confirm": true` {
		t.Errorf("delete without confirm: %v", err)
	}
	if j, _ := wait(c.DeleteSnapshot(ctx, "alpha", "s.1", api.DeleteSnapshotRequest{Confirm: true})); j.State != api.JobDone || j.Kind != api.JobVMSnapshotRm {
		t.Errorf("delete: %+v", j)
	}
	if e := lastEvent(t, h.p, audit.VMSnapshotRm); e.Detail["snapshot"] != "s.1" || e.Detail["client"] != api.ClientLocal {
		t.Errorf("delete's audit: %+v", e)
	}
	if ss, err := c.Snapshots(ctx, "alpha"); err != nil || len(ss) != 0 {
		t.Errorf("after delete: %+v %v", ss, err)
	}

	for _, snap := range []string{"s.1", "..", "s"} {
		if _, err := c.RestoreSnapshot(ctx, "alpha", snap, api.RestoreSnapshotRequest{Confirm: true}); code(err) != api.CodeNotFound {
			t.Errorf("restore %q: %v", snap, err)
		}
		if _, err := c.DeleteSnapshot(ctx, "alpha", snap, api.DeleteSnapshotRequest{Confirm: true}); code(err) != api.CodeNotFound {
			t.Errorf("delete %q: %v", snap, err)
		}
	}
	if _, err := c.CreateSnapshot(ctx, "nope", api.CreateSnapshotRequest{Name: "s"}); code(err) != api.CodeNotFound {
		t.Errorf("nope: %v", err)
	}
}

// A push's job runs the Manager's Push, which needs the VM running.
func TestPushCreds(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	c := h.client(t)
	j, _ := waiter(t, c)(c.PushCreds(context.Background(), "alpha"))
	if j.State != api.JobFailed || j.Kind != api.JobCredsPush || j.Error != "VM alpha is not running (start it: catchpen start alpha)" {
		t.Errorf("push: %+v", j)
	}
	if st, body := h.post(t, api.PathVMPush("alpha"), `{"all":true}`); st != http.StatusBadRequest {
		t.Errorf("with a body: %d %s", st, body)
	}
	if _, err := c.PushCreds(context.Background(), "nope"); code(err) != api.CodeNotFound {
		t.Errorf("nope: %v", err)
	}
}

// A VM a canary hit paused refuses every call here; a VM with an SSH agent
// refuses those that can start it or clone through its agent (reset, rebase,
// restore, push), and takes the others.
func TestHeldVMs(t *testing.T) {
	h := home(t, nil)
	m := &vm.Manager{P: h.p}
	vmtest.FakeQEMU(t, h.p.VM("beta"), "beta", 512, func() bool { _, ok := m.Running("beta"); return ok })
	base := filepath.Join(h.root, "base.qcow2") // missing: a reset that got past the API fails there
	h.writeVM(t, vm.State{Name: "beta", Template: "base", Created: created, Base: base})
	h.disk(t, "beta", "s1")
	write(t, filepath.Join(h.p.VM("beta"), "paused.json"), "{}")
	h.writeVM(t, vm.State{Name: "keys", Template: "base", Created: created, Base: base, SSH: template.SSH{Agent: template.AgentRestricted}})
	h.disk(t, "keys", "s1")
	c := h.client(t)
	ctx := context.Background()
	calls := []struct {
		name string
		keys bool
		call func(vm string) (*api.JobInfo, error)
	}{
		{"reset", true, func(n string) (*api.JobInfo, error) {
			return c.ResetVM(ctx, n, api.ResetVMRequest{Confirm: true, Snapshots: []string{"s1"}})
		}},
		{"rebase", true, func(n string) (*api.JobInfo, error) {
			return c.RebaseVM(ctx, n, api.RebaseVMRequest{Confirm: true, Snapshots: []string{"s1"}})
		}},
		{"restore", true, func(n string) (*api.JobInfo, error) {
			return c.RestoreSnapshot(ctx, n, "s1", api.RestoreSnapshotRequest{Confirm: true})
		}},
		{"push", true, func(n string) (*api.JobInfo, error) { return c.PushCreds(ctx, n) }},
		{"snapshot", false, func(n string) (*api.JobInfo, error) {
			return c.CreateSnapshot(ctx, n, api.CreateSnapshotRequest{Name: "s2"})
		}},
		{"delete", false, func(n string) (*api.JobInfo, error) {
			return c.DeleteSnapshot(ctx, n, "s1", api.DeleteSnapshotRequest{Confirm: true})
		}},
	}
	wait := waiter(t, c)
	for _, k := range calls {
		if _, err := k.call("beta"); code(err) != api.CodeVMPaused {
			t.Errorf("%s of a paused VM: %v", k.name, err)
		}
		j, err := k.call("keys")
		var e *api.Error
		switch {
		case k.keys && (!errors.As(err, &e) || e.Code != api.CodeConflict || !strings.Contains(e.Message, "ssh.agent: restricted")):
			t.Errorf("%s with an SSH agent: %v", k.name, err)
		case !k.keys && err != nil:
			t.Errorf("%s with an SSH agent: %v", k.name, err)
		case !k.keys:
			wait(j, err)
		}
	}
	if l, err := c.ResetLoss(ctx, "beta"); err != nil || !l.Paused || !l.Running {
		t.Errorf("a paused VM's loss: %+v %v", l, err)
	}
}
