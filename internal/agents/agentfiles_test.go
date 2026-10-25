package agents

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/blackshieldpt/curral/internal/gitsrc"
	"github.com/blackshieldpt/curral/internal/template"
)

func skillFiles(content string) []gitsrc.File {
	return []gitsrc.File{{Name: "SKILL.md", Data: []byte(content)}}
}

func allAgents() template.Agents {
	return template.Agents{
		Claude: &template.Agent{}, OpenCode: &template.Agent{}, Pi: &template.Agent{}, OMP: &template.Agent{},
		Codex: &template.Agent{Auth: template.CodexNone},
	}
}

func filesSpec(a template.Agents) Spec {
	return Spec{Agents: a, Home: "/home/agent", AgentFiles: true}
}

func pushFiles(t *testing.T, home string, s Spec) Result {
	t.Helper()
	res, err := Push(context.Background(), localRemote{home}, s, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func warned(res Result, sub string) bool {
	return strings.Contains(strings.Join(res.Warnings, "\n"), sub)
}

func TestAgentFilesSkillsPerAgent(t *testing.T) {
	home := t.TempDir()
	s := filesSpec(allAgents())
	s.Skills = []Bundle{{Agent: "claude", Name: "a", Files: skillFiles("A")}, {Name: "b", Files: skillFiles("B")}}
	pushFiles(t, home, s)
	for agent, dir := range skillDirs {
		if got := exists(filepath.Join(home, dir, "a")); got != (agent == "claude") {
			t.Errorf("%s: skill a present = %v", agent, got)
		}
		if got := readFile(t, filepath.Join(home, dir, "b", "SKILL.md")); got != "B" {
			t.Errorf("%s: b/SKILL.md = %q", agent, got)
		}
		if got := readFile(t, filepath.Join(home, dir, "b", curralMark)); got != "b" {
			t.Errorf("%s: b's mark = %q", agent, got)
		}
	}
	if got := readFile(t, filepath.Join(home, ".claude/skills/a", curralMark)); got != "a" {
		t.Errorf("a's mark = %q", got)
	}
}

func TestAgentFilesKeepsTheUsers(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude/skills")
	write(t, filepath.Join(dir, "mine/SKILL.md"), "mine")
	write(t, filepath.Join(dir, "old", curralMark), "old")
	write(t, filepath.Join(dir, "a/SKILL.md"), "the user's a")
	write(t, filepath.Join(dir, "copy", curralMark), "old") // a mark copied from elsewhere
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("curral's a")}}
	res := pushFiles(t, home, s)
	if !exists(filepath.Join(dir, "mine/SKILL.md")) {
		t.Error("the user's mine was removed")
	}
	if exists(filepath.Join(dir, "old")) {
		t.Error("curral's old was kept")
	}
	if !exists(filepath.Join(dir, "copy", curralMark)) {
		t.Error("copy, whose mark isn't its own name, was removed")
	}
	if got := readFile(t, filepath.Join(dir, "a/SKILL.md")); got != "the user's a" {
		t.Errorf("the user's a was replaced: %q", got)
	}
	if exists(filepath.Join(dir, "a", curralMark)) {
		t.Error("the user's a was marked")
	}
	if !warned(res, "~/.claude/skills/a is yours") {
		t.Errorf("warnings %v", res.Warnings)
	}
}

func TestAgentFilesSecondPushRemoves(t *testing.T) {
	home := t.TempDir()
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("A")}}
	s.Plugins = []Bundle{{Agent: "claude", Name: "p", Files: skillFiles("P")}}
	pushFiles(t, home, s)
	skill, plugin := filepath.Join(home, ".claude/skills/a"), filepath.Join(home, pluginDirs["claude"], "p")
	if !exists(skill) || !exists(plugin) {
		t.Fatal("first push copied nothing")
	}
	s.Skills, s.Plugins = nil, nil
	pushFiles(t, home, s)
	if exists(skill) || exists(plugin) {
		t.Errorf("kept after the second push: skill %v, plugin %v", exists(skill), exists(plugin))
	}
}

