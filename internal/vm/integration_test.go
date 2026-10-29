//go:build integration

// Boots a real VM from the minimal template. Needs KVM and network access.
// Reuses downloaded cloud images from CATCHPEN_TEST_IMAGES (default ~/.local/share/catchpen/images).
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

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
)

// integrationManager sets up a throwaway CATCHPEN_HOME (reusing downloaded images) and builds catchpen.
func integrationManager(t *testing.T) *Manager {
	t.Helper()
	p, bin := vmtest.Home(t)
	return &Manager{P: p, Cfg: config.Default(), Tpl: template.Loader{UserDir: p.Templates()}, Out: vmtest.Writer{T: t}, Bin: bin}
}

func guestRunner(t *testing.T, ctx context.Context, c *ssh.Client) func(string) string {
	return func(cmd string) string {
		t.Helper()
		out, err := sshx.Run(ctx, c, cmd, nil)
		if err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
		return strings.TrimSpace(string(out.Raw()))
	}
}

func TestLifecycle(t *testing.T) {
	m := integrationManager(t)
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
	run := guestRunner(t, ctx, c)

	if got := run("sudo sshd -T | grep -E '^passwordauthentication'"); got != "passwordauthentication no" {
		t.Errorf("sshd: %q", got)
	}
	// A user-data the cloud-init schema rejects leaves every boot "degraded done".
	if got := run("cloud-init status --long | sed -n 's/^extended_status: //p'"); got != "done" {
		t.Errorf("cloud-init extended_status %q, want done", got)
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
	// The generated ssh_config (ProxyCommand through the catchpen binary) works.
	if _, err := exec.LookPath("ssh"); err == nil {
		out, err := exec.CommandContext(ctx, "ssh", "-F", m.P.SSHConfig(), "-o", "BatchMode=yes", "catchpen-it1", "echo", "via-config").CombinedOutput()
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

// TestAgents builds a base with all three agents and checks what the guest
// sees after the credential push. Credentials are fake.
func TestAgents(t *testing.T) {
	m := integrationManager(t)
	vmtest.Template(t, m.P.Templates(), "agents", "", `runtimes: { node: "24" }
agents:
  claude: {}
  opencode: { keys: [ANTHROPIC_API_KEY] }
  pi: { keys: [ANTHROPIC_API_KEY] }
env: { FOO: "literal value", BAR: "secret:IT_BAR" }
mcp:
  - { name: fs, type: stdio, command: [npx, -y, "@modelcontextprotocol/server-filesystem", /tmp] }
`)
	const oauth, key = "sk-ant-oat01-catchpen-integration-fake", "sk-ant-api03-catchpen-integration-fake-key"
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", oauth)
	t.Setenv("ANTHROPIC_API_KEY", key)
	t.Setenv("IT_BAR", "bar from host")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if _, err := m.Create(ctx, "ag1", CreateOpts{Template: "agents"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("ag1") })
	rt, err := m.Start(ctx, "ag1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.keys("ag1").Dial(ctx, rt.SSHPort, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	run := guestRunner(t, ctx, c)

	for _, bin := range []string{"claude", "opencode", "pi"} {
		if got := run(bin + " --version"); got == "" {
			t.Errorf("%s --version printed nothing", bin)
		}
	}
	// ~/.ssh/environment reaches ssh sessions; Claude's token and the API key don't.
	for name, want := range map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "", "FOO": "literal value", "BAR": "bar from host", "ANTHROPIC_API_KEY": "", "DISABLE_AUTOUPDATER": "1", "PI_OFFLINE": "1"} {
		if got := run("printenv " + name + " || true"); got != want {
			t.Errorf("guest %s = %q, want %q", name, got, want)
		}
	}
	if got := run("catchpen-env printenv CLAUDE_CODE_OAUTH_TOKEN || true"); got != "" {
		t.Errorf("agents catchpen run starts get Claude's token: %q", got)
	}
	if got := run("stat -c %a ~/.ssh/environment ~/.config/catchpen/claude.env ~/.claude.json ~/.local/share/opencode/auth.json ~/.pi/agent/auth.json | sort -u"); got != "600" {
		t.Errorf("modes: %q", got)
	}
	if got := run("jq -r .anthropic.key ~/.local/share/opencode/auth.json ~/.pi/agent/auth.json | sort -u"); got != key {
		t.Errorf("provider keys: %q", got)
	}
	if got := run("jq -r '.mcpServers.fs.command, .hasCompletedOnboarding' ~/.claude.json"); got != "npx\ntrue" {
		t.Errorf("claude.json: %q", got)
	}
	// Claude reaches the API through the proxy with the (fake) subscription token and is rejected.
	run("timeout 60 claude -p hi >/dev/null 2>&1 || true")
	log, _ := os.ReadFile(filepath.Join(m.Dir("ag1"), egress.LogFile))
	if !strings.Contains(string(log), `"host":"api.anthropic.com"`) {
		t.Errorf("no api.anthropic.com request in the egress log:\n%s", log)
	}
	// Restart pushes again: a secret removed on the host disappears from the VM.
	os.Unsetenv("IT_BAR")
	if err := m.Push(ctx, "ag1"); err != nil {
		t.Fatal(err)
	}
	if got := run("grep -c ^BAR= ~/.ssh/environment || true"); got != "0" {
		t.Errorf("removed secret still in the VM env file")
	}
	// claude itself gets the token: point the wrapper at printenv.
	run("sudo ln -sfn /usr/bin/printenv /usr/local/lib/catchpen/claude")
	if got := run("claude CLAUDE_CODE_OAUTH_TOKEN"); got != oauth {
		t.Errorf("claude's token = %q", got)
	}
}
