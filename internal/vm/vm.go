// Package vm implements the VM lifecycle: bases, create, start, stop, destroy.
package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/egress"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/fsutil"
	"github.com/blackshieldpt/curral/internal/hosttmux"
	"github.com/blackshieldpt/curral/internal/notify"
	"github.com/blackshieldpt/curral/internal/paths"
	"github.com/blackshieldpt/curral/internal/qemu"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
)

const (
	buildDiskMB  = 20 << 10
	buildTimeout = 45 * time.Minute
	bootTimeout  = 10 * time.Minute
	stopTimeout  = 60 * time.Second
	pushTimeout  = 3 * time.Minute
	maxBuildLog  = 256 << 20 // a build whose serial logs grow past this is aborted
	maxCPUs      = 256
)

// Files in a VM directory.
const (
	stateFile   = "vm.json"
	runtimeFile = "runtime.json"
	lockFile    = ".lock"
	diskFile    = "disk.qcow2"
	seedFile    = "seed.iso"
	pidFile     = "qemu.pid"
	qmpSock     = "qmp.sock"
	serialSock  = "serial.sock"
	qemuLog     = "qemu.log"
	sshDir      = "ssh"
)

// State is persisted as vms/<name>/vm.json.
type State struct {
	Name     string          `json:"name"`
	Template string          `json:"template"`
	Distro   string          `json:"distro"`
	User     string          `json:"user"`
	Base     string          `json:"base"`
	CPUs     int             `json:"cpus"`
	MemoryMB int64           `json:"memory_mb"`
	DiskMB   int64           `json:"disk_mb"`
	Ports    []template.Port `json:"ports,omitempty"`
	Egress   string          `json:"egress"`
	Docker   bool            `json:"docker"`
	Created  time.Time       `json:"created"`
	Booted   bool            `json:"booted"` // first boot (cloud-init) completed

	// Pushed into the guest at every start (secrets resolved on the host then).
	Agents template.Agents   `json:"agents"`
	Env    map[string]string `json:"env,omitempty"`
	MCP    []template.MCP    `json:"mcp,omitempty"`
	Sync   template.Sync     `json:"sync"`

	Repos      []RepoState            `json:"repos,omitempty"`
	Forges     map[string]forge.Forge `json:"forges,omitempty"`      // snapshot of the forges Repos use
	ForgeUsers map[string]string      `json:"forge_users,omitempty"` // token owner per forge (mode token)
	SSH        template.SSH           `json:"ssh"`
	Ephemeral  bool                   `json:"ephemeral,omitempty"` // destroyed when its up --ephemeral session ends
	Task       string                 `json:"task,omitempty"`      // id of the headless task it runs
	IdleStop   string                 `json:"idle_stop,omitempty"` // the template's idle_stop
	Instance   string                 `json:"instance,omitempty"`  // cloud-init instance-id
	Pool       bool                   `json:"pool,omitempty"`      // a warm pool VM, not claimed yet
	Resume     string                 `json:"resume,omitempty"`    // memory snapshot the next start resumes
	Cache      []string               `json:"cache,omitempty"`     // package caches it uses
	Canaries   []string               `json:"canaries,omitempty"`  // canary kinds planted (values: canaries.json)
}

// Runtime describes a running VM.
type Runtime struct {
	PID      int            `json:"pid"`
	SSHPort  int            `json:"ssh_port"`
	Forwards []qemu.Forward `json:"forwards,omitempty"`
}

