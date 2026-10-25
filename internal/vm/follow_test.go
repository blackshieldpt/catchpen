package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// followOut runs follow over a finished log, writing to out.
func followOut(t *testing.T, out *os.File, log string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "build.log")
	writeFile(t, p, log)
	m := testManager(t)
	m.Out = out
	done := make(chan struct{})
	close(done)
	m.follow(p, done)
}

func TestFollowPrintsProgressWithoutSpinnerOffTerminal(t *testing.T) {
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	followOut(t, out, "noise\nx curral-build: packages\n")
	b, _ := os.ReadFile(out.Name())
	if string(b) != "  packages\n" {
		t.Fatalf("output %q", b)
	}
}
