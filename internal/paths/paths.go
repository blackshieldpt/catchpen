// Package paths resolves curral's XDG directories.
package paths

import (
	"os"
	"path/filepath"
)

type Paths struct {
	Config string // ~/.config/curral
	Data   string // ~/.local/share/curral
	Cache  string // ~/.cache/curral
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

// Default honours CURRAL_HOME (all dirs under one root, used by tests) and XDG vars.
func Default() Paths {
	if root := os.Getenv("CURRAL_HOME"); root != "" {
		return Paths{
			Config: filepath.Join(root, "config"),
			Data:   filepath.Join(root, "data"),
			Cache:  filepath.Join(root, "cache"),
		}
	}
	return Paths{
		Config: filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "curral"),
		Data:   filepath.Join(xdg("XDG_DATA_HOME", ".local/share"), "curral"),
		Cache:  filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "curral"),
	}
}

func (p Paths) Images() string        { return filepath.Join(p.Data, "images") }
func (p Paths) Bases() string         { return filepath.Join(p.Data, "bases") }
func (p Paths) VMs() string           { return filepath.Join(p.Data, "vms") }
func (p Paths) VM(name string) string { return filepath.Join(p.VMs(), name) }
func (p Paths) Templates() string     { return filepath.Join(p.Config, "templates") }
func (p Paths) ConfigFile() string    { return filepath.Join(p.Config, "config.yaml") }
func (p Paths) SSHConfig() string     { return filepath.Join(p.Config, "ssh_config") }

// Ensure creates the data/config directories with private permissions.
func (p Paths) Ensure() error {
	for _, d := range []string{p.Config, p.Templates(), p.Images(), p.Bases(), p.VMs(), p.Cache} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
