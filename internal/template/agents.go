package template

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Claude auth modes.
const (
	ClaudeOAuth  = "oauth-token" // CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token` (subscription)
	ClaudeAPIKey = "api-key"     // ANTHROPIC_API_KEY
)

// Agent configures one coding agent. Version is an npm version or "latest".
type Agent struct {
	Version string   `yaml:"version,omitempty" json:"version,omitempty"`
	Auth    string   `yaml:"auth,omitempty" json:"auth,omitempty"` // claude only
	Keys    []string `yaml:"keys,omitempty" json:"keys,omitempty"` // opencode/pi/omp: provider API key secrets
	// Yolo makes curral run skip permission prompts (claude, opencode); the VM is the sandbox.
	Yolo *bool `yaml:"yolo,omitempty" json:"yolo,omitempty"`
}

// SkipPermissions reports whether curral run starts the agent without permission prompts.
func (a *Agent) SkipPermissions() bool { return a != nil && a.Yolo != nil && *a.Yolo }

type Agents struct {
	Claude   *Agent `yaml:"claude,omitempty" json:"claude,omitempty"`
	OpenCode *Agent `yaml:"opencode,omitempty" json:"opencode,omitempty"`
	Pi       *Agent `yaml:"pi,omitempty" json:"pi,omitempty"`
	OMP      *Agent `yaml:"omp,omitempty" json:"omp,omitempty"` // oh-my-pi
}

func (a Agents) Any() bool {
	return a.Claude != nil || a.OpenCode != nil || a.Pi != nil || a.OMP != nil
}

// MCP is an MCP server rendered into every agent's config.
type MCP struct {
	Name    string            `yaml:"name" json:"name"`
	Type    string            `yaml:"type" json:"type"` // stdio | http | sse
	Command []string          `yaml:"command,omitempty" json:"command,omitempty"`
	URL     string            `yaml:"url,omitempty" json:"url,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`         // values: literal or secret:NAME
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"` // values: literal or secret:NAME
}

type Sync struct {
	ClaudeConfig *bool `yaml:"claude_config,omitempty" json:"claude_config,omitempty"` // default false
	GitIdentity  *bool `yaml:"git_identity,omitempty" json:"git_identity,omitempty"`   // default true
}

func (s Sync) Claude() bool { return s.ClaudeConfig != nil && *s.ClaudeConfig }
func (s Sync) Git() bool    { return s.GitIdentity == nil || *s.GitIdentity }

// Provider maps an API key secret (also the variable omp reads it from) to
// the provider id opencode and pi use and the API host the VM must reach.
type Provider struct {
	ID   string
	Host string
}

var Providers = map[string]Provider{
	"ANTHROPIC_API_KEY":  {"anthropic", "api.anthropic.com"},
	"OPENAI_API_KEY":     {"openai", "api.openai.com"},
	"OPENROUTER_API_KEY": {"openrouter", "openrouter.ai"},
	"GEMINI_API_KEY":     {"google", "generativelanguage.googleapis.com"},
	"GROQ_API_KEY":       {"groq", "api.groq.com"},
	"MISTRAL_API_KEY":    {"mistral", "api.mistral.ai"},
	"XAI_API_KEY":        {"xai", "api.x.ai"},
	"DEEPSEEK_API_KEY":   {"deepseek", "api.deepseek.com"},
}

// SecretRef returns NAME for a "secret:NAME" value.
func SecretRef(v string) (string, bool) { return strings.CutPrefix(v, "secret:") }

func mergeAgent(a, o *Agent) *Agent {
	if o == nil {
		return a
	}
	if a == nil {
		c := *o
		c.Keys = append([]string(nil), o.Keys...)
		return &c
	}
	c := *a
	if o.Version != "" {
		c.Version = o.Version
	}
	if o.Auth != "" {
		c.Auth = o.Auth
	}
	if o.Yolo != nil {
		c.Yolo = o.Yolo
	}
	c.Keys = appendUniq(append([]string(nil), a.Keys...), o.Keys...)
	return &c
}

func (t *Template) mergeAgents(o *Template) {
	t.Agents.Claude = mergeAgent(t.Agents.Claude, o.Agents.Claude)
	t.Agents.OpenCode = mergeAgent(t.Agents.OpenCode, o.Agents.OpenCode)
	t.Agents.Pi = mergeAgent(t.Agents.Pi, o.Agents.Pi)
	t.Agents.OMP = mergeAgent(t.Agents.OMP, o.Agents.OMP)
	for k, v := range o.Env {
		if t.Env == nil {
			t.Env = map[string]string{}
		}
		t.Env[k] = v
	}
	for _, m := range o.MCP {
		replaced := false
		for i := range t.MCP {
			if t.MCP[i].Name == m.Name {
				t.MCP[i], replaced = m, true
			}
		}
		if !replaced {
			t.MCP = append(t.MCP, m)
		}
	}
	if o.Sync.ClaudeConfig != nil {
		t.Sync.ClaudeConfig = o.Sync.ClaudeConfig
	}
	if o.Sync.GitIdentity != nil {
		t.Sync.GitIdentity = o.Sync.GitIdentity
	}
}

