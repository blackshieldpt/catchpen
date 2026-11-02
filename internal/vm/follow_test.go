package vm

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// followOut runs follow over a finished log, writing to out.
func followOut(t *testing.T, out io.Writer, log string) {
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
	followOut(t, out, "noise\nx catchpen-build: packages\n")
	b, _ := os.ReadFile(out.Name())
	if string(b) != "  packages\n" {
		t.Fatalf("output %q", b)
	}
}

// A per-operation writer that isn't a file (an API job's) gets the same plain
// lines as a file; no writer gets nothing, without a panic.
func TestFollowPrintsLinesToAnyWriter(t *testing.T) {
	var out bytes.Buffer
	followOut(t, &out, "noise\nx catchpen-build: packages\n")
	if out.String() != "  packages\n" {
		t.Fatalf("output %q", out.String())
	}
	followOut(t, nil, "x catchpen-build: packages\n")
}
