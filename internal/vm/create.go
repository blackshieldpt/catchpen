package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/canary"
	"github.com/blackshieldpt/catchpen/internal/cloudinit"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/qemu"
	"github.com/blackshieldpt/catchpen/internal/template"
)

type CreateOpts struct {
	Template   string // a -t spec: picks from the category files
	CPUs       int
	Memory     string
	Disk       string
	Rebuild    bool
	AllowOpen  bool     // template asks for unfiltered egress; caller confirmed
	ClaudeAuth string   // overrides agents.claude.auth
	Repos      []string // added to the template's repos
	ReadOnly   bool     // deploy keys are read-only
	SSHAgent   string   // overrides ssh.agent
	AgentKeys  []string // added to ssh.agent_keys
	Ephemeral  bool
	Task       string
	pool       bool // a warm pool VM: no repos, agents or credentials
}

func (m *Manager) Create(ctx context.Context, name string, o CreateOpts) (_ *State, err error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	if _, err := os.Stat(m.dir(name)); err == nil {
		return nil, fmt.Errorf("VM %q already exists", name)
	}
	r, err := m.Resolve(o.Template)
	if err != nil {
		return nil, err
	}
	if o.ClaudeAuth != "" {
		if r.T.Agents.Claude == nil {
			return nil, fmt.Errorf("-t %s has no claude agent", r.T.Name)
		}
		c := *r.T.Agents.Claude
		c.Auth = o.ClaudeAuth
		r.T.Agents.Claude = &c
		if err := r.T.Validate(); err != nil {
			return nil, err
		}
	}
	res := r.T.Resources.Override(template.Resources{CPUs: o.CPUs, Memory: o.Memory, Disk: o.Disk})
	if res.CPUs < 1 || res.CPUs > maxCPUs {
		return nil, fmt.Errorf("invalid CPU count %d (1-%d)", res.CPUs, maxCPUs)
	}
	memMB, err := template.ParseSize(res.Memory)
	if err != nil {
		return nil, err
	}
	diskMB, err := template.ParseSize(res.Disk)
	if err != nil {
		return nil, err
	}
	egressMode := r.T.Network.Egress
	if egressMode == "" {
		egressMode = egress.ModeAllowlist
	}
	if egressMode == egress.ModeOpen && !o.AllowOpen {
		return nil, fmt.Errorf("-t %s has network egress open (unfiltered internet and the host's 127.0.0.1 services); pass --allow-open to confirm", r.T.Name)
	}
	if o.SSHAgent != "" || len(o.AgentKeys) > 0 {
		if o.SSHAgent != "" {
			r.T.SSH.Agent = o.SSHAgent
		}
		r.T.SSH.AgentKeys = append(append([]string(nil), r.T.SSH.AgentKeys...), o.AgentKeys...)
		if err := r.T.SSH.Validate(); err != nil {
			return nil, err
		}
	}
	// Before the pool strip: env decides the aws decoy.
	kinds := canaryKinds(r.T)
	build := r // the base is shared by every VM of the template: built from all of it
	if o.pool {
		t := *r.T
		t.Repos, t.Agents, t.Env, t.MCP, t.Sync, t.SSH = nil, template.Agents{}, nil, nil, template.Sync{}, template.SSH{}
		t.Skills, t.Plugins = nil, nil
		stripped := *r
		stripped.T = &t
		r = &stripped
	}
	repos, forges, err := m.planRepos(append(append([]string(nil), r.T.Repos...), o.Repos...), o.ReadOnly)
	if err != nil {
		return nil, err
	}
	if err := checkAgent(r.T.SSH, forges); err != nil {
		return nil, err
	}
	var allow []string
	if egressMode == egress.ModeAllowlist && !o.pool {
		if allow, err = egress.ExpandPresets(r.T.Network.Presets); err != nil {
			return nil, err
		}
		for _, e := range r.T.Network.Allow {
			if _, err := egress.ParseEntry(e); err != nil {
				return nil, fmt.Errorf("network.allow: %w", err)
			}
			allow = append(allow, e)
		}
		allow = append(allow, agentHosts(r.T)...)
		allow = append(allow, forgeHosts(forges)...)
	}
	base, err := m.EnsureBase(ctx, build, o.Rebuild)
	if err != nil {
		return nil, err
	}
	// Hold the base shared until our state names it, so a concurrent --rebuild
	// (exclusive) either finishes first or sees this VM as a user.
	release, err := fsutil.RLock(base + ".lock")
	if err != nil {
		return nil, err
	}
	defer release()
	if _, err := os.Stat(base); err != nil {
		return nil, fmt.Errorf("base %s disappeared: %w", filepath.Base(base), err)
	}
	baseMB, err := qemu.VirtualSizeMB(base)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(m.P.VMs(), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(m.dir(name), 0o700); err != nil {
		return nil, err
	}
	s := &State{Name: name, Repos: repos, Forges: forges}
	defer func() {
		if err != nil {
			m.releaseForges(s)
			_ = os.RemoveAll(m.dir(name))
		}
	}()
	var pooled *State
	if !o.pool && !o.Rebuild {
		key := poolKey{r.T.Name, r.Distro.Name, base, res.CPUs, memMB, max(diskMB, baseMB), egressMode, r.User, r.T.HasDocker(),
			strings.Join(vmCaches(r.T), ","), strings.Join(kinds, ",")}
		if pooled, err = m.claimPool(name, key); err != nil {
			return nil, err
		}
	}
	instance := ""
	if pooled == nil {
		if err := m.keys(name).Generate(name); err != nil {
			return nil, err
		}
		if len(kinds) > 0 {
			if err := canary.Save(m.dir(name), canary.Generate(kinds)); err != nil {
				return nil, err
			}
		}
		if err := qemu.CreateOverlay(m.vm(name).path(diskFile), base, max(diskMB, baseMB)); err != nil {
			return nil, err
		}
	} else {
		if err := m.keys(name).Rename(name); err != nil {
			return nil, err
		}
		_ = os.Remove(filepath.Join(m.dir(name), egress.DenylistFile))
		instance = pooled.Instance
	}
	if instance, err = m.writeSeed(name, instance, r.User, egressMode, r.T.HasDocker(), vmCaches(r.T)); err != nil {
		return nil, err
	}
	if egressMode == egress.ModeAllowlist {
		if err := egress.WriteList(m.dir(name), egress.AllowlistFile, allow); err != nil {
			return nil, err
		}
		ec := egress.Config{OnDeny: r.T.Network.OnDeny}
		if o.pool { // denied while unclaimed: logged, never prompted
			ec.OnDeny = egress.OnDenyLog
		}
		if r.T.Network.PromptTimeout != "" {
			if ec.PromptTimeout, err = time.ParseDuration(r.T.Network.PromptTimeout); err != nil {
				return nil, err
			}
		}
		if r.T.Network.AllowPrivate != nil {
			ec.AllowPrivate = *r.T.Network.AllowPrivate
		}
		if err := egress.WriteConfig(m.dir(name), ec); err != nil {
			return nil, err
		}
	}
	if err := m.provisionForges(ctx, s); err != nil {
		return nil, err
	}
	*s = State{
		Name: name, Template: r.T.Name, Picks: true, Distro: r.Distro.Name, User: r.User, Base: base,
		CPUs: res.CPUs, MemoryMB: memMB, DiskMB: max(diskMB, baseMB), Ports: r.T.Ports,
		Egress: egressMode, Docker: r.T.HasDocker(), Created: time.Now().UTC(),
		Agents: r.T.Agents, Env: r.T.Env, MCP: r.T.MCP, Skills: r.T.Skills, Plugins: r.T.Plugins, Sync: r.T.Sync,
		Repos: s.Repos, Forges: s.Forges, ForgeUsers: s.ForgeUsers, SSH: r.T.SSH, Ephemeral: o.Ephemeral, Task: o.Task,
		IdleStop: r.T.IdleStop, Instance: instance, Pool: o.pool, Cache: vmCaches(r.T), Canaries: kinds,
	}
	if pooled != nil {
		s.Booted, s.Resume = true, pooled.Resume
	}
	if r.T.SSH.Mode() == template.AgentHost {
		m.logf("warning: %s forwards your host ssh-agent: anything in the VM can use all its keys while you're connected", name)
	}
	if err := m.save(s); err != nil {
		return nil, err
	}
	if err := m.WriteSSHConfig(); err != nil {
		return nil, err
	}
	from := ""
	if pooled != nil {
		from = ", from pool VM " + pooled.Name
	}
	m.logf("created %s (template %s, %s, %d CPU, %s RAM, %s disk, egress %s%s)",
		name, r.T.Name, r.Distro.Name, res.CPUs, res.Memory, res.Disk, egressMode, from)
	detail := map[string]any{"template": r.T.Name, "distro": r.Distro.Name, "egress": egressMode, "repos": len(s.Repos), "ssh_agent": s.SSH.Mode()}
	if pooled != nil {
		detail["pool_vm"] = pooled.Name
	}
	m.audit(audit.VMCreate, name, detail)
	return s, nil
}

// agentHosts are the API hosts the template's agents and remote MCP servers need.
func agentHosts(t *template.Template) []string {
	var hosts []string
	if t.Agents.Claude != nil {
		hosts = append(hosts, "api.anthropic.com:443")
	}
	if c := t.Agents.Codex; c != nil {
		switch c.Auth {
		case template.CodexNone:
		case template.CodexChatGPT:
			hosts = append(hosts, "chatgpt.com:443", "auth.openai.com:443")
		default:
			hosts = append(hosts, "api.openai.com:443")
		}
	}
	for _, d := range template.AgentDefs {
		if a := *d.Get(&t.Agents); a != nil {
			for _, k := range a.Keys { // only agents without auth take keys
				hosts = append(hosts, template.Providers[k].Host+":443")
			}
		}
	}
	for _, s := range t.MCP {
		if u, err := url.Parse(s.URL); err == nil && u.Hostname() != "" {
			port := u.Port()
			if port == "" {
				port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
			}
			hosts = append(hosts, net.JoinHostPort(u.Hostname(), port))
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// writeSeed writes the NoCloud seed; id "" makes a new instance (cloud-init
// runs first boot again), the same id keeps it and just renames the host.
func (m *Manager) writeSeed(name, id, user, egressMode string, docker bool, caches []string) (string, error) {
	k := m.keys(name)
	pub, err := k.ClientPub()
	if err != nil {
		return "", err
	}
	hpriv, hpub, err := k.HostKeyPair()
	if err != nil {
		return "", err
	}
	cs, err := canary.Load(m.dir(name))
	if err != nil {
		return "", err
	}
	if id == "" {
		rnd := make([]byte, 4)
		_, _ = rand.Read(rnd)
		id = fmt.Sprintf("catchpen-%s-%s", name, hex.EncodeToString(rnd))
	}
	ud, err := cloudinit.InstanceUserData(cloudinit.Instance{
		Name: name, User: user, AuthorizedKey: pub,
		HostKey: hpriv, HostPub: hpub, Egress: egressMode, Docker: docker, Cache: caches, Canaries: canary.Files(cs),
	})
	if err != nil {
		return "", err
	}
	return id, cloudinit.WriteSeed(m.vm(name).path(seedFile), map[string][]byte{
		"user-data": ud,
		"meta-data": cloudinit.MetaData(id, name),
	})
}
