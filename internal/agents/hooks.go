package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blackshieldpt/curral/internal/cloudinit"
)

// Agent hooks tell the host (curral notify-stdio, behind a guestfwd) when an
// agent is waiting for input. They carry no credentials; the host decides
// whether and where to notify.

var notifyURL = fmt.Sprintf("http://%s:%d/notify", cloudinit.NotifyAddr, cloudinit.NotifyPort)

const (
	claudeSettings = ".claude/settings.json"
	opencodePlugin = ".config/opencode/plugin/curral-notify.js"
	piExtension    = ".pi/agent/extensions/curral-notify.ts"
	ompExtension   = ".omp/agent/extensions/curral-notify.ts"
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
		if err := p.claudeHooks(); err != nil {
			return fmt.Errorf("claude hooks: %w", err)
		}
	}
	if s.Agents.OpenCode != nil {
		js := `// written by curral at each start; do not edit
import http from "node:http";
export const CurralNotify = async () => ({
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
	if s.Agents.Pi != nil {
		ts := `// written by curral at each start; do not edit
import http from "node:http";
export default function (pi: any) {
	pi.on("agent_settled", async () => {
	` + postJS("pi", "agent_settled") + `
	});
}
`
		if err := p.put(piExtension, []byte(ts)); err != nil {
			return fmt.Errorf("pi extension: %w", err)
		}
	}
	if s.Agents.OMP != nil {
		// omp has no agent_settled; agent_end ends each agent loop.
		ts := `// written by curral at each start; do not edit
import http from "node:http";
export default function (pi: any) {
	pi.on("agent_end", async () => {
	` + postJS("omp", "agent_end") + `
	});
}
`
		if err := p.put(ompExtension, []byte(ts)); err != nil {
			return fmt.Errorf("omp extension: %w", err)
		}
	}
	return nil
}

// claudeHooks adds curral's Notification and Stop hooks to the guest's
// settings.json, keeping any others.
func (p *pusher) claudeHooks() error {
	doc, err := p.getJSON(claudeSettings)
	if err != nil {
		return err
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	cmd := "curl -s -m 5 --noproxy '*' -X POST -H 'Content-Type: application/json' --data-binary @- '" +
		notifyURL + "?agent=claude' >/dev/null 2>&1 || true"
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
	doc["hooks"] = hooks
	return p.putJSON(claudeSettings, doc)
}
