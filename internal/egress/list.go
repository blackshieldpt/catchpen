// Package egress implements catchpen's per-VM egress policy: allow/deny lists,
// the per-connection HTTP proxy QEMU spawns via guestfwd, the prompt watcher
// and the egress log.
package egress

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
)

const (
	AllowlistFile = "allowlist"
	DenylistFile  = "denylist"
	ConfigFile    = "egress.json"
	LogFile       = "egress.log"
	SocketFile    = "egress.sock"
)

// Rule is one parsed list entry.
type Rule struct {
	host     string // normalized; for wildcards the suffix without "*."
	wildcard bool
	port     int // 0 = 80 and 443
}

// String returns the canonical entry form.
func (r Rule) String() string {
	h := r.host
	if r.wildcard {
		h = "*." + h
	}
	if r.port == 0 {
		return h
	}
	return net.JoinHostPort(h, strconv.Itoa(r.port))
}

func normalizeHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

// ParseEntry parses a single list entry.
func ParseEntry(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, errors.New("empty entry")
	}
	host, portStr := s, ""
	switch {
	case net.ParseIP(s) != nil: // bare IP, including unbracketed v6
	case strings.HasPrefix(s, "["):
		if strings.HasSuffix(s, "]") {
			host = s[1 : len(s)-1]
		} else {
			h, p, err := net.SplitHostPort(s)
			if err != nil {
				return Rule{}, fmt.Errorf("invalid entry %q: %w", s, err)
			}
			host, portStr = h, p
		}
		if net.ParseIP(host) == nil {
			return Rule{}, fmt.Errorf("invalid IPv6 address in %q", s)
		}
	case strings.Contains(s, ":"):
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return Rule{}, fmt.Errorf("invalid entry %q: %w", s, err)
		}
		host, portStr = h, p
	}
	var r Rule
	if portStr != "" {
		p, err := parsePort(portStr)
		if err != nil {
			return Rule{}, fmt.Errorf("invalid entry %q: %w", s, err)
		}
		r.port = p
	}
	host = normalizeHost(host)
	if net.ParseIP(host) != nil {
		r.host = host
		return r, nil
	}
	if strings.HasPrefix(host, "*.") {
		r.wildcard = true
		host = host[2:]
	}
	if !validHostname(host) {
		return Rule{}, fmt.Errorf("invalid host in entry %q", s)
	}
	r.host = host
	return r, nil
}

// ParseList parses a list file. '#' starts a comment; blank lines are ignored.
func ParseList(r io.Reader) ([]Rule, error) {
	var rules []Rule
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if strings.TrimSpace(line) == "" {
			continue
		}
		rule, err := ParseEntry(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		rules = append(rules, rule)
	}
	return rules, sc.Err()
}

// LoadList reads a list file; a missing file is an empty list.
func LoadList(path string) ([]Rule, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rules, err := ParseList(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rules, nil
}

// Match reports whether host:port matches any rule.
func Match(rules []Rule, host string, port int) bool {
	h := normalizeHost(host)
	for _, r := range rules {
		if r.port == 0 && port != 80 && port != 443 || r.port != 0 && r.port != port {
			continue
		}
		if r.wildcard && strings.HasSuffix(h, "."+r.host) || !r.wildcard && h == r.host {
			return true
		}
	}
	return false
}

// WriteList replaces dir/file with the given entries (validated, canonical form).
func WriteList(dir, file string, entries []string) error {
	var b strings.Builder
	for _, e := range entries {
		r, err := ParseEntry(e)
		if err != nil {
			return err
		}
		b.WriteString(r.String())
		b.WriteByte('\n')
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, file), []byte(b.String()), 0o600)
}

// AddEntry appends entry to dir/file unless an equivalent entry exists.
func AddEntry(dir, file, entry string) error {
	r, err := ParseEntry(entry)
	if err != nil {
		return err
	}
	return editList(dir, file, func(lines []string) []string {
		for _, l := range lines {
			if lr, ok := lineRule(l); ok && lr == r {
				return lines
			}
		}
		return append(lines, r.String())
	})
}

// RemoveEntry removes entries equivalent to entry from dir/file.
func RemoveEntry(dir, file, entry string) error {
	r, err := ParseEntry(entry)
	if err != nil {
		return err
	}
	return editList(dir, file, func(lines []string) []string {
		out := lines[:0]
		for _, l := range lines {
			if lr, ok := lineRule(l); ok && lr == r {
				continue
			}
			out = append(out, l)
		}
		return out
	})
}

func lineRule(line string) (Rule, bool) {
	s, _, _ := strings.Cut(line, "#")
	if strings.TrimSpace(s) == "" {
		return Rule{}, false
	}
	r, err := ParseEntry(s)
	return r, err == nil
}

// editList rewrites a list under an exclusive flock, preserving comments.
func editList(dir, file string, edit func([]string) []string) error {
	path := filepath.Join(dir, file)
	unlock, err := fsutil.Lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var lines []string
	if s := strings.TrimRight(string(data), "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	lines = edit(lines)
	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}
	return fsutil.WriteFileAtomic(path, []byte(out), 0o600)
}
