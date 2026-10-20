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

func TestLoadRecordNotifyTasks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	c, _ := Load(p)
	if !c.Recording() || c.Tasks.MaxVMs != 2 {
		t.Errorf("defaults: record %v max_vms %d", c.Recording(), c.Tasks.MaxVMs)
	}
	os.WriteFile(p, []byte("record: false\nnotify: { sinks: [desktop], events: [task.done] }\ntasks: { max_vms: 3, max_memory: 24G }\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Recording() || len(c.Notify.Sinks) != 1 || c.Tasks.MaxVMs != 3 || c.Tasks.MaxMemoryMB() != 24*1024 {
		t.Errorf("loaded: %+v", c)
	}
	for _, bad := range []string{"notify: { events: [nope] }\n", "tasks: { max_memory: lots }\n", "tasks: { max_vms: -1 }\n"} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLoadDaemonSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("idle_stop: 30m\nrebuild: weekly\npool:\n  - { template: default, size: 2 }\n  - { template: default, distro: debian-13, size: 1 }\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleStop != "30m" || c.Rebuild != "weekly" || len(c.Pool) != 2 || c.Pool[1].Distro != "debian-13" {
		t.Errorf("loaded: %+v", c)
	}
	for _, bad := range []string{
		"idle_stop: 10s\n", "rebuild: monthly\n", "pool: [{ size: 1 }]\n", "pool: [{ template: a, size: 99 }]\n",
		"pool: [{ template: a, size: 1 }, { template: a, size: 2 }]\n",
	} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
