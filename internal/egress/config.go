package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
)

const (
	OnDenyLog    = "log"
	OnDenyPrompt = "prompt"

	DefaultPromptTimeout = 30 * time.Second
)

// Config is the per-VM egress behaviour stored in egress.json.
type Config struct {
	OnDeny        string
	PromptTimeout time.Duration
	AllowPrivate  bool
	// AllowAll permits any public destination (denylist and private checks still apply).
	// Used for template base builds.
	AllowAll bool
}

type configJSON struct {
	OnDeny        string `json:"on_deny"`
	PromptTimeout string `json:"prompt_timeout"`
	AllowPrivate  bool   `json:"allow_private"`
	AllowAll      bool   `json:"allow_all,omitempty"`
}

func (c Config) MarshalJSON() ([]byte, error) {
	return json.Marshal(configJSON{c.OnDeny, c.PromptTimeout.String(), c.AllowPrivate, c.AllowAll})
}

func (c *Config) UnmarshalJSON(b []byte) error {
	var j configJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	c.OnDeny, c.AllowPrivate, c.AllowAll, c.PromptTimeout = j.OnDeny, j.AllowPrivate, j.AllowAll, 0
	if j.PromptTimeout != "" {
		d, err := time.ParseDuration(j.PromptTimeout)
		if err != nil {
			return fmt.Errorf("prompt_timeout: %w", err)
		}
		c.PromptTimeout = d
	}
	return nil
}

func (c *Config) applyDefaults() error {
	if c.OnDeny == "" {
		c.OnDeny = OnDenyLog
	}
	if c.OnDeny != OnDenyLog && c.OnDeny != OnDenyPrompt {
		return fmt.Errorf("on_deny must be %q or %q, got %q", OnDenyLog, OnDenyPrompt, c.OnDeny)
	}
	if c.PromptTimeout <= 0 {
		c.PromptTimeout = DefaultPromptTimeout
	}
	return nil
}

// WriteConfig writes dir/egress.json.
func WriteConfig(dir string, c Config) error {
	if err := c.applyDefaults(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, ConfigFile), append(b, '\n'), 0o600)
}

// ReadConfig reads dir/egress.json; a missing file yields defaults.
func ReadConfig(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", ConfigFile, err)
		}
	}
	return c, c.applyDefaults()
}
