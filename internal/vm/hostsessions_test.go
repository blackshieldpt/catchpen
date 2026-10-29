package vm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/blackshieldpt/catchpen/internal/hosttmux"
)

// A started task VM gets no host tmux window: the window's attached session
// would forward the VM's SSH agent while the task runs unattended.
func TestAddToHostSessions(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	bin := filepath.Join(t.TempDir(), "fake-catchpen")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tm := &hosttmux.Tmux{Socket: fmt.Sprintf("catchpen-vmtest-%d", os.Getpid()), Bin: bin}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", tm.Socket, "kill-server").Run() })
	t.Setenv("TMUX", "")
	if err := tm.Ensure([]string{"x"}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Tmux: tm}
	m.addToHostSessions(&State{Name: "job", Task: "t1"})
	m.addToHostSessions(&State{Name: "dev"})
	ws, err := tm.Windows()
	slices.Sort(ws)
	if err != nil || !slices.Equal(ws, []string{"dev", "x"}) {
		t.Errorf("windows = %v, %v; want [dev x]", ws, err)
	}
}
