package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/gitsrc"
	"github.com/blackshieldpt/catchpen/internal/template"
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

// staging lists what's left of catchpen's staging folders in dir.
func staging(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, ".catchpen-new*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
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
		if got := readFile(t, filepath.Join(home, dir, "b", catchpenMark)); got != "b" {
			t.Errorf("%s: b's mark = %q", agent, got)
		}
	}
	if got := readFile(t, filepath.Join(home, ".claude/skills/a", catchpenMark)); got != "a" {
		t.Errorf("a's mark = %q", got)
	}
}

func TestAgentFilesKeepsTheUsers(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude/skills")
	write(t, filepath.Join(dir, "mine/SKILL.md"), "mine")
	write(t, filepath.Join(dir, "old", catchpenMark), "old")
	write(t, filepath.Join(dir, "a/SKILL.md"), "the user's a")
	write(t, filepath.Join(dir, "copy", catchpenMark), "old") // a mark copied from elsewhere
	write(t, filepath.Join(dir, "copied", catchpenMark), "old")
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("catchpen's a")}, {Name: "copied", Files: skillFiles("catchpen's copied")}}
	res := pushFiles(t, home, s)
	if !exists(filepath.Join(dir, "mine/SKILL.md")) {
		t.Error("the user's mine was removed")
	}
	if exists(filepath.Join(dir, "old")) {
		t.Error("catchpen's old was kept")
	}
	if !exists(filepath.Join(dir, "copy", catchpenMark)) {
		t.Error("copy, whose mark isn't its own name, was removed")
	}
	if got := readFile(t, filepath.Join(dir, "a/SKILL.md")); got != "the user's a" {
		t.Errorf("the user's a was replaced: %q", got)
	}
	if exists(filepath.Join(dir, "a", catchpenMark)) {
		t.Error("the user's a was marked")
	}
	if !warned(res, "~/.claude/skills/a is yours") {
		t.Errorf("warnings %v", res.Warnings)
	}
	if got := readFile(t, filepath.Join(dir, "copied", catchpenMark)); got != "old" || exists(filepath.Join(dir, "copied/SKILL.md")) {
		t.Errorf("copied, whose mark isn't its own name, was replaced: mark %q", got)
	}
	if !warned(res, "~/.claude/skills/copied is yours") {
		t.Errorf("warnings %v", res.Warnings)
	}
}

// An agent can make part of a folder read-only; catchpen still replaces and
// removes its own folder whole.
func TestAgentFilesReadOnlyFolders(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores folder permissions")
	}
	home := t.TempDir()
	t.Cleanup(func() { exec.Command("chmod", "-R", "u+w", home).Run() })
	dir := filepath.Join(home, ".claude/skills")
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("A")}}
	pushFiles(t, home, s)
	readOnly := func() {
		write(t, filepath.Join(dir, "a/cache/f"), "f")
		for _, d := range []string{"a/cache", "a"} {
			if err := os.Chmod(filepath.Join(dir, d), 0o555); err != nil {
				t.Fatal(err)
			}
		}
	}
	readOnly()
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("A2")}}
	res := pushFiles(t, home, s)
	if got := readFile(t, filepath.Join(dir, "a/SKILL.md")); got != "A2" || exists(filepath.Join(dir, "a/cache")) {
		t.Errorf("a not replaced whole: SKILL.md %q, warnings %v", got, res.Warnings)
	}
	if left := staging(t, dir); len(left) != 0 || warned(res, "can't remove") {
		t.Errorf("the old copy wasn't removed: %v, warnings %v", left, res.Warnings)
	}
	if got := readFile(t, filepath.Join(dir, "a", catchpenMark)); got != "a" {
		t.Errorf("a's mark = %q", got)
	}
	readOnly()
	s.Skills = nil
	res = pushFiles(t, home, s)
	if left := staging(t, dir); exists(filepath.Join(dir, "a")) || len(left) != 0 || warned(res, "failed") || warned(res, "can't remove") {
		t.Errorf("a %v, staging %v, warnings %v", exists(filepath.Join(dir, "a")), left, res.Warnings)
	}
}

