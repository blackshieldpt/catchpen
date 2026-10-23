// Package agents delivers credentials and configuration for Claude Code,
// opencode, pi and omp into a running VM. Secrets are resolved on the host at push
// time and written to 0600 files in the guest user's home; nothing secret is
// baked into bases or seeds.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/blackshieldpt/curral/internal/cloudinit"
	"github.com/blackshieldpt/curral/internal/template"
)

// Remote runs a shell command in the guest as the VM user.
type Remote interface {
	Run(ctx context.Context, cmd string, stdin []byte) ([]byte, error)
}

// Secrets resolves secret names on the host.
type Secrets interface {
	Lookup(ctx context.Context, name string) (string, bool, error)
}

// Spec is what to push into a VM.
type Spec struct {
	Agents    template.Agents
	Env       map[string]string // literal or secret:NAME
	MCP       []template.MCP
	Sync      template.Sync
	ClaudeDir string // host ~/.claude, for sync.claude_config
	GitName   string // host git identity, for sync.git_identity
	GitEmail  string
	ExtraEnv  map[string]string // already-resolved values (e.g. forge tokens for gh)
}

// Result reports what could not be delivered.
type Result struct {
	Missing  []string // secrets not found on the host
	Found    []string // names of secrets pushed (for the audit log)
	Warnings []string
}

// Guest paths, relative to the user's home.
const (
	envFile         = ".ssh/environment"
	claudeEnvFile   = cloudinit.ClaudeEnvFile // read only by the guest's claude wrapper
	claudeJSON      = ".claude.json"
	opencodeAuth    = ".local/share/opencode/auth.json"
	opencodeConfig  = ".config/opencode/opencode.json"
	piAuth          = ".pi/agent/auth.json"
	piMCP           = ".pi/agent/mcp.json"
	ompEnv          = ".omp/agent/.env" // omp keeps logins in agent.db, but reads API keys from here
	ompMCP          = ".omp/agent/mcp.json"
	codexConfig     = ".codex/config.toml" // its auth.json is written by codex login
	claudeOAuthEnv  = "CLAUDE_CODE_OAUTH_TOKEN"
	anthropicKeyEnv = "ANTHROPIC_API_KEY"
	openaiKeyEnv    = "OPENAI_API_KEY"
	claudeKeySuffix = 20      // Claude Code records approved API keys by their last 20 characters
	maxGuestDoc     = 4 << 20 // largest guest config file read back at all (config.toml: maxCodexConfig)
)

// claudeReal is the real claude in bases that wrap it to load claudeEnvFile;
// older bases lack it.
var claudeReal = cloudinit.ClaudeReal

type pusher struct {
	ctx    context.Context
	r      Remote
	sec    Secrets
	res    Result
	missed map[string]bool

	claudeKey string // Claude's API key, in api-key mode
}

// pushers configure each agent in the guest: its settings, MCP servers and,
// for some, its credentials.
var pushers = map[string]func(p *pusher, a *template.Agent, servers []template.MCP) error{
	"claude": func(p *pusher, _ *template.Agent, servers []template.MCP) error {
		return p.claude(p.claudeKey, servers)
	},
	"opencode": (*pusher).opencode,
	"pi":       (*pusher).pi,
	"omp":      (*pusher).omp,
	"codex":    (*pusher).codex,
}

// resolve returns a literal value, or the secret it references.
func (p *pusher) resolve(v string) (string, bool, error) {
	name, ok := template.SecretRef(v)
	if !ok {
		return v, true, nil
	}
	return p.secret(name)
}

func (p *pusher) secret(name string) (string, bool, error) {
	v, ok, err := p.sec.Lookup(p.ctx, name)
	if err != nil {
		return "", false, err
	}
	if !p.missed[name] {
		p.missed[name] = true
		if ok {
			p.res.Found = append(p.res.Found, name)
		} else {
			p.res.Missing = append(p.res.Missing, name)
		}
	}
	return v, ok, nil
}

func (p *pusher) warn(format string, a ...any) {
	p.res.Warnings = append(p.res.Warnings, fmt.Sprintf(format, a...))
}