type Manager struct {
	P     paths.Paths
	Cfg   config.Config
	Tpl   template.Loader
	Out   io.Writer      // progress messages
	In    io.Reader      // terminal input for key passphrases (nil: none)
	Tmux  *hosttmux.Tmux // host tmux session to keep in sync (nil: none)
	Bin   string         // absolute path of the curral binary (ProxyCommand, guestfwd)
	Debug bool           // stream the whole build log; log the serial console; keep failed VMs running
	HTTP  *http.Client   // forge API client (nil: default)
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$|^[a-z]$`)

func ValidName(n string) error {
	if !nameRe.MatchString(n) {
		return fmt.Errorf("invalid VM name %q: use lowercase letters, digits and dashes (max 32)", n)
	}
	return nil
}

func (m *Manager) logf(format string, a ...any) {
	if m.Out != nil {
		fmt.Fprintf(m.Out, format+"\n", a...)
	}
}

func (m *Manager) dir(name string) string { return m.P.VM(name) }

func (m *Manager) tmuxDo(fn func(hosttmux.Tmux) error) {
	if m.Tmux == nil {
		return
	}
	if err := fn(*m.Tmux); err != nil {
		m.logf("warning: host tmux: %v", err)
	}
}

// vmCaches are the caches a VM of t uses: none with egress none, docker
// only with docker.
func vmCaches(t *template.Template) []string {
	if t.Network.Egress == egress.ModeNone {
		return nil
	}
	var out []string
	for _, c := range t.Cache {
		if c != "docker" || t.HasDocker() {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// canaryKinds are the canaries a VM of t gets: no aws decoy when the
// template gives it AWS settings, which its endpoint_url would redirect.
func canaryKinds(t *template.Template) []string {
	aws := false
	for k := range t.Env {
		aws = aws || strings.HasPrefix(k, "AWS_")
	}
	out := slices.DeleteFunc(slices.Clone(t.Canaries), func(k string) bool { return aws && k == "aws" })
	slices.Sort(out)
	return slices.Compact(out)
}

// buildCaches are the caches a base build of t uses: docker isn't running
// with the mirror configured during it.
func buildCaches(t *template.Template) []string {
	return slices.DeleteFunc(vmCaches(t), func(c string) bool { return c == "docker" })
}

func (m *Manager) cacheCmd(dir, name string, caches []string) []string {
	return []string{m.Bin, "cache-stdio", "--dir", dir, "--vm", name, "--store", m.P.Pkg(), "--kinds", strings.Join(caches, ","),
		"--max", strconv.FormatInt(m.Cfg.CacheMaxBytes(), 10)}
}

func notifyCmd(bin, dir, name string, task bool) []string {
	c := []string{bin, "notify-stdio", "--dir", dir, "--vm", name}
	if task {
		c = append(c, "--task") // headless: the task's own result is notified instead
	}
	return c
}

// Notifier delivers events to the sinks in config.yaml notify:.
func (m *Manager) Notifier() *notify.Notifier {
	return &notify.Notifier{Cfg: m.Cfg.Notify, Secrets: m.resolver()}
}

// notify sends an event; failures are warnings.
func (m *Manager) notify(e notify.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Notifier().Send(ctx, e); err != nil {
		m.logf("warning: notification: %v", err)
	}
}

// Audit is the host audit log.
func (m *Manager) Audit() audit.Log { return audit.Log{Path: m.P.Audit()} }

// audit records an event in the host audit log; failures are warnings.
func (m *Manager) audit(event, vm string, detail map[string]any) {
	if err := (audit.Log{Path: m.P.Audit()}).Record(event, vm, detail); err != nil {
		m.logf("warning: audit log: %v", err)
	}
}

// For internal/runner, which runs tasks on the Manager's VMs.

// Logf writes a progress line.
func (m *Manager) Logf(format string, a ...any) { m.logf(format, a...) }

// Notify sends an event; failures are warnings.
func (m *Manager) Notify(e notify.Event) { m.notify(e) }

// AuditEvent records an event in the audit log; failures are warnings.
func (m *Manager) AuditEvent(event, vm string, detail map[string]any) { m.audit(event, vm, detail) }

// Hit reports whether the VM's canaries were found in use.
func (m *Manager) Hit(name string) bool { return m.vm(name).hit() }

// DestroyWithoutTranscripts destroys a VM whose transcripts were copied.
func (m *Manager) DestroyWithoutTranscripts(name string) error { return m.destroy(name, false) }

// IsCommitID reports whether s is a full git commit id.
func IsCommitID(s string) bool { return commitID.MatchString(s) }
func (m *Manager) keys(name string) sshx.Keys {
	return sshx.Keys{Dir: filepath.Join(m.dir(name), sshDir)}
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

func (m *Manager) Load(name string) (*State, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	return m.vm(name).load()
}

func (m *Manager) save(s *State) error { return m.vm(s.Name).save(s) }

// Running returns the runtime of a running VM.
func (m *Manager) Running(name string) (*Runtime, bool) { return m.vm(name).running() }

// readTail returns at most the last n bytes of a file.
func readTail(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := max(st.Size()-n, 0)
	b := make([]byte, st.Size()-off)
	_, err = f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(b), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n\x00 "), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// baseUsers lists VMs whose disk is an overlay on base. A VM directory whose
// state can't be read counts as a user: its disk may well depend on base.
func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Listing --------------------------------------------------------------------

type Info struct {
	State
	Running bool
	Paused  bool // by a canary hit
	Hit     bool // its canaries were used
	Runtime *Runtime
}

func (m *Manager) List() ([]Info, error) {
	ents, err := os.ReadDir(m.P.VMs())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		if !e.IsDir() || ValidName(e.Name()) != nil {
			continue
		}
		s, err := m.Load(e.Name())
		if err != nil {
			continue
		}
		rt, ok := m.Running(e.Name())
		out = append(out, Info{State: *s, Running: ok, Paused: ok && m.Paused(e.Name()), Hit: m.vm(e.Name()).hit(), Runtime: rt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) WriteSSHConfig() error {
	// Serialize regeneration: each writer lists the VMs while holding the lock,
	// so the last write always reflects the current set.
	unlock, err := fsutil.Lock(m.P.SSHConfig() + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	vms, err := m.List()
	if err != nil {
		return err
	}
	var es []sshx.Entry
	for _, v := range vms {
		if v.Pool {
			continue
		}
		e := sshx.Entry{Name: v.Name, User: v.User, Keys: m.keys(v.Name)}
		switch v.SSH.Mode() {
		case template.AgentRestricted:
			e.ForwardAgent = m.agent(v.Name).Socket()
		case template.AgentHost:
			e.ForwardAgent = "$SSH_AUTH_SOCK"
		}
		es = append(es, e)
	}
	return sshx.WriteConfig(m.P.SSHConfig(), m.Bin, es)
}

// SSHPort returns the loopback port of a running VM's sshd.
func (m *Manager) SSHPort(name string) (int, error) {
	if err := ValidName(name); err != nil {
		return 0, err
	}
	rt, ok := m.Running(name)
	if !ok {
		return 0, fmt.Errorf("VM %s is not running (start it: curral start %s)", name, name)
	}
	if rt.SSHPort == 0 {
		return 0, fmt.Errorf("VM %s: runtime state missing", name)
	}
	if m.Paused(name) {
		return 0, fmt.Errorf("VM %s was paused by a canary hit (curral resume %s, or curral destroy %s)", name, name, name)
	}
	return rt.SSHPort, nil
}

func (m *Manager) Dir(name string) string          { return m.dir(name) }
func (m *Manager) SerialSocket(name string) string { return m.vm(name).path(serialSock) }
