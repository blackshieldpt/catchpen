package audit

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordTail(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	l.Record(VMCreate, "a", map[string]any{"template": "default"})
	l.Record(CredsPush, "b", map[string]any{"secrets": []string{"K1", "K2"}})
	l.Record(ExportPR, "a", map[string]any{"branch": "x\x1b]52;c;evil\x07", "title": "a\n2026-01-01T00:00:00Z vm.destroy a"})
	var all, one bytes.Buffer
	if err := l.Tail(context.Background(), "", false, &all); err != nil {
		t.Fatal(err)
	}
	if err := l.Tail(context.Background(), "a", false, &one); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(all.String(), "\n"); n != 3 {
		t.Errorf("all: %d lines\n%s", n, all.String())
	}
	if strings.Contains(one.String(), "creds.push") || !strings.Contains(one.String(), "vm.create") {
		t.Errorf("filtered:\n%s", one.String())
	}
	if !strings.Contains(all.String(), "secrets=K1,K2") {
		t.Errorf("list detail:\n%s", all.String())
	}
	if !strings.Contains(all.String(), `title=a\n2026`) {
		t.Errorf("newline in a value not escaped:\n%s", all.String())
	}
	if strings.Contains(all.String(), "\x1b") || strings.Contains(all.String(), "\x07") {
		t.Errorf("control characters printed: %q", all.String())
	}
	if err := (Log{}).Record(VMStart, "a", nil); err != nil {
		t.Errorf("zero log: %v", err)
	}
}