// Push writes the VM's environment, agent auth and MCP config, and optional
// git identity and Claude config.
func Push(ctx context.Context, r Remote, s Spec, sec Secrets) (Result, error) {
	p := &pusher{ctx: ctx, r: r, sec: sec, missed: map[string]bool{}}
	env := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		v, ok, err := p.resolve(s.Env[k])
		if err != nil {
			return p.res, err
		}
		if ok {
			env[k] = v
		}
	}
	for k, v := range s.ExtraEnv {
		env[k] = v
	}
	if c := s.Agents.Claude; c != nil {
		name := claudeOAuthEnv
		if c.Auth == template.ClaudeAPIKey {
			name = anthropicKeyEnv
		}
		v, ok, err := p.secret(name)
		if err != nil {
			return p.res, err
		}
		claudeEnv := map[string]string{}
		if ok {
			claudeEnv[name] = v
			if name == anthropicKeyEnv {
				p.claudeKey = v
			}
		} else if name == claudeOAuthEnv {
			p.warn("claude: no %s on the host; run `claude setup-token` and store the token (secrets.env or config.yaml secrets)", claudeOAuthEnv)
		}
		// Only claude gets its token or key, where the base wraps claude
		// to load it; older bases get it in every session's environment.
		out, err := p.r.Run(p.ctx, "test -e "+claudeReal+" && echo wrapped || true", nil)
		if err != nil {
			return p.res, err
		}
		if strings.TrimSpace(string(out)) == "wrapped" {
			if err := p.put(claudeEnvFile, envFileContent(claudeEnv)); err != nil {
				return p.res, err
			}
		} else {
			maps.Copy(env, claudeEnv)
			if ok {
				p.warn("claude: this VM's base predates keeping Claude's credential to Claude Code, so every session gets it in its environment (curral reset --latest moves the VM to a current base)")
			}
		}
	}
	if err := p.put(envFile, envFileContent(env)); err != nil {
		return p.res, err
	}
	servers, err := p.mcpServers(s.MCP)
	if err != nil {
		return p.res, err
	}
	for _, d := range template.AgentDefs {
		if a := *d.Get(&s.Agents); a != nil {
			if err := pushers[d.Name](p, a, servers); err != nil {
				return p.res, err
			}
		}
	}
	if s.Sync.Git() && (s.GitName != "" || s.GitEmail != "") {
		var cmds []string
		if s.GitName != "" {
			cmds = append(cmds, "git config --global user.name "+shQuote(s.GitName))
		}
		if s.GitEmail != "" {
			cmds = append(cmds, "git config --global user.email "+shQuote(s.GitEmail))
		}
		if _, err := r.Run(ctx, strings.Join(cmds, " && "), nil); err != nil {
			p.warn("git identity: %v", err)
		}
	}
	if s.Sync.Claude() && s.ClaudeDir != "" {
		tarball, skipped, err := claudeConfigTar(s.ClaudeDir)
		if err != nil {
			return p.res, fmt.Errorf("claude config: %w", err)
		}
		for _, k := range skipped {
			p.warn("claude config: not synced: %s", k)
		}
		if _, err := r.Run(ctx, `umask 077; mkdir -p "$HOME/.claude" && tar -xf - --no-same-owner -C "$HOME/.claude"`, tarball); err != nil {
			return p.res, fmt.Errorf("claude config: %w", err)
		}
	}
	// After the config sync, which may replace settings.json.
	if err := p.hooks(s); err != nil {
		return p.res, err
	}
	return p.res, nil
}

