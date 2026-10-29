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

	"github.com/blackshieldpt/catchpen/internal/fsutil"
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

// maxLine bounds the lines Tail passes on: a line of maxLine bytes or more,
// counting its line ending, is skipped.
const maxLine = 16 << 20

// ErrWriterDone, as the cause of Tail's context (context.WithCancelCause),
// says the file's writer has finished: Tail then passes on a last line that
// has no newline. Any other end of ctx leaves such a line out, since a writer
// may still be in the middle of it.
var ErrWriterDone = errors.New("writer done")

const poll = 250 * time.Millisecond

// Tail calls fn for each non-empty line; with follow it keeps reading new
// lines (across rotations) until ctx is done, and passes on a last line
// without a newline only if ctx's cause is ErrWriterDone. Lines over maxLine
// are skipped without being held in memory.
func Tail(ctx context.Context, path string, follow bool, fn func(line string) error) error {
	f, err := openWhenExists(ctx, path, follow, poll)
	if f == nil || err != nil {
		return err
	}
	return tail(ctx, f, path, follow, false, fn, nil)
}

// TailRotated is Tail for a log only Append writes: it first passes on the
// lines of path.1, the file the last rotation moved aside. Use Tail for a
// file that never rotates. Following relies on every writer using Append.
// A file Append rotated out is final, so its last line is passed on even
// without a newline; ErrWriterDone applies to path as in Tail.
func TailRotated(ctx context.Context, path string, follow bool, fn func(line string) error) error {
	return tailRotated(ctx, path, follow, fn, nil)
}

func tailRotated(ctx context.Context, path string, follow bool, fn func(line string) error, onEOF func()) error {
	f, old, err := openPair(path)
	if err != nil {
		return err
	}
	if old != nil {
		err := tail(ctx, old, path+".1", false, false, fn, nil)
		// A cancel while path.1 was read stops here, unless the writer is
		// done: then path is read to its end as well.
		if err == nil && ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrWriterDone) {
			if f != nil {
				f.Close()
			}
			return nil
		}
		if err != nil {
			if f != nil {
				f.Close()
			}
			return err
		}
	}
	if f == nil {
		if f, err = openWhenExists(ctx, path, follow, poll); f == nil || err != nil {
			return err
		}
	}
	return tail(ctx, f, path, follow, true, fn, onEOF)
}

// openPair opens path and path.1 (nil when missing) under the writers' lock,
// so no rotation falls between the two.
func openPair(path string) (cur, old *os.File, err error) {
	unlock, err := fsutil.RLock(path + ".lock")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) { // no directory: no log yet
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer unlock()
	if cur, err = os.Open(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	if old, err = os.Open(path + ".1"); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			if cur != nil {
				cur.Close()
			}
			return nil, nil, err
		}
		old = nil
	}
	return cur, old, nil
}

// tail reads f, opened from path, as Tail does, and closes it. rotating says
// only Append writes path, so a rotated-out file is final and path.1 holds
// it. onEOF, if set, runs each time a follow reaches the end of the file
// (tests rotate there).
func tail(ctx context.Context, f *os.File, path string, follow, rotating bool, fn func(line string) error, onEOF func()) error {
	defer func() { f.Close() }()
	r := bufio.NewReader(f)
	var partial []byte
	skip := false          // inside a line over maxLine
	draining := false      // path was rotated: reading the old file to its end
	emit := func() error { // a skipped line left partial empty
		line := strings.TrimSpace(string(partial))
		partial, skip = partial[:0], false
		if line != "" {
			return fn(line)
		}
		return nil
	}
	for {
		chunk, err := r.ReadSlice('\n')
		if !skip {
			if len(partial)+len(chunk) > maxLine {
				partial, skip = partial[:0], true
			} else {
				partial = append(partial, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == nil {
			if err := emit(); err != nil {
				return err
			}
			continue
		}
		if err != io.EOF {
			return err
		}
		if !follow {
			return emit()
		}
		if onEOF != nil {
			onEOF()
		}
		switch {
		case !rotated(f, path):
		case !rotating:
			// Old file fully read: continue with the new one from its start.
			nf, err := os.Open(path)
			if err == nil {
				f.Close()
				f, r, partial, skip = nf, bufio.NewReader(nf), partial[:0], false
				continue
			}
		case !draining:
			// Append never writes the old file once it renames it, but may
			// have appended to it after the read above: read to its end.
			draining = true
			continue
		default:
			nf, old, err := openPair(path)
			if err != nil {
				return err // as a read without follow would
			}
			if nf != nil {
				if err := emit(); err != nil { // the old file is final
					nf.Close()
					if old != nil {
						old.Close()
					}
					return err
				}
				// path.1 is not the file just read: a second rotation came
				// first, and path.1 holds the lines in between.
				if old != nil && !sameFile(old, f) {
					if err := tail(ctx, old, path+".1", false, false, fn, nil); err != nil {
						nf.Close()
						return err
					}
				} else if old != nil {
					old.Close()
				}
				f.Close()
				f, r, skip, draining = nf, bufio.NewReader(nf), false, false
				continue
			}
			if old != nil {
				old.Close()
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), ErrWriterDone) {
				return emit()
			}
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

// sameFile reports whether a and b are the same file.
func sameFile(a, b *os.File) bool {
	x, err := a.Stat()
	if err != nil {
		return false
	}
	y, err := b.Stat()
	return err == nil && os.SameFile(x, y)
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
