package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")

	c, err := Load(p) // missing file
	if err != nil || c.Defaults.User != "agent" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("empty file: %v", err)
	}

	cfg := `
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
	os.WriteFile(p, []byte("idle_stop: 30m\nrebuild: weekly\npool:\n  - { template: default, size: 2 }\n  - { template: \" debian-13, large \", size: 1 }\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleStop != "30m" || c.Rebuild != "weekly" || len(c.Pool) != 2 || c.Pool[1].Template != "debian-13,large" {
		t.Errorf("loaded: %+v", c)
	}
	if c.CacheMaxBytes() != 20<<30 {
		t.Errorf("default cache_max %d", c.CacheMaxBytes())
	}
	os.WriteFile(p, []byte("cache_max: 512M\n"), 0o600)
	if c, err := Load(p); err != nil || c.CacheMaxBytes() != 512<<20 {
		t.Errorf("cache_max: %d %v", c.CacheMaxBytes(), err)
	}
	for _, bad := range []string{
		"idle_stop: 10s\n", "rebuild: monthly\n", "pool: [{ size: 1 }, { template: default, size: 1 }]\n", "pool: [{ template: a, size: 99 }]\n",
		"pool: [{ template: a, size: 1 }, { template: a, size: 2 }]\n", "cache_max: 20GB\n", "cache_max: 9000000T\n", "canary_action: destroy\n",
	} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOldKeysExplained(t *testing.T) {
	for _, body := range []string{"defaults: { distro: debian-13 }\n", "defaults: { template: go }\n", "pool: [{ template: default, distro: debian-13, size: 1 }]\n"} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "are gone") {
			t.Errorf("%q: %v", body, err)
		}
	}
}
