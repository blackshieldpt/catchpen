package vm

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackshieldpt/curral/internal/template"
)

// envVM saves a VM created from template gh with env, and writes gh.yaml.
func envVM(t *testing.T, env map[string]string, tpl string) *Manager {
	t.Helper()
	m := testManager(t)
	if err := os.MkdirAll(m.P.VM("dev"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &State{Name: "dev", Template: "gh", User: "agent", Env: env,
		Agents: template.Agents{Claude: &template.Agent{Auth: template.ClaudeOAuth}}}
	if err := m.save(s); err != nil {
		t.Fatal(err)
	}
	if tpl != "" {
		writeFile(t, filepath.Join(m.P.Templates(), "gh.yaml"), tpl)
	}
	return m
}

func TestRefreshEnvTakesTheTemplatesEnv(t *testing.T) {
	m := envVM(t, map[string]string{"TZ": "UTC", "OLD": "x"}, "extends: default\nenv: { GH_TOKEN: \"secret:GH_TOKEN\", TZ: Europe/Lisbon }\n")
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
	m := envVM(t, nil, "extends: default\nagents: { claude: { auth: api-key } }\nenv: { ANTHROPIC_API_KEY: \"secret:K\" }\n")
	if err := m.refreshEnv("dev"); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err %v", err)
	}
	if s, _ := m.Load("dev"); len(s.Env) != 0 {
		t.Fatalf("env %v", s.Env)
	}
}
