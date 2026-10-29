package jsonl

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("a\n\n  \nb\nc"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := Tail(context.Background(), path, false, func(l string) error { got = append(got, l); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("lines %q", got)
	}
}

// A guest-written log (task output) can hold one huge line: Tail skips it
// without holding it in memory, and goes on with the next lines.
func TestTailSkipsOverlongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	data := append([]byte("a\n"), bytes.Repeat([]byte("x"), 128<<20)...)
	data = append(data, "\nb\n"...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := Tail(context.Background(), path, false, func(l string) error { got = append(got, l); return nil })
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("got %d lines, want a and b", len(got))
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 160<<20 {
		t.Errorf("allocated %d MiB reading a 128 MiB line", grew>>20)
	}
}
