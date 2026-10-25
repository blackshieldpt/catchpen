//go:build integration

package vm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/egress"
)

// TestSkillsAndPlugins creates a VM with built-in skills and plugins for
// claude, codex and pi, and checks each agent gets its own, that Claude loads
// them, that Claude's marketplace isn't cloned although GitHub is reachable,
// and that a restart replaces curral's folders and keeps the user's.
func TestSkillsAndPlugins(t *testing.T) {
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-curral-integration-fake")
	const tpl = "minimal,claude,codex,pi,network:github,commit-commands,codex-superpowers,pi-permission-gate"
	if _, err := m.Create(ctx, "sk", CreateOpts{Template: tpl}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("sk") })
	rt, err := m.Start(ctx, "sk")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.keys("sk").Dial(ctx, rt.SSHPort, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	guest := guestRunner(t, ctx, c)

	const claudeSkill = "test -f ~/.claude/skills/skill-creator/SKILL.md && cat ~/.claude/skills/skill-creator/.curral"
	for _, a := range []struct{ cmd, want string }{
		{claudeSkill + " || true", "skill-creator"},
		{"test -e ~/.pi/agent/skills/skill-creator && echo y || true", ""},
		{"test -e ~/.codex/skills/skill-creator && echo y || true", ""},
		{"grep -c CLAUDE_CODE_PLUGIN_DIRS ~/.config/curral/claude.env || true", "1"},
		{"test -f ~/.codex/plugins/cache/curral/superpowers/local/.codex-plugin/plugin.json && echo y || true", "y"},
		{"grep -c permission-gate.ts ~/.pi/agent/settings.json || true", "1"},
	} {
		if got := guest(a.cmd); got != a.want {
			t.Errorf("%s: %q, want %q", a.cmd, got, a.want)
		}
	}
	if got := guest("grep -c 'superpowers@curral' ~/.codex/config.toml || true"); got == "" || got == "0" {
		t.Errorf("codex config.toml doesn't enable superpowers@curral: %q", got)
	}

	// Claude sees the skill and the plugin. GitHub is allowed, so the
	// marketplace clone could happen: check the egress log after.
	logFile := filepath.Join(m.Dir("sk"), egress.LogFile)
	before := len(logLines(t, logFile))
	initEvent := guest("timeout 90 claude -p hi --output-format stream-json --verbose 2>/dev/null | head -1 || true")
	var ev map[string]any
	if err := json.Unmarshal([]byte(initEvent), &ev); err != nil || ev["type"] != "system" || ev["subtype"] != "init" {
		t.Errorf("claude's first event isn't system/init (%v): %s", err, initEvent)
	}
	plugins := names(ev["plugins"])
	hasPlugin := slices.ContainsFunc(plugins, func(n string) bool { return n == "commit-commands" || strings.HasPrefix(n, "commit-commands@") })
	if _, ok := ev["plugins"]; !ok {
		out := guest("claude plugin list 2>&1 || true")
		hasPlugin = strings.Contains(out, "commit-commands")
		t.Logf("init event has no plugins field; claude plugin list:\n%s", out)
	}
	if !hasPlugin {
		t.Errorf("claude doesn't show plugin commit-commands: %s", initEvent)
	}
	inSkills, inCommands := count(names(ev["skills"]), "skill-creator"), count(names(ev["slash_commands"]), "skill-creator")
	if inSkills+inCommands == 0 || inSkills > 1 || inCommands > 1 {
		t.Errorf("claude shows skill-creator %d times in skills and %d in slash commands, want once in either: %s", inSkills, inCommands, initEvent)
	}

	// No marketplace clone, before any codex command (codex may reach GitHub
	// on its own).
	if got := guest("test -e ~/.claude/plugins/marketplaces/claude-plugins-official && echo y || true"); got != "" {
		t.Error("claude cloned its official marketplace")
	}
	after := logLines(t, logFile)
	if len(after) < before { // rotated
		before = 0
	}
	for _, l := range after[before:] {
		if strings.Contains(l, "github.com") {
			t.Errorf("egress to GitHub during claude -p: %s", l)
		}
	}

	// Codex sees its plugin, if it can list plugins without a model call.
	help := guest("codex --help 2>&1 || true")
	if strings.Contains(help, "\n  plugin ") {
		if out := guest("timeout 60 codex plugin list 2>&1 || true"); !strings.Contains(out, "superpowers") {
			t.Errorf("codex plugin list doesn't show superpowers:\n%s", out)
		}
	} else {
		t.Logf("codex has no plugin listing; codex --help:\n%s", help)
	}

	// A restart replaces curral's folder and keeps the user's.
	guest("touch ~/.claude/skills/skill-creator/stray && mkdir -p ~/.claude/skills/mine && echo mine > ~/.claude/skills/mine/SKILL.md")
	c.Close()
	if err := m.Stop("sk", false); err != nil {
		t.Fatal(err)
	}
	rt, err = m.Start(ctx, "sk")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	c2, err := m.keys("sk").Dial(ctx, rt.SSHPort, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	guest = guestRunner(t, ctx, c2)
	if got := guest("test -e ~/.claude/skills/skill-creator/stray && echo y || true"); got != "" {
		t.Error("stray file in curral's skill folder survived the restart")
	}
	if got := guest("cat ~/.claude/skills/mine/SKILL.md || true"); got != "mine" {
		t.Errorf("the user's skill folder: %q", got)
	}
	if got := guest(claudeSkill + " || true"); got != "skill-creator" {
		t.Errorf("after restart: %q", got)
	}
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) || len(b) == 0 {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// names lists the strings in v, a JSON list of strings or of objects with a
// name.
func names(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		switch x := x.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			if n, ok := x["name"].(string); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

func count(s []string, x string) int {
	n := 0
	for _, y := range s {
		if y == x {
			n++
		}
	}
	return n
}
