package runner

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/internal/task"
	"github.com/blackshieldpt/catchpen/internal/vm"
)

// Cancelling or removing a lost task (its supervisor gone) destroys its VM,
// its transcripts copied first, unless a canary hit it: then the VM is kept,
// paused as it is or else stopped, and the task's record is left as it was.
func TestReapKeepsHitVM(t *testing.T) {
	r := testRunner(t)
	m := r.M
	st := m.Tasks()
	lost := func(id string) *task.Task {
		t.Helper()
		tk := &task.Task{ID: id, State: task.Running, VM: "task-" + id, Created: time.Now(), PID: 1 << 30}
		if err := st.Create(tk, "p"); err != nil {
			t.Fatal(err)
		}
		fakeQEMU(t, m, tk.VM, 512)
		return tk
	}
	write := func(name, file, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(m.Dir(name), file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := func(id string) {
		t.Helper()
		if got, err := st.Load(id); err != nil || got.State != task.Running || got.Error != "" || !got.Ended.IsZero() {
			t.Errorf("task %s's record changed: %+v %v", id, got, err)
		}
	}
	refused := func(tk *task.Task) string {
		return "task " + tk.ID + ": " + tk.VM + ": its canaries were found in use: kept, with the task; destroy the VM first (catchpen destroy " + tk.VM + ")"
	}

	// A VM a hit paused is kept as it is.
	p := lost("aaaaaaa1")
	write(p.VM, "canary-hit.json", "{}")
	write(p.VM, "paused.json", "{}")
	if err := r.Cancel(p.ID); !errors.Is(err, vm.ErrHit) || err.Error() != refused(p) {
		t.Errorf("paused: %v", err)
	}
	if _, ok := m.Running(p.VM); !ok || !m.Paused(p.VM) {
		t.Errorf("paused VM not kept as it was: running %v, paused %v", ok, m.Paused(p.VM))
	}
	unchanged(p.ID)

	// One whose canaries were found in use is kept, stopped.
	h := lost("bbbbbbb2")
	write(h.VM, "canary-hit.json", "{}")
	if err := r.Remove(h.ID); !errors.Is(err, vm.ErrHit) || err.Error() != refused(h) {
		t.Errorf("hit: %v", err)
	}
	if !m.Hit(h.VM) {
		t.Error("hit VM not kept")
	}
	if _, ok := m.Running(h.VM); ok {
		t.Error("hit VM kept running, not stopped")
	}
	unchanged(h.ID)

	// Once the VM is destroyed, as the error says, the cancel goes through.
	if err := m.Destroy(p.VM); err != nil {
		t.Fatal(err)
	}
	if err := r.Cancel(p.ID); err != nil {
		t.Errorf("cancel after destroying the VM: %v", err)
	}

	// One without a hit is destroyed, its agents' transcripts copied first:
	// the host dials the guest's sshd.
	c := lost("ccccccc3")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dialed := make(chan struct{}, 1)
	go func() {
		if conn, err := ln.Accept(); err == nil {
			conn.Close()
			dialed <- struct{}{}
		}
	}()
	if err := (sshx.Keys{Dir: filepath.Join(m.Dir(c.VM), "ssh")}).Generate(c.VM); err != nil {
		t.Fatal(err)
	}
	write(c.VM, "vm.json", `{"name":"`+c.VM+`","user":"u","agents":{"claude":{}}}`)
	write(c.VM, "runtime.json", `{"ssh_port":`+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)+`}`)
	if err := r.Cancel(c.ID); err != nil {
		t.Errorf("no hit: %v", err)
	}
	select {
	case <-dialed:
	case <-time.After(5 * time.Second):
		t.Error("no transcripts copied: the guest's sshd wasn't dialed")
	}
	if _, err := os.Stat(m.Dir(c.VM)); !os.IsNotExist(err) {
		t.Errorf("VM without a hit not destroyed: %v", err)
	}
	if got, err := st.Load(c.ID); err != nil || got.State != task.Cancelled {
		t.Errorf("task without a hit: %+v %v", got, err)
	}
}

// Removing an ended task whose VM a canary hit paused, or whose decoys were
// used, is refused while that VM exists, and the task's directory kept; once
// the VM is destroyed, it is removed. An ended task whose VM was kept with
// no hit is removed, its VM left alone.
func TestRemoveKeepsHitTask(t *testing.T) {
	r := testRunner(t)
	m := r.M
	st := m.Tasks()
	ended := func(id, state, file string) *task.Task {
		t.Helper()
		tk := &task.Task{ID: id, State: state, VM: "task-" + id, Created: time.Now()}
		if err := st.Create(tk, "p"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(st.Path(id, "supervisor.log"), []byte("log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fakeQEMU(t, m, tk.VM, 512)
		if file != "" {
			if err := os.WriteFile(filepath.Join(m.Dir(tk.VM), file), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return tk
	}
	for _, tk := range []*task.Task{ended("ddddddd4", task.Failed, "paused.json"), ended("eeeeeee5", task.Done, "canary-hit.json")} {
		want := "task " + tk.ID + ": " + tk.VM + ": its canaries were found in use: kept, with the task; destroy the VM first (catchpen destroy " + tk.VM + ")"
		if err := r.Remove(tk.ID); !errors.Is(err, vm.ErrHit) || err.Error() != want {
			t.Errorf("%s: %v", tk.ID, err)
		}
		if _, err := os.Stat(st.Path(tk.ID, "supervisor.log")); err != nil {
			t.Errorf("%s's directory not kept: %v", tk.ID, err)
		}
		if err := m.Destroy(tk.VM); err != nil {
			t.Fatal(err)
		}
		if err := r.Remove(tk.ID); err != nil {
			t.Errorf("%s, its VM destroyed: %v", tk.ID, err)
		}
		if _, err := os.Stat(st.Path(tk.ID, "")); !os.IsNotExist(err) {
			t.Errorf("%s not removed: %v", tk.ID, err)
		}
	}
	k := ended("fffffff6", task.Done, "")
	if err := r.Remove(k.ID); err != nil {
		t.Errorf("kept, no hit: %v", err)
	}
	if _, err := os.Stat(st.Path(k.ID, "")); !os.IsNotExist(err) {
		t.Errorf("kept, no hit: not removed: %v", err)
	}
	if _, ok := m.Running(k.VM); !ok {
		t.Error("kept, no hit: its VM was touched")
	}
}
