package vm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/canary"
	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/notify"
	"github.com/blackshieldpt/curral/internal/qemu"
)

// pausedFile marks a VM a canary hit paused (removed by resume and stop).
const pausedFile = "paused.json"

// CanaryHit records one of name's canaries found in use (where: how it was
// seen) and acts on it as config canary_action says.
func (m *Manager) CanaryHit(name string, c *canary.Canary, where string) {
	action := cmp.Or(m.Cfg.CanaryAction, config.CanaryPause)
	then := map[string]string{config.CanaryPause: "paused it (curral resume " + name + ")", config.CanaryStop: "stopped it", config.CanaryLog: "left it running"}[action]
	var err error
	switch action {
	case config.CanaryPause:
		err = m.pause(name, c, where)
	case config.CanaryStop:
		err = m.Stop(name, true)
	}
	if err != nil {
		then = "failed to " + action + " it: " + err.Error()
		m.logf("warning: canary hit in %s: %v", name, err)
	}
	// A guest retrying in a loop (action log) gets a few records, not one each
	// (the audit log is shared and rotates).
	lim := notify.Limiter{Path: m.file(name, "canary.bucket"), Burst: 3, Every: time.Minute}
	if ok, _ := lim.Allow(time.Now()); ok {
		m.audit(audit.CanaryHit, name, map[string]any{"kind": c.Kind, "where": where, "action": action})
		m.notify(notify.Event{Kind: notify.CanaryHit, VM: name, Title: "curral " + name + ": canary used",
			Message: fmt.Sprintf("its %s decoy credentials were used (%s); curral %s", c.Kind, where, then)})
	}
}

func (m *Manager) pause(name string, c *canary.Canary, where string) error {
	q, err := qemu.DialQMP(m.file(name, qmpSock), 5*time.Second)
	if err != nil {
		return err
	}
	defer q.Close()
	if _, err := q.Execute("stop", nil); err != nil {
		return err
	}
	return writeJSON(m.file(name, pausedFile), map[string]any{"kind": c.Kind, "where": where, "time": time.Now().UTC()})
}

// Paused reports whether a canary hit paused the VM.
func (m *Manager) Paused(name string) bool { return fileExists(m.file(name, pausedFile)) }

// Resume continues a VM a canary hit paused, and sets its clock (which
// stood still).
func (m *Manager) Resume(ctx context.Context, name string) error {
	if _, ok := m.Running(name); !ok || !m.Paused(name) {
		return fmt.Errorf("%s was not paused by a canary hit", name)
	}
	q, err := qemu.DialQMP(m.file(name, qmpSock), 5*time.Second)
	if err != nil {
		return err
	}
	defer q.Close()
	if _, err := q.Execute("cont", nil); err != nil {
		return err
	}
	_ = os.Remove(m.file(name, pausedFile))
	m.audit(audit.VMResume, name, nil)
	m.withGuest(ctx, name, func(c *ssh.Client) { m.syncClock(ctx, c) })
	return nil
}

// ErrCanary refuses an export carrying one of the VM's canaries.
var ErrCanary = errors.New("refused: it contains the VM's decoy credentials")

// scanExport reads r for name's canaries (what: where they'd be); a hit is
// acted on and refuses the export.
func (m *Manager) scanExport(name string, r io.Reader, what string) error {
	cs, err := canary.Load(m.dir(name))
	if err != nil || len(cs) == 0 {
		return err
	}
	c, err := canary.Scan(r, cs)
	if err != nil {
		return err
	}
	if c != nil {
		m.CanaryHit(name, c, what)
		return fmt.Errorf("%s: %w", what, ErrCanary)
	}
	return nil
}

// scanCommits scans the commits ref brings into the host repository dir
// (messages and diffs) for name's canaries.
func (m *Manager) scanCommits(ctx context.Context, name, dir, ref string) error {
	if cs, err := canary.Load(m.dir(name)); err != nil || len(cs) == 0 {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Merges' own changes and binary files included; nothing from the
	// user's config that runs programs on guest data.
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "-c", "core.fsmonitor=false", "-c", "log.showSignature=false",
		"log", "-p", "--diff-merges=separate", "--text", "--no-ext-diff", "--no-textconv",
		"--format=%B", ref, "--not", "--exclude=refs/curral*", "--all")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := m.scanExport(name, out, "in commits pulled from it"); err != nil {
		cancel() // stop git early
		_ = cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("scanning the pulled commits: %w", err)
	}
	return nil
}

// cancelOnPause cancels ctx once a canary hit pauses name: nothing in the
// guest answers then.
func (m *Manager) cancelOnPause(ctx context.Context, name string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if m.Paused(name) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}
