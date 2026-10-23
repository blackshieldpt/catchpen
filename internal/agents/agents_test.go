package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
)

// localRemote runs the guest-side commands in a local shell with HOME set to
// a temp dir, so the real scripts are exercised.
type localRemote struct{ home string }

func (l localRemote) Run(ctx context.Context, cmd string, stdin []byte) (sshx.GuestOutput, error) {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = []string{"HOME=" + l.home, "PATH=" + os.Getenv("PATH")}
	c.Stdin = strings.NewReader(string(stdin))
	out, err := c.Output()
	if err != nil {
		return sshx.Guest(out), fmt.Errorf("%s: %w", cmd, err)
	}
	return sshx.Guest(out), nil
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
			OMP:      &template.Agent{Keys: []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY"}},
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
	for _, f := range []string{envFile, claudeJSON, opencodeAuth, opencodeConfig, piAuth, piMCP, ompEnv, ompMCP} {
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

	if b, _ := os.ReadFile(filepath.Join(home, ompEnv)); string(b) != "OPENROUTER_API_KEY="+orKey+"\n" {
		t.Errorf("omp env: %q", b)
	}
	om := readJSON(t, filepath.Join(home, ompMCP))
	if get(om, "mcpServers", "fs", "type") != "stdio" || get(om, "mcpServers", "fs", "env", "TOKEN") != "fs-secret" ||
		get(om, "mcpServers", "old", "type") != "sse" || get(om, "mcpServers", "docs", "headers", "Authorization") != "Bearer x" {
		t.Errorf("omp mcp: %v", om)
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

// The tests run on the host (or in a curral VM, whose base has the real
// path): bases without the claude wrapper, unless a test says otherwise.
func TestMain(m *testing.M) {
	claudeReal = "/nonexistent/curral-test/claude"
	os.Exit(m.Run())
}

// In a base that wraps claude, its token or key goes only to the wrapper's
// file; older bases get it in the shared environment, with a warning.
func TestPushClaudeCredentialOnlyForClaude(t *testing.T) {
	real := claudeReal
	t.Cleanup(func() { claudeReal = real })
	claudeReal = filepath.Join(t.TempDir(), "claude")
	write(t, claudeReal, "")
	for _, auth := range []string{template.ClaudeOAuth, template.ClaudeAPIKey} {
		home := t.TempDir()
		s := Spec{Agents: template.Agents{Claude: &template.Agent{Auth: auth}, OpenCode: &template.Agent{}}, Env: map[string]string{"FOO": "bar"}}
		sec := fakeSecrets{"CLAUDE_CODE_OAUTH_TOKEN": oauth, "ANTHROPIC_API_KEY": apiKey}
		res, err := Push(context.Background(), localRemote{home}, s, sec)
		if err != nil {
			t.Fatal(err)
		}
		name, val := "CLAUDE_CODE_OAUTH_TOKEN", oauth
		if auth == template.ClaudeAPIKey {
			name, val = "ANTHROPIC_API_KEY", apiKey
		}
		env, _ := os.ReadFile(filepath.Join(home, envFile))
		if strings.Contains(string(env), val) || !strings.Contains(string(env), "FOO=bar\n") {
			t.Errorf("%s: shared env:\n%s", auth, env)
		}
		ce, _ := os.ReadFile(filepath.Join(home, claudeEnvFile))
		if !strings.Contains(string(ce), name+"="+val+"\n") || strings.Contains(string(ce), "FOO") {
			t.Errorf("%s: claude env:\n%s", auth, ce)
		}
		if st, err := os.Stat(filepath.Join(home, claudeEnvFile)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: claude env file: %v", auth, err)
		}
		if len(res.Warnings) != 0 {
			t.Errorf("%s: warnings %v", auth, res.Warnings)
		}
		// A credential removed on the host disappears on the next push.
		if _, err := Push(context.Background(), localRemote{home}, s, fakeSecrets{}); err != nil {
			t.Fatal(err)
		}
		if ce, _ = os.ReadFile(filepath.Join(home, claudeEnvFile)); strings.Contains(string(ce), val) {
			t.Errorf("%s: stale credential kept: %s", auth, ce)
		}
	}

	claudeReal = filepath.Join(t.TempDir(), "missing")
	home := t.TempDir()
	res, err := Push(context.Background(), localRemote{home}, Spec{Agents: template.Agents{Claude: &template.Agent{}}}, fakeSecrets{"CLAUDE_CODE_OAUTH_TOKEN": oauth})
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := os.ReadFile(filepath.Join(home, envFile)); !strings.Contains(string(env), "CLAUDE_CODE_OAUTH_TOKEN="+oauth) {
		t.Errorf("old base: token not in the shared env:\n%s", env)
	}
	if _, err := os.Stat(filepath.Join(home, claudeEnvFile)); err == nil {
		t.Error("old base: claude env file written")
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "reset --latest") {
		t.Errorf("old base: warnings %v", res.Warnings)
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
	big := strings.Repeat(" ", maxGuestDoc+1)
	write(t, filepath.Join(home, ".claude.json"), "{"+big+"}")
	_, err := Push(context.Background(), localRemote{home}, Spec{Agents: template.Agents{Claude: &template.Agent{}}}, fakeSecrets{})
	if err == nil || !strings.Contains(err.Error(), "~/.claude.json is over 4 MiB") {
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

func TestPushHooks(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, claudeSettings), `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"mine"}]}]}}`)
	for i := 0; i < 2; i++ { // pushing again must not duplicate curral's hooks
		if _, err := Push(context.Background(), localRemote{home}, fullSpec(), fakeSecrets{}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(home, claudeSettings))
	var doc struct {
		Model string                      `json:"model"`
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if doc.Model != "opus" || len(doc.Hooks["Stop"]) != 2 || len(doc.Hooks["Notification"]) != 1 || strings.Count(s, notifyURL) != 2 || !strings.Contains(s, `"mine"`) {
		t.Errorf("settings.json:\n%s", s)
	}
	for f, ev := range map[string]string{opencodePlugin: "session.idle", piExtension: `pi.on("agent_settled"`, ompExtension: `pi.on("agent_end"`} {
		b, err := os.ReadFile(filepath.Join(home, f))
		if err != nil || !strings.Contains(string(b), notifyURL) || !strings.Contains(string(b), "node:http") || !strings.Contains(string(b), ev) {
			t.Errorf("%s: %v\n%s", f, err, b)
		}
	}
}

// fakeCodex puts a codex on PATH that keeps its login in ~/.codex/auth.json
// as the real one does: login --with-api-key stores stdin, login status
// reports an API key login, logout removes it; anything else fails.
func fakeCodex(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "codex"), `#!/bin/sh
f="$HOME/.codex/auth.json"
case "$*" in
"login --with-api-key") cat > "$f" ;;
"login status") grep -q '^sk-' "$f" 2>/dev/null && echo "Logged in using an API key" || echo "Logged in using ChatGPT" ;;
logout) rm -f "$f" ;;
*) exit 2 ;;
esac
`)
	os.Chmod(filepath.Join(bin, "codex"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func TestPushCodex(t *testing.T) {
	fakeCodex(t)
	home := t.TempDir()
	write(t, filepath.Join(home, codexConfig), "model = \"gpt-x\"\ncheck_for_update_on_startup = true\nsandbox_mode = \"read-only\"\n[mcp_servers.mine]\ncommand = \"x\"\n")
	s := fullSpec()
	s.Agents = template.Agents{Codex: &template.Agent{}}
	sec := fakeSecrets{"OPENAI_API_KEY": "sk-openai-test", "FS_TOKEN": "fs-secret"}
	res, err := Push(context.Background(), localRemote{home}, s, sec)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".codex/auth.json")); string(b) != "sk-openai-test" {
		t.Errorf("codex login got %q", b)
	}
	if env, _ := os.ReadFile(filepath.Join(home, envFile)); strings.Contains(string(env), "sk-openai-test") {
		t.Errorf("the OpenAI key is in the shared env:\n%s", env)
	}
	b, err := os.ReadFile(filepath.Join(home, codexConfig))
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(home, codexConfig)); st.Mode().Perm() != 0o600 {
		t.Errorf("config.toml mode %v", st.Mode())
	}
	cfg := map[string]any{}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config.toml: %v\n%s", err, b)
	}
	if cfg["model"] != "gpt-x" || cfg["sandbox_mode"] != "read-only" || get(cfg, "mcp_servers", "mine", "command") != "x" {
		t.Errorf("user settings not kept:\n%s", b)
	}
	if cfg["check_for_update_on_startup"] != false || cfg["cli_auth_credentials_store"] != "file" || get(cfg, "analytics", "enabled") != false ||
		get(cfg, "feedback", "enabled") != false || get(cfg, "otel", "metrics_exporter") != "none" {
		t.Errorf("curral settings:\n%s", b)
	}
	if n, _ := cfg["notify"].([]any); len(n) != 4 || n[0] != "sh" || !strings.Contains(n[2].(string), notifyURL) || !strings.Contains(n[2].(string), `"agent":"codex"`) {
		t.Errorf("notify = %v", cfg["notify"])
	}
	if get(cfg, "mcp_servers", "fs", "command") != "npx" || get(cfg, "mcp_servers", "fs", "env", "TOKEN") != "fs-secret" ||
		get(cfg, "mcp_servers", "docs", "url") != "https://mcp.example.com/mcp" || get(cfg, "mcp_servers", "docs", "http_headers", "Authorization") != "Bearer x" {
		t.Errorf("mcp_servers:\n%s", b)
	}
	if get(cfg, "mcp_servers", "old") != nil || !strings.Contains(strings.Join(res.Warnings, "\n"), "codex does not support SSE") {
		t.Errorf("SSE server: %v", res.Warnings)
	}

	// ChatGPT mode keeps the VM's own sign-in, but drops an API key login
	// left from api-key mode.
	chatgpt := Spec{Agents: template.Agents{Codex: &template.Agent{Auth: template.CodexChatGPT}}}
	if _, err := Push(context.Background(), localRemote{home}, chatgpt, sec); err != nil {
		t.Fatalf("chatgpt mode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex/auth.json")); err == nil {
		t.Error("switching to chatgpt kept the API key login")
	}
	write(t, filepath.Join(home, ".codex/auth.json"), `{"auth_mode":"chatgpt"}`)
	if _, err := Push(context.Background(), localRemote{home}, chatgpt, sec); err != nil {
		t.Fatalf("chatgpt mode: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".codex/auth.json")); string(b) != `{"auth_mode":"chatgpt"}` {
		t.Errorf("chatgpt sign-in changed: %q", b)
	}
	// Installed only: no login, no warning, and an old API key login is dropped.
	home = t.TempDir()
	if _, err := Push(context.Background(), localRemote{home}, s, sec); err != nil {
		t.Fatal(err)
	}
	res, err = Push(context.Background(), localRemote{home}, Spec{Agents: template.Agents{Codex: &template.Agent{Auth: template.CodexNone}}}, sec)
	if _, serr := os.Stat(filepath.Join(home, ".codex/auth.json")); err != nil || serr == nil || len(res.Warnings) != 0 || slices.Contains(res.Found, "OPENAI_API_KEY") {
		t.Errorf("auth none: %v, auth.json kept %v, warnings %v, found %v", err, serr == nil, res.Warnings, res.Found)
	}
	// No key on the host: a warning, and no login.
	res, err = Push(context.Background(), localRemote{t.TempDir()}, Spec{Agents: template.Agents{Codex: &template.Agent{}}}, fakeSecrets{})
	if err != nil || !strings.Contains(strings.Join(res.Warnings, "\n"), "codex: no OPENAI_API_KEY") {
		t.Errorf("no key: %v %v", res.Warnings, err)
	}
	// A config.toml that isn't TOML, or that would take the host's parser too
	// deep or too long, is kept aside and replaced.
	for name, c := range map[string]struct{ body, why string }{
		"invalid":   {"[[not toml", "was not valid TOML"},
		"deep":      {"a = " + strings.Repeat("[", 100) + strings.Repeat("]", 100), "nests more than 64 levels"},
		"large":     {strings.Repeat("# filler\n", maxCodexConfig/8), "is over 256 KiB"},
		"notify":    {"notify = [\"my-notifier\"]\n", ""},
		"dotted":    {strings.Repeat("a.", (maxCodexConfig-8)/2) + "a = 1\n", "nests more than 64 levels"},
		"mcpscalar": {"mcp_servers = 1\nanalytics = \"x\"\n", ""},
	} {
		home = t.TempDir()
		write(t, filepath.Join(home, codexConfig), c.body)
		spec := chatgpt
		if name == "mcpscalar" {
			spec = s // with MCP servers, so mcp_servers is merged into
		}
		res, err = Push(context.Background(), localRemote{home}, spec, sec)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w := strings.Join(res.Warnings, "\n")
		_, backedUp := os.Stat(filepath.Join(home, codexConfig+".curral-bak"))
		switch name {
		case "notify":
			if !strings.Contains(w, "replaced notify") {
				t.Errorf("notify: warnings %v", res.Warnings)
			}
		case "mcpscalar": // valid TOML within the limits: merged
			if backedUp == nil {
				t.Errorf("%s: replaced", name)
			}
			cfg := map[string]any{}
			b, _ := os.ReadFile(filepath.Join(home, codexConfig))
			if err := toml.Unmarshal(b, &cfg); err != nil || get(cfg, "mcp_servers", "docs", "url") == nil {
				t.Errorf("%s: MCP servers not merged in: %v\n%s", name, err, b)
			}
		default:
			if backedUp != nil || !strings.Contains(w, "~/"+codexConfig+" "+c.why+";") {
				t.Errorf("%s: warnings %v", name, res.Warnings)
			}
			if b, _ := os.ReadFile(filepath.Join(home, codexConfig)); !strings.Contains(string(b), "sandbox_mode = 'danger-full-access'") {
				t.Errorf("%s: default sandbox_mode:\n%.200s", name, b)
			}
		}
		if b, _ := os.ReadFile(filepath.Join(home, codexConfig)); !strings.Contains(string(b), "check_for_update_on_startup = false") {
			t.Errorf("%s: config not written:\n%.200s", name, b)
		}
	}
}

// Every registered agent is configured by Push and gets curral's
// notification hook.
func TestEveryAgentPushedWithHook(t *testing.T) {
	if len(pushers) != len(template.AgentDefs) {
		t.Errorf("%d pushers for %d agents", len(pushers), len(template.AgentDefs))
	}
	for _, d := range template.AgentDefs {
		if pushers[d.Name] == nil {
			t.Errorf("no pusher for %s", d.Name)
			continue
		}
		home := t.TempDir()
		var s Spec
		*d.Get(&s.Agents) = &template.Agent{Auth: map[string]string{"codex": template.CodexNone}[d.Name]}
		if _, err := Push(context.Background(), localRemote{home}, s, fakeSecrets{}); err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		found := false
		filepath.WalkDir(home, func(path string, e fs.DirEntry, err error) error {
			if b, _ := os.ReadFile(path); err == nil && !e.IsDir() && strings.Contains(string(b), notifyURL) {
				found = true
			}
			return nil
		})
		if !found {
			t.Errorf("%s: no notification hook pushed", d.Name)
		}
	}
}
