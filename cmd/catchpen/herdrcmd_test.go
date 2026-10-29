package main

import (
	"os"
	"path/filepath"
	"testing"
)

// catchpen herdr must refuse bad arguments before it runs herdr at all:
// starting a server first leaves one running behind an error.
func TestHerdrChecksBeforeStarting(t *testing.T) {
	t.Setenv("CATCHPEN_HOME", t.TempDir()) // no VMs
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	fake := "#!/bin/sh\necho \"$*\" >>'" + log + "'\necho 'status: running'\n"
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, args := range [][]string{{"typo"}, {"--sync", "dev"}, {"--sync", "--agent", "pi"}, {}} {
		c := herdrCmd()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil {
			t.Errorf("herdr %q: no error", args)
		}
		if b, err := os.ReadFile(log); err == nil {
			t.Errorf("herdr %q ran herdr: %q", args, b)
			os.Remove(log)
		}
	}
}
