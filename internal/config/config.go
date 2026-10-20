// Package config loads ~/.config/curral/config.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/curral/internal/creds"
	"github.com/blackshieldpt/curral/internal/image"
	"github.com/blackshieldpt/curral/internal/notify"
	"github.com/blackshieldpt/curral/internal/template"
)

type Defaults struct {
	Distro    string             `yaml:"distro"`
	Template  string             `yaml:"template"`
	Resources template.Resources `yaml:"resources"`
	User      string             `yaml:"user"`
}

type Config struct {
	Defaults Defaults              `yaml:"defaults"`
	Images   map[string]image.Spec `yaml:"images"` // custom distros, by name
	// Secrets maps a secret name to where it comes from (default: env var, then secrets.env).
	Secrets map[string]creds.Source `yaml:"secrets"`
	// Record turns host-side session recording off when false (default on).
	Record *bool         `yaml:"record"`
	Notify notify.Config `yaml:"notify"`
	Tasks  TaskLimits    `yaml:"tasks"`
}

// TaskLimits is the budget headless tasks queue for: VMs running at once
// (all curral VMs count) and their total memory.
type TaskLimits struct {
	MaxVMs    int    `yaml:"max_vms"`    // default 2
	MaxMemory string `yaml:"max_memory"` // e.g. 16G; default: no limit
}

// MaxMemoryMB is the memory budget in MiB (0: none).
func (t TaskLimits) MaxMemoryMB() int64 {
	mb, _ := template.ParseSize(t.MaxMemory)
	return mb
}

// Recording reports whether interactive sessions are recorded.
func (c Config) Recording() bool { return c.Record == nil || *c.Record }

func Default() Config {
	return Config{Tasks: TaskLimits{MaxVMs: 2}, Defaults: Defaults{
		Distro:    "ubuntu-24.04",
		Template:  "default",
		Resources: template.Resources{CPUs: 4, Memory: "8G", Disk: "40G"},
		User:      "agent",
	}}
}

// Load reads path over the defaults; a missing file yields the defaults.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var f Config
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if f.Defaults.Distro != "" {
		c.Defaults.Distro = f.Defaults.Distro
	}
	if f.Defaults.Template != "" {
		c.Defaults.Template = f.Defaults.Template
	}
	if f.Defaults.User != "" {
		c.Defaults.User = f.Defaults.User
	}
	c.Defaults.Resources = c.Defaults.Resources.Override(f.Defaults.Resources)
	c.Images = f.Images
	c.Secrets = f.Secrets
	c.Record = f.Record
	c.Notify = f.Notify
	c.Tasks = f.Tasks
	if c.Tasks.MaxVMs == 0 {
		c.Tasks.MaxVMs = 2
	}
	if c.Tasks.MaxVMs < 0 {
		return c, fmt.Errorf("%s: tasks.max_vms must be positive", path)
	}
	if c.Tasks.MaxMemory != "" {
		if _, err := template.ParseSize(c.Tasks.MaxMemory); err != nil {
			return c, fmt.Errorf("%s: tasks.max_memory: %w", path, err)
		}
	}
	if err := c.Notify.Validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	for name, s := range c.Secrets {
		if (len(s.Command) == 0) == (s.Env == "") {
			return c, fmt.Errorf("%s: secrets.%s: set exactly one of command or env", path, name)
		}
	}
	if err := validUser(c.Defaults.User); err != nil {
		return c, fmt.Errorf("%s: defaults.user: %w", path, err)
	}
	return c, nil
}

var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func validUser(u string) error {
	if !userRe.MatchString(u) || u == "root" {
		return fmt.Errorf("invalid user name %q", u)
	}
	return nil
}

// Distro resolves a distro name against custom images then the built-in catalog.
func (c Config) Distro(name string) (image.Spec, error) {
	if s, ok := c.Images[name]; ok {
		s.Name = name
		return s, s.Validate()
	}
	return image.Builtin(name)
}
