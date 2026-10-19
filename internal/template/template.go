// Package template loads, merges and validates VM templates.
package template

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/curral/internal/egress"
)

//go:embed builtin
var builtinFS embed.FS

type Resources struct {
	CPUs   int    `yaml:"cpus,omitempty" json:"cpus,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
	Disk   string `yaml:"disk,omitempty" json:"disk,omitempty"`
}

// Override returns r with non-zero fields of o applied.
func (r Resources) Override(o Resources) Resources {
	if o.CPUs != 0 {
		r.CPUs = o.CPUs
	}
	if o.Memory != "" {
		r.Memory = o.Memory
	}
	if o.Disk != "" {
		r.Disk = o.Disk
	}
	return r
}

type Runtimes struct {
	Node   string `yaml:"node,omitempty" json:"node,omitempty"`     // major version, e.g. "24"
	Go     string `yaml:"go,omitempty" json:"go,omitempty"`         // e.g. "1.27.2"
	Python string `yaml:"python,omitempty" json:"python,omitempty"` // "uv"
}

type File struct {
	Src  string `yaml:"src" json:"src"`
	Dst  string `yaml:"dst" json:"dst"` // absolute, or ~/ for the guest user's home
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

type Network struct {
	Egress        string   `yaml:"egress,omitempty" json:"egress,omitempty"` // allowlist | open | none
	Presets       []string `yaml:"presets,omitempty" json:"presets,omitempty"`
	Allow         []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	OnDeny        string   `yaml:"on_deny,omitempty" json:"on_deny,omitempty"` // log | prompt
	PromptTimeout string   `yaml:"prompt_timeout,omitempty" json:"prompt_timeout,omitempty"`
	AllowPrivate  *bool    `yaml:"allow_private,omitempty" json:"allow_private,omitempty"`
}

// Port is a guest port forwarded to 127.0.0.1; Host 0 means pick a free port at start.
type Port struct {
	Host  int `json:"host"`
	Guest int `json:"guest"`
}

func (p *Port) UnmarshalYAML(n *yaml.Node) error {
	h, g, ok := strings.Cut(n.Value, ":")
	if !ok {
		h, g = "0", h
	}
	var err error
	if p.Host, err = strconv.Atoi(h); err != nil || p.Host < 0 || p.Host > 65535 {
		return fmt.Errorf("line %d: bad port %q", n.Line, n.Value)
	}
	if p.Guest, err = strconv.Atoi(g); err != nil || p.Guest < 1 || p.Guest > 65535 {
		return fmt.Errorf("line %d: bad port %q", n.Line, n.Value)
	}
	return nil
}

type Template struct {
	Name        string            `yaml:"name" json:"name"`
	Extends     string            `yaml:"extends,omitempty" json:"-"`
	Description string            `yaml:"description,omitempty" json:"-"`
	Distro      string            `yaml:"distro,omitempty" json:"distro,omitempty"`
	Resources   Resources         `yaml:"resources,omitempty" json:"resources"`
	Packages    []string          `yaml:"packages,omitempty" json:"packages,omitempty"`
	Docker      *bool             `yaml:"docker,omitempty" json:"docker,omitempty"`
	Runtimes    Runtimes          `yaml:"runtimes,omitempty" json:"runtimes"`
	Scripts     []string          `yaml:"scripts,omitempty" json:"scripts,omitempty"`
	Files       []File            `yaml:"files,omitempty" json:"files,omitempty"`
	Ports       []Port            `yaml:"ports,omitempty" json:"ports,omitempty"`
	Network     Network           `yaml:"network,omitempty" json:"network"`
	Agents      Agents            `yaml:"agents,omitempty" json:"agents"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"` // pushed at each start; literal or secret:NAME
	MCP         []MCP             `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	Sync        Sync              `yaml:"sync,omitempty" json:"sync"`

	// Source is "builtin" or the file path; assets resolve relative to it.
	Source string `yaml:"-" json:"-"`
	// assets holds script/file contents keyed by the reference used in the template.
	assets map[string][]byte
}

func (t *Template) HasDocker() bool { return t.Docker != nil && *t.Docker }

// Asset returns the content of a script or file src referenced by the template.
func (t *Template) Asset(ref string) []byte { return t.assets[ref] }

// Loader finds templates in the user dir first, then built-ins.
type Loader struct {
	UserDir string
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// List returns all template names with their source.
func (l Loader) List() (map[string]string, error) {
	out := map[string]string{}
	ents, _ := fs.ReadDir(builtinFS, "builtin")
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			out[n] = "builtin"
		}
	}
	ents, err := os.ReadDir(l.UserDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && nameRe.MatchString(n) {
			out[n] = filepath.Join(l.UserDir, e.Name())
		}
	}
	return out, nil
}

// raw loads one template file without resolving extends.
func (l Loader) raw(name string) (*Template, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid template name %q", name)
	}
	var (
		b      []byte
		fsys   fs.FS
		dir    string
		source string
	)
	p := filepath.Join(l.UserDir, name+".yaml")
	if data, err := os.ReadFile(p); err == nil {
		b, fsys, dir, source = data, os.DirFS(l.UserDir), ".", p
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	} else if data, err := fs.ReadFile(builtinFS, "builtin/"+name+".yaml"); err == nil {
		b, fsys, dir, source = data, builtinFS, "builtin", "builtin"
	} else {
		return nil, fmt.Errorf("template %q not found", name)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var t Template
	if err := dec.Decode(&t); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	if t.Name != "" && t.Name != name {
		return nil, fmt.Errorf("template %s: name field %q does not match file name", name, t.Name)
	}
	t.Name, t.Source = name, source
	t.assets = map[string][]byte{}
	refs := append([]string{}, t.Scripts...)
	for _, f := range t.Files {
		refs = append(refs, f.Src)
	}
	for _, r := range refs {
		clean := path.Clean(r)
		if path.IsAbs(clean) || strings.HasPrefix(clean, "..") || clean == "." {
			return nil, fmt.Errorf("template %s: asset %q must be a relative path inside the template directory", name, r)
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, clean))
		if err != nil {
			return nil, fmt.Errorf("template %s: asset %q: %w", name, r, err)
		}
		t.assets[r] = data
	}
	return &t, nil
}

// Load resolves name and its extends chain into one validated template.
func (l Loader) Load(name string) (*Template, error) {
	var chain []*Template
	seen := map[string]bool{}
	for n := name; n != ""; {
		if seen[n] {
			return nil, fmt.Errorf("template %s: extends cycle at %q", name, n)
		}
		seen[n] = true
		t, err := l.raw(n)
		if err != nil {
			return nil, err
		}
		chain = append(chain, t)
		n = t.Extends
	}
	res := &Template{assets: map[string][]byte{}}
	for i := len(chain) - 1; i >= 0; i-- {
		res.merge(chain[i])
	}
	res.Name, res.Source, res.Description = chain[0].Name, chain[0].Source, chain[0].Description
	if err := res.Validate(); err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	return res, nil
}

// merge applies o over t: scalars override, lists append (deduplicated).
func (t *Template) merge(o *Template) {
	if o.Distro != "" {
		t.Distro = o.Distro
	}
	t.Resources = t.Resources.Override(o.Resources)
	t.Packages = appendUniq(t.Packages, o.Packages...)
	if o.Docker != nil {
		t.Docker = o.Docker
	}
	if o.Runtimes.Node != "" {
		t.Runtimes.Node = o.Runtimes.Node
	}
	if o.Runtimes.Go != "" {
		t.Runtimes.Go = o.Runtimes.Go
	}
	if o.Runtimes.Python != "" {
		t.Runtimes.Python = o.Runtimes.Python
	}
	t.Scripts = appendUniq(t.Scripts, o.Scripts...)
	t.Files = append(t.Files, o.Files...)
	t.Ports = append(t.Ports, o.Ports...)
	if o.Network.Egress != "" {
		t.Network.Egress = o.Network.Egress
	}
	t.Network.Presets = appendUniq(t.Network.Presets, o.Network.Presets...)
	t.Network.Allow = appendUniq(t.Network.Allow, o.Network.Allow...)
	if o.Network.OnDeny != "" {
		t.Network.OnDeny = o.Network.OnDeny
	}
	if o.Network.PromptTimeout != "" {
		t.Network.PromptTimeout = o.Network.PromptTimeout
	}
	if o.Network.AllowPrivate != nil {
		t.Network.AllowPrivate = o.Network.AllowPrivate
	}
	t.mergeAgents(o)
	for k, v := range o.assets {
		t.assets[k] = v
	}
}

func appendUniq(a []string, b ...string) []string {
	for _, s := range b {
		dup := false
		for _, x := range a {
			if x == s {
				dup = true
				break
			}
		}
		if !dup {
			a = append(a, s)
		}
	}
	return a
}

var (
	pkgRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(=[A-Za-z0-9.+~:-]+)?$`)
	nodeRe = regexp.MustCompile(`^[0-9]{1,3}$`)
	goRe   = regexp.MustCompile(`^1\.[0-9]{1,3}(\.[0-9]{1,3})?$`)
	modeRe = regexp.MustCompile(`^0?[0-7]{3}$`)
	sizeRe = regexp.MustCompile(`^([0-9]+)([MGT])$`)
	dstRe  = regexp.MustCompile(`^(/|~/)[A-Za-z0-9._/@+-]+$`)
)

