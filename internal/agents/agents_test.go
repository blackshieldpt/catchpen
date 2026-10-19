package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackshieldpt/curral/internal/template"
)

// localRemote runs the guest-side commands in a local shell with HOME set to
// a temp dir, so the real scripts are exercised.
type localRemote struct{ home string }

func (l localRemote) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, error) {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = []string{"HOME=" + l.home, "PATH=" + os.Getenv("PATH")}
	c.Stdin = strings.NewReader(string(stdin))
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w", cmd, err)
	}
	return out, nil
}

type fakeSecrets map[string]string

func (f fakeSecrets) Lookup(_ context.Context, name string) (string, bool, error) {
	v, ok := f[name]
	return v, ok, nil
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func get(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	oauth  = "sk-ant-oat01-oauthtoken-0000000000"
	apiKey = "sk-ant-api03-apikey-1111111111111111111111"
	orKey  = "sk-or-v1-openrouter-2222"
)

func fullSpec() Spec {
	return Spec{
		Agents: template.Agents{
			Claude:   &template.Agent{Auth: template.ClaudeOAuth},
			OpenCode: &template.Agent{Keys: []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"}},
			Pi:       &template.Agent{Keys: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}},
		},
		Env: map[string]string{"FOO": "literal value", "BAR": "secret:BAR"},
		MCP: []template.MCP{
			{Name: "fs", Type: "stdio", Command: []string{"npx", "-y", "server-fs", "/w"}, Env: map[string]string{"TOKEN": "secret:FS_TOKEN"}},
			{Name: "docs", Type: "http", URL: "https://mcp.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer x"}},
			{Name: "old", Type: "sse", URL: "https://sse.example.com/sse"},
			{Name: "nokey", Type: "http", URL: "https://k.example.com/mcp", Headers: map[string]string{"X-Key": "secret:MISSING_MCP"}},
		},
		GitName:  "Jo O'Brien",
		GitEmail: "jo@example.com",
	}
}

func TestPushFull(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude.json"), `{"theme":"dark","mcpServers":{"mine":{"type":"stdio","command":"x"}}}`)
	write(t, filepath.Join(home, opencodeAuth), `{"opencode":{"type":"api","key":"keep"}}`)
	sec := fakeSecrets{"CLAUDE_CODE_OAUTH_TOKEN": oauth, "ANTHROPIC_API_KEY": apiKey, "OPENROUTER_API_KEY": orKey, "BAR": "bar-secret", "FS_TOKEN": "fs-secret"}
	res, err := Push(context.Background(), localRemote{home}, fullSpec(), sec)
	if err != nil {
		t.Fatal(err)
	}

	env, _ := os.ReadFile(filepath.Join(home, envFile))
	for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN=" + oauth + "\n", "FOO=literal value\n", "BAR=bar-secret\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env file missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(string(env), "ANTHROPIC_API_KEY") {
		t.Error("ANTHROPIC_API_KEY in the shared env would override Claude's subscription token")
	}
	for _, f := range []string{envFile, claudeJSON, opencodeAuth, opencodeConfig, piAuth, piMCP} {
		st, err := os.Stat(filepath.Join(home, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v mode %v, want 0600", f, err, st.Mode())
		}
	}

	cj := readJSON(t, filepath.Join(home, claudeJSON))
	if cj["hasCompletedOnboarding"] != true || cj["theme"] != "dark" || get(cj, "mcpServers", "mine") == nil {
		t.Errorf("claude.json not merged: %v", cj)
	}
	if get(cj, "mcpServers", "fs", "command") != "npx" || get(cj, "mcpServers", "fs", "env", "TOKEN") != "fs-secret" ||
		get(cj, "mcpServers", "docs", "type") != "http" || get(cj, "mcpServers", "old", "type") != "sse" {
		t.Errorf("claude mcpServers: %v", cj["mcpServers"])
	}
	if get(cj, "mcpServers", "nokey") != nil {
		t.Error("server with a missing secret was configured")
	}
	if get(cj, "customApiKeyResponses") != nil {
		t.Error("oauth mode must not approve an API key")
	}

	oa := readJSON(t, filepath.Join(home, opencodeAuth))
	if get(oa, "anthropic", "key") != apiKey || get(oa, "anthropic", "type") != "api" || get(oa, "openrouter", "key") != orKey || get(oa, "opencode", "key") != "keep" {
		t.Errorf("opencode auth: %v", oa)
	}
	oc := readJSON(t, filepath.Join(home, opencodeConfig))
	if oc["autoupdate"] != false || get(oc, "mcp", "fs", "type") != "local" || get(oc, "mcp", "fs", "environment", "TOKEN") != "fs-secret" ||
		get(oc, "mcp", "docs", "type") != "remote" || get(oc, "mcp", "docs", "headers", "Authorization") != "Bearer x" {
		t.Errorf("opencode config: %v", oc)
	}
	if cmd, _ := get(oc, "mcp", "fs", "command").([]any); len(cmd) != 4 {
		t.Errorf("opencode command: %v", cmd)
	}

	pa := readJSON(t, filepath.Join(home, piAuth))
	if get(pa, "anthropic", "type") != "api_key" || get(pa, "anthropic", "key") != apiKey || get(pa, "openai") != nil {
		t.Errorf("pi auth: %v", pa)
	}
	pm := readJSON(t, filepath.Join(home, piMCP))
	if get(pm, "mcpServers", "fs", "command") != "npx" || get(pm, "mcpServers", "docs", "url") == nil || get(pm, "mcpServers", "old") != nil {
		t.Errorf("pi mcp: %v", pm)
	}

	if strings.Join(res.Missing, ",") != "OPENAI_API_KEY,MISSING_MCP" && strings.Join(res.Missing, ",") != "MISSING_MCP,OPENAI_API_KEY" {
		t.Errorf("missing = %v", res.Missing)
	}
	warns := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"mcp nokey: skipped", "pi does not support SSE"} {
		if !strings.Contains(warns, want) {
			t.Errorf("warnings %q lack %q", warns, want)
		}
	}

	gc, _ := exec.Command("git", "config", "--file", filepath.Join(home, ".gitconfig"), "--get", "user.name").Output()
	if strings.TrimSpace(string(gc)) != "Jo O'Brien" {
		t.Errorf("git user.name = %q", gc)
	}
}

