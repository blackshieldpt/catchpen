// Package jsonl appends to and tails size-rotated JSON-lines logs shared by
// several processes.
package jsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

// Append writes v as one JSON line. Writers serialize on a lock file next to
// path; past max bytes the file is rotated to path.1.
func Append(path string, v any, max int64) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep lines compact; & < > are plain bytes here
	if err := enc.Encode(v); err != nil {
		return err
	}
	unlock, err := fsutil.Lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if st, err := os.Stat(path); err == nil && st.Size() > max {
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

// Tail calls fn for each non-empty line; with follow it keeps reading new
// lines (across rotations) until ctx is done.
func Tail(ctx context.Context, path string, follow bool, fn func(line string) error) error {
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
				if err := fn(line); err != nil {
					return err
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
				return fn(line)
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
