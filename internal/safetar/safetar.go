// Package safetar unpacks an untrusted tar stream (from a guest) into a host
// directory: only regular files and directories, all inside the
// destination, with size and count limits. Modes are not kept.
package safetar

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bound what one archive may write.
type Limits struct {
	Bytes int64 // total file content
	Files int   // files and directories
}

// Result says what was written and skipped.
type Result struct {
	Files    int
	Bytes    int64
	Skipped  []string // first entries that weren't regular files or directories, or had unsafe names
	NSkipped int      // how many were skipped in all
}

const maxSkippedNames = 20

func (r *Result) skip(name string) {
	r.NSkipped++
	if len(r.Skipped) < maxSkippedNames {
		if len(name) > 200 {
			name = name[:200] + "…"
		}
		r.Skipped = append(r.Skipped, printable(name))
	}
}

// Extract unpacks r into dest, which must exist.
func Extract(r io.Reader, dest string, lim Limits) (Result, error) {
	var res Result
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, err
		}
		name, ok := clean(h.Name)
		if !ok {
			res.skip(h.Name)
			continue
		}
		if res.Files++; res.Files > lim.Files {
			return res, fmt.Errorf("more than %d entries", lim.Files)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := mkdirs(dest, name); err != nil {
				return res, err
			}
		case tar.TypeReg:
			if res.Bytes+h.Size > lim.Bytes {
				return res, fmt.Errorf("more than %d bytes", lim.Bytes)
			}
			if err := mkdirs(dest, path.Dir(name)); err != nil {
				return res, err
			}
			// O_EXCL and O_NOFOLLOW: a later entry can't overwrite or follow anything.
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscallNoFollow, 0o600)
			if err != nil {
				res.skip(h.Name)
				continue
			}
			n, err := io.Copy(f, io.LimitReader(tr, h.Size))
			res.Bytes += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return res, err
			}
		default:
			res.Files--
			res.skip(h.Name)
		}
	}
}

// clean returns a safe relative slash path, or false.
func clean(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", false
	}
	c := path.Clean(name)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	for _, part := range strings.Split(c, "/") {
		if part == ".." {
			return "", false
		}
	}
	return c, true
}

// mkdirs creates dest/rel one level at a time, refusing anything that
// already exists and isn't a plain directory (so no symlink is followed).
func mkdirs(dest, rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	cur := dest
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		switch {
		case err == nil && st.IsDir():
		case err == nil:
			return fmt.Errorf("%s exists and is not a directory", printable(rel))
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o700); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}
