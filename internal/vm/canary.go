package vm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/canary"
	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/notify"
	"github.com/blackshieldpt/curral/internal/qemu"
	"github.com/blackshieldpt/curral/internal/sshx"
)

// pausedFile marks a VM a canary hit paused (removed by resume and stop);
// hitFile records that one was found in use at all (kept until destroy).
const (
	pausedFile = "paused.json"
	hitFile    = "canary-hit.json"
)

// CanaryHit records one of name's canaries found in use (where: how it was
// seen) and acts on it as config canary_action says.
func (m *Manager) CanaryHit(name string, c *canary.Canary, where string) {
	action := cmp.Or(m.Cfg.CanaryAction, config.CanaryPause)
	_ = writeJSON(m.file(name, hitFile), map[string]any{"kind": c.Kind, "where": where, "time": time.Now().UTC()})
	then := map[string]string{config.CanaryPause: "paused it (curral resume " + name + ")", config.CanaryStop: "stopped it", config.CanaryLog: "left it running"}[action]
	var err error
	switch action {
	case config.CanaryPause:
		if err = m.pause(name, c, where); err != nil {
			// A VM that can't be paused (its QMP socket busy, as during a
			// snapshot) is stopped instead of left running.
			if _, running := m.Running(name); !running {
				then, err = "found it not running", nil
			} else if serr := m.Stop(name, true); serr != nil {
				err = errors.Join(err, serr)
			} else {
				then, err = "couldn't pause it ("+err.Error()+"), so stopped it", nil
			}
		}
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
	// QMP takes one client at a time, and each guest connection is its own
	// process: another hit may hold it for a moment.
	sock := m.file(name, qmpSock)
	var q *qemu.QMP
	var err error
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		q, err = qemu.DialQMP(sock, 5*time.Second)
		if _, serr := os.Stat(sock); err == nil || serr != nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer q.Close()
	if m.Paused(name) {
		return nil // by an earlier hit
	}
	if _, err := q.Execute("stop", nil); err != nil {
		return err
	}
	// Marked while QMP is still held, so resume can't come in between; a VM
	// that can't be marked couldn't be resumed, so it runs on.
	if err := writeJSON(m.file(name, pausedFile), map[string]any{"kind": c.Kind, "where": where, "time": time.Now().UTC()}); err != nil {
		_, _ = q.Execute("cont", nil)
		return err
	}
	return nil
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
	return m.scanWith(name, what, func(cs []canary.Canary) (*canary.Canary, error) { return canary.Scan(r, cs) })
}

// guestText returns s, or a note in its place when s carries one of
// name's decoys (as written, or as the task record and notifications clean
// it) or they can't be read. A hit is acted on.
func (m *Manager) guestText(name, s, what string) string {
	if s == "" {
		return s
	}
	p := sshx.Printable(s)
	err := m.scanExport(name, strings.NewReader(s+"\n"+p+"\n"+notify.Clean(p, len(p)+1)), what)
	switch {
	case errors.Is(err, ErrCanary):
		return "(withheld: it contained the VM's decoy credentials)"
	case err != nil:
		return "(withheld: the VM's canaries couldn't be read)"
	}
	return s
}

// scanPatch is scanExport for a patch file, binary files included; unless
// it is clean, it is removed.
func (m *Manager) scanPatch(name, path, what string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = m.scanWith(name, what, func(cs []canary.Canary) (*canary.Canary, error) { return canary.ScanPatch(f, cs) })
	f.Close()
	if err != nil {
		os.Remove(path)
	}
	return err
}

func (m *Manager) scanWith(name, what string, scan func([]canary.Canary) (*canary.Canary, error)) error {
	cs, err := canary.Load(m.dir(name))
	if err != nil || len(cs) == 0 {
		return err
	}
	c, err := scan(cs)
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
// (headers, messages and diffs) for name's canaries.
func (m *Manager) scanCommits(ctx context.Context, name, dir, ref string) error {
	return m.scanRevs(ctx, name, dir, []string{ref, "--not", "--exclude=refs/curral*", "--all"})
}

// scanRevs scans the commits revs (git log arguments) name in dir.
func (m *Manager) scanRevs(ctx context.Context, name, dir string, revs []string) error {
	if cs, err := canary.Load(m.dir(name)); err != nil || len(cs) == 0 {
		return err
	}
	// Whole commits as stored (no re-encoding: an encoding header would
	// garble them), root commits, merges' own changes and binary files
	// included; nothing from the user's config that runs programs on guest
	// data or changes the output. Then the messages and names as re-encoded
	// to UTF-8, as format-patch and forges show them.
	for _, args := range [][]string{
		{"-c", "log.showRoot=true", "log", "-p", "--diff-merges=separate", "--text", "--no-ext-diff", "--no-textconv",
			"--no-color", "--encoding=none", "--pretty=raw"},
		{"log", "--no-color", "--encoding=UTF-8", "--format=%an%n%ae%n%cn%n%ce%n%B"},
	} {
		if err := m.scanGit(ctx, name, dir, append(args, revs...)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) scanGit(ctx context.Context, name, dir string, args []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := hostGitExec(ctx, dir, append([]string{"-c", "log.showSignature=false"}, args...)...)
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
