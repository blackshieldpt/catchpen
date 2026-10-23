// Package vm implements the VM lifecycle: bases, create, start, stop, destroy.
package vm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/curral/internal/agents"
	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/cache"
	"github.com/blackshieldpt/curral/internal/canary"
	"github.com/blackshieldpt/curral/internal/cloudinit"
	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/egress"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/fsutil"
	"github.com/blackshieldpt/curral/internal/hosttmux"
	"github.com/blackshieldpt/curral/internal/image"
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
func (m *Manager) keys(name string) sshx.Keys {
	return sshx.Keys{Dir: filepath.Join(m.dir(name), sshDir)}
}
func (m *Manager) file(name, f string) string { return filepath.Join(m.dir(name), f) }

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
	b, err := os.ReadFile(m.file(name, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no VM named %q (see: curral ls)", name)
	}
	if err != nil {
		return nil, err
	}
	var s State
	return &s, json.Unmarshal(b, &s)
}

func (m *Manager) save(s *State) error { return writeJSON(m.file(s.Name, stateFile), s) }

// Running returns the runtime of a running VM.
func (m *Manager) Running(name string) (*Runtime, bool) {
	pid, ok := qemu.Pid(m.file(name, pidFile), name)
	if !ok {
		return nil, false
	}
	var rt Runtime
	b, err := os.ReadFile(m.file(name, runtimeFile))
	if err != nil || json.Unmarshal(b, &rt) != nil {
		return &Runtime{PID: pid}, true
	}
	rt.PID = pid
	return &rt, true
}

// Template resolution ------------------------------------------------------

type Resolved struct {
	T      *template.Template
	Distro image.Spec
	User   string
	Hash   string
}

func (m *Manager) Resolve(tplName, distro string) (*Resolved, error) {
	t, err := m.Tpl.Load(tplName)
	if err != nil {
		return nil, err
	}
	d := distro
	if d == "" {
		d = t.Distro
	}
	if d == "" {
		d = m.Cfg.Defaults.Distro
	}
	spec, err := m.Cfg.Distro(d)
	if err != nil {
		return nil, err
	}
	t.Distro = d
	user := m.Cfg.Defaults.User
	// The caches a build uses aren't baked in: they don't make another base.
	ud, err := cloudinit.BuildUserData(t, user, nil)
	if err != nil {
		return nil, err
	}
	return &Resolved{T: t, Distro: spec, User: user, Hash: baseHash(spec, user, ud)}, nil
}

// baseHash identifies a base: the source image (URL and pin) plus the rendered
// build seed, which carries everything from the template that is baked in.
func baseHash(spec image.Spec, user string, buildUserData []byte) string {
	h := sha256.New()
	for _, f := range []string{spec.URL, spec.ChecksumURL, spec.SHA256, spec.SHA512, user, string(buildUserData)} {
		fmt.Fprintf(h, "%d:%s\x00", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// basePath is generation gen of the template base: <tpl>-<hash>.qcow2 for the
// first, <tpl>-<hash>.<gen>.qcow2 for rebuilds. A rebuild never replaces a
// file VMs may be overlays on.
func (r *Resolved) basePath(p paths.Paths, gen int) string {
	if gen <= 1 {
		return filepath.Join(p.Bases(), fmt.Sprintf("%s-%s.qcow2", r.T.Name, r.Hash))
	}
	return filepath.Join(p.Bases(), fmt.Sprintf("%s-%s.%d.qcow2", r.T.Name, r.Hash, gen))
}

// baseGen returns the newest built generation (0 if none).
func (r *Resolved) baseGen(p paths.Paths) int {
	gen := 0
	if _, err := os.Stat(r.basePath(p, 1)); err == nil {
		gen = 1
	}
	matches, _ := filepath.Glob(filepath.Join(p.Bases(), fmt.Sprintf("%s-%s.*.qcow2", r.T.Name, r.Hash)))
	for _, mt := range matches {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(mt), r.T.Name+"-"+r.Hash+"."), ".qcow2"))
		if err == nil && n > gen {
			gen = n
		}
	}
	return gen
}

