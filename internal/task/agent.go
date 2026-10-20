package task

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// GuestPrompt is where the prompt is written in the guest.
const GuestPrompt = ".curral/task-prompt.md"

// Command is the guest shell command that runs the agent headless, in the
// repo, with stdout as its event stream. The prompt goes on stdin where the
// agent reads it from there, so it is never parsed as options (or, for pi,
// as an @file reference).
func Command(s Spec) string {
	stdin := ` <"$HOME/` + GuestPrompt + `"`
	switch s.Agent {
	case "claude":
		c := "claude -p --output-format stream-json --verbose --dangerously-skip-permissions"
		if s.MaxBudgetUSD > 0 {
			c += " --max-budget-usd " + strconv.FormatFloat(s.MaxBudgetUSD, 'f', -1, 64)
		}
		return c + stdin
	case "opencode":
		return `opencode run --format json --auto -- "$(cat "$HOME/` + GuestPrompt + `")" </dev/null`
	case "pi":
		return "pi -p --mode json" + stdin
	}
	return `CURRAL_PROMPT_FILE="$HOME/` + GuestPrompt + `" sh -c ` + shQuote(s.Cmd) + stdin
}

// Summary is what a finished run reported.
type Summary struct {
	Result string // the agent's final message
	Usage  Usage
	Error  string // an error the agent reported in its stream
}

const maxResult = 64 << 10

// Summarize reads an agent's event stream.
func Summarize(agent string, r io.Reader) Summary {
	var s Summary
	if agent == "cmd" {
		b, _ := io.ReadAll(r)
		if len(b) > maxResult {
			b = b[len(b)-maxResult:]
		}
		s.Result = string(b)
		return s
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch agent {
		case "claude":
			if str(ev, "type") != "result" {
				continue
			}
			s.Result = str(ev, "result")
			s.Usage.Turns = int(num(ev, "num_turns"))
			s.Usage.CostUSD = num(ev, "total_cost_usd")
			if u, ok := ev["usage"].(map[string]any); ok {
				s.Usage.InputTokens = int64(num(u, "input_tokens") + num(u, "cache_read_input_tokens") + num(u, "cache_creation_input_tokens"))
				s.Usage.OutputTokens = int64(num(u, "output_tokens"))
			}
			s.Error = "" // the last result decides
			if b, _ := ev["is_error"].(bool); b {
				s.Error = "claude: " + str(ev, "subtype")
			}
		case "opencode":
			part, _ := ev["part"].(map[string]any)
			switch str(ev, "type") {
			case "text":
				if t := strings.TrimSpace(str(part, "text")); t != "" {
					s.Result = t
				}
			case "step_finish":
				s.Error = "" // a step after an error: it recovered
				s.Usage.Turns++
				s.Usage.CostUSD += num(part, "cost")
				if tk, ok := part["tokens"].(map[string]any); ok {
					s.Usage.InputTokens += int64(num(tk, "input"))
					s.Usage.OutputTokens += int64(num(tk, "output"))
				}
			case "error":
				e, _ := ev["error"].(map[string]any)
				d, _ := e["data"].(map[string]any)
				s.Error = "opencode: " + firstNonEmpty(str(d, "message"), str(e, "name"))
			}
		case "pi":
			if str(ev, "type") != "message_end" {
				continue
			}
			m, _ := ev["message"].(map[string]any)
			if str(m, "role") != "assistant" {
				continue
			}
			s.Usage.Turns++
			if t := strings.TrimSpace(textContent(m["content"])); t != "" {
				s.Result = t
			}
			if u, ok := m["usage"].(map[string]any); ok {
				s.Usage.InputTokens += int64(num(u, "input") + num(u, "cacheRead") + num(u, "cacheWrite"))
				s.Usage.OutputTokens += int64(num(u, "output"))
				if c, ok := u["cost"].(map[string]any); ok {
					s.Usage.CostUSD += num(c, "total")
				}
			}
			s.Error = ""
			if str(m, "stopReason") == "error" {
				s.Error = "pi: " + firstNonEmpty(str(m, "errorMessage"), "error")
			}
		}
	}
	if err := sc.Err(); err != nil {
		s.Error = "reading the agent's output: " + err.Error()
	}
	return s
}

// Render turns one line of an agent's stream into display text ("" to skip).
func Render(agent string, line []byte) string {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return ""
	}
	if agent == "cmd" {
		return string(line)
	}
	var ev map[string]any
	if json.Unmarshal(line, &ev) != nil {
		return string(line)
	}
	switch agent {
	case "claude":
		switch str(ev, "type") {
		case "assistant":
			m, _ := ev["message"].(map[string]any)
			var out []string
			items, _ := m["content"].([]any)
			for _, it := range items {
				c, _ := it.(map[string]any)
				switch str(c, "type") {
				case "text":
					out = append(out, str(c, "text"))
				case "tool_use":
					out = append(out, "→ "+str(c, "name")+" "+short(c["input"]))
				}
			}
			return strings.Join(out, "\n")
		case "result":
			return fmt.Sprintf("== %s: %d turns, $%.4f", str(ev, "subtype"), int(num(ev, "num_turns")), num(ev, "total_cost_usd"))
		}
	case "opencode":
		part, _ := ev["part"].(map[string]any)
		switch str(ev, "type") {
		case "text":
			return str(part, "text")
		case "tool_use":
			st, _ := part["state"].(map[string]any)
			return "→ " + str(part, "tool") + " " + short(st["input"])
		case "error":
			b, _ := json.Marshal(ev["error"])
			return "error: " + string(b)
		}
	case "pi":
		switch str(ev, "type") {
		case "message_end":
			m, _ := ev["message"].(map[string]any)
			if str(m, "role") == "assistant" {
				return textContent(m["content"])
			}
		case "tool_execution_start":
			return "→ " + str(ev, "toolName") + " " + short(ev["args"])
		}
	}
	return ""
}

func textContent(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	items, _ := v.([]any)
	var out []string
	for _, it := range items {
		c, _ := it.(map[string]any)
		if str(c, "type") == "text" {
			out = append(out, str(c, "text"))
		}
	}
	return strings.Join(out, "\n")
}

func short(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	if len(b) > 120 {
		return string(b[:117]) + "..."
	}
	return string(b)
}

func str(m map[string]any, k string) string  { s, _ := m[k].(string); return s }
func num(m map[string]any, k string) float64 { f, _ := m[k].(float64); return f }

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