// Files the user can't delete (an agent may make them with sudo) are parked
// with a warning and never block a later copy.
func TestAgentFilesLeftoversDontBlock(t *testing.T) {
	rm, err := exec.LookPath("rm")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir() // an rm that can't remove a tree holding a file named stuck
	write(t, filepath.Join(bin, "rm"), `#!/bin/sh
for a; do [ -e "$a" ] && find "$a" -name stuck 2>/dev/null | grep -q . && exit 1; done
exec `+rm+` "$@"
`)
	if err := os.Chmod(filepath.Join(bin, "rm"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", bin+":"+path)
	home := t.TempDir()
	dir := filepath.Join(home, ".claude/skills")
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	for i, want := range []string{"A", "A2", "A3"} {
		s.Skills = []Bundle{{Name: "a", Files: skillFiles(want)}}
		res := pushFiles(t, home, s)
		if got := readFile(t, filepath.Join(dir, "a/SKILL.md")); got != want {
			t.Errorf("push %d: a/SKILL.md = %q; warnings %v", i, got, res.Warnings)
		}
		if i > 0 && !warned(res, "can't remove its old copies") {
			t.Errorf("push %d: no leftover warning: %v", i, res.Warnings)
		}
		write(t, filepath.Join(dir, "a/stuck"), "")
	}
	if exists(filepath.Join(dir, ".catchpen-new")) {
		t.Error(".catchpen-new is left in place, where the next copy needs it")
	}
	t.Setenv("PATH", path) // the files can be removed again: the next push does
	if res := pushFiles(t, home, s); len(staging(t, dir)) != 0 || warned(res, "can't remove") {
		t.Errorf("leftovers kept: %v, warnings %v", staging(t, dir), res.Warnings)
	}
}

// A copy interrupted between its two moves leaves the old folder aside; the
// next push puts it back.
func TestAgentFilesRestoresAnInterruptedCopy(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude/skills")
	write(t, filepath.Join(dir, ".catchpen-new/.old/a", catchpenMark), "a")
	write(t, filepath.Join(dir, ".catchpen-new/.old/a/SKILL.md"), "A")
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{{Name: "a", Failed: true}}
	pushFiles(t, home, s)
	if !exists(filepath.Join(dir, "a/SKILL.md")) || len(staging(t, dir)) != 0 {
		t.Errorf("a restored %v, staging left %v", exists(filepath.Join(dir, "a/SKILL.md")), staging(t, dir))
	}
	// And when the next push copies.
	write(t, filepath.Join(dir, ".catchpen-new/.old/b", catchpenMark), "b")
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("A2")}, {Name: "b", Files: skillFiles("B2")}}
	res := pushFiles(t, home, s)
	for n, want := range map[string]string{"a": "A2", "b": "B2"} {
		if got := readFile(t, filepath.Join(dir, n, "SKILL.md")); got != want {
			t.Errorf("%s/SKILL.md = %q; warnings %v", n, got, res.Warnings)
		}
	}
	if left := staging(t, dir); len(left) != 0 {
		t.Errorf("staging left: %v", left)
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
	write(t, filepath.Join(home, ".claude/skills/a", catchpenMark), "a")
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
	for _, want := range []string{"CLAUDE_CODE_PLUGIN_DIRS=/home/agent/.local/share/catchpen/plugins/claude/p\n", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1\n"} {
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
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("catchpen's a")}}
	res := pushFiles(t, home, s)
	if got := readFile(t, filepath.Join(home, ".claude/skills/a/SKILL.md")); got != "host's a" {
		t.Errorf("a/SKILL.md = %q", got)
	}
	if exists(filepath.Join(home, ".claude/skills/a", catchpenMark)) {
		t.Error("catchpen copied a")
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
	write(t, filepath.Join(home, codexConfig), "[plugins.\"stale@catchpen\"]\nenabled = true\n[plugins.\"mine@other\"]\nenabled = true\n")
	s := filesSpec(template.Agents{Codex: &template.Agent{Auth: template.CodexNone}})
	s.Plugins = []Bundle{{Agent: "codex", Name: "p", Files: []gitsrc.File{{Name: ".codex-plugin/plugin.json", Data: []byte("{}")}}}}
	pushFiles(t, home, s)
	root := filepath.Join(home, pluginDirs["codex"])
	if got := readFile(t, filepath.Join(root, "p/local/.codex-plugin/plugin.json")); got != "{}" {
		t.Errorf("plugin.json = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "p", catchpenMark)); got != "p" {
		t.Errorf("mark = %q", got)
	}
	cfg := readTOML(t, filepath.Join(home, codexConfig))
	if get(cfg, "plugins", "p@catchpen", "enabled") != true || get(cfg, "features", "remote_plugin") != false {
		t.Errorf("config.toml: %v", cfg)
	}
	if get(cfg, "plugins", "stale@catchpen") != nil || get(cfg, "plugins", "mine@other", "enabled") != true {
		t.Errorf("config.toml plugins: %v", cfg["plugins"])
	}

	s.Plugins = nil
	pushFiles(t, home, s)
	cfg = readTOML(t, filepath.Join(home, codexConfig))
	if get(cfg, "plugins", "p@catchpen") != nil || get(cfg, "plugins", "mine@other", "enabled") != true {
		t.Errorf("second push: plugins %v", cfg["plugins"])
	}
	if exists(filepath.Join(root, "p")) {
		t.Error("second push: p kept")
	}
}

func TestAgentFilesOpencodePlugin(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, opencodeConfig), `{"plugin": ["npm-thing", ["with-opts", {"x": 1}], "/home/agent/.local/share/catchpen/plugins/opencode/stale"]}`)
	s := filesSpec(template.Agents{OpenCode: &template.Agent{}})
	s.Plugins = []Bundle{
		{Agent: "opencode", Name: "p", Files: []gitsrc.File{{Name: "package.json", Data: []byte("{}")}}},
		{Agent: "opencode", Name: "nopkg", Files: []gitsrc.File{{Name: "index.ts", Data: []byte("x")}}},
	}
	res := pushFiles(t, home, s)
	want := []any{"npm-thing", []any{"with-opts", map[string]any{"x": float64(1)}}, "/home/agent/.local/share/catchpen/plugins/opencode/p"}
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
	if want := []any{"/home/agent/.local/share/catchpen/plugins/pi/q"}; !reflect.DeepEqual(doc["packages"], want) {
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
		t.Errorf("second push: p %v, catchpen-notify.ts %v", exists(filepath.Join(dir, "p")), exists(filepath.Join(home, ompExtension)))
	}
}

