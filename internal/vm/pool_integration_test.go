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

	"github.com/blackshieldpt/catchpen/internal/config"
)

func TestPoolAndIdleStop(t *testing.T) {
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	m.Cfg.Pool = []config.PoolEntry{{Template: "minimal", Size: 1}}
	n, err := m.FillPool(ctx)
	if err != nil || n != 1 {
		t.Fatalf("fill: %d %v", n, err)
	}
	t.Cleanup(func() { _, _ = m.DrainPool() })
	pool, _ := m.PoolVMs()
	if len(pool) != 1 || !pool[0].Ready() {
		t.Fatalf("pool %+v", pool)
	}
	if ss, _ := m.Snapshots(pool[0].Name); len(ss) != 1 || ss[0].Name != poolSnap || ss[0].VMStateMB == 0 {
		t.Fatalf("pool snapshots %+v", ss)
	}
	if n, err := m.FillPool(ctx); err != nil || n != 0 {
		t.Errorf("second fill: %d %v", n, err)
	}

	if _, err := m.Create(ctx, "pw", CreateOpts{Template: "minimal"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("pw") })
	if pool, _ := m.PoolVMs(); len(pool) != 0 {
		t.Fatalf("pool after claim %+v", pool)
	}
	start := time.Now()
	if _, err := m.Start(ctx, "pw"); err != nil {
		t.Fatal(err)
	}
	t.Logf("claimed VM ready in %s", time.Since(start).Round(time.Second))
	port, err := m.SSHPort("pw")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.keys("pw").Dial(ctx, port, "agent")
	if err != nil {
		t.Fatal(err)
	}
	guest := guestRunner(t, ctx, c)
	if got := guest("hostname; grep -cw pw /etc/hosts; grep -c pool- /etc/hosts || true"); got != "pw\n1\n0" {
		t.Errorf("hostname, hosts: %q", got)
	}
	if g, _ := strconv.ParseInt(guest("date +%s"), 10, 64); g < time.Now().Unix()-10 {
		t.Errorf("guest clock %d, host %d", g, time.Now().Unix())
	}
	if ss, _ := m.Snapshots("pw"); len(ss) != 0 {
		t.Errorf("snapshots after claim %+v", ss)
	}
	if s, _ := m.Load("pw"); s.Resume != "" || s.Pool {
		t.Errorf("state after claim %+v", s)
	}
	out, err := exec.CommandContext(ctx, "ssh", "-F", m.P.SSHConfig(), "-o", "BatchMode=yes", "catchpen-pw", "hostname").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "pw" {
		t.Errorf("openssh: %v %s", err, out)
	}

	// Idle stop: an open connection keeps it running, then it stops.
	m.Cfg.IdleStop = "1m"
	var tr IdleTracker
	now := time.Now()
	for i, want := range []int{0, 0} {
		idle, err := tr.Check(m, now.Add(time.Duration(i)*2*time.Minute))
		if err != nil || len(idle) != want {
			t.Fatalf("check %d with a connection: %v %v", i, idle, err)
		}
	}
	c.Close()
	time.Sleep(2 * time.Second)
	idle, err := tr.Check(m, now.Add(4*time.Minute))
	if err != nil || len(idle) != 1 || idle[0].Name != "pw" {
		t.Fatalf("check without connections: %v %v", idle, err)
	}
	if err := m.StopIdle("pw", idle[0].For); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Running("pw"); ok {
		t.Error("still running after idle stop")
	}
	if b, _ := os.ReadFile(m.P.Audit()); !strings.Contains(string(b), `"vm.idle_stop"`) {
		t.Error("no idle stop in the audit log")
	}

	// A normal boot later keeps the claimed name (from the rewritten seed).
	if _, err := m.Start(ctx, "pw"); err != nil {
		t.Fatal(err)
	}
	port, _ = m.SSHPort("pw")
	c, err = m.keys("pw").Dial(ctx, port, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := guestRunner(t, ctx, c)("hostname"); got != "pw" {
		t.Errorf("hostname after reboot: %q", got)
	}
}
