package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "p.md"), []byte("fix the bug"), 0o600)
	s := Spec{Repo: "g:o/r", PromptFile: "p.md"}
	if err := s.Normalize(dir, dir); err != nil {
		t.Fatal(err)
	}
	if s.Agent != "claude" || s.Out != OutPR || s.Into != dir || s.Prompt != "fix the bug" || s.Timeout != 2*time.Hour {
		t.Errorf("defaults: %+v", s)
	}
	s = Spec{Prompt: "x"}
	if err := s.Normalize(dir, dir); err != nil || s.Out != OutNone || s.Into != "" {
		t.Errorf("no repo: %+v %v", s, err)
	}
	for _, c := range []struct {
		s    Spec
		want string
	}{
		{Spec{}, "needs a prompt"},
		{Spec{Prompt: "x", Agent: "bash"}, "unknown agent"},
		{Spec{Prompt: "x", Agent: "cmd"}, "--cmd"},
		{Spec{Prompt: "x", Cmd: "ls"}, "--cmd"},
		{Spec{Prompt: "x", Out: OutPR}, "needs a --repo"},
		{Spec{Prompt: "x", Out: "zip", Repo: "g:o/r"}, "--out must be"},
		{Spec{Prompt: "x", Timeout: time.Second}, "timeout"},
		{Spec{Prompt: "x", Agent: "pi", MaxBudgetUSD: 1}, "claude only"},
		{Spec{Prompt: strings.Repeat("x", MaxPrompt+1)}, "too long"},
		{Spec{Prompt: "x", PromptFile: "p.md"}, "not both"},
	} {
		if err := c.s.Normalize(dir, dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want %q", c.s, err, c.want)
		}
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.md"), []byte("prompt b"), 0o600)
	p := filepath.Join(dir, "tasks.yaml")
	os.WriteFile(p, []byte(`
defaults: { timeout: 30m, out: patch, repo: "g:o/default" }
matrix: { agent: [claude, opencode], repo: ["g:o/a", "g:o/b"] }
tasks:
  - { prompt: "a" }
  - { prompt_file: b.md, agent: pi, repo: "g:o/c" }
`), 0o600)
	cwd := t.TempDir()
	specs, err := ParseFile(p, cwd)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range specs {
		got = append(got, s.Agent+"@"+s.Repo+":"+s.Prompt+":"+s.Out+":"+s.Timeout.String())
	}
	want := "claude@g:o/a:a:patch:30m0s claude@g:o/b:a:patch:30m0s opencode@g:o/a:a:patch:30m0s opencode@g:o/b:a:patch:30m0s pi@g:o/c:prompt b:patch:30m0s"
	if strings.Join(got, " ") != want {
		t.Errorf("expanded:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if specs[0].Into != "" {
		t.Errorf("patch output has into %q", specs[0].Into)
	}
	os.WriteFile(p, []byte("tasks: [{ prompt: x, repo: 'g:o/r' }]\n"), 0o600)
	if s, err := ParseFile(p, cwd); err != nil || s[0].Into != cwd {
		t.Errorf("into should default to the working directory: %+v %v", s, err)
	}
	os.WriteFile(p, []byte("tasks: [{ prompt: x, bogus: 1 }]\n"), 0o600)
	if _, err := ParseFile(p, cwd); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestStore(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	tk := &Task{ID: NewID(), State: Queued, Created: time.Now()}
	if err := st.Create(tk, "the prompt"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(tk.ID, func(t *Task) error { t.State = Running; t.PID = 999999; return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(tk.ID)
	if err != nil || got.State != Running || got.Status() != "lost" {
		t.Errorf("load: %+v %v (status %s)", got, err, got.Status())
	}
	if list, _ := st.List(); len(list) != 1 {
		t.Errorf("list: %v", list)
	}
	if _, err := st.Load("../../etc"); err == nil {
		t.Error("bad id accepted")
	}
	if b, _ := os.ReadFile(st.Path(tk.ID, "prompt.md")); string(b) != "the prompt" {
		t.Errorf("prompt %q", b)
	}
}

func TestCommand(t *testing.T) {
	c := Command(Spec{Agent: "claude", MaxBudgetUSD: 2.5})
	if c != `claude -p --output-format stream-json --verbose --dangerously-skip-permissions --max-budget-usd 2.5 <"$HOME/.curral/task-prompt.md"` {
		t.Errorf("claude: %s", c)
	}
	if c := Command(Spec{Agent: "opencode"}); !strings.Contains(c, `--auto -- "$(cat`) {
		t.Errorf("opencode: %s", c)
	}
	if c := Command(Spec{Agent: "cmd", Cmd: "echo 'hi'"}); !strings.Contains(c, `sh -c 'echo '\''hi'\'''`) || !strings.Contains(c, `<"$HOME/.curral/task-prompt.md"`) {
		t.Errorf("cmd: %s", c)
	}
}

const claudeStream = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Fixed it.","num_turns":3,"total_cost_usd":0.12,"usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":50}}
`

const opencodeStream = `{"type":"step_start","part":{}}
{"type":"text","part":{"type":"text","text":"Done: renamed."}}
{"type":"tool_use","part":{"tool":"edit","state":{"input":{"file":"a.go"}}}}
{"type":"step_finish","part":{"cost":0.01,"tokens":{"input":100,"output":20}}}
{"type":"step_finish","part":{"cost":0.02,"tokens":{"input":50,"output":5}}}
`

const piStream = `{"type":"session","version":3}
{"type":"tool_execution_start","toolName":"bash","args":{"command":"ls"}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"All good."}],"usage":{"input":7,"output":3,"cost":{"total":0.5}}}}
{"type":"message_end","message":{"role":"user","content":"x"}}
`

func TestSummarize(t *testing.T) {
	s := Summarize("claude", strings.NewReader(claudeStream))
	if s.Result != "Fixed it." || s.Usage != (Usage{Turns: 3, InputTokens: 100, OutputTokens: 50, CostUSD: 0.12}) || s.Error != "" {
		t.Errorf("claude: %+v", s)
	}
	s = Summarize("claude", strings.NewReader(`{"type":"result","subtype":"error_max_budget","is_error":true}`))
	if s.Error != "claude: error_max_budget" {
		t.Errorf("claude error: %+v", s)
	}
	s = Summarize("opencode", strings.NewReader(opencodeStream))
	if s.Result != "Done: renamed." || s.Usage.Turns != 2 || s.Usage.InputTokens != 150 || s.Usage.CostUSD < 0.0299 {
		t.Errorf("opencode: %+v", s)
	}
	for _, a := range []string{"pi", "omp"} {
		s = Summarize(a, strings.NewReader(piStream))
		if s.Result != "All good." || s.Usage.Turns != 1 || s.Usage.CostUSD != 0.5 {
			t.Errorf("%s: %+v", a, s)
		}
	}
	if s := Summarize("omp", strings.NewReader(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"429"}}`)); s.Error != "omp: 429" {
		t.Errorf("omp error: %+v", s)
	}
	if c := Command(Spec{Agent: "omp"}); c != `omp -p --mode json <"$HOME/.curral/task-prompt.md"` {
		t.Errorf("omp command %q", c)
	}
	retried := `{"type":"result","is_error":true,"subtype":"error_during_execution"}` + "\n" + `{"type":"result","is_error":false,"subtype":"success","result":"ok"}`
	if s := Summarize("claude", strings.NewReader(retried)); s.Error != "" || s.Result != "ok" {
		t.Errorf("claude retry: %+v", s)
	}
	if s := Summarize("pi", strings.NewReader(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"429"}}`+"\n"+piStream)); s.Error != "" {
		t.Errorf("pi retry: %+v", s)
	}
	if s := Summarize("claude", strings.NewReader(strings.Repeat("x", 17<<20))); !strings.Contains(s.Error, "reading the agent's output") {
		t.Errorf("oversized line: %+v", s)
	}
	if s := Summarize("cmd", strings.NewReader("line1\nline2\n")); s.Result != "line1\nline2\n" {
		t.Errorf("cmd: %+v", s)
	}
}

func TestRender(t *testing.T) {
	var out []string
	for _, l := range strings.Split(claudeStream, "\n") {
		if r := Render("claude", []byte(l)); r != "" {
			out = append(out, r)
		}
	}
	if got := strings.Join(out, "|"); got != `Looking.`+"\n"+`→ Bash {"command":"ls"}|== success: 3 turns, $0.1200` {
		t.Errorf("claude render: %q", got)
	}
	for _, a := range []string{"pi", "omp"} {
		if r := Render(a, []byte(`{"type":"tool_execution_start","toolName":"bash","args":{}}`)); r != "→ bash {}" {
			t.Errorf("%s render: %q", a, r)
		}
	}
	if r := Render("opencode", []byte("not json")); r != "not json" {
		t.Errorf("raw line: %q", r)
	}
}
