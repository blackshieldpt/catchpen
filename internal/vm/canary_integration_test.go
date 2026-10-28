//go:build integration

package vm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/vmtest"

	"github.com/blackshieldpt/catchpen/internal/canary"
	"github.com/blackshieldpt/catchpen/internal/qemu"
	"github.com/blackshieldpt/catchpen/internal/sshx"
)

// TestCanaries plants decoys, uses one from the guest and checks the VM is
// paused before the request gets an answer, then resumes it.
func TestCanaries(t *testing.T) {
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	vmtest.Template(t, m.P.Templates(), "cn", "", "canaries: [aws, env, git]\ncache: [go]\n")
	if _, err := m.Create(ctx, "cn", CreateOpts{Template: "cn"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("cn") })
	// As a crash or reboot while paused leaves it.
	if err := os.WriteFile(m.vm("cn").path(pausedFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, "cn"); err != nil {
		t.Fatal(err)
	}
	if m.Paused("cn") {
		t.Error("a stale pause marker survived start")
	}
	port, err := m.SSHPort("cn")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.keys("cn").Dial(ctx, port, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cs, _ := canary.Load(m.dir("cn"))
	guest := guestRunner(t, ctx, c)
	if got := guest("stat -c '%U %a' ~/.aws ~/.aws/credentials ~/.aws/config ~/.env.production ~/.git-credentials"); got != "agent 755\nagent 600\nagent 600\nagent 600\nagent 600" {
		t.Errorf("decoys: %q", got)
	}
	if got := guest("grep -c " + cs[0].Values[0] + " ~/.aws/credentials"); got != "1" {
		t.Errorf("aws key: %q", got)
	}

	// Using the env decoy: curl goes through the proxy to the canary host.
	done := make(chan string, 1)
	go func() {
		rctx, rcancel := context.WithTimeout(ctx, 3*time.Minute)
		defer rcancel()
		out, _ := sshx.Run(rctx, c, `bash -lc '. ~/.env.production; curl -s -o /dev/null -w %{http_connect} -H "Authorization: Bearer $INTERNAL_API_TOKEN" "$INTERNAL_API_URL/v1/users"; echo " done"'`, nil)
		done <- string(out.Raw())
	}()
	deadline := time.Now().Add(time.Minute)
	for !m.Paused("cn") && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if !m.Paused("cn") {
		t.Fatal("not paused after a canary hit")
	}
	q, err := qemu.DialQMP(m.vm("cn").path(qmpSock), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := q.Status()
	q.Close()
	if st != "paused" {
		t.Errorf("QEMU status %q", st)
	}
	select {
	case out := <-done:
		t.Fatalf("the guest got an answer while paused: %q", out)
	case <-time.After(3 * time.Second):
	}
	vms, _ := m.List()
	if len(vms) != 1 || !vms[0].Paused {
		t.Errorf("ls: %+v", vms)
	}
	b, _ := os.ReadFile(m.P.Audit())
	if !strings.Contains(string(b), `"canary.hit"`) || !strings.Contains(string(b), "connection to "+cs[1].Hosts[0]+":443") {
		t.Errorf("audit: %s", b)
	}

	if err := m.Resume(ctx, "cn"); err != nil {
		t.Fatal(err)
	}
	if out := <-done; out != "403 done\n" { // the proxy refused the CONNECT
		t.Errorf("after resume: %q", out)
	}
	if m.Paused("cn") {
		t.Error("still marked paused")
	}
	if got := guest("echo ok"); got != "ok" {
		t.Errorf("guest after resume: %q", got)
	}
	if _, err := m.SSHPort("cn"); err != nil {
		t.Errorf("ssh after resume: %v", err)
	}

	// The package cache pauses the VM too.
	go sshx.Run(ctx, c, `. ~/.env.production; curl -s -m 60 -o /dev/null -H "Authorization: Bearer $INTERNAL_API_TOKEN" http://10.0.2.102/go/x/@v/list`, nil)
	deadline = time.Now().Add(time.Minute)
	for !m.Paused("cn") && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if !m.Paused("cn") {
		t.Fatal("not paused after a canary hit at the cache")
	}
	if err := m.Resume(ctx, "cn"); err != nil {
		t.Fatal(err)
	}
}
