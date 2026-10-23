package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/curral/internal/agents"
	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/cache"
	"github.com/blackshieldpt/curral/internal/cloudinit"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/hosttmux"
	"github.com/blackshieldpt/curral/internal/qemu"
	"github.com/blackshieldpt/curral/internal/sshx"
)

func (m *Manager) Start(ctx context.Context, name string) (*Runtime, error) {
	if s, err := m.Load(name); err == nil && s.Pool {
		return nil, fmt.Errorf("%s is a warm pool VM; curral up claims one (curral pool drain removes them)", name)
	}
	return m.start(ctx, name, nil)
}

// resume starts a VM from a memory snapshot, on the machine type it was
// saved with.
type resume struct{ snap, machine string }

// start boots a VM, or with from resumes it from a memory snapshot.
func (m *Manager) start(ctx context.Context, name string, from *resume) (*Runtime, error) {
	s, err := m.Load(name)
	if err != nil {
		return nil, err
	}
	held, unlock, err := m.vm(name).lock()
	if err != nil {
		return nil, err
	}
	// The lock covers launching; it is released before the (long) boot wait so
	// stop/destroy are not blocked behind it.
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	if s, err = m.Load(name); err != nil { // destroyed while we waited for the lock
		return nil, err
	}
	if rt, ok := m.Running(name); ok {
		if from != nil {
			return nil, fmt.Errorf("%s was started meanwhile; run the restore again", name)
		}
		return rt, nil
	}
	if _, err := os.Stat(s.Base); err != nil {
		return nil, fmt.Errorf("base image %s missing: %w", s.Base, err)
	}
	claimed := "" // the pool snapshot this start resumes
	switch {
	case s.Resume != "" && from == nil:
		if from, err = m.takeResume(held, s); err != nil {
			return nil, err
		}
		if from != nil {
			claimed = from.snap
		}
	case s.Resume != "": // restored by hand: never resume the pool's memory twice
		if from.snap == s.Resume {
			claimed = from.snap
		}
		s.Resume = ""
		if err := m.save(s); err != nil {
			return nil, err
		}
	}
	dropClaimed := func() { // after a failed resume, which won't be retried
		if claimed != "" {
			_ = qemu.SnapshotOffline("-d", claimed, m.vm(name).path(diskFile))
			_ = m.setSnapMeta(name, claimed, nil)
		}
	}
	rt := &Runtime{}
	if rt.SSHPort, err = qemu.FreePort(); err != nil {
		return nil, err
	}
	for _, p := range s.Ports {
		h := p.Host
		if h == 0 {
			if h, err = qemu.FreePort(); err != nil {
				return nil, err
			}
		}
		rt.Forwards = append(rt.Forwards, qemu.Forward{Host: h, Guest: p.Guest})
	}
	for _, f := range []string{qmpSock, serialSock, pidFile, pausedFile} { // pausedFile: left by a crash or reboot
		_ = os.Remove(m.vm(name).path(f))
	}
	spec := qemu.Spec{
		Name: name, CPUs: s.CPUs, MemoryMB: s.MemoryMB,
		Disk: m.vm(name).path(diskFile), Seed: m.vm(name).path(seedFile),
		SSHPort: rt.SSHPort, Forwards: rt.Forwards, Egress: s.Egress,
		ProxyCmd: []string{m.Bin, "egress-stdio", "--dir", m.dir(name), "--vm", name},
		ProxyIP:  cloudinit.ProxyAddr, ProxyPort: cloudinit.ProxyPort,
		NotifyCmd: notifyCmd(m.Bin, m.dir(name), name, s.Task != ""),
		NotifyIP:  cloudinit.NotifyAddr, NotifyPort: cloudinit.NotifyPort,
		QMP: m.vm(name).path(qmpSock), Serial: m.vm(name).path(serialSock), SerialLog: m.Debug,
		Pidfile: m.vm(name).path(pidFile),
	}
	if len(s.Cache) > 0 {
		spec.CacheCmd, spec.CacheIP, spec.CachePort = m.cacheCmd(m.dir(name), name, s.Cache), cache.Addr, cache.Port
	}
	if from != nil {
		spec.LoadVM, spec.Machine = from.snap, from.machine
	}
	logFile, err := os.OpenFile(m.vm(name).path(qemuLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "--- %s start\n", time.Now().Format(time.RFC3339))
	pid, err := qemu.Launch(spec, m.dir(name), logFile)
	if err != nil {
		dropClaimed()
		return nil, fmt.Errorf("%w (see %s)", err, m.vm(name).path(qemuLog))
	}
	rt.PID = pid
	if err := writeJSON(m.vm(name).path(runtimeFile), rt); err != nil {
		_ = m.stopLocked(held, true)
		return nil, err
	}
	locked = false
	unlock()

	m.logf("started %s (pid %d), waiting for ssh", name, pid)
	m.audit(audit.VMStart, name, map[string]any{"pid": pid})
	wctx, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	alive := func() bool { _, ok := qemu.Pid(m.vm(name).path(pidFile), name); return ok }
	failed := func(err error) (*Runtime, error) {
		if m.Debug {
			return nil, fmt.Errorf("%s: %w (VM left running for debugging; serial log: %s)", name, err, m.vm(name).path(serialSock+".log"))
		}
		_ = m.Stop(name, true)
		dropClaimed()
		return nil, fmt.Errorf("%s: %w (VM stopped; retry with --debug to keep it running and log the console)", name, err)
	}
	g, err := m.waitGuest(ctx, wctx, name, rt.SSHPort, s.User, alive)
	if err != nil {
		return failed(err)
	}
	defer g.Close()
	c := g.Client
	if err := m.startAgent(ctx, s); err != nil {
		return failed(fmt.Errorf("ssh agent: %w", err))
	}
	if from != nil {
		m.syncClock(ctx, c)
	}
	if claimed != "" {
		m.claimed(wctx, c, name, claimed)
	}
	if !s.Booted {
		m.logf("waiting for first-boot setup")
		out, err := sshx.Run(wctx, c, "cloud-init status --wait >/dev/null; rc=$?; cloud-init status --long; exit $rc", nil)
		var ee *sshx.ExitError
		if err != nil && !(errors.As(err, &ee) && ee.Status == 2) { // 2 = finished with recoverable errors
			// The output is guest-controlled; strip control characters before it reaches the terminal.
			return failed(fmt.Errorf("first boot: %w\n%s", err, out))
		}
		if err := m.markBooted(name); err != nil {
			return nil, err
		}
	}
	if s.Pool {
		return rt, nil
	}
	if err := m.push(ctx, g, s); err != nil {
		return rt, fmt.Errorf("%s is running, but pushing credentials failed: %w (retry: curral creds push %s)", name, err, name)
	}
	m.logf("%s is ready: curral ssh %s", name, name)
	m.tmuxDo(func(t hosttmux.Tmux) error { return t.Add(name) })
	return rt, nil
}

// Push delivers credentials and agent config into a running VM.
func (m *Manager) Push(ctx context.Context, name string) error {
	g, s, err := m.DialGuest(ctx, name)
	if err != nil {
		return err
	}
	defer g.Close()
	if err := m.push(ctx, g, s); err != nil {
		return err
	}
	m.logf("pushed credentials to %s", name)
	return nil
}

type sshRemote struct{ c *ssh.Client }

// GuestRemote runs commands for internal/agents over c.
func GuestRemote(c *ssh.Client) agents.Remote { return sshRemote{c} }

func (r sshRemote) Run(ctx context.Context, cmd string, stdin []byte) (sshx.GuestOutput, error) {
	return sshx.Run(ctx, r.c, cmd, bytes.NewReader(stdin))
}

func (m *Manager) push(ctx context.Context, g *Guest, s *State) error {
	spec := agents.Spec{Agents: s.Agents, Env: s.Env, MCP: s.MCP, Sync: s.Sync}
	if s.Sync.Claude() {
		spec.ClaudeDir = hostClaudeDir()
	}
	if s.Sync.Git() {
		spec.GitName, spec.GitEmail = hostGit("user.name"), hostGit("user.email")
	}
	cloneCtx := ctx // clones can take longer than the config push
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	gcreds, err := m.forgeCreds(ctx, s)
	if err != nil {
		return err
	}
	spec.ExtraEnv = forge.GitHubEnv(gcreds)
	res, err := agents.Push(ctx, sshRemote{g.Client}, spec, m.resolver())
	for _, w := range res.Warnings {
		m.logf("warning: %s", w)
	}
	if len(res.Missing) > 0 {
		m.logf("secrets not found on the host: %s (set them in the environment, %s, or config.yaml secrets)",
			strings.Join(res.Missing, ", "), m.P.SecretsFile())
	}
	m.audit(audit.CredsPush, s.Name, map[string]any{"secrets": res.Found, "missing": res.Missing, "ok": err == nil})
	if err != nil {
		return err
	}
	return m.pushForges(cloneCtx, sshRemote{g.Client}, s, gcreds)
}

func hostClaudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func hostGit(key string) string {
	out, err := exec.Command("git", "config", "--global", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (m *Manager) markBooted(name string) error {
	_, unlock, err := m.vm(name).lock()
	if err != nil {
		return err
	}
	defer unlock()
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	s.Booted = true
	return m.save(s)
}
