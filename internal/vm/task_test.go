package vm

import (
	"fmt"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/task"
)

// fakeSupervisor is a live process whose argv looks like curral task-supervise <id>.
func fakeSupervisor(t *testing.T, id string) int {
	t.Helper()
	c := exec.Command("bash", "-c", "sleep 60; true", "task-supervise", id) // "; true": bash would exec a lone command, losing this argv
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	return c.Process.Pid
}

func TestClaimSlot(t *testing.T) {
	m := testManager(t)
	m.Cfg.Tasks.MaxVMs = 2
	m.Cfg.Tasks.MaxMemory = "10G"
	st := m.Tasks()
	add := func(state string, mem int, batch string, parallel int, alive bool) *task.Task {
		tk := &task.Task{ID: task.NewID(), State: state, Created: time.Now(), MemoryMB: mem, Spec: task.Spec{Batch: batch, Parallel: parallel}}
		tk.VM = "task-" + tk.ID
		if alive {
			tk.PID = fakeSupervisor(t, tk.ID)
		}
		if err := st.Create(tk, "p"); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	claim := func(tk *task.Task) bool {
		t.Helper()
		ok, err := m.claimSlot(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	m.Cfg.Tasks.MaxVMs = 1
	add(task.Starting, 4096, "", 0, false) // dead supervisor: doesn't count
	a := add(task.Queued, 4096, "b1", 1, true)
	if !claim(a) {
		t.Fatal("first task not admitted (a dead task's claim counted)")
	}
	m.Cfg.Tasks.MaxVMs = 2
	b := add(task.Queued, 4096, "b1", 1, true)
	if claim(b) {
		t.Error("batch --parallel 1 exceeded")
	}
	c := add(task.Queued, 8192, "", 0, true)
	if claim(c) {
		t.Error("memory budget exceeded (4G + 8G > 10G)")
	}
	d := add(task.Queued, 4096, "", 0, true)
	if !claim(d) {
		t.Error("second VM within budget not admitted")
	}
	e := add(task.Queued, 1024, "", 0, true)
	if claim(e) {
		t.Error("max_vms exceeded")
	}
	if got, _ := st.Load(d.ID); got.State != task.Starting || got.Started.IsZero() {
		t.Errorf("claimed task: %+v", got)
	}
	if _, err := m.claimSlot(d.ID); err == nil {
		t.Error("a task that isn't queued was claimed again")
	}
}

// fakeQEMU is a live process that qemu.Pid takes for VM name's QEMU.
func fakeQEMU(t *testing.T, m *Manager, name string, memMB int) {
	t.Helper()
	writeFile(t, m.file(name, stateFile), fmt.Sprintf(`{"name":%q,"memory_mb":%d}`, name, memMB))
	c := exec.Command("bash", "-c", `exec -a qemu-system-x86_64 bash -c 'sleep 60; true' -name curral-`+name)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	writeFile(t, m.file(name, pidFile), strconv.Itoa(c.Process.Pid))
	for i := 0; i < 50; i++ {
		if _, ok := m.Running(name); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake QEMU not seen as running")
}

// A claimed task whose VM is already up is counted once, not twice.
func TestClaimSlotCountsRunningTaskVMOnce(t *testing.T) {
	m := testManager(t)
	m.Cfg.Tasks.MaxVMs = 2
	st := m.Tasks()
	a := &task.Task{ID: task.NewID(), State: task.Running, Created: time.Now(), MemoryMB: 1024}
	a.VM = "task-" + a.ID
	a.PID = fakeSupervisor(t, a.ID)
	st.Create(a, "p")
	fakeQEMU(t, m, a.VM, 1024)
	b := &task.Task{ID: task.NewID(), State: task.Queued, Created: time.Now(), MemoryMB: 1024}
	b.VM = "task-" + b.ID
	b.PID = fakeSupervisor(t, b.ID)
	st.Create(b, "p")
	if ok, err := m.claimSlot(b.ID); err != nil || !ok {
		t.Errorf("second task not admitted with one VM running and max_vms 2: %v %v", ok, err)
	}
}
