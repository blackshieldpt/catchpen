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

// Set and Unset change only the lines of their name, and what Set writes
// ParseFile reads back as it was set.
func TestSetUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	read := func(b []byte) map[string]string {
		t.Helper()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := ParseFile(path)
		if err != nil {
			t.Fatalf("%q: %v", b, err)
		}
		return m
	}
	set := func(b, name, v string) (string, bool) {
		t.Helper()
		out, replaced, err := Set([]byte(b), name, v)
		if err != nil {
			t.Fatalf("Set(%q, %s): %v", b, name, err)
		}
		return string(out), replaced
	}
	for _, v := range []string{"plain", " lead", "trail ", "\ttab", `"quoted"`, `'single'`, `"`, `a"`, `"a'`, "a=b#c", "export X=1", strings.Repeat("x", MaxValue)} {
		out, _ := set("A=1", "K", v)
		if got := read([]byte(out)); got["K"] != v || got["A"] != "1" {
			t.Errorf("%q: read back %q", v, got)
		}
	}

	file := "## secrets\n# K=\nexport K=old\nB=2\n  K = older\n# K=\n"
	if got, replaced := set(file, "K", "new"); got != "## secrets\n# K=\nB=2\nK=new\n# K=\n" || !replaced {
		t.Errorf("replaced: %q %v", got, replaced) // the last line setting K: ParseFile's
	}
	if got, _ := set("export K=old\nB=2\n", "K", "new"); got != "export K=new\nB=2\n" {
		t.Errorf("export kept: %q", got)
	}
	// A value on an earlier line is lost too, though the last line sets none.
	if got, replaced := set("K=real\nK=\n", "K", "new"); got != "K=new\n" || !replaced {
		t.Errorf("an earlier value: %q %v", got, replaced)
	}
	if got, replaced := set("K=\n", "K", "new"); got != "K=new\n" || replaced {
		t.Errorf("no value: %q %v", got, replaced)
	}
	if got, replaced := set("K=\"\"\n", "K", "new"); got != "K=new\n" || replaced {
		t.Errorf("no value, quoted: %q %v", got, replaced)
	}
	if got, replaced := set("# A note\n# K=\nB=2\n", "K", "v"); got != "# A note\nK=v\nB=2\n" || replaced {
		t.Errorf("a placeholder: %q %v", got, replaced)
	}
	if got, _ := set("B=2", "K", "v"); got != "B=2\nK=v\n" {
		t.Errorf("appended: %q", got)
	}
	if got, _ := set("", "K", "v"); got != "K=v\n" {
		t.Errorf("a new file: %q", got)
	}
	// Set checks what it writes: no name or value adds a line.
	for _, bad := range [][2]string{{"A\nB", "v"}, {"A=B", "v"}, {"K", "v\nB=2"}, {"K", ""}} {
		if out, _, err := Set([]byte("A=1\n"), bad[0], bad[1]); err == nil {
			t.Errorf("Set(%q, %q): %q", bad[0], bad[1], out)
		}
	}
	if got, found := Unset([]byte(file), "K"); string(got) != "## secrets\n# K=\nB=2\n# K=\n" || !found {
		t.Errorf("unset: %q %v", got, found)
	}
	if got, found := Unset([]byte("KK=1\nK_2=2\n"), "K"); string(got) != "KK=1\nK_2=2\n" || found {
		t.Errorf("unset of a prefix: %q %v", got, found)
	}

	for v, want := range map[string]string{"": "empty", "a\nb": "newline", "a\rb": "newline", "a\x00b": "NUL", strings.Repeat("x", MaxValue+1): "longer than 32768"} {
		if err := CheckValue(v); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.20q: %v, want %s", v, err, want)
		}
	}
	for name, ok := range map[string]bool{"GH_TOKEN": true, "_x9": true, "my.key": true, "A-B": true, "9X": false, ".x": false, "-x": false, "A B": false, "A=": false, "A/B": false, "": false, strings.Repeat("A", 129): false} {
		if (CheckName(name) == nil) != ok {
			t.Errorf("CheckName %q: %v", name, !ok)
		}
	}
}
