// Package forge manages GitHub and Gitea forges: the forges.yaml registry,
// repo reference resolution, host-side API calls (deploy keys, tokens, pull
// requests) and pinned SSH host keys.
package forge

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

const (
	GitHub = "github"
	Gitea  = "gitea"
)

// Auth modes.
const (
	ModeDeployKey   = "deploy-key"   // per-VM, per-repo SSH key added via the API
	ModeMintedToken = "minted-token" // Gitea: per-VM token minted via the API
	ModeToken       = "token"        // a user-supplied token given to the VM
	ModeNone        = "none"         // anonymous HTTPS clone; work leaves via curral pull/pr
	ModeAgent       = "agent"        // SSH with keys from the VM's forwarded (restricted) agent; nothing stored
)

// UsesSSH reports whether the VM reaches the forge over SSH.
func (f Forge) UsesSSH() bool { return f.Auth.Mode == ModeDeployKey || f.Auth.Mode == ModeAgent }

// AgentAlias is the guest SSH alias and agent-constraint name for an agent-mode forge.
func (f Forge) AgentAlias() string { return "curral-forge-" + f.Name }

type Auth struct {
	Mode     string   `yaml:"mode" json:"mode"`
	Token    string   `yaml:"token,omitempty" json:"token,omitempty"`       // secret:NAME; API token (host side) or the VM's token (mode token)
	User     string   `yaml:"user,omitempty" json:"user,omitempty"`         // Gitea minted-token: account to mint for
	Password string   `yaml:"password,omitempty" json:"password,omitempty"` // Gitea minted-token: secret:NAME (Gitea mints only with basic auth)
	Scopes   []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`     // minted-token scopes
}

type Forge struct {
	Name     string   `yaml:"name" json:"name"`
	Type     string   `yaml:"type" json:"type"`
	Host     string   `yaml:"host" json:"host"`
	HTTPURL  string   `yaml:"http_url,omitempty" json:"http_url,omitempty"` // default https://<host>
	SSHPort  int      `yaml:"ssh_port,omitempty" json:"ssh_port,omitempty"` // default 22
	HostKeys []string `yaml:"host_keys,omitempty" json:"host_keys,omitempty"`
	Auth     Auth     `yaml:"auth" json:"auth"`
}

func (f Forge) WebURL() string {
	if f.HTTPURL != "" {
		return strings.TrimRight(f.HTTPURL, "/")
	}
	return "https://" + f.Host
}

func (f Forge) Port() int {
	if f.SSHPort != 0 {
		return f.SSHPort
	}
	return 22
}

// APIURL is the REST API base.
func (f Forge) APIURL() string {
	if f.Type == GitHub {
		if f.Host == "github.com" {
			return "https://api.github.com"
		}
		return f.WebURL() + "/api/v3"
	}
	return f.WebURL() + "/api/v1"
}

// Hosts are the host:port entries a VM using this forge must reach.
func (f Forge) Hosts() []string {
	u, _ := url.Parse(f.WebURL())
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	hosts := []string{net.JoinHostPort(u.Hostname(), port)}
	if f.UsesSSH() {
		hosts = append(hosts, net.JoinHostPort(f.Host, strconv.Itoa(f.Port())))
	}
	if f.Type == GitHub && f.Host == "github.com" {
		hosts = append(hosts, "api.github.com:443", "codeload.github.com:443", "objects.githubusercontent.com:443")
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

var (
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	hostRe  = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	scopeRe = regexp.MustCompile(`^[a-z:_-]{1,64}$`)
	userRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

func (f Forge) Validate() error {
	if !nameRe.MatchString(f.Name) {
		return fmt.Errorf("forge: invalid name %q", f.Name)
	}
	if f.Type != GitHub && f.Type != Gitea {
		return fmt.Errorf("forge %s: type must be github or gitea", f.Name)
	}
	if !hostRe.MatchString(f.Host) {
		return fmt.Errorf("forge %s: invalid host %q", f.Name, f.Host)
	}
	if f.HTTPURL != "" {
		u, err := url.Parse(f.HTTPURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
			return fmt.Errorf("forge %s: invalid http_url %q", f.Name, f.HTTPURL)
		}
	}
	if f.SSHPort < 0 || f.SSHPort > 65535 {
		return fmt.Errorf("forge %s: invalid ssh_port", f.Name)
	}
	ref := func(field, v string) error {
		if v == "" {
			return nil
		}
		if n, ok := strings.CutPrefix(v, "secret:"); !ok || !regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`).MatchString(n) {
			return fmt.Errorf("forge %s: auth.%s must be secret:NAME", f.Name, field)
		}
		return nil
	}
	if err := errors.Join(ref("token", f.Auth.Token), ref("password", f.Auth.Password)); err != nil {
		return err
	}
	switch f.Auth.Mode {
	case ModeNone, ModeAgent:
	case ModeToken, ModeDeployKey:
		if f.Auth.Token == "" {
			return fmt.Errorf("forge %s: auth.mode %s needs auth.token", f.Name, f.Auth.Mode)
		}
	case ModeMintedToken:
		if f.Type != Gitea {
			return fmt.Errorf("forge %s: minted-token is Gitea only (GitHub can't mint fine-grained tokens via the API)", f.Name)
		}
		if !userRe.MatchString(f.Auth.User) || f.Auth.Password == "" {
			return fmt.Errorf("forge %s: minted-token needs auth.user and auth.password", f.Name)
		}
		for _, s := range f.Auth.Scopes {
			if !scopeRe.MatchString(s) {
				return fmt.Errorf("forge %s: invalid scope %q", f.Name, s)
			}
		}
	default:
		return fmt.Errorf("forge %s: auth.mode must be deploy-key, minted-token, token, agent or none", f.Name)
	}
	if f.UsesSSH() && len(f.HostKeys) == 0 {
		return fmt.Errorf("forge %s: no pinned host keys (re-add it with curral forge add)", f.Name)
	}
	for _, k := range f.HostKeys {
		if _, err := ParseHostKey(k); err != nil {
			return fmt.Errorf("forge %s: host key: %w", f.Name, err)
		}
	}
	return nil
}

