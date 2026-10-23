package template

// AgentDef describes one agent curral can install and run. Adding an agent
// takes an entry here and a field in Agents, its credential push
// (internal/agents), its task command and stream parser (internal/task), and
// its docs; tests fail while one of those is missing.
type AgentDef struct {
	Name string
	Get  func(*Agents) **Agent // its field in Agents
	// Package is its npm package, smoke-tested with `<Name> --version`, and
	// Env the environment every session gets for it. omp has no package: the
	// build script installs its release binary.
	Package string
	Env     []string
	Node    bool     // needs runtimes.node
	Auth    []string // its auth modes; nil: it takes provider keys instead
	Yolo    string   // curral run's flag to skip permission prompts; "": it has none
	// Transcripts are its session files, under the guest home.
	Transcripts []string
}

// AgentDefs are the agents, in the order curral installs and configures them.
var AgentDefs = []AgentDef{
	{Name: "claude", Get: func(a *Agents) **Agent { return &a.Claude }, Package: "@anthropic-ai/claude-code",
		Env: []string{"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}, Node: true,
		Auth: []string{ClaudeOAuth, ClaudeAPIKey}, Yolo: "--dangerously-skip-permissions", Transcripts: []string{".claude/projects"}},
	{Name: "opencode", Get: func(a *Agents) **Agent { return &a.OpenCode }, Package: "opencode-ai",
		Env: []string{"OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_SHARE=1"}, Node: true, Yolo: "--auto",
		Transcripts: []string{".local/share/opencode/opencode.db", ".local/share/opencode/opencode.db-wal", ".local/share/opencode/opencode.db-shm"}},
	{Name: "pi", Get: func(a *Agents) **Agent { return &a.Pi }, Package: "@earendil-works/pi-coding-agent",
		Env: []string{"PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1", "PI_TELEMETRY=0"}, Node: true, Transcripts: []string{".pi/agent/sessions"}},
	{Name: "omp", Get: func(a *Agents) **Agent { return &a.OMP }, Transcripts: []string{".omp/agent/sessions"}},
	// Update checks and telemetry are off in its config.toml.
	{Name: "codex", Get: func(a *Agents) **Agent { return &a.Codex }, Package: "@openai/codex", Node: true,
		Auth: []string{CodexAPIKey, CodexChatGPT, CodexNone}, Yolo: "--yolo", Transcripts: []string{".codex/sessions"}},
}

// AgentDefByName finds an agent by name.
func AgentDefByName(name string) (AgentDef, bool) {
	for _, d := range AgentDefs {
		if d.Name == name {
			return d, true
		}
	}
	return AgentDef{}, false
}

// AgentNames are the agents' names, in order.
func AgentNames() []string {
	var out []string
	for _, d := range AgentDefs {
		out = append(out, d.Name)
	}
	return out
}

// Get returns the named agent's settings (nil: not installed, or no such agent).
func (a *Agents) Get(name string) *Agent {
	if d, ok := AgentDefByName(name); ok {
		return *d.Get(a)
	}
	return nil
}
