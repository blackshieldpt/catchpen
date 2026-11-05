// Package creds resolves secrets on the host. A secret NAME comes from, in order:
// a configured source (config.yaml `secrets:`), the environment variable NAME,
// or ~/.config/catchpen/secrets.env.
package creds

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Source is a configured way to obtain one secret.
type Source struct {
	Command []string `yaml:"command,omitempty"` // stdout, trailing newline trimmed
	Env     string   `yaml:"env,omitempty"`     // a differently named host env var
}

type Resolver struct {
	Sources map[string]Source
	File    string              // secrets.env path
	Getenv  func(string) string // os.Getenv when nil
	file    map[string]string   // parsed lazily
	cache   map[string]string
}

const commandTimeout = 30 * time.Second

// Lookup returns the secret's value; ok is false when no source has it.
func (r *Resolver) Lookup(ctx context.Context, name string) (string, bool, error) {
	if v, ok := r.cache[name]; ok {
		return v, true, nil
	}
	v, ok, err := r.lookup(ctx, name)
	if err != nil || !ok {
		return "", ok, err
	}
	if strings.ContainsAny(v, "\n\r\x00") {
		return "", false, fmt.Errorf("secret %s contains a newline or NUL", name)
	}
	if r.cache == nil {
		r.cache = map[string]string{}
	}
	r.cache[name] = v
	return v, true, nil
}

func (r *Resolver) lookup(ctx context.Context, name string) (string, bool, error) {
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if s, ok := r.Sources[name]; ok {
		switch {
		case len(s.Command) > 0:
			ctx, cancel := context.WithTimeout(ctx, commandTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
			cmd.Stdin = nil
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				return "", false, fmt.Errorf("secret %s: %s: %w", name, s.Command[0], err)
			}
			v := strings.TrimRight(string(out), "\r\n")
			return v, v != "", nil
		case s.Env != "":
			v := getenv(s.Env)
			return v, v != "", nil
		}
	}
	if v := getenv(name); v != "" {
		return v, true, nil
	}
	if r.file == nil {
		f, err := ParseFile(r.File)
		if err != nil {
			return "", false, err
		}
		r.file = f
	}
	v, ok := r.file[name]
	return v, ok && v != "", nil
}

// ParseFile reads a secrets.env file: NAME=value lines, optional `export `,
// optional single or double quotes, # comments. It must not be readable by
// group or others. A missing file is empty.
func ParseFile(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users (mode %o); run: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("%s:%d: expected NAME=value", path, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// NameRe is what a secret's name must match to be listed, set or unset over
// the API: letters, digits, _, . and -, not starting with a digit, . or -.
var NameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

// MaxValue is the longest value Set takes: ParseFile reads lines of at most
// 64 KiB (bufio.Scanner's limit), and a longer one would break the file.
const MaxValue = 32 << 10

// CheckName reports why name can't be a secret's name over the API.
func CheckName(name string) error {
	if !NameRe.MatchString(name) {
		return errors.New("a secret's name is letters, digits, _, . and -, not starting with a digit, . or -, up to 128")
	}
	return nil
}

// CheckValue reports why v can't be a secret's value in secrets.env.
func CheckValue(v string) error {
	switch {
	case v == "":
		return errors.New("the value is empty: unset the secret instead")
	case strings.ContainsAny(v, "\n\r\x00"):
		return errors.New("the value contains a newline, CR or NUL")
	case len(v) > MaxValue:
		return fmt.Errorf("the value is longer than %d bytes", MaxValue)
	}
	return nil
}

// parsed is the name and value a secrets.env line sets, as ParseFile reads
// them; ok is false for a blank line or a comment.
func parsed(line string) (name, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	k, v, _ := strings.Cut(strings.TrimPrefix(line, "export "), "=")
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return strings.TrimSpace(k), v, true
}

// Set returns b, a secrets.env file ParseFile reads, with name set to v, and
// whether a line it replaced or removed held a value. The last line setting
// name is replaced, keeping its `export `, and the others setting it are
// removed; with none, the first `# NAME=` line (as catchpen init writes
// them) is replaced, or the line is appended. Every other line is kept as it
// is. A name CheckName refuses or a value CheckValue refuses is an error.
func Set(b []byte, name, v string) ([]byte, bool, error) {
	if err := CheckName(name); err != nil {
		return nil, false, err
	}
	if err := CheckValue(v); err != nil {
		return nil, false, err
	}
	if v != strings.TrimSpace(v) || len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = `"` + v + `"` // ParseFile strips one pair of matching quotes
	}
	lines := strings.SplitAfter(string(b), "\n")
	at, replaced := -1, false
	for i, l := range lines {
		if k, old, ok := parsed(l); ok && k == name {
			at, replaced = i, replaced || old != ""
		}
	}
	if at < 0 {
		for i, l := range lines {
			if c, ok := strings.CutPrefix(strings.TrimSpace(l), "#"); ok && strings.TrimSpace(c) == name+"=" {
				at = i
				break
			}
		}
	}
	line := name + "=" + v + "\n"
	if at < 0 {
		if n := len(b); n > 0 && b[n-1] != '\n' {
			line = "\n" + line
		}
		return append(b[:len(b):len(b)], line...), false, nil
	}
	if strings.HasPrefix(strings.TrimSpace(lines[at]), "export ") {
		line = "export " + line
	}
	var out strings.Builder
	for i, l := range lines {
		k, _, ok := parsed(l)
		switch {
		case i == at:
			out.WriteString(line)
		case !ok || k != name:
			out.WriteString(l)
		}
	}
	return []byte(out.String()), replaced, nil
}

// Unset returns b, a secrets.env file ParseFile reads, without the lines
// that set name, and whether there was one.
func Unset(b []byte, name string) ([]byte, bool) {
	var out strings.Builder
	found := false
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if k, _, ok := parsed(l); ok && k == name {
			found = true
			continue
		}
		out.WriteString(l)
	}
	return []byte(out.String()), found
}
