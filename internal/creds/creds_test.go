package creds

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupOrder(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secrets.env")
	os.WriteFile(file, []byte("# comment\nexport FROM_FILE='file value'\nBOTH=file\nQUOTED=\"q\"\nEMPTY=\n"), 0o600)
	env := map[string]string{"BOTH": "env", "ALIAS_SRC": "aliased"}
	r := &Resolver{
		File:   file,
		Getenv: func(k string) string { return env[k] },
		Sources: map[string]Source{
			"CMD":   {Command: []string{"printf", "from-cmd\\n"}},
			"ALIAS": {Env: "ALIAS_SRC"},
			"FAIL":  {Command: []string{"false"}},
			"BOTH":  {Command: []string{"printf", "cmd-wins"}},
		},
	}
	ctx := context.Background()
	for name, want := range map[string]string{"CMD": "from-cmd", "ALIAS": "aliased", "BOTH": "cmd-wins", "FROM_FILE": "file value", "QUOTED": "q"} {
		v, ok, err := r.Lookup(ctx, name)
		if err != nil || !ok || v != want {
			t.Errorf("Lookup(%s) = %q, %v, %v; want %q", name, v, ok, err, want)
		}
	}
	for _, name := range []string{"MISSING", "EMPTY"} {
		if _, ok, err := r.Lookup(ctx, name); ok || err != nil {
			t.Errorf("Lookup(%s): ok=%v err=%v, want not found", name, ok, err)
		}
	}
	if _, _, err := r.Lookup(ctx, "FAIL"); err == nil {
		t.Error("failing command must be an error, not a silent miss")
	}
	env["ENVONLY"] = "e"
	if v, ok, _ := r.Lookup(ctx, "ENVONLY"); !ok || v != "e" {
		t.Errorf("env lookup: %q %v", v, ok)
	}
}

func TestRejectsMultiline(t *testing.T) {
	r := &Resolver{Sources: map[string]Source{"X": {Command: []string{"printf", "a\\nb"}}}}
	if _, _, err := r.Lookup(context.Background(), "X"); err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseFileErrors(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open.env")
	os.WriteFile(open, []byte("A=b\n"), 0o644)
	if _, err := ParseFile(open); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("world-readable file: %v", err)
	}
	bad := filepath.Join(dir, "bad.env")
	os.WriteFile(bad, []byte("A=b\nnot a pair\n"), 0o600)
	if _, err := ParseFile(bad); err == nil || !strings.Contains(err.Error(), ":2:") {
		t.Errorf("bad line: %v", err)
	}
	if m, err := ParseFile(filepath.Join(dir, "missing")); err != nil || len(m) != 0 {
		t.Errorf("missing file: %v %v", m, err)
	}
}
