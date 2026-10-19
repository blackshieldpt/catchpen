//go:build integration

// Boots a real VM from the minimal template. Needs KVM and network access.
// Reuses downloaded cloud images from CURRAL_TEST_IMAGES (default ~/.local/share/curral/images).
//
//	go test -tags integration -timeout 30m ./internal/vm/
package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/egress"
	"github.com/blackshieldpt/curral/internal/paths"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
)

func TestLifecycle(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURRAL_HOME", root)
	p := paths.Default()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	images := os.Getenv("CURRAL_TEST_IMAGES")
	if images == "" {
		home, _ := os.UserHomeDir()
		images = filepath.Join(home, ".local/share/curral/images")
	}
	if ents, err := os.ReadDir(images); err == nil {
		for _, e := range ents {
			_ = os.Symlink(filepath.Join(images, e.Name()), filepath.Join(p.Images(), e.Name()))
		}
	}
	bin := filepath.Join(root, "curral")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/curral").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	m := &Manager{P: p, Cfg: config.Default(), Tpl: template.Loader{UserDir: p.Templates()}, Out: testWriter{t}, Bin: bin}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	if _, err := m.Create(ctx, "it1", CreateOpts{Template: "minimal"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("it1") })
	rt, err := m.Start(ctx, "it1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.keys("it1").Dial(ctx, rt.SSHPort, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	run := func(cmd string) string {
		t.Helper()
		out, err := sshx.Run(ctx, c, cmd, nil)
		if err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if got := run("sudo sshd -T | grep -E '^passwordauthentication'"); got != "passwordauthentication no" {
		t.Errorf("sshd: %q", got)
	}
	if got := run("sudo passwd -S agent | cut -d' ' -f2"); got != "L" {
		t.Errorf("password not locked: %q", got)
	}
	if got := run(`curl -s -o /dev/null -w '%{http_code}' https://api.anthropic.com/`); got == "000" {
		t.Error("allowlisted host unreachable")
	}
	if got := run(`curl -s -o /dev/null -w '%{http_code}' http://example.com/`); got != "403" {
		t.Errorf("non-allowlisted host: %q, want 403", got)
	}
	if got := run(`curl -s --noproxy '*' -m 5 -o /dev/null -w '%{http_code}' https://1.1.1.1/ || true`); got != "000" {
		t.Errorf("direct egress not blocked: %q", got)
	}
	// Without restrict=on, 10.0.2.2 is the host's loopback: probe a listener we own.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	probe := fmt.Sprintf(`timeout 5 bash -c 'echo > /dev/tcp/10.0.2.2/%d' 2>/dev/null && echo open || echo blocked`, ln.Addr().(*net.TCPAddr).Port)
	if got := run(probe); got != "blocked" {
		t.Errorf("host loopback reachable from guest: %q", got)
	}
	if got := run(`getent ahosts example.com >/dev/null && echo resolved || echo none`); got != "none" {
		t.Errorf("guest DNS resolves names directly: %q", got)
	}
	// The generated ssh_config (ProxyCommand through the curral binary) works.
	if _, err := exec.LookPath("ssh"); err == nil {
		out, err := exec.CommandContext(ctx, "ssh", "-F", m.P.SSHConfig(), "-o", "BatchMode=yes", "curral-it1", "echo", "via-config").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "via-config" {
			t.Errorf("ssh via ssh_config: %v: %s", err, out)
		}
	}
	if err := egress.AddEntry(m.Dir("it1"), egress.AllowlistFile, "example.com"); err != nil {
		t.Fatal(err)
	}
	if got := run(`curl -s -o /dev/null -w '%{http_code}' http://example.com/`); got == "403" || got == "000" {
		t.Errorf("live allow not applied: %q", got)
	}

	if err := m.Stop("it1", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Running("it1"); ok {
		t.Fatal("still running after stop")
	}
	if _, err := m.Start(ctx, "it1"); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}