// BasePath is the template's current base (it may not be built yet).
func (r *Resolved) BasePath(p paths.Paths) string { return r.basePath(p, max(r.baseGen(p), 1)) }

// EnsureBase builds the template base image if it does not exist yet.
func (m *Manager) EnsureBase(ctx context.Context, r *Resolved, rebuild bool) (string, error) {
	if err := m.P.Ensure(); err != nil {
		return "", err
	}
	// Builds of one template version serialize on their own lock, apart from
	// the per-base locks VMs and gc take.
	unlock, err := fsutil.Lock(strings.TrimSuffix(r.basePath(m.P, 1), ".qcow2") + ".build.lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	gen := r.baseGen(m.P)
	if gen > 0 && !rebuild {
		return r.basePath(m.P, gen), nil
	}
	// A rebuild is a new generation: VM disks are overlays on the old file.
	dst := r.basePath(m.P, gen+1)
	ensure := image.Ensure
	if rebuild {
		ensure = image.EnsureLatest
	}
	img, digest, err := ensure(ctx, m.P.Images(), r.Distro, m.Out)
	if err != nil {
		return "", err
	}
	m.logf("building base %s (%s, %s)", filepath.Base(dst), r.Distro.Name, digest)
	stem := strings.TrimSuffix(dst, ".qcow2")
	work := dst + ".building"
	seed := dst + ".seed.iso"
	logf := stem + ".build.log"
	consolef := stem + ".console.log"
	defer os.Remove(work)
	defer os.Remove(seed)
	if err := qemu.Convert(img, work, buildDiskMB); err != nil {
		return "", err
	}
	caches := buildCaches(r.T)
	ud, err := cloudinit.BuildUserData(r.T, r.User, caches)
	if err != nil {
		return "", err
	}
	if err := cloudinit.WriteSeed(seed, map[string][]byte{
		"user-data": ud,
		"meta-data": cloudinit.MetaData("curral-build-"+r.Hash, "curral-build"),
	}); err != nil {
		return "", err
	}
	_ = os.Remove(logf)
	_ = os.Remove(consolef)
	// The build VM gets the same isolation as runtime VMs, but its proxy allows
	// any public destination (package mirrors, installers). Private ranges stay blocked.
	edir := stem + ".egress"
	_ = os.RemoveAll(edir)
	if err := os.Mkdir(edir, 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(edir)
	if err := egress.WriteConfig(edir, egress.Config{AllowAll: true}); err != nil {
		return "", err
	}
	spec := qemu.Spec{
		Name: "build-" + r.T.Name, CPUs: min(runtime.NumCPU(), 4), MemoryMB: 4096,
		Disk: work, Seed: seed, Egress: egress.ModeAllowlist, SerialFile: consolef, LogFile: logf,
		ProxyCmd: []string{m.Bin, "egress-stdio", "--dir", edir, "--vm", "build-" + r.T.Name},
		ProxyIP:  cloudinit.ProxyAddr, ProxyPort: cloudinit.ProxyPort,
	}
	if len(caches) > 0 {
		spec.CacheCmd, spec.CacheIP, spec.CachePort = m.cacheCmd(edir, "build-"+r.T.Name, caches), cache.Addr, cache.Port
	}
	err = m.runForeground(ctx, spec, logf, consolef, buildTimeout)
	_ = os.Rename(filepath.Join(edir, egress.LogFile), stem+".egress.log")
	if err != nil {
		return "", fmt.Errorf("base build failed: %w (logs: %s, %s)", err, logf, consolef)
	}
	tail, err := readTail(logf, 4096)
	if err != nil {
		return "", err
	}
	switch lastLine(tail) {
	case cloudinit.BuildOK:
	case cloudinit.BuildFail:
		return "", fmt.Errorf("base build script failed; see %s", logf)
	default:
		return "", fmt.Errorf("base build ended without a result marker; see %s", logf)
	}
	if err := os.Rename(work, dst); err != nil {
		return "", err
	}
	if err := os.Chmod(dst, 0o400); err != nil {
		return "", err
	}
	m.logf("base ready: %s", dst)
	// The generation this one supersedes goes if nothing uses it, so repeated
	// rebuilds don't pile up bases.
	if gen > 0 {
		if prev := r.basePath(m.P, gen); m.removeBase(prev, false) {
			m.logf("removed the unused previous base %s", filepath.Base(prev))
		}
	}
	return dst, nil
}

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

func (m *Manager) baseUsers(base string) ([]string, error) {
	ents, err := os.ReadDir(m.P.VMs())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var users []string
	for _, e := range ents {
		if !e.IsDir() || ValidName(e.Name()) != nil {
			continue
		}
		s, err := m.Load(e.Name())
		if err != nil {
			users = append(users, e.Name()+" (unreadable state)")
			continue
		}
		// The disk is checked too: a reset or rebase interrupted between
		// changing the disk and saving the state leaves them disagreeing.
		backing, err := qemu.BackingFile(m.file(s.Name, diskFile))
		if s.Base == base || backing == base || (err != nil && fileExists(m.file(s.Name, diskFile))) {
			users = append(users, s.Name)
		}
	}
	return users, nil
}

// runForeground runs QEMU until it exits, echoing build progress lines.
func (m *Manager) runForeground(ctx context.Context, spec qemu.Spec, logf, consolef string, timeout time.Duration) error {
	bin, err := qemu.Binary()
	if err != nil {
		return err
	}
	args, err := spec.Args()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go m.follow(logf, done)
	go capLogs(done, abort, logf, consolef)
	err = cmd.Wait()
	close(done)
	if c := context.Cause(ctx); errors.Is(c, errLogTooLarge) {
		return c
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

var errLogTooLarge = fmt.Errorf("build log exceeded %d MiB", maxBuildLog>>20)

// capLogs aborts the build when a serial log QEMU writes grows past maxBuildLog.
func capLogs(done <-chan struct{}, abort context.CancelCauseFunc, files ...string) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		for _, f := range files {
			if st, err := os.Stat(f); err == nil && st.Size() > maxBuildLog {
				abort(errLogTooLarge)
				return
			}
		}
	}
}

// follow tails a serial log, printing curral-build lines (or everything in debug mode).
func (m *Manager) follow(path string, done <-chan struct{}) {
	var f *os.File
	for f == nil {
		var err error
		if f, err = os.Open(path); err != nil {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var partial string
	for {
		line, err := r.ReadString('\n')
		partial += line
		if err == nil {
			l := strings.TrimRight(partial, "\r\n")
			partial = ""
			if m.Debug || strings.Contains(l, "curral-build:") {
				if i := strings.Index(l, "curral-build:"); i >= 0 && !m.Debug {
					l = "  " + strings.TrimSpace(l[i+len("curral-build:"):])
				}
				m.logf("%s", sshx.Printable(l))
			}
			continue
		}
		select {
		case <-done:
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Create ---------------------------------------------------------------------

type CreateOpts struct {
	Template   string
	Distro     string
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
	tplName := o.Template
	if tplName == "" {
		tplName = m.Cfg.Defaults.Template
	}
	r, err := m.Resolve(tplName, o.Distro)
	if err != nil {
		return nil, err
	}
	if o.ClaudeAuth != "" {
		if r.T.Agents.Claude == nil {
			return nil, fmt.Errorf("template %s has no claude agent", r.T.Name)
		}
		c := *r.T.Agents.Claude
		c.Auth = o.ClaudeAuth
		r.T.Agents.Claude = &c
		if err := r.T.Validate(); err != nil {
			return nil, err
		}
	}
	res := m.Cfg.Defaults.Resources.Override(r.T.Resources).Override(template.Resources{CPUs: o.CPUs, Memory: o.Memory, Disk: o.Disk})
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
		return nil, fmt.Errorf("template %s uses network.egress: open (unfiltered internet and the host's 127.0.0.1 services); pass --allow-open to confirm", r.T.Name)
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
	if o.pool {
		r.T.Repos, r.T.Agents, r.T.Env, r.T.MCP, r.T.Sync, r.T.SSH = nil, template.Agents{}, nil, nil, template.Sync{}, template.SSH{}
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
	base, err := m.EnsureBase(ctx, r, o.Rebuild)
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
			strings.Join(vmCaches(r.T), ","), strings.Join(canaryKinds(r.T), ",")}
		if pooled, err = m.claimPool(name, key); err != nil {
			return nil, err
		}
	}
	instance := ""
	if pooled == nil {
		if err := m.keys(name).Generate(name); err != nil {
			return nil, err
		}
		if kinds := canaryKinds(r.T); len(kinds) > 0 {
			if err := canary.Save(m.dir(name), canary.Generate(kinds)); err != nil {
				return nil, err
			}
		}
		if err := qemu.CreateOverlay(m.file(name, diskFile), base, max(diskMB, baseMB)); err != nil {
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
		Name: name, Template: r.T.Name, Distro: r.Distro.Name, User: r.User, Base: base,
		CPUs: res.CPUs, MemoryMB: memMB, DiskMB: max(diskMB, baseMB), Ports: r.T.Ports,
		Egress: egressMode, Docker: r.T.HasDocker(), Created: time.Now().UTC(),
		Agents: r.T.Agents, Env: r.T.Env, MCP: r.T.MCP, Sync: r.T.Sync,
		Repos: s.Repos, Forges: s.Forges, ForgeUsers: s.ForgeUsers, SSH: r.T.SSH, Ephemeral: o.Ephemeral, Task: o.Task,
		IdleStop: r.T.IdleStop, Instance: instance, Pool: o.pool, Cache: vmCaches(r.T), Canaries: canaryKinds(r.T),
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
	for _, a := range []*template.Agent{t.Agents.OpenCode, t.Agents.Pi, t.Agents.OMP} {
		if a != nil {
			for _, k := range a.Keys {
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
		id = fmt.Sprintf("curral-%s-%s", name, hex.EncodeToString(rnd))
	}
	ud, err := cloudinit.InstanceUserData(cloudinit.Instance{
		Name: name, User: user, AuthorizedKey: pub,
		HostKey: hpriv, HostPub: hpub, Egress: egressMode, Docker: docker, Cache: caches, Canaries: canary.Files(cs),
	})
	if err != nil {
		return "", err
	}
	return id, cloudinit.WriteSeed(m.file(name, seedFile), map[string][]byte{
		"user-data": ud,
		"meta-data": cloudinit.MetaData(id, name),
	})
}

// Start ----------------------------------------------------------------------

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
	unlock, err := fsutil.Lock(m.file(name, lockFile))
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
		if from, err = m.takeResume(s); err != nil {
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
			_ = qemu.SnapshotOffline("-d", claimed, m.file(name, diskFile))
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
		_ = os.Remove(m.file(name, f))
	}
	spec := qemu.Spec{
		Name: name, CPUs: s.CPUs, MemoryMB: s.MemoryMB,
		Disk: m.file(name, diskFile), Seed: m.file(name, seedFile),
		SSHPort: rt.SSHPort, Forwards: rt.Forwards, Egress: s.Egress,
		ProxyCmd: []string{m.Bin, "egress-stdio", "--dir", m.dir(name), "--vm", name},
		ProxyIP:  cloudinit.ProxyAddr, ProxyPort: cloudinit.ProxyPort,
		NotifyCmd: notifyCmd(m.Bin, m.dir(name), name, s.Task != ""),
		NotifyIP:  cloudinit.NotifyAddr, NotifyPort: cloudinit.NotifyPort,
		QMP: m.file(name, qmpSock), Serial: m.file(name, serialSock), SerialLog: m.Debug,
		Pidfile: m.file(name, pidFile),
	}
	if len(s.Cache) > 0 {
		spec.CacheCmd, spec.CacheIP, spec.CachePort = m.cacheCmd(m.dir(name), name, s.Cache), cache.Addr, cache.Port
	}
	if from != nil {
		spec.LoadVM, spec.Machine = from.snap, from.machine
	}
	logFile, err := os.OpenFile(m.file(name, qemuLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "--- %s start\n", time.Now().Format(time.RFC3339))
	pid, err := qemu.Launch(spec, m.dir(name), logFile)
	if err != nil {
		dropClaimed()
		return nil, fmt.Errorf("%w (see %s)", err, m.file(name, qemuLog))
	}
	rt.PID = pid
	if err := writeJSON(m.file(name, runtimeFile), rt); err != nil {
		_ = m.stopLocked(name, true)
		return nil, err
	}
	locked = false
	unlock()

	m.logf("started %s (pid %d), waiting for ssh", name, pid)
	m.audit(audit.VMStart, name, map[string]any{"pid": pid})
	wctx, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	alive := func() bool { _, ok := qemu.Pid(m.file(name, pidFile), name); return ok }
	failed := func(err error) (*Runtime, error) {
		if m.Debug {
			return nil, fmt.Errorf("%s: %w (VM left running for debugging; serial log: %s)", name, err, m.file(name, serialSock+".log"))
		}
		_ = m.Stop(name, true)
		dropClaimed()
		return nil, fmt.Errorf("%s: %w (VM stopped; retry with --debug to keep it running and log the console)", name, err)
	}
	c, err := m.keys(name).WaitReady(wctx, rt.SSHPort, s.User, alive)
	if err != nil {
		return failed(err)
	}
	defer c.Close()
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
			return failed(fmt.Errorf("first boot: %w\n%s", err, sshx.Printable(strings.TrimSpace(string(out)))))
		}
		if err := m.markBooted(name); err != nil {
			return nil, err
		}
	}
	if s.Pool {
		return rt, nil
	}
	if err := m.push(ctx, c, s); err != nil {
		return rt, fmt.Errorf("%s is running, but pushing credentials failed: %w (retry: curral creds push %s)", name, err, name)
	}
	m.logf("%s is ready: curral ssh %s", name, name)
	m.tmuxDo(func(t hosttmux.Tmux) error { return t.Add(name) })
	return rt, nil
}

// Push delivers credentials and agent config into a running VM.
func (m *Manager) Push(ctx context.Context, name string) error {
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	port, err := m.SSHPort(name)
	if err != nil {
		return err
	}
	c, err := m.keys(name).Dial(ctx, port, s.User)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := m.push(ctx, c, s); err != nil {
		return err
	}
	m.logf("pushed credentials to %s", name)
	return nil
}

type sshRemote struct{ c *ssh.Client }

func (r sshRemote) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, error) {
	return sshx.Run(ctx, r.c, cmd, bytes.NewReader(stdin))
}

func (m *Manager) push(ctx context.Context, c *ssh.Client, s *State) error {
	defer m.closeOnPause(ctx, s.Name, c)()
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
	res, err := agents.Push(ctx, sshRemote{c}, spec, m.resolver())
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
	return m.pushForges(cloneCtx, sshRemote{c}, s, gcreds)
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
	unlock, err := fsutil.Lock(m.file(name, lockFile))
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

// Stop / destroy -------------------------------------------------------------

func (m *Manager) Stop(name string, force bool) error {
	if _, err := m.Load(name); err != nil {
		return err
	}
	unlock, err := fsutil.Lock(m.file(name, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	return m.stopLocked(name, force)
}

func (m *Manager) stopLocked(name string, force bool) error {
	pidfile := m.file(name, pidFile)
	pid, ok := qemu.Pid(pidfile, name)
	if !ok {
		return nil
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
	if q, err := qemu.DialQMP(m.file(name, qmpSock), 5*time.Second); err == nil {
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
	_ = m.agent(name).Stop()
	for _, f := range []string{runtimeFile, qmpSock, serialSock, pidFile, pausedFile} {
		_ = os.Remove(m.file(name, f))
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
		return fmt.Errorf("no VM named %q (see: curral ls)", name)
	}
	if transcripts {
		m.saveTranscripts(name)
	}
	unlock, err := fsutil.Lock(m.file(name, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.stopLocked(name, true); err != nil {
		return err
	}
	if s, err := m.Load(name); err == nil {
		m.releaseForges(s)
	} else if _, err := os.Stat(m.file(name, forgeDir)); err == nil {
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
		out = append(out, Info{State: *s, Running: ok, Paused: ok && m.Paused(e.Name()), Hit: fileExists(m.file(e.Name(), hitFile)), Runtime: rt})
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
func (m *Manager) SerialSocket(name string) string { return m.file(name, serialSock) }