func TestAgentFilesChecks(t *testing.T) {
	home := t.TempDir()
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{{Name: "a b", Files: skillFiles("x")}, {Name: "catchpen-x", Files: skillFiles("x")}, {Agent: "nope", Name: "z", Files: skillFiles("x")}}
	s.Plugins = []Bundle{{Agent: "codex", Name: "p", Files: skillFiles("x")}, {Name: "y", Files: skillFiles("x")},
		{Agent: "pi", Name: "b1", Bundled: "../x"}, {Agent: "pi", Name: "b2", Bundled: "a//b"}}
	res := pushFiles(t, home, s)
	for _, w := range []string{`skill "a b": invalid name; skipped`, `skill "catchpen-x": invalid name; skipped`, "plugin p: codex isn't installed in this VM; skipped",
		`skill z: unknown agent "nope"; skipped`, `plugin y: unknown agent ""; skipped`,
		`plugin b1: invalid bundled path "../x"; skipped`, `plugin b2: invalid bundled path "a//b"; skipped`} {
		if !warned(res, w) {
			t.Errorf("no warning %q in %v", w, res.Warnings)
		}
	}
	for _, p := range []string{".claude/skills/a b", ".claude/skills/catchpen-x", ".claude/skills/z", pluginDirs["codex"]} {
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

// A fetched folder holding catchpen's mark is refused, and the others still
// get theirs, so none is mistaken for the user's later.
func TestAgentFilesMarksEveryCopy(t *testing.T) {
	home := t.TempDir()
	s := filesSpec(template.Agents{Claude: &template.Agent{}})
	s.Skills = []Bundle{
		{Name: "a", Files: append(skillFiles("A"), gitsrc.File{Name: ".catchpen/x", Data: []byte("x")})},
		{Name: "b", Files: skillFiles("B")},
	}
	res := pushFiles(t, home, s)
	if !warned(res, "skill a: it holds a .catchpen, which is catchpen's; skipped") || exists(filepath.Join(home, ".claude/skills/a")) {
		t.Errorf("a: %v", res.Warnings)
	}
	if got := readFile(t, filepath.Join(home, ".claude/skills/b", catchpenMark)); got != "b" {
		t.Errorf("b's mark = %q", got)
	}
	if exists(filepath.Join(home, ".claude/skills/.catchpen-new")) {
		t.Error("the unpacking folder was left behind")
	}
}

// Plugins whose fetch failed keep their folder and config entry.
func TestAgentFilesFailedPluginsKeep(t *testing.T) {
	home := t.TempDir()
	s := filesSpec(template.Agents{Codex: &template.Agent{Auth: template.CodexNone}, OpenCode: &template.Agent{}, Pi: &template.Agent{}})
	pkg := []gitsrc.File{{Name: "package.json", Data: []byte("{}")}}
	s.Plugins = []Bundle{{Agent: "codex", Name: "p", Files: pkg}, {Agent: "opencode", Name: "q", Files: pkg}, {Agent: "pi", Name: "r", Files: pkg}}
	pushFiles(t, home, s)
	for i := range s.Plugins {
		s.Plugins[i].Files, s.Plugins[i].Failed = nil, true
	}
	pushFiles(t, home, s)
	for _, p := range []string{pluginDirs["codex"] + "/p/local/package.json", pluginDirs["opencode"] + "/q/package.json", pluginDirs["pi"] + "/r/package.json"} {
		if !exists(filepath.Join(home, p)) {
			t.Errorf("%s was removed", p)
		}
	}
	var cfg map[string]any
	if err := toml.Unmarshal([]byte(readFile(t, filepath.Join(home, codexConfig))), &cfg); err != nil {
		t.Fatal(err)
	}
	if get(cfg, "plugins", "p@catchpen", "enabled") != true {
		t.Errorf("codex config: %v", cfg)
	}
	if oc := readJSON(t, filepath.Join(home, opencodeConfig)); !reflect.DeepEqual(oc["plugin"], []any{"/home/agent/" + pluginDirs["opencode"] + "/q"}) {
		t.Errorf("opencode plugin = %v", oc["plugin"])
	}
	if pi := readJSON(t, filepath.Join(home, piSettings)); !reflect.DeepEqual(pi["packages"], []any{"/home/agent/" + pluginDirs["pi"] + "/r"}) {
		t.Errorf("pi packages = %v", pi["packages"])
	}
}

// catchpen removes only the pi extensions it added; the user's stay.
func TestAgentFilesPiKeepsTheUsersExtensions(t *testing.T) {
	old := piPackage
	t.Cleanup(func() { piPackage = old })
	piPackage = t.TempDir()
	write(t, filepath.Join(piPackage, "examples/extensions/plan-mode.ts"), "x")
	write(t, filepath.Join(piPackage, "examples/extensions/other.ts"), "x")
	home := t.TempDir()
	mine := piPackage + "/examples/extensions/mine.ts"
	planMode := piPackage + "/examples/extensions/plan-mode.ts" // listed by the user too
	write(t, filepath.Join(home, piSettings), `{"extensions": ["`+mine+`", "`+planMode+`"]}`)
	s := filesSpec(template.Agents{Pi: &template.Agent{}})
	s.Plugins = []Bundle{
		{Agent: "pi", Name: "plan-mode", Bundled: "examples/extensions/plan-mode.ts"},
		{Agent: "pi", Name: "other", Bundled: "examples/extensions/other.ts"},
	}
	pushFiles(t, home, s)
	if doc := readJSON(t, filepath.Join(home, piSettings)); len(stringList(doc["extensions"])) != 3 {
		t.Errorf("extensions after adding = %v", doc["extensions"])
	}
	s.Plugins = nil
	pushFiles(t, home, s)
	if doc := readJSON(t, filepath.Join(home, piSettings)); !reflect.DeepEqual(doc["extensions"], []any{mine, planMode}) {
		t.Errorf("extensions = %v", doc["extensions"])
	}
}

// What the guest put there is the user's: a dangling symlink, a FIFO for a
// mark (which must not block the push), a folder catchpen's plugin would use.
func TestAgentFilesLeavesOddFoldersAlone(t *testing.T) {
	real := claudeReal
	t.Cleanup(func() { claudeReal = real })
	claudeReal = filepath.Join(t.TempDir(), "claude")
	write(t, claudeReal, "")

	home := t.TempDir()
	skills := filepath.Join(home, ".claude/skills")
	if err := os.MkdirAll(filepath.Join(skills, "f"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent", filepath.Join(skills, "a")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(skills, "f", catchpenMark), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, pluginDirs["codex"], "p/local/x"), "mine")
	write(t, filepath.Join(home, pluginDirs["claude"], "q/x"), "mine")
	write(t, filepath.Join(home, pluginDirs["claude"], "v/x"), "mine")
	write(t, filepath.Join(home, pluginDirs["claude"], "k", catchpenMark), "k")
	s := filesSpec(template.Agents{Claude: &template.Agent{}, Codex: &template.Agent{Auth: template.CodexNone}})
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("A")}, {Name: "f", Files: skillFiles("F")}}
	x := []gitsrc.File{{Name: "x", Data: []byte("catchpen")}}
	s.Plugins = []Bundle{
		{Agent: "codex", Name: "p", Files: x}, {Agent: "claude", Name: "q", Files: x}, {Agent: "claude", Name: "r", Files: x},
		// Failed fetches: catchpen's old copy k stays loaded; the user's v and a missing w aren't.
		{Agent: "claude", Name: "k", Failed: true}, {Agent: "claude", Name: "v", Failed: true}, {Agent: "claude", Name: "w", Failed: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Push(ctx, localRemote{home}, s, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(filepath.Join(skills, "a")); err != nil || l != "/nonexistent" {
		t.Errorf("the user's symlink: %q %v", l, err)
	}
	if !warned(res, "f: ~/.claude/skills/f is yours") {
		t.Errorf("warnings %v", res.Warnings)
	}
	if readFile(t, filepath.Join(home, pluginDirs["codex"], "p/local/x")) != "mine" {
		t.Error("the user's codex folder was replaced")
	}
	if cfg := readFile(t, filepath.Join(home, codexConfig)); strings.Contains(cfg, "p@catchpen") {
		t.Errorf("the user's folder was turned on as catchpen's plugin:\n%s", cfg)
	}
	if ce := readFile(t, filepath.Join(home, claudeEnvFile)); !strings.Contains(ce, "CLAUDE_CODE_PLUGIN_DIRS=/home/agent/.local/share/catchpen/plugins/claude/r:/home/agent/.local/share/catchpen/plugins/claude/k\n") {
		t.Errorf("claude should load catchpen's r and k only:\n%s", ce)
	}
}

// A setting that isn't a list is the user's: left alone, with a warning.
func TestAgentFilesLeavesNonListsAlone(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, opencodeConfig), `{"plugin": "x"}`)
	s := filesSpec(template.Agents{OpenCode: &template.Agent{}})
	s.Plugins = []Bundle{{Agent: "opencode", Name: "q", Files: []gitsrc.File{{Name: "package.json", Data: []byte("{}")}}}}
	res := pushFiles(t, home, s)
	if oc := readJSON(t, filepath.Join(home, opencodeConfig)); oc["plugin"] != "x" || !warned(res, "plugin isn't a list") {
		t.Errorf("plugin = %v, warnings %v", oc["plugin"], res.Warnings)
	}
}

// With claude.ai sync on and host settings synced, the host's settings.json
// decides the claude.ai sync keys.
func TestClaudeAISyncKeepsSyncedSettings(t *testing.T) {
	home, host := t.TempDir(), t.TempDir()
	write(t, filepath.Join(host, "settings.json"), `{"syncClaudeAiSkills": true, "syncClaudeAiPlugins": true}`)
	s := fullSpec()
	tru := true
	s.Sync = template.Sync{ClaudeAI: &tru, ClaudeConfig: &tru}
	s.ClaudeDir = host
	if _, err := Push(context.Background(), localRemote{home}, s, fakeSecrets{}); err != nil {
		t.Fatal(err)
	}
	if st := readJSON(t, filepath.Join(home, claudeSettings)); st["syncClaudeAiSkills"] != true || st["syncClaudeAiPlugins"] != true {
		t.Errorf("settings.json: %v", st)
	}
}

func TestSetListSeesEveryChange(t *testing.T) {
	doc := map[string]any{"k": []any{"a", "b"}}
	changed, err := setList(doc, "k", func(string) bool { return true }, []any{"a b"})
	if err != nil || !changed {
		t.Errorf("setList = %v, %v; doc %v", changed, err, doc)
	}
}

// Two pushes at once take turns at a root: they share its staging folder.
func TestAgentFilesPushesTakeTurns(t *testing.T) {
	home := t.TempDir()
	lock := filepath.Join(home, ".local/share/catchpen/files.lock")
	write(t, lock, "")
	unlock, err := fsutil.Lock(lock)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(time.Second, unlock)
	time.AfterFunc(500*time.Millisecond, func() {
		if exists(filepath.Join(home, skillDirs["codex"], "a")) {
			t.Error("copied while the other push held the lock")
		}
	})
	// codex: the skill copy is the push's first guest script on files.
	s := filesSpec(template.Agents{Codex: &template.Agent{Auth: template.CodexNone}})
	s.Skills = []Bundle{{Name: "a", Files: skillFiles("A")}}
	start := time.Now()
	res := pushFiles(t, home, s)
	if time.Since(start) < time.Second || readFile(t, filepath.Join(home, skillDirs["codex"], "a/SKILL.md")) != "A" {
		t.Errorf("the push didn't wait for the other one: %v, warnings %v", time.Since(start), res.Warnings)
	}
	// A push with nothing to copy only cleans up, and waits too.
	if unlock, err = fsutil.Lock(lock); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(time.Second, unlock)
	s.Skills = nil
	start = time.Now()
	res = pushFiles(t, home, s)
	if time.Since(start) < time.Second || exists(filepath.Join(home, skillDirs["codex"], "a")) {
		t.Errorf("the cleanup didn't wait for the other push: %v, warnings %v", time.Since(start), res.Warnings)
	}
}
