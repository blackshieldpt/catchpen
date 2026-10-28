package vm

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/cache"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
)

// GCItem is something gc removed (or would remove).
type GCItem struct {
	Path  string
	Bytes int64 // disk usage
	Why   string
}

// Leftovers of interrupted work are only removed once nothing can still be
// writing them.
const gcTempAge = 2 * buildTimeout

// GC removes bases no VM uses (keeping each template's current one), build
// leftovers, partial downloads and destroy leftovers, and trims the package
// caches to cache_max. Recordings, transcripts, tasks and the audit log are
// never touched.
func (m *Manager) GC(dryRun bool) ([]GCItem, error) {
	var items []GCItem
	add := func(p, why string) {
		items = append(items, GCItem{Path: p, Bytes: usage(p), Why: why})
		if !dryRun {
			_ = os.RemoveAll(p)
		}
	}
	old := func(p string) bool {
		st, err := os.Lstat(p)
		return err == nil && time.Since(st.ModTime()) > gcTempAge
	}

	ents, err := os.ReadDir(m.P.Bases())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	newest := map[string]int{}   // distro-hash -> newest generation
	current := map[string]bool{} // distro-hash a recorded -t still resolves to
	// With a broken template file nothing resolves; keep every base rather
	// than take them all for unused.
	_, libErr := m.Tpl.Library()
	if libErr != nil {
		m.logf("gc: keeping all bases until the templates load: %v", libErr)
	}
	for _, e := range ents {
		mt := baseNameRe.FindStringSubmatch(e.Name())
		if mt == nil {
			continue
		}
		key, gen := mt[1]+"-"+mt[2], baseFileGen(mt[3])
		newest[key] = max(newest[key], gen)
		if _, seen := current[key]; !seen {
			current[key] = libErr != nil || m.baseWanted(mt[1], mt[2])
		}
	}
	kept := map[string]bool{} // distro-hash with a base left
	var specs []string
	for _, e := range ents {
		p := filepath.Join(m.P.Bases(), e.Name())
		if mt := baseNameRe.FindStringSubmatch(e.Name()); mt != nil {
			key, gen := mt[1]+"-"+mt[2], baseFileGen(mt[3])
			if current[key] && gen == newest[key] {
				kept[key] = true
				continue
			}
			if sz := usage(p); !m.removeBase(p, dryRun) {
				kept[key] = true
			} else {
				items = append(items, GCItem{Path: p, Bytes: sz, Why: "unused base"})
				stem := strings.TrimSuffix(p, ".qcow2")
				for _, sfx := range []string{".build.log", ".console.log", ".egress.log"} {
					if _, err := os.Lstat(stem + sfx); err == nil {
						add(stem+sfx, "log of a removed base")
					}
				}
			}
			continue
		}
		if stem, ok := strings.CutSuffix(e.Name(), ".specs"); ok {
			specs = append(specs, stem)
			continue
		}
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".specs.") {
			if old(p) {
				add(p, "interrupted write of a base's list")
			}
			continue
		}
		for _, sfx := range []string{".building", ".seed.iso", ".egress", ".build.log", ".console.log", ".egress.log"} {
			stem, ok := strings.CutSuffix(e.Name(), sfx)
			if !ok {
				continue
			}
			if sfx != ".building" && sfx != ".seed.iso" && sfx != ".egress" {
				if _, err := os.Stat(filepath.Join(m.P.Bases(), stem+".qcow2")); err == nil || !old(p) {
					break // a base's log, or a build that may be running
				}
				add(p, "log of a failed build")
			} else if old(p) {
				add(p, "interrupted build")
			}
			break
		}
	}

	for _, key := range specs {
		if !kept[key] && libErr == nil {
			m.removeSpecs(key, dryRun, add)
		}
	}

	if ents, err := os.ReadDir(m.P.Images()); err == nil {
		for _, e := range ents {
			if p := filepath.Join(m.P.Images(), e.Name()); strings.HasSuffix(e.Name(), ".part") && old(p) {
				add(p, "interrupted download")
			}
		}
	}
	if ents, err := os.ReadDir(m.P.VMs()); err == nil {
		for _, e := range ents {
			p := filepath.Join(m.P.VMs(), e.Name())
			switch {
			case strings.HasPrefix(e.Name(), ".destroyed-"):
				add(p, "interrupted destroy")
			case e.IsDir() && ValidName(e.Name()) == nil && old(filepath.Join(p, diskFile+".reset")):
				add(filepath.Join(p, diskFile+".reset"), "interrupted reset")
			}
		}
	}
	if n, b, err := (cache.Store{Dir: m.P.Pkg()}).Trim(m.Cfg.CacheMaxBytes(), dryRun); err != nil {
		m.logf("warning: trimming the package caches: %v", err)
	} else if n > 0 {
		items = append(items, GCItem{Path: m.P.Pkg(), Bytes: b, Why: fmt.Sprintf("%d least recently used package cache files, over cache_max", n)})
	}
	if !dryRun && len(items) > 0 {
		var n int64
		for _, it := range items {
			n += it.Bytes
		}
		m.audit(audit.GC, "", map[string]any{"removed": len(items), "bytes": n})
	}
	return items, nil
}

// removeBase reports whether base is unused, and (unless dryRun) deletes it
// with its lock file, holding the lock: a VM being created on it either names
// it first or finds it gone. A base whose lock is held (a VM is being
// created, reset or rebased on it) is skipped rather than waited for.
func (m *Manager) removeBase(base string, dryRun bool) bool {
	if dryRun {
		users, err := m.baseUsers(base)
		return err == nil && len(users) == 0
	}
	unlock, ok, err := fsutil.TryLock(base + ".lock")
	if err != nil || !ok {
		if err == nil {
			m.logf("skipping %s: in use right now", filepath.Base(base))
		}
		return false
	}
	defer unlock()
	users, err := m.baseUsers(base)
	if err != nil || len(users) > 0 || os.Remove(base) != nil {
		return false
	}
	_ = os.Remove(base + ".lock")
	return true
}

// removeSpecs removes the list of base key, which has no generation left.
// A first build records its spec before its base exists, holding the build
// lock, which it creates first.
func (m *Manager) removeSpecs(key string, dryRun bool, add func(p, why string)) {
	lock := filepath.Join(m.P.Bases(), key+".build.lock")
	if _, err := os.Stat(lock); err == nil {
		if dryRun {
			return
		}
		unlock, ok, err := fsutil.TryLock(lock)
		if err != nil || !ok {
			return
		}
		defer unlock()
		if built, _ := filepath.Glob(filepath.Join(m.P.Bases(), key+"*.qcow2")); len(built) > 0 {
			return // finished since gc listed the folder
		}
	}
	add(filepath.Join(m.P.Bases(), key+".specs"), "list of a base that's gone")
}

func baseFileGen(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return 1
}

// usage is the disk space p takes (blocks, so sparse images count right).
func usage(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				n += st.Blocks * 512
			}
		}
		return nil
	})
	return n
}