func TestPushAPIKeyModeAndRepush(t *testing.T) {
	home := t.TempDir()
	suffix := apiKey[len(apiKey)-20:]
	write(t, filepath.Join(home, ".claude.json"), `{"customApiKeyResponses":{"approved":["other"],"rejected":["`+suffix+`"]}}`)
	s := Spec{Agents: template.Agents{Claude: &template.Agent{Auth: template.ClaudeAPIKey}}, Env: map[string]string{"GONE": "secret:GONE"}}
	sec := fakeSecrets{"ANTHROPIC_API_KEY": apiKey, "GONE": "v1"}
	if _, err := Push(context.Background(), localRemote{home}, s, sec); err != nil {
		t.Fatal(err)
	}
	cj := readJSON(t, filepath.Join(home, claudeJSON))
	if a, _ := get(cj, "customApiKeyResponses", "approved").([]any); len(a) != 2 || (a[0] != suffix && a[1] != suffix) {
		t.Errorf("approved = %v", a)
	}
	if r, _ := get(cj, "customApiKeyResponses", "rejected").([]any); len(r) != 0 {
		t.Errorf("rejected = %v", r)
	}
	env, _ := os.ReadFile(filepath.Join(home, envFile))
	if !strings.Contains(string(env), "ANTHROPIC_API_KEY="+apiKey) || !strings.Contains(string(env), "GONE=v1") {
		t.Fatalf("env: %s", env)
	}
	// A secret removed on the host disappears from the VM on the next push.
	delete(sec, "GONE")
	res, err := Push(context.Background(), localRemote{home}, s, sec)
	if err != nil {
		t.Fatal(err)
	}
	if env, _ = os.ReadFile(filepath.Join(home, envFile)); strings.Contains(string(env), "GONE") {
		t.Errorf("stale secret kept: %s", env)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "GONE" {
		t.Errorf("missing = %v", res.Missing)
	}
}

func TestPushWarnsWithoutClaudeToken(t *testing.T) {
	res, err := Push(context.Background(), localRemote{t.TempDir()}, Spec{Agents: template.Agents{Claude: &template.Agent{}}}, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "claude setup-token") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestPushReplacesInvalidJSON(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude.json"), "{not json")
	res, err := Push(context.Background(), localRemote{home}, Spec{Agents: template.Agents{Claude: &template.Agent{}}}, fakeSecrets{"CLAUDE_CODE_OAUTH_TOKEN": oauth})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude.json.curral-bak")); string(b) != "{not json" {
		t.Errorf("backup = %q", b)
	}
	if readJSON(t, filepath.Join(home, claudeJSON))["hasCompletedOnboarding"] != true {
		t.Error("not replaced")
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "not valid JSON") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestPushClaudeConfig(t *testing.T) {
	host := t.TempDir()
	write(t, filepath.Join(host, "settings.json"), `{"model":"opus","env":{"SECRET":"x"},"hooks":{"Stop":[]},"apiKeyHelper":"/bin/k","permissions":{"allow":["Bash(ls)"]}}`)
	write(t, filepath.Join(host, "CLAUDE.md"), "be terse\n")
	write(t, filepath.Join(host, ".credentials.json"), `{"token":"never"}`)
	write(t, filepath.Join(host, "projects", "p", "x.jsonl"), "history")
	write(t, filepath.Join(host, "agents", "rev.md"), "reviewer")
	write(t, filepath.Join(host, "commands", ".git", "config"), "skip dot dirs")
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	write(t, filepath.Join(fakeHome, ".ssh", "id_ed25519"), "PRIVATE KEY")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.txt"), "outside")
	ext := t.TempDir()
	write(t, filepath.Join(ext, "s1", "SKILL.md"), "skill one")
	write(t, filepath.Join(ext, "s1", ".env"), "TOKEN=x")
	os.Symlink(ext, filepath.Join(host, "skills"))                                         // top-level link: allowed
	os.Symlink(ext, filepath.Join(ext, "loop"))                                            // must not recurse forever
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ext, "s1", "leak.txt")) // escapes: skipped
	os.RemoveAll(filepath.Join(host, "commands"))
	os.Symlink(filepath.Join(fakeHome, ".ssh"), filepath.Join(host, "commands")) // credentials dir: refused

	home := t.TempDir()
	tru := true
	res, err := Push(context.Background(), localRemote{home}, Spec{Sync: template.Sync{ClaudeConfig: &tru}, ClaudeDir: host}, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(home, ".claude")
	st := readJSON(t, filepath.Join(g, "settings.json"))
	if st["model"] != "opus" || st["env"] != nil || st["hooks"] != nil || st["apiKeyHelper"] != nil || get(st, "permissions", "allow") == nil {
		t.Errorf("settings = %v", st)
	}
	for f, want := range map[string]string{"CLAUDE.md": "be terse\n", "agents/rev.md": "reviewer", "skills/s1/SKILL.md": "skill one"} {
		if b, err := os.ReadFile(filepath.Join(g, f)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", f, b, err)
		}
	}
	for _, f := range []string{".credentials.json", "projects", "commands", "skills/s1/leak.txt", "skills/s1/.env"} {
		if _, err := os.Stat(filepath.Join(g, f)); err == nil {
			t.Errorf("%s copied into the VM", f)
		}
	}
	warns := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"not synced: settings.json env", "skills/s1/leak.txt (links outside", "commands (points into a credentials directory)"} {
		if !strings.Contains(warns, want) {
			t.Errorf("warnings %q lack %q", warns, want)
		}
	}
}

func TestPushRefusesHugeGuestJSON(t *testing.T) {
	home := t.TempDir()
	big := strings.Repeat(" ", maxGuestJSON+1)
	write(t, filepath.Join(home, ".claude.json"), "{"+big+"}")
	_, err := Push(context.Background(), localRemote{home}, Spec{Agents: template.Agents{Claude: &template.Agent{}}}, fakeSecrets{})
	if err == nil || !strings.Contains(err.Error(), ".claude.json") {
		t.Fatalf("err = %v", err)
	}
}

func TestPushReplacesSymlinkedGuestFile(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	write(t, target, `{"keep":"me"}`)
	os.Symlink(target, filepath.Join(home, ".claude.json"))
	if _, err := Push(context.Background(), localRemote{home}, Spec{Agents: template.Agents{Claude: &template.Agent{}}}, fakeSecrets{}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(home, ".claude.json")); fi.Mode()&os.ModeSymlink != 0 {
		t.Error("symlink kept")
	}
	if b, _ := os.ReadFile(target); string(b) != `{"keep":"me"}` {
		t.Errorf("symlink target written: %s", b)
	}
}

func TestClaudeConfigSkipsCredentialsUnderSyncedParent(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	write(t, filepath.Join(fakeHome, ".config", "gh", "hosts.yml"), "oauth_token: x")
	write(t, filepath.Join(fakeHome, ".config", "curral", "secrets.env"), "K=v")
	write(t, filepath.Join(fakeHome, ".config", "tool", "conf"), "fine")
	host := t.TempDir()
	os.Symlink(filepath.Join(fakeHome, ".config"), filepath.Join(host, "skills")) // parent of credential dirs
	os.Symlink(fakeHome, filepath.Join(host, "agents"))                           // the home dir itself
	home := t.TempDir()
	tru := true
	res, err := Push(context.Background(), localRemote{home}, Spec{Sync: template.Sync{ClaudeConfig: &tru}, ClaudeDir: host}, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(home, ".claude")
	if b, err := os.ReadFile(filepath.Join(g, "skills", "tool", "conf")); err != nil || string(b) != "fine" {
		t.Errorf("ordinary file not synced: %v", err)
	}
	for _, f := range []string{"skills/gh/hosts.yml", "skills/curral/secrets.env", "agents"} {
		if _, err := os.Stat(filepath.Join(g, f)); err == nil {
			t.Errorf("%s copied into the VM", f)
		}
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "skills/gh (credentials directory)") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}
