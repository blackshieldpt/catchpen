package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/catchpen/internal/agents"
	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/cache"
	"github.com/blackshieldpt/catchpen/internal/cloudinit"
	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/gitsrc"
	"github.com/blackshieldpt/catchpen/internal/hostherdr"
	"github.com/blackshieldpt/catchpen/internal/hosttmux"
	"github.com/blackshieldpt/catchpen/internal/qemu"
	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/internal/template"
)

func (m *Manager) Start(ctx context.Context, name string) (*Runtime, error) {
	if s, err := m.Load(name); err == nil && s.Pool {
		return nil, fmt.Errorf("%s is a warm pool VM; catchpen up claims one (catchpen pool drain removes them)", name)
	}
	return m.start(ctx, name, nil)
}

// resume starts a VM from a memory snapshot, on the machine type it was
// saved with.
type resume struct{ snap, machine string }

// start boots a VM, or with from resumes it from a memory snapshot.
func (m *Manager) start(ctx context.Context, name string, from *resume) (*Runtime, error) {
	if _, err := m.Load(name); err != nil { // no lock file for a VM that doesn't exist
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
	s, err := m.Load(name)
	if err != nil { // destroyed while we waited for the lock
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
		return rt, fmt.Errorf("%s is running, but pushing credentials failed: %w (retry: catchpen creds push %s)", name, err, name)
	}
	m.logf("%s is ready: catchpen ssh %s", name, name)
	m.tmuxDo(func(t hosttmux.Tmux) error { return t.Add(name) })
	if s.Task == "" { // a task's agent runs headless; a second one would only get in its way
		m.herdrDo(func(h hostherdr.Herdr) error { return h.Add(name, s.DefaultAgent()) })
	}
	return rt, nil
}

// Push delivers credentials, agent config, skills and plugins into a running VM,
// after taking its env:, skills: and plugins: from its template as it is now.
func (m *Manager) Push(ctx context.Context, name string) error {
	if err := m.refreshEnv(name); err != nil {
		return err
	}
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

// refreshEnv replaces the VM's saved env:, skills: and plugins: with its template's, so
// names added to the template after the VM was created reach it. A template that no
// longer loads keeps the saved ones, with a warning.
func (m *Manager) refreshEnv(name string) error {
	_, unlock, err := m.vm(name).lock()
	if err != nil {
		return err
	}
	defer unlock()
	s, err := m.vm(name).load()
	if err != nil {
		return err
	}
	if s.Pool || s.Template == "" {
		return nil
	}
	if !s.Picks {
		m.logf("warning: %s was created with the old-style template %s; it keeps the env, skills and plugins it was created with", name, s.Template)
		return nil
	}
	t, err := m.Tpl.Load(s.Template)
	if err != nil {
		m.logf("warning: %v; %s keeps the env, skills and plugins it was created with", err, name)
		return nil
	}
	// The VM's Claude auth can differ from the template's (catchpen up --claude-auth).
	if c := s.Agents.Claude; c != nil && c.Auth != template.ClaudeAPIKey {
		if _, ok := t.Env["ANTHROPIC_API_KEY"]; ok {
			return fmt.Errorf("template %s: env.ANTHROPIC_API_KEY would override %s's Claude subscription token", s.Template, name)
		}
	}
	skillsChanged := !slices.Equal(s.Skills, t.Skills) || !slices.Equal(s.Plugins, t.Plugins)
	if skillsChanged {
		s.Skills, s.Plugins = t.Skills, t.Plugins
		m.logf("%s: skills and plugins from template %s: %d skills, %d plugins", name, s.Template, len(s.Skills), len(s.Plugins))
	}
	if maps.Equal(s.Env, t.Env) {
		if skillsChanged {
			return m.vm(name).save(s)
		}
		return nil
	}
	var changes []string
	for _, k := range slices.Sorted(maps.Keys(t.Env)) {
		if old, ok := s.Env[k]; !ok {
			changes = append(changes, "+"+k)
		} else if old != t.Env[k] {
			changes = append(changes, "~"+k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		if _, ok := t.Env[k]; !ok {
			changes = append(changes, "-"+k)
		}
	}
	m.logf("%s: env from template %s: %s", name, s.Template, strings.Join(changes, " "))
	s.Env = t.Env
	return m.vm(name).save(s)
}

type sshRemote struct{ c *ssh.Client }

// GuestRemote runs commands for internal/agents over c.
func GuestRemote(c *ssh.Client) agents.Remote { return sshRemote{c} }

func (r sshRemote) Run(ctx context.Context, cmd string, stdin []byte) (sshx.GuestOutput, error) {
	return sshx.Run(ctx, r.c, cmd, bytes.NewReader(stdin))
}

func (m *Manager) push(ctx context.Context, g *Guest, s *State) error {
	spec := m.pushSpec(ctx, s)
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

// pushSpec is what push sends for s, its skills and plugins fetched on the host.
func (m *Manager) pushSpec(ctx context.Context, s *State) agents.Spec {
	spec := agents.Spec{Agents: s.Agents, Env: s.Env, MCP: s.MCP, Sync: s.Sync}
	if s.Sync.Claude() {
		spec.ClaudeDir = hostClaudeDir()
	}
	if s.Sync.Git() {
		spec.GitName, spec.GitEmail = hostGit("user.name"), hostGit("user.email")
	}
	// A cold fetch can be slow; it must not eat the config push's time.
	fctx, fcancel := context.WithTimeout(ctx, 5*time.Minute)
	spec.Skills, spec.Plugins = m.agentBundles(fctx, s)
	fcancel()
	spec.Home = "/home/" + s.User
	spec.AgentFiles = true
	return spec
}

// fetchProtocols are the git protocols skills and plugins may use; nil is
// https only. Tests use "file".
var fetchProtocols []string

// agentBundles fetches the VM's skills and plugins on the host. A failed fetch
// is a warning, and its bundle is marked Failed so the guest keeps its copy.
func (m *Manager) agentBundles(ctx context.Context, s *State) (skills, plugins []agents.Bundle) {
	f := gitsrc.Fetcher{Dir: m.P.Sources(), Protocols: fetchProtocols}
	type fetched struct {
		files []gitsrc.File
		err   error
	}
	memo := map[string]fetched{}
	fetch := func(kind, name, git, ref, dir string) agents.Bundle {
		k := git + "\x00" + ref + "\x00" + dir
		r, ok := memo[k]
		if !ok {
			r.files, r.err = f.Files(ctx, git, ref, dir)
			memo[k] = r
		}
		if r.err != nil {
			if !ok { // once per source
				m.logf("warning: %s %s from %s: %v; the VM keeps its current copy", kind, name, git, r.err)
			}
			return agents.Bundle{Name: name, Failed: true}
		}
		return agents.Bundle{Name: name, Files: r.files}
	}
	for _, e := range s.Skills {
		if e.Agent != "" && s.Agents.Get(e.Agent) == nil {
			continue
		}
		b := fetch("skill", e.DirName(), e.Git, e.Ref, e.Path)
		b.Agent = e.Agent
		skills = append(skills, b)
	}
	for _, e := range s.Plugins {
		if s.Agents.Get(e.Agent) == nil { // not fetched; the push warns about it
			plugins = append(plugins, agents.Bundle{Agent: e.Agent, Name: e.DirName(), Failed: true})
			continue
		}
		if e.Bundled != "" {
			plugins = append(plugins, agents.Bundle{Agent: "pi", Name: e.DirName(), Bundled: e.Bundled})
			continue
		}
		b := fetch("plugin", e.DirName(), e.Git, e.Ref, e.Path)
		b.Agent = e.Agent
		plugins = append(plugins, b)
	}
	return skills, plugins
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
