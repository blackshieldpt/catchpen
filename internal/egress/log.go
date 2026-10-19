package egress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

const (
	ActionAllow       = "allow"
	ActionDeny        = "deny"
	ActionPromptAllow = "prompt-allow"
	ActionPromptDeny  = "prompt-deny"
	ActionError       = "error"
)

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

// appendLog writes one JSON line. Writers across proxy processes serialize on a
// lock, so rotation (one previous generation is kept) can't race a write.
func appendLog(dir string, e LogEntry) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep lines compact; & < > are plain token bytes here
	if err := enc.Encode(e); err != nil {
		return err
	}
	path := filepath.Join(dir, LogFile)
	unlock, err := fsutil.Lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(buf.Bytes())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
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
	const poll = 250 * time.Millisecond
	f, err := openWhenExists(ctx, path, follow, poll)
	if f == nil || err != nil {
		return err
	}
	defer func() { f.Close() }()
	r := bufio.NewReader(f)
	var partial string
	for {
		chunk, err := r.ReadString('\n')
		partial += chunk
		if err == nil {
			if line := strings.TrimSpace(partial); line != "" {
				if _, werr := fmt.Fprintln(w, formatEntry(line)); werr != nil {
					return werr
				}
			}
			partial = ""
			continue
		}
		if err != io.EOF {
			return err
		}
		if !follow {
			if line := strings.TrimSpace(partial); line != "" {
				_, err := fmt.Fprintln(w, formatEntry(line))
				return err
			}
			return nil
		}
		if rotated(f, path) {
			// Old file fully read: continue with the new one from its start.
			nf, err := os.Open(path)
			if err == nil {
				f.Close()
				f, r, partial = nf, bufio.NewReader(nf), ""
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
	}
}

// openWhenExists opens path, waiting for it to appear in follow mode.
// It returns (nil, nil) when there is nothing to read.
func openWhenExists(ctx context.Context, path string, follow bool, poll time.Duration) (*os.File, error) {
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if !follow {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(poll):
		}
	}
}

// rotated reports whether path now names a different file than f.
func rotated(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && !os.SameFile(a, b)
}
