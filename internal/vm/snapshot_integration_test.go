//go:build integration

package vm

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSnapshots(t *testing.T) {
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	if _, err := m.Create(ctx, "sn1", CreateOpts{Template: "minimal"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("sn1") })
	if _, err := m.Start(ctx, "sn1"); err != nil {
		t.Fatal(err)
	}
	// guest runs a command over a fresh connection (restores drop connections).
	guest := func(cmd string) string {
		t.Helper()
		port, err := m.SSHPort("sn1")
		if err != nil {
			t.Fatal(err)
		}
		c, err := m.keys("sn1").Dial(ctx, port, "agent")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return guestRunner(t, ctx, c)(cmd)
	}
	guest(`echo one > f; (setsid sleep 3000 >/dev/null 2>&1 & echo $! > pid)`)

	// Memory snapshot of a running VM: restore brings back files and processes.
	if _, err := m.Snapshot(ctx, "sn1", "ram1", false); err != nil {
		t.Fatal(err)
	}
	ss, err := m.Snapshots("sn1")
	if err != nil || len(ss) != 1 || ss[0].VMStateMB == 0 {
		t.Fatalf("snapshots %+v %v", ss, err)
	}
	clockOK := func(when string) {
		t.Helper()
		if g, _ := strconv.ParseInt(guest("date +%s"), 10, 64); g < time.Now().Unix()-10 || g > time.Now().Unix()+10 {
			t.Errorf("%s: guest clock %d, host %d", when, g, time.Now().Unix())
		}
	}
	clockOK("after savevm")
	meta := m.snapMetas("sn1")["ram1"]
	if meta.QEMU == "" || !strings.HasPrefix(meta.Machine, "pc-q35-") {
		t.Errorf("snapshot metadata %+v", meta)
	}
	// Memory saved by another QEMU is refused on a running VM, which keeps running.
	m.setSnapMeta("sn1", "ram1", &snapMeta{QEMU: "0.0.1", Machine: meta.Machine})
	if err := m.Restore(ctx, "sn1", "ram1", false); err == nil || !strings.Contains(err.Error(), "--disk-only") {
		t.Errorf("restore across QEMU versions: %v", err)
	}
	if got := guest("echo up"); got != "up" {
		t.Errorf("VM after refused restore: %q", got)
	}
	m.setSnapMeta("sn1", "ram1", &meta)
	guest(`echo two > f; kill $(cat pid)`)
	time.Sleep(30 * time.Second) // so a stale guest clock would show
	if err := m.Restore(ctx, "sn1", "ram1", false); err != nil {
		t.Fatal(err)
	}
	if got := guest(`cat f; kill -0 $(cat pid) && echo alive`); got != "one\nalive" {
		t.Errorf("after memory restore: %q", got)
	}
	clockOK("after restore")

	// Disk-only snapshot of a running VM; restoring it restarts the VM.
	if _, err := m.Snapshot(ctx, "sn1", "disk1", true); err != nil {
		t.Fatal(err)
	}
	guest(`echo three > f; sync`)
	if err := m.Restore(ctx, "sn1", "disk1", false); err != nil {
		t.Fatal(err)
	}
	if got := guest(`cat f; kill -0 $(cat pid) 2>/dev/null && echo alive || echo rebooted`); got != "one\nrebooted" {
		t.Errorf("after disk restore: %q", got)
	}

	// A memory snapshot of a stopped VM starts it from that memory.
	if err := m.Stop("sn1", false); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(ctx, "sn1", "ram1", false); err != nil {
		t.Fatal(err)
	}
	if got := guest(`cat f; kill -0 $(cat pid) && echo alive`); got != "one\nalive" {
		t.Errorf("after restoring a stopped VM: %q", got)
	}
	// Just the disk of a memory snapshot: a fresh boot.
	guest(`echo four > f; sync`)
	if err := m.Restore(ctx, "sn1", "ram1", true); err != nil {
		t.Fatal(err)
	}
	if got := guest(`cat f; kill -0 $(cat pid) 2>/dev/null && echo alive || echo rebooted`); got != "one\nrebooted" {
		t.Errorf("after restore --disk-only: %q", got)
	}
	if err := m.DeleteSnapshot("sn1", "disk1"); err != nil {
		t.Fatal(err)
	}
	if ss, _ = m.Snapshots("sn1"); len(ss) != 1 || ss[0].Name != "ram1" {
		t.Errorf("after rmsnap: %+v", ss)
	}

	// A rebuilt base (a copy stands in for one): ls marks the VM stale, rebase
	// moves it keeping its files, gc then removes the old base.
	s, _ := m.Load("sn1")
	r, err := m.Resolve(s.Template, s.Distro)
	if err != nil {
		t.Fatal(err)
	}
	gen2 := r.basePath(m.P, 2)
	if out, err := exec.Command("cp", s.Base, gen2).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if cur, _ := m.CurrentBase(s.Template, s.Distro); cur != gen2 {
		t.Fatalf("current base %s", cur)
	}
	if err := m.Rebase(ctx, "sn1"); err != nil {
		t.Fatal(err)
	}
	if got := guest(`cat f`); got != "one" {
		t.Errorf("after rebase: %q", got)
	}
	if s2, _ := m.Load("sn1"); s2.Base != gen2 {
		t.Errorf("base after rebase: %s", s2.Base)
	}
	if ss, _ = m.Snapshots("sn1"); len(ss) != 0 {
		t.Errorf("snapshots after rebase: %+v", ss)
	}
	items, err := m.GC(false)
	if err != nil {
		t.Fatal(err)
	}
	removed := false
	for _, it := range items {
		removed = removed || it.Path == s.Base
	}
	if _, err := os.Stat(s.Base); !removed || err == nil {
		t.Errorf("old base not collected: %+v", items)
	}

	// Reset: a fresh disk, first boot runs again (as a new cloud-init instance).
	firstID := guest(`cloud-init query instance-id`)
	t.Logf("before reset: %s", guest(`cloud-init status --long || true`))
	if err := m.Reset(ctx, "sn1", false); err != nil {
		t.Fatal(err)
	}
	t.Logf("after reset: %s", guest(`cloud-init status --long || true`))
	if got := guest(`test -e f && echo kept || echo gone`); got != "gone" {
		t.Errorf("after reset: %q", got)
	}
	if id := guest(`cloud-init query instance-id`); id == firstID {
		t.Errorf("first boot didn't run again (instance id %s)", id)
	}
	if s3, _ := m.Load("sn1"); !s3.Booted {
		t.Error("not marked booted after reset")
	}
}
