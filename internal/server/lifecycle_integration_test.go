//go:build integration

package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// TestLifecycle drives a real VM through the API: create, start (its
// client gone as soon as the job is started), a snapshot with its memory,
// a credentials push, a restore and the snapshot's deletion while it runs,
// stop, reset, a rebase onto the base it is on (refused), and destroy, each
// a job whose audit event names the client.
func TestLifecycle(t *testing.T) {
	p, bin := vmtest.Home(t)
	vmtest.Template(t, p.Templates(), "lc", "", "")
	mgr := func() (*vm.Manager, error) {
		return &vm.Manager{P: p, Cfg: config.Default(), Tpl: template.Loader{UserDir: p.Templates()}, Bin: bin}, nil
	}
	t.Cleanup(func() {
		if m, _ := mgr(); m.Destroy("lc") == nil {
			t.Log("destroyed lc after the test")
		}
	})
	h := home(t, mgr)
	c := h.client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	wait := func(j *api.JobInfo, err error) (*api.JobInfo, []string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		end, err := c.WaitJob(ctx, j.ID, func(l string) { lines = append(lines, l) })
		if err != nil {
			t.Fatal(err)
		}
		if end.State != api.JobDone {
			t.Fatalf("%s failed: %s\n%s", end.Kind, end.Error, strings.Join(lines, "\n"))
		}
		return end, lines
	}
	state := func(want string) {
		t.Helper()
		if v, err := c.VM(ctx, "lc"); err != nil || v.State != want {
			t.Fatalf("lc: %+v %v, want %s", v, err, want)
		}
	}

	if _, lines := wait(c.CreateVM(ctx, api.CreateVMRequest{Name: "lc", Template: "lc"})); !slices.ContainsFunc(lines,
		func(l string) bool { return strings.HasPrefix(l, "created lc (template lc") }) {
		t.Errorf("create's progress: %q", lines)
	}
	state(api.VMStopped)

	// The start's client goes away at once; the job runs on.
	gone, goneCancel := context.WithCancel(ctx)
	j, err := h.client(t).StartVM(gone, "lc")
	goneCancel()
	wait(j, err)
	state(api.VMRunning)

	wait(c.CreateSnapshot(ctx, "lc", api.CreateSnapshotRequest{Name: "s1"}))
	if ss, err := c.Snapshots(ctx, "lc"); err != nil || len(ss) != 1 || ss[0].Name != "s1" || ss[0].MemoryMB == 0 {
		t.Fatalf("snapshots: %+v %v", ss, err)
	}
	wait(c.PushCreds(ctx, "lc"))
	if _, lines := wait(c.RestoreSnapshot(ctx, "lc", "s1", api.RestoreSnapshotRequest{Confirm: true})); !slices.Contains(lines, "lc restored") {
		t.Errorf("restore's progress: %q", lines)
	}
	state(api.VMRunning)
	wait(c.DeleteSnapshot(ctx, "lc", "s1", api.DeleteSnapshotRequest{Confirm: true}))

	wait(c.StopVM(ctx, "lc", false))
	state(api.VMStopped)

	if _, lines := wait(c.ResetVM(ctx, "lc", api.ResetVMRequest{Confirm: true})); !slices.ContainsFunc(lines,
		func(l string) bool { return strings.HasPrefix(l, "reset lc on ") }) {
		t.Errorf("reset's progress: %q", lines)
	}
	state(api.VMStopped)
	j, err = c.RebaseVM(ctx, "lc", api.RebaseVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if end, err := c.WaitJob(ctx, j.ID, func(string) {}); err != nil || end.State != api.JobFailed || end.Error != "lc is already on the current base" {
		t.Errorf("rebase: %+v %v", end, err)
	}

	if _, err := c.DestroyVM(ctx, "lc", api.DestroyVMRequest{}); code(err) != api.CodeConfirmRequired {
		t.Fatalf("destroy without confirm: %v", err)
	}
	wait(c.DestroyVM(ctx, "lc", api.DestroyVMRequest{Confirm: true}))
	if _, err := c.VM(ctx, "lc"); code(err) != api.CodeNotFound {
		t.Errorf("after destroy: %v", err)
	}
	for _, ev := range []string{audit.VMCreate, audit.VMStart, audit.VMSnapshot, audit.CredsPush, audit.VMRestore,
		audit.VMSnapshotRm, audit.VMStop, audit.VMReset, audit.VMDestroy} {
		if e := lastEvent(t, p, ev); e.VM != "lc" || e.Detail["client"] != api.ClientLocal {
			t.Errorf("%s: %+v", ev, e)
		}
	}
}
