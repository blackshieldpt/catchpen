package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/image"
	"github.com/blackshieldpt/curral/internal/paths"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	p := paths.Paths{Config: filepath.Join(root, "config"), Data: filepath.Join(root, "data"), Cache: filepath.Join(root, "cache")}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	return &Manager{P: p, Cfg: config.Default(), Tpl: template.Loader{UserDir: p.Templates()}, Bin: "/usr/local/bin/curral"}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLastLine(t *testing.T) {
	for in, want := range map[string]string{
		"a\nb\nCURRAL-BUILD-OK\n":         "CURRAL-BUILD-OK",
		"CURRAL-BUILD-OK\r\n\x00\x00":     "CURRAL-BUILD-OK",
		"CURRAL-BUILD-OK\nlater output\n": "later output", // the marker must be last
		"":                                "",
		"  x  ":                           "x",
	} {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrintable(t *testing.T) {
	in := "ok\tline\n\x1b]0;title\x07\x1b[31mred\r\x7f\u009bcsi é"
	want := "ok\tline\n]0;title[31mredcsi é"
	if got := sshx.Printable(in); got != want {
		t.Fatalf("printable = %q, want %q", got, want)
	}
}

func TestReadTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	writeFile(t, p, strings.Repeat("x", 10000)+"\nEND\n")
	got, err := readTail(p, 100)
	if err != nil || len(got) != 100 || !strings.HasSuffix(got, "\nEND\n") {
		t.Fatalf("readTail = %d bytes %q..., %v", len(got), got[len(got)-5:], err)
	}
	writeFile(t, p, "short\n")
	if got, _ := readTail(p, 100); got != "short\n" {
		t.Fatalf("short file: %q", got)
	}
}

func TestBaseHash(t *testing.T) {
	s := image.Spec{Name: "x", URL: "https://e/x.img", SHA256: strings.Repeat("a", 64)}
	h := baseHash(s, "agent", []byte("ud"))
	pinned := s
	pinned.SHA256 = strings.Repeat("b", 64)
	for name, other := range map[string]string{
		"pin":       baseHash(pinned, "agent", []byte("ud")),
		"user":      baseHash(s, "dev", []byte("ud")),
		"user-data": baseHash(s, "agent", []byte("ud2")),
		"boundary":  baseHash(image.Spec{Name: "x", URL: "https://e/x.img" + strings.Repeat("a", 64)}, "agent", []byte("ud")),
	} {
		if other == h {
			t.Errorf("%s change does not change the base hash", name)
		}
	}
	if baseHash(s, "agent", []byte("ud")) != h || len(h) != 16 {
		t.Error("hash not stable")
	}
}

func TestCreateRefusesOpenEgressWithoutConfirmation(t *testing.T) {
	m := testManager(t)
	writeFile(t, filepath.Join(m.P.Templates(), "wide.yaml"), "extends: minimal\nnetwork: { egress: open }\n")
	_, err := m.Create(context.Background(), "a", CreateOpts{Template: "wide"})
	if err == nil || !strings.Contains(err.Error(), "--allow-open") {
		t.Fatalf("err = %v, want refusal mentioning --allow-open", err)
	}
	if _, err := os.Stat(m.dir("a")); !os.IsNotExist(err) {
		t.Error("VM directory created despite refusal")
	}
	if ents, _ := os.ReadDir(m.P.Bases()); len(ents) != 0 {
		t.Errorf("refusal happened after base work started: %v", ents)
	}
}

func TestCreateRejectsBadCPUs(t *testing.T) {
	m := testManager(t)
	for _, n := range []int{-1, maxCPUs + 1} {
		if _, err := m.Create(context.Background(), "a", CreateOpts{Template: "minimal", CPUs: n}); err == nil || !strings.Contains(err.Error(), "CPU") {
			t.Errorf("cpus %d: err = %v", n, err)
		}
	}
}

func TestRebuildRefusedWhileBaseInUse(t *testing.T) {
	m := testManager(t)
	r, err := m.Resolve("minimal", "")
	if err != nil {
		t.Fatal(err)
	}
	base := r.BasePath(m.P)
	writeFile(t, base, "base")
	os.MkdirAll(m.dir("user1"), 0o700)
	if err := writeJSON(filepath.Join(m.dir("user1"), stateFile), State{Name: "user1", Base: base}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(m.dir("other"), stateFile), `{"name":"other","base":"/elsewhere"}`)
	writeFile(t, filepath.Join(m.dir("broken"), stateFile), "{garbage")
	writeFile(t, filepath.Join(m.P.VMs(), ".destroyed-x-1", stateFile), "{garbage")

	// Without --rebuild the existing base is reused.
	if got, err := m.EnsureBase(context.Background(), r, false); err != nil || got != base {
		t.Fatalf("EnsureBase = %q, %v", got, err)
	}
	_, err = m.EnsureBase(context.Background(), r, true)
	if err == nil {
		t.Fatal("rebuild of a base in use was allowed")
	}
	for _, want := range []string{"user1", "broken (unreadable state)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	for _, not := range []string{"other", "destroyed"} {
		if strings.Contains(err.Error(), not) {
			t.Errorf("error %q wrongly names %s", err, not)
		}
	}
	if b, _ := os.ReadFile(base); string(b) != "base" {
		t.Error("base modified")
	}
}

func needQemuImg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
}