func TestAgentFilesFailedFetchKeeps(t *testing.T) {
	home := t.TempDir()
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("A")}}
	pushFiles(t, home, s)
	s.Skills = []Bundle{{Name: "a", Failed: true}}
	pushFiles(t, home, s)
	if got := readFile(t, filepath.Join(home, ".claude/skills/a/SKILL.md")); got != "A" {
		t.Errorf("a/SKILL.md = %q", got)
	}
}

func TestAgentFilesNoManagementWithoutTheFlag(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/skills/a", curralMark), "a")
	pushFiles(t, home, Spec{Agents: template.Agents{Claude: &template.Agent{}}})
	if !exists(filepath.Join(home, ".claude/skills/a")) {
		t.Error("a marked folder was removed without AgentFiles")
	}
}

func TestAgentFilesClaudePlugin(t *testing.T) {
	real := claudeReal
	t.Cleanup(func() { claudeReal = real })
	claudeReal = filepath.Join(t.TempDir(), "claude")
	write(t, claudeReal, "")

	home := t.TempDir()
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Plugins = []Bundle{{Agent: "claude", Name: "p", Files: []gitsrc.File{{Name: ".claude-plugin/plugin.json", Data: []byte("{}")}}}}
	pushFiles(t, home, s)
	ce := readFile(t, filepath.Join(home, claudeEnvFile))
	for _, want := range []string{"CLAUDE_CODE_PLUGIN_DIRS=/home/agent/.local/share/curral/plugins/claude/p\n", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1\n"} {
		if !strings.Contains(ce, want) {
			t.Errorf("claude env lacks %q:\n%s", want, ce)
		}
	}
	if got := readFile(t, filepath.Join(home, pluginDirs["claude"], "p/.claude-plugin/plugin.json")); got != "{}" {
		t.Errorf("plugin.json = %q", got)
	}

	// With the config sync on, the host's own skill wins.
	host := t.TempDir()
	write(t, filepath.Join(host, "skills/a/SKILL.md"), "host's a")
	tru := true
	home = t.TempDir()
	s = filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Sync, s.ClaudeDir = template.Sync{ClaudeConfig: &tru}, host
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("curral's a")}}
	res := pushFiles(t, home, s)
	if got := readFile(t, filepath.Join(home, ".claude/skills/a/SKILL.md")); got != "host's a" {
		t.Errorf("a/SKILL.md = %q", got)
	}
	if exists(filepath.Join(home, ".claude/skills/a", curralMark)) {
		t.Error("curral copied a")
	}
	if !warned(res, "your synced ~/.claude/skills/a is used instead") {
		t.Errorf("warnings %v", res.Warnings)
	}
}

func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	cfg := map[string]any{}
	if err := toml.Unmarshal([]byte(readFile(t, path)), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAgentFilesCodexPlugin(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, codexConfig), "[plugins.\"stale@curral\"]\nenabled = true\n[plugins.\"mine@other\"]\nenabled = true\n")
	s := filesSpec(template.Agents{Codex: &template.Agent{Auth: template.CodexNone}})
	s.Plugins = []Bundle{{Agent: "codex", Name: "p", Files: []gitsrc.File{{Name: ".codex-plugin/plugin.json", Data: []byte("{}")}}}}
	pushFiles(t, home, s)
	root := filepath.Join(home, pluginDirs["codex"])
	if got := readFile(t, filepath.Join(root, "p/local/.codex-plugin/plugin.json")); got != "{}" {
		t.Errorf("plugin.json = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "p", curralMark)); got != "p" {
		t.Errorf("mark = %q", got)
	}
	cfg := readTOML(t, filepath.Join(home, codexConfig))
	if get(cfg, "plugins", "p@curral", "enabled") != true || get(cfg, "features", "remote_plugin") != false {
		t.Errorf("config.toml: %v", cfg)
	}
	if get(cfg, "plugins", "stale@curral") != nil || get(cfg, "plugins", "mine@other", "enabled") != true {
		t.Errorf("config.toml plugins: %v", cfg["plugins"])
	}

	s.Plugins = nil
	pushFiles(t, home, s)
	cfg = readTOML(t, filepath.Join(home, codexConfig))
	if get(cfg, "plugins", "p@curral") != nil || get(cfg, "plugins", "mine@other", "enabled") != true {
		t.Errorf("second push: plugins %v", cfg["plugins"])
	}
	if exists(filepath.Join(root, "p")) {
		t.Error("second push: p kept")
	}
}

