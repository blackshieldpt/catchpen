package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/qemu"
)

// vmDir is one VM's directory (vms/<name>) and the state kept in it: its
// saved State, its QEMU runtime, the canary markers, and its lock.
//
// Locks, in the order they may be taken (never an earlier one while holding a
// later one):
//  1. the pool fill lock, then the pool lock (vms/.pool.lock);
//  2. a VM's lock (vms/<name>/.lock), held while its disk, state or QEMU
//     process change;
//  3. a skill or plugin repo's cache lock (internal/gitsrc), a leaf taken
//     before the push: nothing else is taken while it is held (a pool fill
//     holds 1 around it).
//
// A VM's lock isn't re-entrant: flock makes a second taker in the same
// process wait, forever if it's the holder. Code holding it has a heldVM and
// calls the functions that take one (stopLocked, takeResume), never Stop,
// Destroy, Restore or CanaryHit, which take the lock themselves.
type vmDir struct {
	m    *Manager
	name string
}

func (m *Manager) vm(name string) vmDir { return vmDir{m, name} }

// path is a file in the VM's directory.
func (d vmDir) path(f string) string { return filepath.Join(d.m.P.VM(d.name), f) }

func (d vmDir) has(f string) bool { return fileExists(d.path(f)) }

// heldVM is a VM whose lock its holder has taken.
type heldVM struct{ vmDir }

func (d vmDir) lock() (heldVM, func(), error) {
	unlock, err := fsutil.Lock(d.path(lockFile))
	return heldVM{d}, unlock, err
}

// tryLock takes the VM's lock if it's free (ok false if not).
func (d vmDir) tryLock() (h heldVM, unlock func(), ok bool, err error) {
	unlock, ok, err = fsutil.TryLock(d.path(lockFile))
	return heldVM{d}, unlock, ok, err
}

func (d vmDir) load() (*State, error) {
	b, err := os.ReadFile(d.path(stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no VM named %q (see: catchpen ls)", d.name)
	}
	if err != nil {
		return nil, err
	}
	var s State
	return &s, json.Unmarshal(b, &s)
}

func (d vmDir) save(s *State) error { return writeJSON(d.path(stateFile), s) }

// running returns the runtime of a running VM.
func (d vmDir) running() (*Runtime, bool) {
	pid, ok := qemu.Pid(d.path(pidFile), d.name)
	if !ok {
		return nil, false
	}
	var rt Runtime
	b, err := os.ReadFile(d.path(runtimeFile))
	if err != nil || json.Unmarshal(b, &rt) != nil {
		return &Runtime{PID: pid}, true
	}
	rt.PID = pid
	return &rt, true
}

// paused: a canary hit paused it (removed by resume and stop). hit: its
// canaries were found in use at all (kept until destroy).
func (d vmDir) paused() bool { return d.has(pausedFile) }
func (d vmDir) hit() bool    { return d.has(hitFile) }
