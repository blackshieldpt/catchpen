// Package daemon runs catchpen's background jobs: warm pool refills, idle
// stop, scheduled base rebuilds and desktop egress prompts. Everything it
// does can also be done by hand.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
)

const (
	idleEvery    = time.Minute
	poolEvery    = 2 * time.Minute
	rebuildEvery = 10 * time.Minute
	promptEvery  = 30 * time.Second
	poolBackoff  = 30 * time.Minute
)

type Daemon struct {
	// Manager is called every round, so config.yaml edits apply.
	Manager func() (*vm.Manager, error)
	// Prompts answers egress prompts for the VMs in dirs (name -> directory)
	// until ctx is done; nil turns prompt dispatch off.
	Prompts func(ctx context.Context, m *vm.Manager, dirs map[string]string) error
	Log     io.Writer
}

func (d *Daemon) logf(format string, a ...any) {
	fmt.Fprintf(d.Log, format+"\n", a...)
}

func lockPath(p paths.Paths) string  { return filepath.Join(p.Data, "daemon.lock") }
func stampPath(p paths.Paths) string { return filepath.Join(p.Data, "rebuild.stamp") }

// Running reports whether a daemon holds the lock.
func Running(p paths.Paths) bool {
	unlock, ok, err := fsutil.TryLock(lockPath(p))
	if err != nil {
		return false
	}
	if ok {
		unlock()
	}
	return !ok
}

// Run runs the jobs until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	m, err := d.Manager()
	if err != nil {
		return err
	}
	if err := m.P.Ensure(); err != nil {
		return err
	}
	unlock, ok, err := fsutil.TryLock(lockPath(m.P))
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("catchpen daemon is already running")
	}
	defer unlock()
	d.logf("catchpen daemon running")

	var wg sync.WaitGroup
	loop := func(job string, every time.Duration, fn func(context.Context, *vm.Manager) error) {
		wg.Go(func() {
			for {
				if m, err := d.Manager(); err != nil {
					d.logf("%s: %v", job, err)
				} else if err := fn(ctx, m); err != nil && ctx.Err() == nil {
					d.logf("%s: %v", job, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(every):
				}
			}
		})
	}
	idle := &vm.IdleTracker{}
	loop("idle stop", idleEvery, func(_ context.Context, m *vm.Manager) error { return d.idle(m, idle) })
	var retry time.Time
	loop("pool", poolEvery, func(ctx context.Context, m *vm.Manager) error { return d.pool(ctx, m, &retry) })
	loop("rebuild", rebuildEvery, d.rebuild)
	if d.Prompts != nil {
		wg.Go(func() { d.prompts(ctx) })
	}
	wg.Wait()
	return nil
}

func (d *Daemon) idle(m *vm.Manager, t *vm.IdleTracker) error {
	idle, err := t.Check(m, time.Now())
	if err != nil {
		return err
	}
	var errs []error
	for _, i := range idle {
		d.logf("stopping %s: idle for %s", i.Name, i.For.Round(time.Minute))
		errs = append(errs, m.StopIdle(i.Name, i.For))
	}
	return errors.Join(errs...)
}

// pool fills the pool; after a failure it waits poolBackoff, so a template
// that fails to build or boot isn't retried every round.
func (d *Daemon) pool(ctx context.Context, m *vm.Manager, retry *time.Time) error {
	if time.Now().Before(*retry) {
		return nil
	}
	n, err := m.FillPool(ctx)
	if n > 0 {
		d.logf("pool: added %d VM(s)", n)
	}
	if errors.Is(err, vm.ErrFillRunning) {
		return nil
	}
	if err != nil {
		*retry = time.Now().Add(poolBackoff)
		err = fmt.Errorf("%w (next try in %s)", err, poolBackoff)
	}
	return err
}

// rebuild rebuilds the bases in use once rebuild: has passed since the
// last time (counted from the first check), then replaces stale pool VMs
// and removes the bases nothing uses any more.
func (d *Daemon) rebuild(ctx context.Context, m *vm.Manager) error {
	every, _ := template.ParseInterval(m.Cfg.Rebuild)
	if every == 0 {
		return nil
	}
	now := time.Now()
	b, err := os.ReadFile(stampPath(m.P))
	last, perr := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil || perr != nil {
		return writeStamp(m.P, now)
	}
	if now.Sub(last) < every {
		return nil
	}
	// Stamped first: a build that keeps failing is retried next interval,
	// not every round.
	if err := writeStamp(m.P, now); err != nil {
		return err
	}
	targets, err := m.RebuildTargets()
	if err != nil {
		return err
	}
	d.logf("rebuilding %d base(s)", len(targets))
	err = m.Rebuild(ctx, targets)
	if _, perr := m.FillPool(ctx); perr != nil && !errors.Is(perr, vm.ErrFillRunning) {
		err = errors.Join(err, perr)
	}
	items, gerr := m.GC(false)
	for _, it := range items {
		d.logf("gc: removed %s (%s)", it.Path, it.Why)
	}
	return errors.Join(err, gerr)
}

func writeStamp(p paths.Paths, t time.Time) error {
	return fsutil.WriteFileAtomic(stampPath(p), []byte(t.UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// prompts keeps one watcher serving the VMs whose egress asks (on_deny:
// prompt), restarting it as VMs come and go. VMs another watcher serves
// (catchpen egress watch) are left to it.
func (d *Daemon) prompts(ctx context.Context) {
	type watcher struct {
		dirs   map[string]string
		cancel context.CancelFunc
		done   chan struct{}
	}
	var w *watcher
	stop := func() {
		if w != nil {
			w.cancel()
			<-w.done
			w = nil
		}
	}
	defer stop()
	for {
		if w != nil {
			select {
			case <-w.done: // the watcher failed: start over
				stop()
			default:
			}
		}
		var cur map[string]string
		if w != nil {
			cur = w.dirs
		}
		if m, err := d.Manager(); err != nil {
			d.logf("prompts: %v", err)
		} else if want, stale := promptVMs(m, cur); stale || !maps.Equal(want, cur) {
			stop()
			if len(want) > 0 {
				wctx, cancel := context.WithCancel(ctx)
				nw := &watcher{dirs: want, cancel: cancel, done: make(chan struct{})}
				go func() {
					defer close(nw.done)
					if err := d.Prompts(wctx, m, want); err != nil && wctx.Err() == nil {
						d.logf("prompts: %v", err)
					}
				}()
				w = nw
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(promptEvery):
		}
	}
}

// promptVMs are the VMs to answer prompts for; cur is what we serve now,
// stale whether a VM in it lost our socket.
func promptVMs(m *vm.Manager, cur map[string]string) (_ map[string]string, stale bool) {
	vms, err := m.List()
	if err != nil {
		return cur, false
	}
	out := map[string]string{}
	for _, v := range vms {
		if v.Pool || v.Egress != egress.ModeAllowlist {
			continue
		}
		dir := m.Dir(v.Name)
		if cfg, err := egress.ReadConfig(dir); err != nil || cfg.OnDeny != egress.OnDenyPrompt {
			continue
		}
		// Ours while the socket is: a VM destroyed and created again under the
		// same name has a new directory without it, and gets a new watcher.
		sock := filepath.Join(dir, egress.SocketFile)
		if cur[v.Name] == dir && !exists(sock) {
			stale = true
		}
		if cur[v.Name] != dir && served(sock) {
			continue
		}
		out[v.Name] = dir
	}
	return out, stale
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func served(sock string) bool {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err == nil {
		c.Close()
	}
	return err == nil
}
