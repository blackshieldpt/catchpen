package audit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `catchpen audit` shows the events rotated out to audit.jsonl.1 first.
func TestTailReadsRotatedFile(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	if err := l.Record(VMCreate, "oldvm", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(l.Path, l.Path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(VMStart, "newvm", nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := l.Tail(context.Background(), "", false, &out); err != nil {
		t.Fatal(err)
	}
	o, n := strings.Index(out.String(), "oldvm"), strings.Index(out.String(), "newvm")
	if o < 0 || n < 0 || o > n {
		t.Errorf("want oldvm then newvm:\n%s", out.String())
	}
	var one bytes.Buffer
	if err := l.Tail(context.Background(), "oldvm", false, &one); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(one.String(), "oldvm") || strings.Contains(one.String(), "newvm") {
		t.Errorf("filtered to oldvm:\n%s", one.String())
	}
}