// Loopback reports whether the forge is on the host's loopback, which VMs can't
// reach (inside a VM, localhost is the VM).
func (f Forge) Loopback() bool {
	u, _ := url.Parse(f.WebURL())
	for _, h := range []string{f.Host, u.Hostname()} {
		if strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
			return true
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return false
}

// Registry is the forges.yaml file.
type Registry struct {
	Forges []Forge `yaml:"forges"`
}

func Load(path string) (Registry, error) {
	var r Registry
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, f := range r.Forges {
		if err := f.Validate(); err != nil {
			return r, fmt.Errorf("%s: %w", path, err)
		}
		if seen[f.Name] {
			return r, fmt.Errorf("%s: duplicate forge %q", path, f.Name)
		}
		seen[f.Name] = true
	}
	return r, nil
}

// Update applies fn to the registry under a lock and saves it.
func Update(path string, fn func(*Registry) error) error {
	unlock, err := fsutil.Lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	r, err := Load(path)
	if err != nil {
		return err
	}
	if err := fn(&r); err != nil {
		return err
	}
	for _, f := range r.Forges {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	b, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append([]byte("# managed by curral forge add/rm\n"), b...), 0o600)
}

func (r Registry) Get(name string) (Forge, bool) {
	for _, f := range r.Forges {
		if f.Name == name {
			return f, true
		}
	}
	return Forge{}, false
}

// Repo is a repository on a forge.
type Repo struct {
	Forge string `json:"forge"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

func (r Repo) Path() string   { return r.Owner + "/" + r.Name }
func (r Repo) String() string { return r.Forge + ":" + r.Path() }

var partRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

func repoPath(p string) (owner, name string, err error) {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	owner, name, ok := strings.Cut(p, "/")
	if !ok || !partRe.MatchString(owner) || !partRe.MatchString(name) || strings.HasPrefix(name, ".") || strings.HasPrefix(owner, ".") {
		return "", "", fmt.Errorf("expected owner/repo, got %q", p)
	}
	return owner, name, nil
}

// Resolve maps a repo reference to a configured forge: forge:owner/repo, or
// an https/ssh/scp-style URL whose host matches a forge.
func (r Registry) Resolve(ref string) (Forge, Repo, error) {
	var host, path string
	switch {
	case strings.Contains(ref, "://"):
		u, err := url.Parse(ref)
		if err != nil || u.Hostname() == "" {
			return Forge{}, Repo{}, fmt.Errorf("invalid repo URL %q", ref)
		}
		host, path = u.Hostname(), u.Path
	default:
		before, after, ok := strings.Cut(ref, ":")
		if !ok {
			return Forge{}, Repo{}, fmt.Errorf("repo %q: use forge:owner/repo or a URL", ref)
		}
		if f, ok := r.Get(before); ok && !strings.Contains(before, "@") {
			owner, name, err := repoPath(after)
			if err != nil {
				return Forge{}, Repo{}, fmt.Errorf("repo %q: %w", ref, err)
			}
			return f, Repo{Forge: f.Name, Owner: owner, Name: name}, nil
		}
		_, host, _ = strings.Cut(before, "@") // git@host:owner/repo
		if host == "" {
			host = before
		}
		path = after
	}
	owner, name, err := repoPath(path)
	if err != nil {
		return Forge{}, Repo{}, fmt.Errorf("repo %q: %w", ref, err)
	}
	for _, f := range r.Forges {
		wu, _ := url.Parse(f.WebURL())
		if strings.EqualFold(f.Host, host) || strings.EqualFold(wu.Hostname(), host) {
			return f, Repo{Forge: f.Name, Owner: owner, Name: name}, nil
		}
	}
	return Forge{}, Repo{}, r.noForge(ref, host)
}

// noForge explains that repos clone only from added forges, and how to add one.
func (r Registry) noForge(ref, host string) error {
	have := "no forges are configured"
	if len(r.Forges) > 0 {
		var hs []string
		for _, f := range r.Forges {
			hs = append(hs, f.Name+" ("+f.Host+")")
		}
		have = "configured forges: " + strings.Join(hs, ", ")
	}
	add := "curral forge add <name> --type github|gitea --host " + host + " --mode deploy-key|token|agent|none"
	if strings.EqualFold(host, "github.com") {
		add = "curral forge add github --type github --host github.com --mode deploy-key|token|agent|none"
	}
	return fmt.Errorf("repo %q: no forge for host %s (%s). curral clones only from forges added with "+
		"curral forge add; add one, then retry:\n  %s\nModes: https://blackshieldpt.github.io/curral/forges/#modes", ref, host, have, add)
}
