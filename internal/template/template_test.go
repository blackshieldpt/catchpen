package template

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestAgentsMerge(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", `
extends: default
agents:
  claude: { version: "2.1.303" }
  opencode: { keys: [OPENROUTER_API_KEY] }
env: { FOO: bar, TOKEN: "secret:TOKEN" }
mcp:
  - { name: fs, type: stdio, command: [npx, server-fs] }
sync: { claude_config: true }
`)
	write(t, dir, "b.yaml", `
extends: a
agents:
  opencode: { keys: [ANTHROPIC_API_KEY, OPENROUTER_API_KEY] }
env: { FOO: baz }
mcp:
  - { name: fs, type: http, url: "https://fs.example.com/mcp" }
  - { name: docs, type: sse, url: "https://docs.example.com/sse", headers: { Authorization: "secret:DOCS" } }
`)
	tp, err := Loader{UserDir: dir}.Load("b")
	if err != nil {
		t.Fatal(err)
	}
	a := tp.Agents
	if a.Claude == nil || a.Claude.Auth != ClaudeOAuth || a.Claude.Version != "2.1.303" || a.Pi == nil {
		t.Errorf("agents not merged: claude=%+v pi=%+v", a.Claude, a.Pi)
	}
	if !slices.Equal(a.OpenCode.Keys, []string{"OPENROUTER_API_KEY", "ANTHROPIC_API_KEY"}) {
		t.Errorf("opencode keys = %v", a.OpenCode.Keys)
	}
	if tp.Env["FOO"] != "baz" || tp.Env["TOKEN"] != "secret:TOKEN" {
		t.Errorf("env = %v", tp.Env)
	}
	if len(tp.MCP) != 2 || tp.MCP[0].Type != "http" || tp.MCP[1].Name != "docs" {
		t.Errorf("mcp = %+v", tp.MCP)
	}
	if !tp.Sync.Claude() || !tp.Sync.Git() {
		t.Errorf("sync = %+v", tp.Sync)
	}
	// The parent's agent struct is not aliased by the merge.
	pa, _ := Loader{UserDir: dir}.Load("a")
	if len(pa.Agents.OpenCode.Keys) != 1 {
		t.Errorf("parent keys mutated: %v", pa.Agents.OpenCode.Keys)
	}
}

func TestAgentsValidation(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"nonode":     {"agents: { claude: {} }\n", "agents need runtimes.node"},
		"badauth":    {"extends: default\nagents: { claude: { auth: password } }\n", "agents.claude.auth"},
		"subauth":    {"extends: default\nagents: { pi: { auth: oauth-token } }\n", "Claude Code only"},
		"claudekeys": {"extends: default\nagents: { claude: { keys: [OPENAI_API_KEY] } }\n", "agents.claude.keys"},
		"badkey":     {"extends: default\nagents: { pi: { keys: [MY_KEY] } }\n", "unknown provider key"},
		"badver":     {"extends: default\nagents: { pi: { version: \"1.0; rm\" } }\n", "agents.pi.version"},
		"reserved":   {"env: { PATH: /x }\n", "env: invalid name"},
		"lower":      {"env: { foo: x }\n", "env: invalid name"},
		"secretname": {"env: { A: \"secret:bad name\" }\n", "invalid secret name"},
		"override":   {"extends: default\nenv: { ANTHROPIC_API_KEY: \"secret:K\" }\n", "override Claude's subscription token"},
		"mcpname":    {"mcp: [{ name: \"a b\", type: stdio, command: [x] }]\n", "mcp: invalid name"},
		"mcptype":    {"mcp: [{ name: a, type: ws, url: \"https://x\" }]\n", "type must be"},
		"mcpstdio":   {"mcp: [{ name: a, type: stdio }]\n", "need command"},
		"mcpurl":     {"mcp: [{ name: a, type: http, url: \"file:///etc\" }]\n", "http(s) url"},
		"mcphdr":     {"mcp: [{ name: a, type: http, url: \"https://x\", headers: { \"Bad Header\": v } }]\n", "invalid header"},
	}
	dir := t.TempDir()
	for name, c := range cases {
		write(t, dir, name+".yaml", c.body)
	}
	for name, c := range cases {
		_, err := Loader{UserDir: dir}.Load(name)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	write(t, dir, "apikey.yaml", "extends: default\nagents: { claude: { auth: api-key } }\nenv: { ANTHROPIC_API_KEY: \"secret:K\" }\n")
	if _, err := (Loader{UserDir: dir}).Load("apikey"); err != nil {
		t.Errorf("api-key mode with ANTHROPIC_API_KEY env: %v", err)
	}
}

func TestSSHAgentConfig(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "ssh: { agent: restricted, agent_keys: [~/.ssh/id_ed25519], confirm: false }\n")
	write(t, dir, "b.yaml", "extends: a\nssh: { agent_keys: [/keys/deploy] }\n")
	tp, err := Loader{UserDir: dir}.Load("b")
	if err != nil {
		t.Fatal(err)
	}
	if tp.SSH.Mode() != AgentRestricted || tp.SSH.ConfirmEach() || !slices.Equal(tp.SSH.AgentKeys, []string{"~/.ssh/id_ed25519", "/keys/deploy"}) {
		t.Errorf("ssh = %+v", tp.SSH)
	}
	if (SSH{}).Mode() != AgentOff || !(SSH{}).ConfirmEach() {
		t.Error("defaults")
	}
	for body, want := range map[string]string{
		"ssh: { agent: always }\n":                                 "ssh.agent must be",
		"ssh: { agent: restricted }\n":                             "needs ssh.agent_keys",
		"ssh: { agent: restricted, agent_keys: [relative/key] }\n": "absolute or ~/ path",
		"ssh: { agent: host, agent_keys: [/k] }\n":                 "only applies to ssh.agent: restricted",
	} {
		write(t, dir, "x.yaml", body)
		if _, err := (Loader{UserDir: dir}).Load("x"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

func TestAgentYolo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "runtimes: { node: \"24\" }\nagents: { claude: { yolo: true }, opencode: { yolo: true } }\n")
	write(t, dir, "b.yaml", "extends: a\nagents: { opencode: { yolo: false } }\n")
	tp, err := Loader{UserDir: dir}.Load("b")
	if err != nil {
		t.Fatal(err)
	}
	if !tp.Agents.Claude.SkipPermissions() || tp.Agents.OpenCode.SkipPermissions() {
		t.Errorf("yolo merge: claude %v opencode %v", tp.Agents.Claude.SkipPermissions(), tp.Agents.OpenCode.SkipPermissions())
	}
	if (*Agent)(nil).SkipPermissions() {
		t.Error("nil agent")
	}
	write(t, dir, "c.yaml", "runtimes: { node: \"24\" }\nagents: { pi: { yolo: true } }\n")
	if _, err := (Loader{UserDir: dir}).Load("c"); err == nil || !strings.Contains(err.Error(), "agents.pi.yolo") {
		t.Errorf("pi yolo: %v", err)
	}
}

func TestParseInterval(t *testing.T) {
	for s, want := range map[string]time.Duration{"": 0, "off": 0, "daily": 24 * time.Hour, "weekly": 168 * time.Hour, "90m": 90 * time.Minute} {
		if d, err := ParseInterval(s); err != nil || d != want {
			t.Errorf("ParseInterval(%q) = %s, %v", s, d, err)
		}
	}
	for _, s := range []string{"30s", "-1h", "soon", "0"} {
		if _, err := ParseInterval(s); err == nil {
			t.Errorf("ParseInterval(%q) accepted", s)
		}
	}
}
