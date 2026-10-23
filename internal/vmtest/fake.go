// Package vmtest holds what the tests of internal/vm and internal/runner
// share. It does not import vm, so vm's own tests can use it.
package vmtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// FakeQEMU is a live process that qemu.Pid takes for the QEMU of the VM
// named name, whose directory is dir; it returns once running reports it up.
func FakeQEMU(t *testing.T, dir, name string, memMB int, running func() bool) {
	t.Helper()
	write(t, filepath.Join(dir, "vm.json"), fmt.Sprintf(`{"name":%q,"memory_mb":%d}`, name, memMB))
	c := exec.Command("bash", "-c", `exec -a qemu-system-x86_64 bash -c 'sleep 60; true' -name curral-`+name)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	write(t, filepath.Join(dir, "qemu.pid"), strconv.Itoa(c.Process.Pid))
	for i := 0; i < 50; i++ {
		if running() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake QEMU not seen as running")
}

// FakeSupervisor is a live process whose argv looks like curral task-supervise <id>.
func FakeSupervisor(t *testing.T, id string) int {
	t.Helper()
	c := exec.Command("bash", "-c", "sleep 60; true", "task-supervise", id) // "; true": bash would exec a lone command, losing this argv
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	return c.Process.Pid
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
