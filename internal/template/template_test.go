package template

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinDefaultMergesBase(t *testing.T) {
	tp, err := Loader{UserDir: t.TempDir()}.Load("default")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"git", "curl", "build-essential"} {
		if !slices.Contains(tp.Packages, p) {
			t.Errorf("missing package %s in %v", p, tp.Packages)
		}
	}
	if !tp.HasDocker() || tp.Runtimes.Node == "" || tp.Network.Egress != "allowlist" {
		t.Errorf("unexpected merge result: %+v", tp)
	}
	if !slices.Contains(tp.Network.Presets, "anthropic") || !slices.Contains(tp.Network.Presets, "github") {
		t.Errorf("presets not merged: %v", tp.Network.Presets)
	}
}

func TestUserTemplateOverridesAndExtendsBuiltin(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "scripts/hello.sh", "echo hi\n")
	write(t, dir, "files/rc", "set -o vi\n")
	write(t, dir, "mine.yaml", `
extends: minimal
packages: [git, golang-go]
runtimes: { go: "1.27" }
scripts: [scripts/hello.sh]
files: [{ src: files/rc, dst: ~/.inputrc, mode: "0600" }]
ports: [3000, "8080:80"]
docker: false
`)
	tp, err := Loader{UserDir: dir}.Load("mine")
	if err != nil {
		t.Fatal(err)
	}
	if tp.Resources.CPUs != 2 {
		t.Errorf("resources not inherited from minimal: %+v", tp.Resources)
	}
	if n := strings.Count(strings.Join(tp.Packages, " "), "git "); n != 1 {
		t.Errorf("git duplicated: %v", tp.Packages)
	}
	if string(tp.Asset("scripts/hello.sh")) != "echo hi\n" || string(tp.Asset("files/rc")) != "set -o vi\n" {
		t.Error("assets not loaded")
	}
	want := []Port{{Host: 0, Guest: 3000}, {Host: 8080, Guest: 80}}
	if !slices.Equal(tp.Ports, want) {
		t.Errorf("ports = %v, want %v", tp.Ports, want)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"cycle":    {"extends: cycle2\n", "extends cycle"},
		"escape":   {"scripts: [../../etc/passwd]\n", "must be a relative path"},
		"abs":      {"scripts: [/etc/passwd]\n", "must be a relative path"},
		"badpkg":   {"packages: [\"git; rm -rf /\"]\n", "invalid package name"},
		"badnode":  {"runtimes: { node: \"lts; x\" }\n", "runtimes.node"},
		"baddst":   {"files: [{ src: f, dst: \"relative/path\" }]\n", "invalid dst"},
		"egress":   {"network: { egress: sometimes }\n", "network.egress"},
		"ondeny":   {"network: { on_deny: ask }\n", "network.on_deny"},
		"timeout0": {"network: { prompt_timeout: 0s }\n", "prompt_timeout"},
		"timeoutx": {"network: { prompt_timeout: soon }\n", "prompt_timeout"},
		"unknown":  {"frobnicate: true\n", "field frobnicate not found"},
		"named":    {"name: other\n", "does not match file name"},
		"missing":  {"scripts: [nope.sh]\n", "asset \"nope.sh\""},
	}
	dir := t.TempDir()
	write(t, dir, "cycle2.yaml", "extends: cycle\n")
	write(t, dir, "f", "x")
	for name, c := range cases {
		write(t, dir, name+".yaml", c.body)
	}
	for name, c := range cases {
		_, err := (Loader{UserDir: dir}).Load(name)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
	if _, err := (Loader{UserDir: dir}).Load("../x"); err == nil || !strings.Contains(err.Error(), "invalid template name") {
		t.Errorf("bad name: %v", err)
	}
}

func TestPromptTimeoutAccepted(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "p.yaml", "network: { prompt_timeout: 1m30s }\n")
	if _, err := (Loader{UserDir: dir}).Load("p"); err != nil {
		t.Fatal(err)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"512M": 512, "8G": 8192, "1T": 1 << 20} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "8", "8g", "0M", "-1G", "8GB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): expected error", in)
		}
	}
}