func TestCreateRollsBackOnFailure(t *testing.T) {
	needQemuImg(t)
	m := testManager(t)
	r, err := m.Resolve("minimal", "")
	if err != nil {
		t.Fatal(err)
	}
	base := r.BasePath(m.P)
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", base, "1G").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// Fails at the last step (ssh_config refuses %-tokens in the binary path),
	// after keys, overlay, seed, egress files and state were written.
	m.Bin = "/opt/cur%ral"
	_, err = m.Create(context.Background(), "a", CreateOpts{Template: "minimal"})
	if err == nil {
		t.Fatal("Create succeeded")
	}
	if _, err := os.Stat(m.dir("a")); !os.IsNotExist(err) {
		t.Fatalf("VM directory left behind after failed create: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Errorf("base removed by rollback: %v", err)
	}

	m.Bin = "/usr/local/bin/curral"
	s, err := m.Create(context.Background(), "a", CreateOpts{Template: "minimal", CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUs != 1 || s.Base != base || s.Egress != "allowlist" {
		t.Errorf("state = %+v", s)
	}
	for _, f := range []string{stateFile, diskFile, seedFile, "allowlist", "ssh"} {
		if _, err := os.Stat(m.file("a", f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(m.P.SSHConfig()); !strings.Contains(string(b), "Host curral-a\n") {
		t.Errorf("ssh_config lacks the VM: %q", b)
	}
	if _, err := m.Create(context.Background(), "a", CreateOpts{Template: "minimal"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate create: %v", err)
	}
}

func TestDestroyBrokenVM(t *testing.T) {
	m := testManager(t)
	writeFile(t, filepath.Join(m.dir("half"), "ssh", "id_ed25519"), "k")
	if err := m.Destroy("half"); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(m.P.VMs())
	if len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	if err := m.Destroy("half"); err == nil || !strings.Contains(err.Error(), "no VM named") {
		t.Errorf("destroy of missing VM: %v", err)
	}
	if err := m.Destroy("../x"); err == nil {
		t.Error("invalid name accepted")
	}
}

func TestCreateWithAgents(t *testing.T) {
	needQemuImg(t)
	m := testManager(t)
	writeFile(t, filepath.Join(m.P.Templates(), "ag.yaml"), `
extends: minimal
runtimes: { node: "24" }
agents:
  claude: {}
  pi: { keys: [OPENROUTER_API_KEY, ANTHROPIC_API_KEY] }
env: { FOO: "secret:FOO" }
mcp:
  - { name: docs, type: http, url: "https://mcp.example.com:8443/mcp" }
  - { name: plain, type: sse, url: "http://sse.example.com/sse" }
  - { name: fs, type: stdio, command: [npx, fs] }
`)
	r, err := m.Resolve("ag", "")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", r.BasePath(m.P), "1G").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := m.Create(context.Background(), "a", CreateOpts{Template: "ag", ClaudeAuth: "password"}); err == nil || !strings.Contains(err.Error(), "agents.claude.auth") {
		t.Fatalf("invalid --claude-auth: %v", err)
	}
	s, err := m.Create(context.Background(), "a", CreateOpts{Template: "ag", ClaudeAuth: "api-key"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Agents.Claude == nil || s.Agents.Claude.Auth != "api-key" || s.Env["FOO"] != "secret:FOO" || len(s.MCP) != 3 {
		t.Errorf("state = %+v", s)
	}
	b, _ := os.ReadFile(m.file("a", "allowlist"))
	for _, want := range []string{"api.anthropic.com:443", "openrouter.ai:443", "mcp.example.com:8443", "sse.example.com:80"} {
		if !strings.Contains(string(b), want+"\n") {
			t.Errorf("allowlist lacks %s", want)
		}
	}
	// The override applies to this VM only.
	if tp, _ := m.Tpl.Load("ag"); tp.Agents.Claude.Auth != "" {
		t.Errorf("template mutated: %+v", tp.Agents.Claude)
	}
	if _, err := m.Create(context.Background(), "b", CreateOpts{Template: "minimal", ClaudeAuth: "api-key"}); err == nil || !strings.Contains(err.Error(), "no claude agent") {
		t.Errorf("claude-auth on template without claude: %v", err)
	}
}