func TestAgentFilesOpencodePlugin(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, opencodeConfig), `{"plugin": ["npm-thing", ["with-opts", {"x": 1}], "/home/agent/.local/share/curral/plugins/opencode/stale"]}`)
	s := filesSpec(template.Agents{OpenCode: &template.Agent{}})
	s.Plugins = []Bundle{
		{Agent: "opencode", Name: "p", Files: []gitsrc.File{{Name: "package.json", Data: []byte("{}")}}},
		{Agent: "opencode", Name: "nopkg", Files: []gitsrc.File{{Name: "index.ts", Data: []byte("x")}}},
	}
	res := pushFiles(t, home, s)
	want := []any{"npm-thing", []any{"with-opts", map[string]any{"x": float64(1)}}, "/home/agent/.local/share/curral/plugins/opencode/p"}
	if got := readJSON(t, filepath.Join(home, opencodeConfig))["plugin"]; !reflect.DeepEqual(got, want) {
		t.Errorf("plugin = %#v", got)
	}
	if !exists(filepath.Join(home, pluginDirs["opencode"], "p/package.json")) || exists(filepath.Join(home, pluginDirs["opencode"], "nopkg")) {
		t.Error("wrong plugin folders")
	}
	if !warned(res, "plugin nopkg: opencode plugins need a package.json; skipped") {
		t.Errorf("warnings %v", res.Warnings)
	}
	if env := readFile(t, filepath.Join(home, envFile)); !strings.Contains(env, "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1\n") {
		t.Errorf("env:\n%s", env)
	}
}

func TestAgentFilesPiPlugins(t *testing.T) {
	old := piPackage
	t.Cleanup(func() { piPackage = old })
	piPackage = t.TempDir()
	write(t, filepath.Join(piPackage, "examples/extensions/plan-mode.ts"), "x")
	home := t.TempDir()
	s := filesSpec(template.Agents{Pi: &template.Agent{}})
	s.Plugins = []Bundle{
		{Agent: "pi", Name: "q", Files: []gitsrc.File{{Name: "package.json", Data: []byte("{}")}}},
		{Agent: "pi", Name: "plan-mode", Bundled: "examples/extensions/plan-mode.ts"},
		{Agent: "pi", Name: "gone", Bundled: "examples/extensions/gone.ts"},
	}
	res := pushFiles(t, home, s)
	doc := readJSON(t, filepath.Join(home, piSettings))
	if want := []any{"/home/agent/.local/share/curral/plugins/pi/q"}; !reflect.DeepEqual(doc["packages"], want) {
		t.Errorf("packages = %v", doc["packages"])
	}
	if want := []any{piPackage + "/examples/extensions/plan-mode.ts"}; !reflect.DeepEqual(doc["extensions"], want) {
		t.Errorf("extensions = %v", doc["extensions"])
	}
	if !exists(filepath.Join(home, pluginDirs["pi"], "q/package.json")) {
		t.Error("q not copied")
	}
	if !warned(res, "plugin gone: examples/extensions/gone.ts isn't in this VM's pi package; skipped") {
		t.Errorf("warnings %v", res.Warnings)
	}
}