func envFileContent(env map[string]string) []byte {
	var b strings.Builder
	b.WriteString("# written by curral at each start; do not edit\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return []byte(b.String())
}

func (p *pusher) mcpServers(ms []template.MCP) ([]template.MCP, error) {
	var out []template.MCP
	for _, m := range ms {
		ok := true
		res := func(in map[string]string) (map[string]string, error) {
			if len(in) == 0 {
				return nil, nil
			}
			o := map[string]string{}
			for k, v := range in {
				rv, found, err := p.resolve(v)
				if err != nil {
					return nil, err
				}
				ok = ok && found
				o[k] = rv
			}
			return o, nil
		}
		env, err := res(m.Env)
		if err != nil {
			return nil, err
		}
		hdr, err := res(m.Headers)
		if err != nil {
			return nil, err
		}
		if !ok {
			p.warn("mcp %s: skipped (missing secret)", m.Name)
			continue
		}
		m.Env, m.Headers = env, hdr
		out = append(out, m)
	}
	return out, nil
}

func (p *pusher) claude(apiKey string, servers []template.MCP) error {
	doc, err := p.getJSON(claudeJSON)
	if err != nil {
		return err
	}
	doc["hasCompletedOnboarding"] = true
	if apiKey != "" {
		// Pre-approve the key so Claude Code doesn't ask whether to use it.
		resp := obj(doc, "customApiKeyResponses")
		suffix := apiKey[max(len(apiKey)-claudeKeySuffix, 0):]
		resp["approved"] = addString(resp["approved"], suffix)
		resp["rejected"] = removeString(resp["rejected"], suffix)
	}
	if len(servers) > 0 {
		mcp := obj(doc, "mcpServers")
		for _, s := range servers {
			e := map[string]any{"type": s.Type}
			if s.Type == "stdio" {
				e["command"], e["args"] = s.Command[0], s.Command[1:]
				if s.Env != nil {
					e["env"] = s.Env
				}
			} else {
				e["url"] = s.URL
				if s.Headers != nil {
					e["headers"] = s.Headers
				}
			}
			mcp[s.Name] = e
		}
	}
	return p.putJSON(claudeJSON, doc)
}

func (p *pusher) providerKeys(a *template.Agent) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range a.Keys {
		v, ok, err := p.secret(k)
		if err != nil {
			return nil, err
		}
		if ok {
			out[template.Providers[k].ID] = v
		}
	}
	return out, nil
}

func (p *pusher) opencode(a *template.Agent, servers []template.MCP) error {
	keys, err := p.providerKeys(a)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		auth, err := p.getJSON(opencodeAuth)
		if err != nil {
			return err
		}
		for id, k := range keys {
			auth[id] = map[string]any{"type": "api", "key": k}
		}
		if err := p.putJSON(opencodeAuth, auth); err != nil {
			return err
		}
	}
	cfg, err := p.getJSON(opencodeConfig)
	if err != nil {
		return err
	}
	if _, ok := cfg["$schema"]; !ok {
		cfg["$schema"] = "https://opencode.ai/config.json"
	}
	cfg["autoupdate"] = false
	cfg["share"] = "disabled"
	if len(servers) > 0 {
		mcp := obj(cfg, "mcp")
		for _, s := range servers {
			e := map[string]any{"enabled": true}
			if s.Type == "stdio" {
				e["type"], e["command"] = "local", s.Command
				if s.Env != nil {
					e["environment"] = s.Env
				}
			} else {
				e["type"], e["url"] = "remote", s.URL
				if s.Headers != nil {
					e["headers"] = s.Headers
				}
			}
			mcp[s.Name] = e
		}
	}
	return p.putJSON(opencodeConfig, cfg)
}

func (p *pusher) pi(a *template.Agent, servers []template.MCP) error {
	keys, err := p.providerKeys(a)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		auth, err := p.getJSON(piAuth)
		if err != nil {
			return err
		}
		for id, k := range keys {
			auth[id] = map[string]any{"type": "api_key", "key": k}
		}
		if err := p.putJSON(piAuth, auth); err != nil {
			return err
		}
	}
	var entries []template.MCP
	for _, s := range servers {
		if s.Type == "sse" {
			p.warn("mcp %s: pi does not support SSE servers; skipped for pi", s.Name)
			continue
		}
		entries = append(entries, s)
	}
	if len(entries) == 0 {
		return nil
	}
	doc, err := p.getJSON(piMCP)
	if err != nil {
		return err
	}
	addMCPServers(doc, entries, false)
	return p.putJSON(piMCP, doc)
}

// addMCPServers writes servers into doc's mcpServers the way pi and omp
// read them (omp also wants each server's type).
func addMCPServers(doc map[string]any, servers []template.MCP, typed bool) {
	mcp := obj(doc, "mcpServers")
	for _, s := range servers {
		e := map[string]any{}
		if typed {
			e["type"] = s.Type
		}
		if s.Type == "stdio" {
			e["command"], e["args"] = s.Command[0], s.Command[1:]
			if s.Env != nil {
				e["env"] = s.Env
			}
		} else {
			e["url"] = s.URL
			if s.Headers != nil {
				e["headers"] = s.Headers
			}
		}
		mcp[s.Name] = e
	}
}