func (t *Template) Validate() error {
	for _, p := range t.Packages {
		if !pkgRe.MatchString(p) {
			return fmt.Errorf("invalid package name %q", p)
		}
	}
	if r := t.Runtimes; r.Node != "" && !nodeRe.MatchString(r.Node) {
		return fmt.Errorf("runtimes.node must be a major version, got %q", r.Node)
	}
	if r := t.Runtimes; r.Go != "" && !goRe.MatchString(r.Go) {
		return fmt.Errorf("runtimes.go must look like 1.27 or 1.27.2, got %q", r.Go)
	}
	if r := t.Runtimes; r.Python != "" && r.Python != "uv" {
		return fmt.Errorf(`runtimes.python must be "uv", got %q`, r.Python)
	}
	for _, f := range t.Files {
		if !dstRe.MatchString(f.Dst) || strings.Contains(f.Dst, "..") {
			return fmt.Errorf("files: invalid dst %q", f.Dst)
		}
		if f.Mode != "" && !modeRe.MatchString(f.Mode) {
			return fmt.Errorf("files: invalid mode %q", f.Mode)
		}
	}
	if t.Resources.CPUs < 0 || t.Resources.CPUs > 256 {
		return fmt.Errorf("resources.cpus out of range")
	}
	for _, s := range []string{t.Resources.Memory, t.Resources.Disk} {
		if s != "" {
			if _, err := ParseSize(s); err != nil {
				return err
			}
		}
	}
	switch t.Network.Egress {
	case "", egress.ModeAllowlist, egress.ModeOpen, egress.ModeNone:
	default:
		return fmt.Errorf("network.egress must be allowlist, open or none")
	}
	switch t.Network.OnDeny {
	case "", egress.OnDenyLog, egress.OnDenyPrompt:
	default:
		return fmt.Errorf("network.on_deny must be log or prompt")
	}
	if pt := t.Network.PromptTimeout; pt != "" {
		if d, err := time.ParseDuration(pt); err != nil || d <= 0 {
			return fmt.Errorf("network.prompt_timeout: invalid duration %q", pt)
		}
	}
	return t.validateAgents()
}

// ParseSize parses "512M", "8G", "1T" into MiB.
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (use e.g. 512M, 8G)", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	switch m[2] {
	case "G":
		n <<= 10
	case "T":
		n <<= 20
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n, nil
}

// YAML renders the resolved template (for `template show`).
func (t *Template) YAML() ([]byte, error) {
	c := *t
	c.Extends = ""
	return yaml.Marshal(&c)
}
