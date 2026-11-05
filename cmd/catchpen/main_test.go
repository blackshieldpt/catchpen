package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/task"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
)

// TestMain makes this test binary harmless as a task supervisor: task run
// starts os.Executable() task-supervise <id>, which in a test is this binary,
// and would otherwise run every test again. TestTaskSuperviseSignals starts
// it as a real one.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "task-supervise" {
		if os.Getenv("CATCHPEN_TEST_SUPERVISE") != "" {
			main()
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A task's supervisor cancels the task on the first SIGTERM (task cancel's)
// and outlives a second one while it tears down, then ends the task.
func TestTaskSuperviseSignals(t *testing.T) {
	t.Setenv("CATCHPEN_HOME", t.TempDir())
	p := paths.Default()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Another task fills the budget, so the supervisor waits for it.
	if err := os.WriteFile(p.ConfigFile(), []byte("tasks: { max_vms: 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := task.Store{Dir: p.Tasks()}
	busy := &task.Task{ID: "aaaaaaa1", State: task.Running, VM: "task-aaaaaaa1", Created: time.Now(), PID: vmtest.FakeSupervisor(t, "aaaaaaa1")}
	tk := &task.Task{ID: "bbbbbbb2", State: task.Queued, VM: "task-bbbbbbb2", Created: time.Now(), Spec: task.Spec{Agent: "cmd", Cmd: "true", Prompt: "p"}}
	for _, x := range []*task.Task{busy, tk} {
		if err := st.Create(x, "p"); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	c := exec.Command(os.Args[0], "task-supervise", tk.ID)
	c.Env = append(os.Environ(), "CATCHPEN_TEST_SUPERVISE=1")
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { c.Wait(); close(exited) }()
	t.Cleanup(func() { c.Process.Kill(); <-exited })
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if got, err := st.Load(tk.ID); err == nil && got.PID == c.Process.Pid {
			break
		}
		select {
		case <-exited:
			t.Fatalf("supervisor exited before it ran the task:\n%s", out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor didn't take the task")
		}
	}
	// Holding the task's lock keeps the teardown from ending the task.
	unlock, err := fsutil.Lock(st.Path(tk.ID, "task.lock"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
	}
	select {
	case <-exited:
		unlock()
		t.Fatalf("supervisor didn't outlive the second SIGTERM: %v\n%s", c.ProcessState, out.String())
	default:
	}
	unlock()
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatal("supervisor didn't end after its teardown")
	}
	if c.ProcessState.ExitCode() != 1 || !strings.Contains(out.String(), "catchpen: context canceled") {
		t.Errorf("supervisor: %v\n%s", c.ProcessState, out.String())
	}
	if got, err := st.Load(tk.ID); err != nil || got.State != task.Cancelled || got.Ended.IsZero() {
		t.Errorf("task: %+v %v", got, err)
	}
	if b, _ := os.ReadFile(p.Audit()); !strings.Contains(string(b), `"event":"task.end","vm":"task-bbbbbbb2"`) {
		t.Errorf("no task.end in the audit log:\n%s", b)
	}
}
