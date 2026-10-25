package vm

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/blackshieldpt/curral/internal/template"
	"github.com/blackshieldpt/curral/internal/vmtest"
)

// envVM saves a VM created from template gh with env, and writes gh.yaml.
func envVM(t *testing.T, env map[string]string, tpl string) *Manager {
	t.Helper()
	m := testManager(t)
	if err := os.MkdirAll(m.P.VM("dev"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &State{Name: "dev", Template: "envtpl", Picks: true, User: "agent", Env: env,
		Agents: template.Agents{Claude: &template.Agent{Auth: template.ClaudeOAuth}}}
	if err := m.save(s); err != nil {
		t.Fatal(err)
	}
	if tpl != "" {
		vmtest.Template(t, m.P.Templates(), "envtpl", "default", tpl)
	}
	return m
}

func TestRefreshEnvTakesTheTemplatesEnv(t *testing.T) {
	m := envVM(t, map[string]string{"TZ": "UTC", "OLD": "x"}, "env: { GH_TOKEN: \"secret:GH_TOKEN\", TZ: Europe/Lisbon }\n")
	var out strings.Builder
	m.Out = &out
	if err := m.refreshEnv("dev"); err != nil {
		t.Fatal(err)
	}
	s, err := m.Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"GH_TOKEN": "secret:GH_TOKEN", "TZ": "Europe/Lisbon"}
	if !maps.Equal(s.Env, want) {
		t.Fatalf("env %v, want %v", s.Env, want)
	}
	if !strings.Contains(out.String(), "+GH_TOKEN ~TZ -OLD") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRefreshEnvKeepsTheSavedEnvWithoutTheTemplate(t *testing.T) {
	m := envVM(t, map[string]string{"TZ": "UTC"}, "")
	var out strings.Builder
	m.Out = &out
	if err := m.refreshEnv("dev"); err != nil {
		t.Fatal(err)
	}
	if s, _ := m.Load("dev"); s.Env["TZ"] != "UTC" {
		t.Fatalf("env %v", s.Env)
	}
	if !strings.Contains(out.String(), "keeps the env it was created with") {
		t.Fatalf("output %q", out.String())
	}
}

// The VM's Claude uses its subscription token although the template says api-key.
func TestRefreshEnvRefusesAnAPIKeyOverridingTheVMsClaudeToken(t *testing.T) {
	m := envVM(t, nil, "agents: { claude: { auth: api-key } }\nenv: { ANTHROPIC_API_KEY: \"secret:K\" }\n")
	if err := m.refreshEnv("dev"); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err %v", err)
	}
	if s, _ := m.Load("dev"); len(s.Env) != 0 {
		t.Fatalf("env %v", s.Env)
	}
}

// A VM made before category files has an old template name, which may now
// name other sections: its env and base are left alone.
func TestOldTemplateNamesAreNotReinterpreted(t *testing.T) {
	m := envVM(t, map[string]string{"TZ": "UTC"}, "env: { TZ: Europe/Lisbon }\n")
	s, _ := m.Load("dev")
	s.Picks = false
	if err := m.save(s); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	m.Out = &out
	if err := m.refreshEnv("dev"); err != nil || !strings.Contains(out.String(), "old-style template envtpl") {
		t.Fatalf("%v %q", err, out.String())
	}
	if s, _ := m.Load("dev"); s.Env["TZ"] != "UTC" {
		t.Fatalf("env %v", s.Env)
	}
	if _, err := m.baseFor(context.Background(), s); err == nil || !strings.Contains(err.Error(), "old-style template") {
		t.Errorf("baseFor: %v", err)
	}
	if got, _ := m.RebuildTargets(); len(got) != 0 {
		t.Errorf("targets %v", got)
	}
}

// Skills and plugins come from the template too, even when the env is the same.
func TestRefreshTakesTheTemplatesSkills(t *testing.T) {
	m := envVM(t, map[string]string{"TZ": "UTC"}, "env: { TZ: UTC }\n"+
		"skills: [{ git: \"https://example.com/s.git\", ref: \"0123456789abcdef0123456789abcdef01234567\", path: skills/a }]\n")
	var out strings.Builder
	m.Out = &out
	if err := m.refreshEnv("dev"); err != nil {
		t.Fatal(err)
	}
	s, err := m.Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	def, err := template.Loader{}.Load("")
	if err != nil {
		t.Fatal(err)
	}
	want := append(def.Skills, template.Skill{Git: "https://example.com/s.git", Ref: "0123456789abcdef0123456789abcdef01234567", Path: "skills/a"})
	if !slices.Equal(s.Skills, want) {
		t.Fatalf("skills %v, want %v", s.Skills, want)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("%d skills, 0 plugins", len(want))) {
		t.Fatalf("output %q", out.String())
	}
}

// A failed fetch marks its bundle Failed (the guest keeps its copy), and
// entries for agents the VM doesn't have aren't fetched.
func TestAgentBundles(t *testing.T) {
	m := testManager(t)
	ref := strings.Repeat("a", 40)
	s := &State{
		Agents: template.Agents{Claude: &template.Agent{}},
		Skills: []template.Skill{
			{Git: "https://127.0.0.1:1/x", Ref: ref, Path: "s"},
			{Agent: "codex", Git: "https://127.0.0.1:1/y", Ref: ref, Path: "c"},
		},
		Plugins: []template.Plugin{{Agent: "codex", Git: "https://127.0.0.1:1/y", Ref: ref, Path: "p"}},
	}
	skills, plugins := m.agentBundles(context.Background(), s)
	if len(skills) != 1 || skills[0].Name != "s" || !skills[0].Failed {
		t.Errorf("skills = %+v", skills)
	}
	if len(plugins) != 1 || !plugins[0].Failed || plugins[0].Agent != "codex" {
		t.Errorf("plugins = %+v", plugins)
	}
	if ents, _ := os.ReadDir(m.P.Sources()); len(ents) > 2 { // one repo and its lock
		t.Errorf("fetched for an agent the VM doesn't have: %v", ents)
	}
}
