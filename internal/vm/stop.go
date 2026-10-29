package vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/hostherdr"
	"github.com/blackshieldpt/catchpen/internal/hosttmux"
	"github.com/blackshieldpt/catchpen/internal/qemu"
)

func (m *Manager) Stop(name string, force bool) error {
	if _, err := m.Load(name); err != nil {
		return err
	}
	held, unlock, err := m.vm(name).lock()
	if err != nil {
		return err
	}
	defer unlock()
	return m.stopLocked(held, force)
}

func (m *Manager) stopLocked(h heldVM, force bool) error {
	name := h.name
	pidfile := m.vm(name).path(pidFile)
	pid, ok := qemu.Pid(pidfile, name)
	if !ok { // not running, or QEMU exited on its own (the guest powered off)
		return m.cleanupRuntime(name)
	}
	force = force || m.Paused(name) // a paused guest can't shut down
	m.audit(audit.VMStop, name, map[string]any{"pid": pid, "force": force})
	wait := func(d time.Duration) bool {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if _, ok := qemu.Pid(pidfile, name); !ok {
				return true
			}
			time.Sleep(250 * time.Millisecond)
		}
		return false
	}
	if q, err := qemu.DialQMP(m.vm(name).path(qmpSock), 5*time.Second); err == nil {
		if !force {
			m.logf("stopping %s", name)
			if _, err := q.Execute("system_powerdown", nil); err == nil && wait(stopTimeout) {
				q.Close()
				return m.cleanupRuntime(name)
			}
			m.logf("%s did not power off in %s, forcing", name, stopTimeout)
		}
		_, _ = q.Execute("quit", nil)
		q.Close()
		if wait(10 * time.Second) {
			return m.cleanupRuntime(name)
		}
	}
	// QMP unreachable: SIGTERM makes QEMU exit cleanly, SIGKILL as last resort.
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if wait(10 * time.Second) {
		return m.cleanupRuntime(name)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if !wait(10 * time.Second) {
		return fmt.Errorf("could not stop %s (pid %d)", name, pid)
	}
	return m.cleanupRuntime(name)
}

func (m *Manager) cleanupRuntime(name string) error {
	m.stopForwards(name)
	m.tmuxDo(func(t hosttmux.Tmux) error { return t.Close(name) })
	m.herdrDo(func(h hostherdr.Herdr) error { return h.Close(name) })
	_ = m.agent(name).Stop()
	for _, f := range []string{runtimeFile, qmpSock, serialSock, pidFile, pausedFile} {
		_ = os.Remove(m.vm(name).path(f))
	}
	return nil
}

// Destroy stops and deletes a VM. It also works on a VM whose state file is
// missing or corrupt (e.g. an interrupted create).
func (m *Manager) Destroy(name string) error { return m.destroy(name, true) }

func (m *Manager) destroy(name string, transcripts bool) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if _, err := os.Stat(m.dir(name)); err != nil {
		return fmt.Errorf("no VM named %q (see: catchpen ls)", name)
	}
	if transcripts {
		m.saveTranscripts(name)
	}
	held, unlock, err := m.vm(name).lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.stopLocked(held, true); err != nil {
		return err
	}
	if s, err := m.Load(name); err == nil {
		m.releaseForges(s)
	} else if _, err := os.Stat(m.vm(name).path(forgeDir)); err == nil {
		m.logf("warning: %s has unreadable state; deploy keys or tokens it created may remain on the forge", name)
	}
	// Move the directory out of the way atomically first (while locked): a start
	// waiting on the lock, or an egress edit, then finds the VM gone rather than
	// half-deleted. Dot-names are never listed as VMs.
	tomb := filepath.Join(m.P.VMs(), fmt.Sprintf(".destroyed-%s-%d", name, time.Now().UnixNano()))
	if err := os.Rename(m.dir(name), tomb); err != nil {
		return err
	}
	if err := os.RemoveAll(tomb); err != nil {
		return err
	}
	m.logf("destroyed %s", name)
	m.audit(audit.VMDestroy, name, nil)
	return m.WriteSSHConfig()
}

// saveTranscripts copies a running VM's agent session stores to the host
// before something discards them.
func (m *Manager) saveTranscripts(name string) {
	s, err := m.Load(name)
	if err != nil || !s.Agents.Any() {
		return
	}
	if _, ok := m.Running(name); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := m.CopyTranscripts(ctx, name); err != nil {
		m.logf("warning: %v", err)
	}
}
