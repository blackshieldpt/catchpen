package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
)

// Store is the on-disk cache: <Dir>/<kind>/<xx>/<sha256 of the key>, with
// a .meta file beside each object and a .lock file while one is fetching it.
// An object exists only once complete (written to a temp file, renamed).
type Store struct {
	Dir    string
	statfs func(dir string) (avail, total int64, err error) // nil: statfs
}

type meta struct {
	URL          string `json:"url"`
	Type         string `json:"type,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

const (
	metaSfx = ".meta"
	lockSfx = ".lock"
	tmpPfx  = ".tmp-"
	tmpAge  = time.Hour // older temp files are leftovers of killed fetches

	trimEvery = 10 * time.Minute // how often fetches trim the store to its limit
	minFree   = 5                // percent of the filesystem storing never fills
)

func (st Store) path(kind, key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(st.Dir, kind, h[:2], h)
}

// lock takes the lock for fetching the object at path. Release removes the
// lock file first: one waiting on it then finds the object, or fetches it
// too if the fetch failed (as may one starting meanwhile). Both are harmless.
func (st Store) lock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	unlock, err := fsutil.Lock(path + lockSfx)
	if err != nil {
		return nil, err
	}
	return func() {
		_ = os.Remove(path + lockSfx)
		unlock()
	}, nil
}

// room reports whether an object of size bytes fits, leaving minFree percent
// of the filesystem free (the limit is enforced only every trimEvery).
func (st Store) room(size int64) bool {
	f := st.statfs
	if f == nil {
		f = statfs
	}
	avail, total, err := f(st.Dir)
	if err != nil {
		return false
	}
	return avail-size > total*minFree/100
}

// statfs reports the bytes available to us, and in all, on dir's filesystem.
func statfs(dir string) (avail, total int64, err error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, 0, err
	}
	bs := int64(fs.Bsize)
	return int64(fs.Bavail) * bs, int64(fs.Blocks) * bs, nil
}

// trimSometimes trims the store to max, at most every trimEvery across
// the processes sharing it.
func (st Store) trimSometimes(max int64) {
	stamp := filepath.Join(st.Dir, ".trimmed")
	if info, err := os.Stat(stamp); max <= 0 || err == nil && time.Since(info.ModTime()) < trimEvery {
		return
	}
	unlock, ok, err := fsutil.TryLock(stamp + lockSfx)
	if err != nil || !ok {
		return
	}
	defer unlock()
	if err := os.WriteFile(stamp, nil, 0o600); err != nil {
		return
	}
	_, _, _ = st.Trim(max, false)
}

func readMeta(path string) meta {
	var m meta
	if b, err := os.ReadFile(path + metaSfx); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// commit moves a complete, synced temp file into place.
func commit(tmp, path string, m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(path+metaSfx, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Usage is what one kind holds.
type Usage struct {
	Kind  string
	Files int
	Bytes int64
}

type object struct {
	path  string
	bytes int64
	used  time.Time // a hit touches the object
}

// objects lists the complete objects of kind, removing (unless dryRun)
// temp files of fetches that died.
func (st Store) objects(kind string, dryRun bool) ([]object, error) {
	var out []object
	err := filepath.WalkDir(filepath.Join(st.Dir, kind), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		n := d.Name()
		if d.IsDir() || strings.HasSuffix(n, metaSfx) || strings.HasSuffix(n, lockSfx) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if strings.HasPrefix(n, tmpPfx) {
			if !dryRun && time.Since(info.ModTime()) > tmpAge {
				_ = os.Remove(p)
			}
			return nil
		}
		out = append(out, object{p, info.Size(), info.ModTime()})
		return nil
	})
	return out, err
}

// Usage reports each kind's objects.
func (st Store) Usage() ([]Usage, error) {
	var out []Usage
	for _, k := range Kinds {
		objs, err := st.objects(k, true)
		if err != nil {
			return nil, err
		}
		u := Usage{Kind: k, Files: len(objs)}
		for _, o := range objs {
			u.Bytes += o.bytes
		}
		out = append(out, u)
	}
	return out, nil
}

// Trim removes the least recently used objects until the store holds at
// most max bytes, and returns what it removed (or would, with dryRun).
func (st Store) Trim(max int64, dryRun bool) (files int, bytes int64, err error) {
	var all []object
	var total int64
	for _, k := range Kinds {
		objs, err := st.objects(k, dryRun)
		if err != nil {
			return 0, 0, err
		}
		for _, o := range objs {
			total += o.bytes
		}
		all = append(all, objs...)
	}
	slices.SortFunc(all, func(a, b object) int { return a.used.Compare(b.used) })
	for _, o := range all {
		if total <= max {
			break
		}
		if !dryRun {
			if err := os.Remove(o.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return files, bytes, err
			}
			_ = os.Remove(o.path + metaSfx)
		}
		total -= o.bytes
		files++
		bytes += o.bytes
	}
	return files, bytes, nil
}

// Clear removes everything the given kinds (all, if none) hold.
func (st Store) Clear(kinds ...string) error {
	if len(kinds) == 0 {
		kinds = Kinds
	}
	for _, k := range kinds {
		if err := Valid(k); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(st.Dir, k)); err != nil {
			return err
		}
	}
	return nil
}
