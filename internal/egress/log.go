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

	"github.com/blackshieldpt/catchpen/internal/jsonl"
	"github.com/blackshieldpt/catchpen/internal/sshx"
)

const (
	ActionAllow       = "allow"
	ActionDeny        = "deny"
	ActionPromptAllow = "prompt-allow"
	ActionPromptDeny  = "prompt-deny"
	ActionError       = "error"
	ActionForward     = "forward" // host service exposed to the guest with catchpen forward -R
	ActionCache       = "cache"   // fetched upstream by a package cache
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

func formatEntry(e LogEntry) string {
	// Hosts and reasons can come from upstreams or the guest: one line each.
	p := func(s string) string { return strings.ReplaceAll(sshx.Printable(s), "\n", " ") }
	s := fmt.Sprintf("%s %-5s %s  %s %s  %s", e.Time.UTC().Format(time.RFC3339),
		strings.ToUpper(p(e.Action)), p(e.VM), p(e.Method), net.JoinHostPort(p(e.Host), strconv.Itoa(e.Port)), p(e.Reason))
	return strings.TrimRight(s, " ")
}

// TailEntries calls fn with each entry of the log, egress.log.1 first,
// skipping lines that don't decode as JSON. With follow it keeps going, across
// rotations, until ctx is done. Hosts and reasons can come from upstreams or
// the guest: TailLog makes them printable.
func TailEntries(ctx context.Context, path string, follow bool, fn func(LogEntry) error) error {
	return tailLog(ctx, path, follow, func(e LogEntry, _ string, ok bool) error {
		if !ok {
			return nil
		}
		return fn(e)
	})
}

// TailLog prints the log in human-readable form, egress.log.1 first; with
// follow it keeps printing new lines (across rotations) until ctx is done. A
// line that doesn't decode is printed as it is, made printable.
func TailLog(ctx context.Context, path string, follow bool, w io.Writer) error {
	return tailLog(ctx, path, follow, func(e LogEntry, line string, ok bool) error {
		var s string
		if ok {
			s = formatEntry(e)
		} else {
			s = sshx.Printable(line)
		}
		_, err := fmt.Fprintln(w, s)
		return err
	})
}

// tailLog calls fn with each line of the log, decoded (ok) or not.
func tailLog(ctx context.Context, path string, follow bool, fn func(e LogEntry, line string, ok bool) error) error {
	return jsonl.TailRotated(ctx, path, follow, func(line string) error {
		var e LogEntry
		err := json.Unmarshal([]byte(line), &e)
		return fn(e, line, err == nil)
	})
}
