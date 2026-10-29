package vm

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// startTestAgent starts a real ssh-agent on name's socket, as a restricted agent
// is, records its pid where the VM's agent keeps it, and returns it.
func startTestAgent(t *testing.T, m *Manager, name string) int {
	t.Helper()
	if _, err := exec.LookPath("ssh-agent"); err != nil {
		t.Skip("no ssh-agent")
	}
	a := m.agent(name)
	if len(a.Socket()) > 100 {
		t.Skipf("socket path too long for a unix socket, set TMPDIR to a short directory: %s", a.Socket())
	}
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ssh-agent", "-a", a.Socket()).Output()
	if err != nil {
		t.Fatal(err)
	}
	mm := regexp.MustCompile(`SSH_AGENT_PID=([0-9]+)`).FindSubmatch(out)
	if mm == nil {
		t.Fatalf("no pid in %q", out)
	}
	pid, _ := strconv.Atoi(string(mm[1]))
	sock := a.Socket()
	t.Cleanup(func() { // only if pid is still this agent, not a reused pid
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil && bytes.Contains(b, []byte(sock)) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if err := os.WriteFile(filepath.Join(a.Dir, "agent.pid"), mm[1], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Running(); !ok {
		t.Fatal("agent not running")
	}
	return pid
}

// gone reports whether process pid has exited (a zombie counts), waiting
// up to 2 s.
func gone(pid int) bool {
	for range 100 {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if i := bytes.LastIndexByte(b, ')'); err != nil || i < 0 || bytes.HasPrefix(b[i:], []byte(") Z")) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// When the guest powered itself off (QEMU is gone), stop and destroy still
// stop the VM's restricted ssh-agent and remove its runtime files.
func TestStopAfterQEMUExited(t *testing.T) {
	m := testManager(t)
	for _, destroy := range []bool{false, true} {
		name := "v"
		if destroy {
			name = "d"
		}
		if err := os.MkdirAll(m.dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := m.save(&State{Name: name}); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{runtimeFile, pidFile} {
			if err := os.WriteFile(m.vm(name).path(f), []byte("999999999"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		pid := startTestAgent(t, m, name)
		var err error
		if destroy {
			err = m.Destroy(name)
		} else {
			err = m.Stop(name, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !gone(pid) {
			t.Errorf("destroy=%v: the VM's ssh-agent (pid %d) is still running", destroy, pid)
		}
		if !destroy {
			if _, err := os.Stat(m.vm(name).path(runtimeFile)); !os.IsNotExist(err) {
				t.Errorf("runtime file left: %v", err)
			}
		}
	}
}
