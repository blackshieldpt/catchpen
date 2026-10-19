package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")

	c, err := Load(p) // missing file
	if err != nil || c.Defaults.Distro != "ubuntu-24.04" || c.Defaults.User != "agent" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("empty file: %v", err)
	}

	cfg := `
defaults:
  distro: mydeb
  resources: { memory: 16G }
images:
  mydeb:
    url: https://example.com/deb.qcow2
    sha512: cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd
`
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.Resources.Memory != "16G" || c.Defaults.Resources.CPUs != 4 {
		t.Errorf("resources not merged: %+v", c.Defaults.Resources)
	}
	s, err := c.Distro("mydeb")
	if err != nil || s.Name != "mydeb" || s.SHA512 != "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd" {
		t.Errorf("custom distro: %+v %v", s, err)
	}
	if _, err := c.Distro("ubuntu-26.04"); err != nil {
		t.Errorf("builtin distro: %v", err)
	}
	if _, err := c.Distro("nope"); err == nil {
		t.Error("expected unknown distro error")
	}

	for _, bad := range []string{"defaults: { user: root }\n", "defaults: { user: \"a b\" }\n", "bogus: 1\n"} {
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