var (
	envNameRe   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	versionRe   = regexp.MustCompile(`^(latest|[0-9]+(\.[0-9]+){0,2}(-[0-9A-Za-z.]+)?)$`)
	mcpNameRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	headerRe    = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	reservedEnv = map[string]bool{"HOME": true, "USER": true, "PATH": true, "SHELL": true, "LOGNAME": true}
)

func validValue(v string) error {
	if name, ok := SecretRef(v); ok {
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("invalid secret name %q", name)
		}
		return nil
	}
	if strings.ContainsAny(v, "\n\r\x00") {
		return fmt.Errorf("value contains a newline or NUL")
	}
	return nil
}

func (t *Template) validateAgents() error {
	if (t.Agents.Claude != nil || t.Agents.OpenCode != nil || t.Agents.Pi != nil) && t.Runtimes.Node == "" {
		return fmt.Errorf("agents need runtimes.node (omp doesn't)")
	}
	for name, a := range map[string]*Agent{"claude": t.Agents.Claude, "opencode": t.Agents.OpenCode, "pi": t.Agents.Pi, "omp": t.Agents.OMP} {
		if a == nil {
			continue
		}
		if a.Version != "" && !versionRe.MatchString(a.Version) {
			return fmt.Errorf("agents.%s.version: invalid %q", name, a.Version)
		}
		// omp's releases are tagged v<major>.<minor>.<patch>; npm resolves partial versions.
		if name == "omp" && a.Version != "" && a.Version != "latest" && strings.Count(strings.SplitN(a.Version, "-", 2)[0], ".") != 2 {
			return fmt.Errorf("agents.omp.version must be latest or a full version (like 18.12.5), not %q", a.Version)
		}
		if name == "claude" {
			switch a.Auth {
			case "", ClaudeOAuth, ClaudeAPIKey:
			default:
				return fmt.Errorf("agents.claude.auth must be %s or %s", ClaudeOAuth, ClaudeAPIKey)
			}
			if len(a.Keys) > 0 {
				return fmt.Errorf("agents.claude.keys is not supported; use agents.claude.auth")
			}
		} else if a.Auth != "" {
			return fmt.Errorf("agents.%s.auth is not supported (subscription tokens are for Claude Code only); use keys", name)
		}
		if (name == "pi" || name == "omp") && a.Yolo != nil {
			return fmt.Errorf("agents.%s.yolo is not supported (%[1]s has no permission prompts)", name)
		}
		for _, k := range a.Keys {
			if _, ok := Providers[k]; !ok {
				return fmt.Errorf("agents.%s.keys: unknown provider key %q (known: %s)", name, k, strings.Join(providerKeys(), ", "))
			}
		}
	}
	for k, v := range t.Env {
		if !envNameRe.MatchString(k) || reservedEnv[k] {
			return fmt.Errorf("env: invalid name %q", k)
		}
		if err := validValue(v); err != nil {
			return fmt.Errorf("env.%s: %w", k, err)
		}
	}
	if c := t.Agents.Claude; c != nil && c.Auth != ClaudeAPIKey {
		if _, ok := t.Env["ANTHROPIC_API_KEY"]; ok {
			return fmt.Errorf("env.ANTHROPIC_API_KEY would override Claude's subscription token; give it to opencode, pi or omp with agents.<agent>.keys instead")
		}
	}
	for _, m := range t.MCP {
		if !mcpNameRe.MatchString(m.Name) {
			return fmt.Errorf("mcp: invalid name %q", m.Name)
		}
		switch m.Type {
		case "stdio":
			if len(m.Command) == 0 || m.URL != "" || len(m.Headers) > 0 {
				return fmt.Errorf("mcp %s: stdio servers need command (and no url/headers)", m.Name)
			}
		case "http", "sse":
			u, err := url.Parse(m.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || len(m.Command) > 0 || len(m.Env) > 0 {
				return fmt.Errorf("mcp %s: %s servers need an http(s) url (and no command/env)", m.Name, m.Type)
			}
		default:
			return fmt.Errorf("mcp %s: type must be stdio, http or sse", m.Name)
		}
		for k, v := range m.Env {
			if !envNameRe.MatchString(k) {
				return fmt.Errorf("mcp %s: invalid env name %q", m.Name, k)
			}
			if err := validValue(v); err != nil {
				return fmt.Errorf("mcp %s env.%s: %w", m.Name, k, err)
			}
		}
		for k, v := range m.Headers {
			if !headerRe.MatchString(k) {
				return fmt.Errorf("mcp %s: invalid header %q", m.Name, k)
			}
			if err := validValue(v); err != nil {
				return fmt.Errorf("mcp %s header %s: %w", m.Name, k, err)
			}
		}
	}
	return nil
}

func providerKeys() []string {
	var ks []string
	for k := range Providers {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
