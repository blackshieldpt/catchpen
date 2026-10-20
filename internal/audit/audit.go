// Package audit keeps the host-side audit log: one JSON line per event the
// guest cannot forge or erase (lifecycle, credentials, keys, egress decisions,
// forwards, exports, sessions).
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/jsonl"
	"github.com/blackshieldpt/curral/internal/sshx"
)

const maxBytes = 32 << 20

const (
	VMCreate        = "vm.create"
	VMStart         = "vm.start"
	VMStop          = "vm.stop"
	VMDestroy       = "vm.destroy"
	CredsPush       = "creds.push"
	DeployKeyAdd    = "forge.deploy_key.add"
	DeployKeyRemove = "forge.deploy_key.remove"
	TokenMint       = "forge.token.mint"
	TokenDelete     = "forge.token.delete"
	AgentStart      = "ssh_agent.start"
	EgressAllow     = "egress.allow"
	EgressDeny      = "egress.deny"
	EgressPrompt    = "egress.prompt"
	ForwardOpen     = "forward.open"
	ForwardStop     = "forward.stop"
	ExportPull      = "export.pull"
	ExportPR        = "export.pr"
	SessionStart    = "session.start"
	SessionEnd      = "session.end"
	Copy            = "cp"
)

// Event is one audit line.
type Event struct {
	Time   time.Time      `json:"time"`
	Event  string         `json:"event"`
	VM     string         `json:"vm,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}

// Log is the audit log at Path.
type Log struct{ Path string }

// Record appends an event. A zero Log records nothing.
func (l Log) Record(event, vm string, detail map[string]any) error {
	if l.Path == "" {
		return nil
	}
	return jsonl.Append(l.Path, Event{Time: time.Now().UTC(), Event: event, VM: vm, Detail: detail}, maxBytes)
}

// Tail prints events (of one VM if vm is set) in human-readable form.
func (l Log) Tail(ctx context.Context, vm string, follow bool, w io.Writer) error {
	return jsonl.Tail(ctx, l.Path, follow, func(line string) error {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil
		}
		if vm != "" && e.VM != vm {
			return nil
		}
		_, err := fmt.Fprintln(w, Format(e))
		return err
	})
}

var oneLine = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`)

// Format renders an event on one line; values that may come from the guest
// are made printable.
func Format(e Event) string {
	keys := make([]string, 0, len(e.Detail))
	for k := range e.Detail {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-24s %s", e.Time.UTC().Format(time.RFC3339), oneLine.Replace(sshx.Printable(e.Event)), oneLine.Replace(sshx.Printable(e.VM)))
	for _, k := range keys {
		v := e.Detail[k]
		var s string
		switch x := v.(type) {
		case []any:
			parts := make([]string, len(x))
			for i, p := range x {
				parts[i] = fmt.Sprint(p)
			}
			s = strings.Join(parts, ",")
		default:
			s = fmt.Sprint(x)
		}
		fmt.Fprintf(&b, " %s=%s", k, oneLine.Replace(sshx.Printable(s)))
	}
	return strings.TrimRight(b.String(), " ")
}
