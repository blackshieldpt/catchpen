// Package config loads ~/.config/curral/config.yaml.
package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/curral/internal/creds"
	"github.com/blackshieldpt/curral/internal/image"
	"github.com/blackshieldpt/curral/internal/notify"
	"github.com/blackshieldpt/curral/internal/template"
)

type Defaults struct {
	User string `yaml:"user"`
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
	// IdleStop stops VMs idle this long (curral daemon); templates may override it.
	IdleStop string `yaml:"idle_stop"`
	// Rebuild is how often curral daemon rebuilds the bases in use.
	Rebuild string      `yaml:"rebuild"`
	Pool    []PoolEntry `yaml:"pool"`
	// CacheMax caps the package caches (default 20G); the least recently used objects go first.
	CacheMax string `yaml:"cache_max"`
	// CanaryAction is what a canary hit does to the VM: pause (default), stop or log.
	CanaryAction string `yaml:"canary_action"`
}

// PoolEntry asks for Size booted, stopped VMs of a template (a -t spec), ready
// for up and task to claim.
type PoolEntry struct {
	Template string `yaml:"template"`
	Size     int    `yaml:"size"`
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

const defaultCacheMax = "20G"

// Canary actions.
const (
	CanaryPause = "pause"
	CanaryStop  = "stop"
	CanaryLog   = "log"
)

// CacheMaxBytes is the package caches' size limit.
func (c Config) CacheMaxBytes() int64 {
	mb, _ := template.ParseSize(cmp.Or(c.CacheMax, defaultCacheMax))
	return mb << 20
}

// Recording reports whether interactive sessions are recorded.
func (c Config) Recording() bool { return c.Record == nil || *c.Record }

func Default() Config {
	return Config{Tasks: TaskLimits{MaxVMs: 2}, Defaults: Defaults{User: "agent"}}
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
		for _, k := range []string{"distro", "template", "resources"} {
			if strings.Contains(err.Error(), "field "+k+" not found") {
				return c, fmt.Errorf("%s: %w (defaults.distro, defaults.template, defaults.resources and pool distro are gone: set the defaults in templates/distros.yaml and resources.yaml, and give a pool entry's distro in its template, e.g. \"debian-13,go-1.27\")", path, err)
			}
		}
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if f.Defaults.User != "" {
		c.Defaults.User = f.Defaults.User
	}
	c.Images = f.Images
	c.Secrets = f.Secrets
	c.Record = f.Record
	c.Notify = f.Notify
	c.Tasks = f.Tasks
	c.IdleStop, c.Rebuild, c.Pool = f.IdleStop, f.Rebuild, f.Pool
	c.CacheMax = f.CacheMax
	switch c.CanaryAction = f.CanaryAction; c.CanaryAction {
	case "", CanaryPause, CanaryStop, CanaryLog:
	default:
		return c, fmt.Errorf("%s: canary_action must be pause, stop or log", path)
	}
	if _, err := template.ParseSize(cmp.Or(c.CacheMax, defaultCacheMax)); err != nil {
		return c, fmt.Errorf("%s: cache_max: %w", path, err)
	}
	if _, err := template.ParseInterval(c.IdleStop); err != nil {
		return c, fmt.Errorf("%s: idle_stop: %w", path, err)
	}
	if _, err := template.ParseInterval(c.Rebuild); err != nil {
		return c, fmt.Errorf("%s: rebuild: %w", path, err)
	}
	seen := map[string]bool{}
	for i, e := range c.Pool {
		if e.Size < 0 || e.Size > 32 {
			return c, fmt.Errorf("%s: pool: each entry needs a size of 0-32", path)
		}
		c.Pool[i].Template = template.NormalizeSpec(e.Template)
		if seen[c.Pool[i].Template] {
			return c, fmt.Errorf("%s: pool: template %s listed twice", path, c.Pool[i].Template)
		}
		seen[c.Pool[i].Template] = true
	}
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