func (p *pusher) omp(a *template.Agent, servers []template.MCP) error {
	var env strings.Builder
	for _, k := range a.Keys {
		v, ok, err := p.secret(k)
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintf(&env, "%s=%s\n", k, v)
		}
	}
	if err := p.put(ompEnv, []byte(env.String())); err != nil {
		return err
	}
	if len(servers) == 0 {
		return nil
	}
	doc, err := p.getJSON(ompMCP)
	if err != nil {
		return err
	}
	addMCPServers(doc, servers, true)
	return p.putJSON(ompMCP, doc)
}

// The guest's config.toml is parsed on the host. go-toml recurses without a
// depth limit, and writing deep tables back costs quadratic memory: small and
// shallow files only, or a guest could take the host down.
const (
	maxCodexConfig = 256 << 10
	maxTOMLDepth   = 64
)

// limitError rejects a guest file curral won't parse; it says why.
type limitError string

func (e limitError) Error() string { return string(e) }

func tomlUnmarshal(b []byte, v any) error {
	if len(b) > maxCodexConfig {
		return limitError("is over 256 KiB")
	}
	depth := 0
	for _, c := range b {
		switch c {
		case '[', '{':
			if depth++; depth > maxTOMLDepth {
				return limitError("nests more than 64 levels")
			}
		case ']', '}':
			depth = max(depth-1, 0)
		}
	}
	if err := toml.Unmarshal(b, v); err != nil {
		return err
	}
	if m, ok := v.(*map[string]any); ok && tooDeep(*m, maxTOMLDepth) {
		return limitError("nests more than 64 levels") // dotted keys, which the bracket count misses
	}
	return nil
}

// tooDeep reports whether v nests tables or arrays more than left deep.
func tooDeep(v any, left int) bool {
	switch x := v.(type) {
	case map[string]any:
		if left == 0 {
			return true
		}
		for _, e := range x {
			if tooDeep(e, left-1) {
				return true
			}
		}
	case []any:
		if left == 0 {
			return true
		}
		for _, e := range x {
			if tooDeep(e, left-1) {
				return true
			}
		}
	}
	return false
}

func (p *pusher) codex(a *template.Agent, servers []template.MCP) error {
	cfg, err := p.getDoc(codexConfig, "TOML", tomlUnmarshal)
	if err != nil {
		return err
	}
	// Versions change by rebuilding the base; nothing leaves the VM but the
	// model traffic; logins live in auth.json, not a keyring the VM lacks.
	cfg["check_for_update_on_startup"] = false
	cfg["cli_auth_credentials_store"] = "file"
	obj(cfg, "analytics")["enabled"] = false
	obj(cfg, "feedback")["enabled"] = false
	obj(cfg, "otel")["metrics_exporter"] = "none"
	// Codex's bubblewrap sandbox can't set up its network namespace in the
	// guest; the VM is the sandbox. Left alone once set.
	if _, ok := cfg["sandbox_mode"]; !ok {
		cfg["sandbox_mode"] = "danger-full-access"
	}
	// Codex adds the event as a last argument, which sh -c leaves in $1.
	notify := []any{"sh", "-c", curlPost(`'{"agent":"codex","hook_event_name":"agent-turn-complete"}'`, ""), "curral-notify"}
	if old, ok := cfg["notify"]; ok && fmt.Sprint(old) != fmt.Sprint(notify) {
		p.warn("codex: replaced notify in ~/%s with curral's notification hook", codexConfig)
	}
	cfg["notify"] = notify
	if len(servers) > 0 {
		mcp := obj(cfg, "mcp_servers")
		for _, s := range servers {
			e := map[string]any{}
			switch s.Type {
			case "stdio":
				e["command"], e["args"] = s.Command[0], s.Command[1:]
				if s.Env != nil {
					e["env"] = s.Env
				}
			case "http":
				e["url"] = s.URL
				if s.Headers != nil {
					e["http_headers"] = s.Headers
				}
			default:
				p.warn("mcp %s: codex does not support SSE servers; skipped for codex", s.Name)
				continue
			}
			mcp[s.Name] = e
		}
	}
	b, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := p.put(codexConfig, b); err != nil {
		return err
	}
	if a.Auth == template.CodexChatGPT || a.Auth == template.CodexNone {
		// chatgpt: signed in inside the VM with codex login --device-auth.
		// Either way, an API key login from api-key mode would stay in use.
		_, err := p.r.Run(p.ctx, "if codex login status 2>&1 | grep -qi 'api key'; then codex logout >/dev/null 2>&1; fi; true", nil)
		return err
	}
	key, ok, err := p.secret(openaiKeyEnv)
	if err != nil {
		return err
	}
	if !ok {
		p.warn("codex: no %s on the host; store it (secrets.env or config.yaml secrets), or use agents.codex.auth: chatgpt", openaiKeyEnv)
		return nil
	}
	// The key goes on stdin, into ~/.codex/auth.json (0600).
	if _, err := p.r.Run(p.ctx, "codex login --with-api-key >/dev/null", []byte(key)); err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	return nil
}

