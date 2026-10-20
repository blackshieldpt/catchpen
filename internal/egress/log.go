package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/jsonl"
)

const (
	ActionAllow       = "allow"
	ActionDeny        = "deny"
	ActionPromptAllow = "prompt-allow"
	ActionPromptDeny  = "prompt-deny"
	ActionError       = "error"
	ActionForward     = "forward" // host service exposed to the guest with curral forward -R
)

// Append writes an entry to a VM's egress log.
func Append(dir string, e LogEntry) error { return appendLog(dir, e) }

const maxLogBytes = 32 << 20

// LogEntry is one line of egress.log.
type LogEntry struct {
	Time   time.Time `json:"time"`
	VM     string    `json:"vm"`
	Method string    `json:"method"`
	Host   string    `json:"host"`
	Port   int       `json:"port"`
	Action string    `json:"action"`
	Reason string    `json:"reason,omitempty"`
}

// appendLog writes one JSON line; writers across proxy processes serialize.
func appendLog(dir string, e LogEntry) error {
	return jsonl.Append(filepath.Join(dir, LogFile), e, maxLogBytes)
}

func formatEntry(line string) string {
	var e LogEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return line
	}
	s := fmt.Sprintf("%s %-5s %s  %s %s  %s", e.Time.UTC().Format(time.RFC3339),
		strings.ToUpper(e.Action), e.VM, e.Method, net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), e.Reason)
	return strings.TrimRight(s, " ")
}

// TailLog prints the log in human-readable form; with follow it keeps
// printing new lines (across rotations) until ctx is done.
func TailLog(ctx context.Context, path string, follow bool, w io.Writer) error {
	return jsonl.Tail(ctx, path, follow, func(line string) error {
		_, err := fmt.Fprintln(w, formatEntry(line))
		return err
	})
}
