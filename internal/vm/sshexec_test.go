package vm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A guest that floods the system ssh's output can't grow host memory: only
// the first 64 KiB are kept.
func TestSSHExecBoundsOutput(t *testing.T) {
	m := testManager(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nhead -c 268435456 /dev/zero\necho tail >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := m.sshExec(context.Background(), "v", "true")
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Error("want the command's exit status as an error")
	}
	if n := len(out.Raw()); n != 64<<10 {
		t.Errorf("kept %d bytes, want %d", n, 64<<10)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("allocated %d MiB for 256 MiB of output", grew>>20)
	}
}