func TestAgentFilesOmpPlugin(t *testing.T) {
	home := t.TempDir()
	s := filesSpec(template.Agents{OMP: &template.Agent{}})
	s.Plugins = []Bundle{
		{Agent: "omp", Name: "p", Files: []gitsrc.File{{Name: "index.ts", Data: []byte("x")}}},
		{Agent: "omp", Name: "noindex", Files: []gitsrc.File{{Name: "README.md", Data: []byte("x")}}},
	}
	res := pushFiles(t, home, s)
	dir := filepath.Join(home, pluginDirs["omp"])
	if !exists(filepath.Join(dir, "p/index.ts")) || exists(filepath.Join(dir, "noindex")) {
		t.Error("wrong plugin folders")
	}
	if !warned(res, "plugin noindex: omp plugins need") {
		t.Errorf("warnings %v", res.Warnings)
	}
	s.Plugins = nil
	pushFiles(t, home, s)
	if exists(filepath.Join(dir, "p")) || !exists(filepath.Join(home, ompExtension)) {
		t.Errorf("second push: p %v, curral-notify.ts %v", exists(filepath.Join(dir, "p")), exists(filepath.Join(home, ompExtension)))
	}
}

func TestAgentFilesChecks(t *testing.T) {
	home := t.TempDir()
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{{Name: "a b", Files: skillFiles("x")}, {Name: "curral-x", Files: skillFiles("x")}, {Agent: "nope", Name: "z", Files: skillFiles("x")}}
	s.Plugins = []Bundle{{Agent: "codex", Name: "p", Files: skillFiles("x")}, {Name: "y", Files: skillFiles("x")}}
	res := pushFiles(t, home, s)
	for _, w := range []string{`skill "a b": invalid name; skipped`, `skill "curral-x": invalid name; skipped`, "plugin p: codex isn't installed in this VM; skipped",
		`skill z: unknown agent "nope"; skipped`, `plugin y: unknown agent ""; skipped`} {
		if !warned(res, w) {
			t.Errorf("no warning %q in %v", w, res.Warnings)
		}
	}
	for _, p := range []string{".claude/skills/a b", ".claude/skills/curral-x", ".claude/skills/z", pluginDirs["codex"]} {
		if exists(filepath.Join(home, p)) {
			t.Errorf("%s exists", p)
		}
	}

	s = filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Home = "/tmp/x"
	if _, err := Push(context.Background(), localRemote{t.TempDir()}, s, fakeSecrets{}); err == nil {
		t.Error("Push took the home /tmp/x")
	}
}

func TestAgentFilesEveryAgentHasFolders(t *testing.T) {
	for _, d := range template.AgentDefs {
		if skillDirs[d.Name] == "" || pluginDirs[d.Name] == "" {
			t.Errorf("%s: skills %q, plugins %q", d.Name, skillDirs[d.Name], pluginDirs[d.Name])
		}
	}
}

func TestPushTurnsOffBackgroundFetches(t *testing.T) {
	home := t.TempDir()
	if _, err := Push(context.Background(), localRemote{home}, fullSpec(), fakeSecrets{}); err != nil {
		t.Fatal(err)
	}
	st := readJSON(t, filepath.Join(home, claudeSettings))
	if st["syncClaudeAiSkills"] != false || st["syncClaudeAiPlugins"] != false {
		t.Errorf("settings.json: %v", st)
	}
	if env := readFile(t, filepath.Join(home, envFile)); !strings.Contains(env, "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1\n") {
		t.Errorf("env:\n%s", env)
	}

	s := fullSpec()
	tru := true
	s.Sync = template.Sync{ClaudeAI: &tru, OpenCodeClaudeSkills: &tru}
	if _, err := Push(context.Background(), localRemote{home}, s, fakeSecrets{}); err != nil {
		t.Fatal(err)
	}
	st = readJSON(t, filepath.Join(home, claudeSettings))
	if _, ok := st["syncClaudeAiSkills"]; ok {
		t.Errorf("settings.json: %v", st)
	}
	if _, ok := st["syncClaudeAiPlugins"]; ok {
		t.Errorf("settings.json: %v", st)
	}
	if env := readFile(t, filepath.Join(home, envFile)); strings.Contains(env, "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS") {
		t.Errorf("env:\n%s", env)
	}
}
