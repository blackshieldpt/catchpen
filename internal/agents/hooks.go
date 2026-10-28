package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blackshieldpt/catchpen/internal/cloudinit"
)

// Agent hooks tell the host (catchpen notify-stdio, behind a guestfwd) when an
// agent is waiting for input. They carry no credentials; the host decides
// whether and where to notify.

var notifyURL = fmt.Sprintf("http://%s:%d/notify", cloudinit.NotifyAddr, cloudinit.NotifyPort)

const (
	claudeSettings = ".claude/settings.json"
	opencodePlugin = ".config/opencode/plugin/catchpen-notify.js"
	piExtension    = ".pi/agent/extensions/catchpen-notify.ts"
	ompExtension   = ".omp/agent/extensions/catchpen-notify.ts"
)

// postJS posts a small JSON body with node:http, which ignores the proxy
// environment (Bun's and undici's fetch may not).
func postJS(agent, event string) string {
	return fmt.Sprintf(`const body = JSON.stringify({ agent: %q, hook_event_name: %q });
	await new Promise((resolve) => {
		const req = http.request(%q, { method: "POST", timeout: 5000,
			headers: { "content-type": "application/json", "content-length": Buffer.byteLength(body) } },
			(res) => { res.resume(); res.on("end", resolve); });
		req.on("error", resolve);
		req.on("timeout", () => { req.destroy(); resolve(); });
		req.end(body);
	});`, agent, event, notifyURL)
}

func (p *pusher) hooks(s Spec) error {
	if s.Agents.Claude != nil {
		if err := p.claudeHooks(s); err != nil {
			return fmt.Errorf("claude hooks: %w", err)
		}
	}
	if s.Agents.OpenCode != nil {
		js := `// written by catchpen at each start; do not edit
import http from "node:http";
export const CatchpenNotify = async () => ({
	event: async ({ event }) => {
		if (event.type !== "session.idle") return;
	` + postJS("opencode", "session.idle") + `
	},
});
`
		if err := p.put(opencodePlugin, []byte(js)); err != nil {
			return fmt.Errorf("opencode plugin: %w", err)
		}
	}
	// omp has no agent_settled; agent_end ends each agent loop.
	for _, e := range []struct {
		on              bool
		agent, path, ev string
	}{{s.Agents.Pi != nil, "pi", piExtension, "agent_settled"}, {s.Agents.OMP != nil, "omp", ompExtension, "agent_end"}} {
		if !e.on {
			continue
		}
		ts := `// written by catchpen at each start; do not edit
import http from "node:http";
export default function (pi: any) {
	pi.on("` + e.ev + `", async () => {
	` + postJS(e.agent, e.ev) + `
	});
}
`
		if err := p.put(e.path, []byte(ts)); err != nil {
			return fmt.Errorf("%s extension: %w", e.agent, err)
		}
	}
	return nil
}

// curlPost is a shell command posting data (curl's --data-binary argument,
// shell-quoted; @- reads stdin) to the notification relay, failures ignored.
func curlPost(data, query string) string {
	return "curl -s -m 5 --noproxy '*' -X POST -H 'Content-Type: application/json' --data-binary " + data +
		" '" + notifyURL + query + "' >/dev/null 2>&1 || true"
}

// claudeHooks adds catchpen's Notification and Stop hooks to the guest's
// settings.json, keeping any others. Unless sync.claude_ai is on, it also
// turns off the skill and plugin sync from claude.ai.
func (p *pusher) claudeHooks(s Spec) error {
	doc, err := p.getJSON(claudeSettings)
	if err != nil {
		return err
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	cmd := curlPost("@-", "?agent=claude")
	for _, ev := range []string{"Notification", "Stop"} {
		list, _ := hooks[ev].([]any)
		var keep []any
		for _, h := range list {
			if b, _ := json.Marshal(h); !strings.Contains(string(b), notifyURL) {
				keep = append(keep, h)
			}
		}
		hooks[ev] = append(keep, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}})
	}
	switch {
	case s.Sync.ClaudeAISync() && s.Sync.Claude():
		// The synced host settings.json says what the user wants.
	case s.Sync.ClaudeAISync():
		delete(doc, "syncClaudeAiSkills") // Claude's own default applies
		delete(doc, "syncClaudeAiPlugins")
	default:
		doc["syncClaudeAiSkills"], doc["syncClaudeAiPlugins"] = false, false
	}
	doc["hooks"] = hooks
	return p.putJSON(claudeSettings, doc)
}