// getJSON reads a guest JSON object; a missing file is empty. An unparsable
// file is kept as <file>.curral-bak and replaced.
func (p *pusher) getJSON(rel string) (map[string]any, error) {
	return p.getDoc(rel, "JSON", json.Unmarshal)
}

// tooLarge is the guest command's exit status for a file over maxGuestDoc.
const tooLarge = 3

func (p *pusher) getDoc(rel, format string, unmarshal func([]byte, any) error) (map[string]any, error) {
	// The file is guest-controlled: only regular files, size-capped on the guest side.
	out, err := p.r.Run(p.ctx, fmt.Sprintf(`f="$HOME/%s"; [ -f "$f" ] && [ ! -L "$f" ] || exit 0; [ "$(stat -c %%s "$f")" -le %d ] || exit %d; cat "$f"`, rel, maxGuestDoc, tooLarge), nil)
	if ec := (interface{ ExitCode() int })(nil); errors.As(err, &ec) && ec.ExitCode() == tooLarge {
		return nil, fmt.Errorf("~/%s is over %d MiB, so curral won't read it back: move it aside in the VM and push again", rel, maxGuestDoc>>20)
	}
	if err != nil {
		return nil, fmt.Errorf("read ~/%s: %w", rel, err)
	}
	doc := map[string]any{}
	if strings.TrimSpace(string(out)) == "" {
		return doc, nil
	}
	if err := unmarshal(out, &doc); err != nil {
		why := "was not valid " + format
		var le limitError
		if errors.As(err, &le) {
			why = le.Error()
		}
		p.warn("~/%s %s; saved as ~/%s.curral-bak and replaced", rel, why, rel)
		if _, err := p.r.Run(p.ctx, fmt.Sprintf(`cp -f "$HOME/%s" "$HOME/%s.curral-bak"`, rel, rel), nil); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	}
	return doc, nil
}

func (p *pusher) putJSON(rel string, doc map[string]any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return p.put(rel, append(b, '\n'))
}

func (p *pusher) put(rel string, data []byte) error { return WriteFile(p.ctx, p.r, rel, data) }

// WriteFile atomically writes a 0600 file under the guest user's home. rel is
// a curral-chosen path (no quotes or $).
func WriteFile(ctx context.Context, r Remote, rel string, data []byte) error {
	if strings.ContainsAny(rel, "\"'$`\\\n") || strings.Contains(rel, "..") {
		return fmt.Errorf("invalid guest path %q", rel)
	}
	cmd := fmt.Sprintf(`set -e; umask 077; f="$HOME/%s"; mkdir -p "$(dirname "$f")"; rm -f "$f.curral-tmp"; cat > "$f.curral-tmp"; chmod 600 "$f.curral-tmp"; mv -f -T "$f.curral-tmp" "$f"`, rel)
	if _, err := r.Run(ctx, cmd, data); err != nil {
		return fmt.Errorf("write ~/%s: %w", rel, err)
	}
	return nil
}

func obj(doc map[string]any, key string) map[string]any {
	if m, ok := doc[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	doc[key] = m
	return m
}

func stringList(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func addString(v any, s string) []string {
	a := stringList(v)
	if !slices.Contains(a, s) {
		a = append(a, s)
	}
	sort.Strings(a)
	return a
}

func removeString(v any, s string) []string {
	out := []string{}
	for _, x := range stringList(v) {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
