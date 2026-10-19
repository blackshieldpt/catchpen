// Package creds resolves secrets on the host. A secret NAME comes from, in order:
// a configured source (config.yaml `secrets:`), the environment variable NAME,
// or ~/.config/curral/secrets.env.
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
