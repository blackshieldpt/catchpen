package vm

import (
	"strings"
	"testing"

	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/template"
)

func TestRunCommand(t *testing.T) {
	yes := true
	s := &State{Name: "v", Agents: template.Agents{Claude: &template.Agent{Yolo: &yes}, OpenCode: &template.Agent{}}}
	got, err := RunCommand(s, "claude", []string{"--model", "it's"})
	want := `; tmux new-session -A -s claude -- 'curral-env' 'claude' '--dangerously-skip-permissions' '--model' 'it'\''s'`
	if err != nil || !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "command -v curral-env") || !strings.Contains(got, "has-session -t =claude") {
		t.Errorf("claude:\n got %s\nwant %s (%v)", got, want, err)
	}
	if got, _ := RunCommand(s, "opencode", nil); strings.Contains(got, "--auto") {
		t.Errorf("opencode without yolo: %s", got)
	}
	s.Agents.OpenCode.Yolo = &yes
	if got, _ := RunCommand(s, "opencode", nil); !strings.HasSuffix(got, "'opencode' '--auto'") {
		t.Errorf("opencode yolo: %s", got)
	}
	if _, err := RunCommand(s, "pi", nil); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("pi missing: %v", err)
	}
	s.Agents.OMP = &template.Agent{}
	if got, err := RunCommand(s, "omp", nil); err != nil || !strings.HasSuffix(got, "-s omp -- 'curral-env' 'omp'") {
		t.Errorf("omp: %s %v", got, err)
	}
	s.Agents.Codex = &template.Agent{}
	if got, err := RunCommand(s, "codex", nil); err != nil || !strings.HasSuffix(got, "-s codex -- 'curral-env' 'codex'") {
		t.Errorf("codex: %s %v", got, err)
	}
	s.Agents.Codex.Yolo = &yes
	if got, _ := RunCommand(s, "codex", nil); !strings.HasSuffix(got, "'codex' '--yolo'") {
		t.Errorf("codex yolo: %s", got)
	}
	if _, err := RunCommand(s, "bash", nil); err == nil {
		t.Error("unknown agent accepted")
	}
	if WorkDir(s) != "" || AttachCommand(s) != "tmux new-session -A -s main" {
		t.Errorf("no repos: %q / %s", WorkDir(s), AttachCommand(s))
	}
	s.Repos = []RepoState{{Repo: forge.Repo{Forge: "g", Owner: "o", Name: "api"}}}
	if WorkDir(s) != "/workspace/api" || !strings.Contains(AttachCommand(s), "-s main -c '/workspace/api'") {
		t.Errorf("one repo: %s / %s", WorkDir(s), AttachCommand(s))
	}
	s.Repos = append(s.Repos, RepoState{Repo: forge.Repo{Forge: "g", Owner: "o", Name: "web"}})
	if WorkDir(s) != "/workspace" {
		t.Errorf("two repos: %s", WorkDir(s))
	}
}

func TestRecordings(t *testing.T) {
	m := testManager(t)
	if ids, err := m.Recordings("a"); err != nil || ids != nil {
		t.Errorf("none: %v %v", ids, err)
	}
	writeFile(t, m.RecordingPath("a", "20260102-030405-ssh-ab12"), "{}")
	writeFile(t, m.RecordingPath("a", "20260101-030405-run-cd34"), "{}")
	writeFile(t, m.RecordingPath("a", "notes")+".txt", "x")
	ids, err := m.Recordings("a")
	if err != nil || strings.Join(ids, ",") != "20260101-030405-run-cd34,20260102-030405-ssh-ab12" {
		t.Errorf("ids = %v, %v", ids, err)
	}
	if _, err := m.Recordings("../x"); err == nil {
		t.Error("bad name accepted")
	}
}

func TestDefaultAgent(t *testing.T) {
	for _, c := range []struct {
		agents template.Agents
		want   string
	}{
		{template.Agents{Claude: &template.Agent{}, Pi: &template.Agent{}}, "claude"},
		{template.Agents{Codex: &template.Agent{}, OpenCode: &template.Agent{}}, "opencode"},
		{template.Agents{}, ""},
	} {
		if got := (&State{Agents: c.agents}).DefaultAgent(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.agents, got, c.want)
		}
	}
}
