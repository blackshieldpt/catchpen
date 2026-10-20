//go:build integration

package vm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/blackshieldpt/curral/internal/hosttmux"
)

// ptyProc is the curral binary running on a test-owned terminal.
type ptyProc struct {
	t    *testing.T
	f    *os.File
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func startPTY(t *testing.T, bin string, args ...string) *ptyProc {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "TMUX=")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	p := &ptyProc{t: t, f: f, cmd: cmd, done: make(chan struct{})}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := f.Read(b)
			p.mu.Lock()
			p.out.Write(b[:n])
			p.mu.Unlock()
			if err != nil {
				close(p.done)
				return
			}
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); f.Close() })
	return p
}

func (p *ptyProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

func (p *ptyProc) waitFor(s string) {
	p.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(p.output(), s) {
		if time.Now().After(deadline) {
			p.t.Fatalf("timed out waiting for %q; output:\n%q", s, p.output())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (p *ptyProc) send(s string) {
	if _, err := p.f.WriteString(s); err != nil {
		p.t.Fatal(err)
	}
}

func (p *ptyProc) wait() int {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(90 * time.Second):
		p.t.Fatalf("still running; output:\n%q", p.output())
	}
	_ = p.cmd.Wait()
	return p.cmd.ProcessState.ExitCode()
}

func recordingWith(t *testing.T, m *Manager, name, kind, text string) bool {
	t.Helper()
	ids, err := m.Recordings(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(id, "-"+kind+"-") {
			continue
		}
		if b, _ := os.ReadFile(m.RecordingPath(name, id)); bytes.Contains(b, []byte(text)) {
			return true
		}
	}
	return false
}

func TestSessions(t *testing.T) {
	m := integrationManager(t)
	tm := &hosttmux.Tmux{Socket: fmt.Sprintf("curral-it-%d", os.Getpid()), Bin: m.Bin}
	m.Tmux = tm
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", tm.Socket, "kill-server").Run() })
	os.WriteFile(filepath.Join(m.P.Templates(), "sess.yaml"), []byte("extends: minimal\nruntimes: { node: \"24\" }\nagents: { claude: {} }\n"), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	if _, err := m.Create(ctx, "se1", CreateOpts{Template: "sess"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("se1") })
	if _, err := m.Start(ctx, "se1"); err != nil {
		t.Fatal(err)
	}

	// An interactive ssh session is recorded on the host.
	p := startPTY(t, m.Bin, "ssh", "se1")
	p.send("echo marker-$((40+2)); exit\n")
	if code := p.wait(); code != 0 || !strings.Contains(p.output(), "marker-42") {
		t.Fatalf("ssh: exit %d\n%q", code, p.output())
	}
	if !recordingWith(t, m, "se1", KindSSH, "marker-42") {
		t.Error("ssh session not recorded")
	}

	// curral run starts the agent (in a guest tmux session) and records it.
	p = startPTY(t, m.Bin, "run", "se1", "claude", "--", "--version")
	if code := p.wait(); code != 0 || !regexp.MustCompile(`\d+\.\d+\.\d+`).MatchString(p.output()) {
		t.Fatalf("run claude --version: exit %d\n%q", code, p.output())
	}
	if !recordingWith(t, m, "se1", KindRun, "[exited]") {
		t.Error("run session not recorded")
	}

	// attach reattaches the same guest tmux session; its prefix is C-a.
	p = startPTY(t, m.Bin, "attach", "se1")
	time.Sleep(2 * time.Second)
	p.send("export X=reattach-$((1+1))\n")
	time.Sleep(time.Second)
	p.send("\x01d")
	p.waitFor("[detached")
	p.wait()
	p = startPTY(t, m.Bin, "attach", "se1")
	time.Sleep(2 * time.Second)
	p.send("echo $X\n")
	p.waitFor("reattach-2")
	p.send("\x01d")
	p.wait()

	// Host tmux: a window per VM, running curral attach (recorded too).
	attaches := func() int {
		ids, _ := m.Recordings("se1")
		n := 0
		for _, id := range ids {
			if strings.Contains(id, "-"+KindAttach+"-") {
				n++
			}
		}
		return n
	}
	before := attaches()
	if err := tm.Ensure([]string{"se1"}); err != nil {
		t.Fatal(err)
	}
	if ws, _ := tm.Windows(); len(ws) != 1 || ws[0] != "se1" {
		t.Errorf("host tmux windows: %v", ws)
	}
	deadline := time.Now().Add(30 * time.Second)
	for attaches() == before {
		if time.Now().After(deadline) {
			t.Error("host tmux window didn't run a recorded curral attach")
			break
		}
		time.Sleep(time.Second)
	}

	// cp both ways.
	src := filepath.Join(t.TempDir(), "in.txt")
	os.WriteFile(src, []byte("copied-content\n"), 0o600)
	dst := filepath.Join(t.TempDir(), "out.txt")
	for _, args := range [][]string{{"cp", src, "se1:/tmp/cp-in"}, {"cp", "se1:/tmp/cp-in", dst}} {
		if out, err := exec.Command(m.Bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("curral %v: %v\n%s", args, err, out)
		}
	}
	if b, _ := os.ReadFile(dst); string(b) != "copied-content\n" {
		t.Errorf("cp round trip: %q", b)
	}

	if out, err := exec.Command(m.Bin, "ssh", "se1", "--", "echo", "via-command").CombinedOutput(); err != nil || !strings.Contains(string(out), "via-command") {
		t.Errorf("ssh -- cmd: %v %q", err, out)
	}

	if err := m.Stop("se1", false); err != nil {
		t.Fatal(err)
	}
	if tm.Exists() {
		t.Error("host tmux window not closed when the VM stopped")
	}
	var alog bytes.Buffer
	m.Audit().Tail(ctx, "se1", false, &alog)
	for _, ev := range []string{"vm.create", "vm.start", "creds.push", "session.start", "session.end", "kind=command", " cp ", "vm.stop"} {
		if !strings.Contains(alog.String(), ev) {
			t.Errorf("audit log lacks %q:\n%s", ev, alog.String())
		}
	}

	// Ephemeral: destroyed after the command, and when curral is signalled.
	out, err := exec.CommandContext(ctx, m.Bin, "up", "eph1", "--ephemeral", "-t", "sess", "--", "sh", "-c", "echo eph-$((1+1))").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "eph-2") {
		t.Fatalf("up --ephemeral: %v\n%s", err, out)
	}
	if _, err := os.Stat(m.Dir("eph1")); !os.IsNotExist(err) {
		t.Error("ephemeral VM left behind")
	}
	c := exec.CommandContext(ctx, m.Bin, "up", "eph2", "--ephemeral", "-t", "sess", "--", "sleep", "600")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Minute)
	for {
		if s, err := m.Load("eph2"); err == nil && s.Ephemeral {
			if _, ok := m.Running("eph2"); ok {
				var a bytes.Buffer
				m.Audit().Tail(ctx, "eph2", false, &a)
				if strings.Contains(a.String(), "creds.push") {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("eph2 did not come up")
		}
		time.Sleep(time.Second)
	}
	c.Process.Signal(syscall.SIGTERM)
	_ = c.Wait()
	if _, err := os.Stat(m.Dir("eph2")); !os.IsNotExist(err) {
		t.Error("ephemeral VM not destroyed after SIGTERM")
	}
	alog.Reset()
	m.Audit().Tail(ctx, "eph2", false, &alog)
	if !strings.Contains(alog.String(), "vm.destroy") {
		t.Errorf("eph2 audit:\n%s", alog.String())
	}

	// Closing the terminal of an ephemeral session (SIGHUP) also destroys it.
	p = startPTY(t, m.Bin, "up", "eph3", "--ephemeral", "-t", "sess")
	p.waitFor("eph3 is ready")
	time.Sleep(3 * time.Second) // into the attached session
	p.f.Close()
	select {
	case <-p.done:
	case <-time.After(2 * time.Minute):
	}
	_ = p.cmd.Wait()
	if _, err := os.Stat(m.Dir("eph3")); !os.IsNotExist(err) {
		t.Error("ephemeral VM not destroyed after its terminal closed")
	}
}
