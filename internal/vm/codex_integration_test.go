//go:build integration

package vm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/vmtest"

	"github.com/blackshieldpt/catchpen/internal/egress"
)

// TestCodex builds a base with Codex and checks it is installed, logged in
// with the host's API key, accepts the config catchpen writes (no update
// checks, catchpen's sandbox mode), and reaches the OpenAI API through the
// egress proxy.
func TestCodex(t *testing.T) {
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	tpl := "runtimes: { node: \"24\" }\nagents: { codex: {} }\n"
	vmtest.Template(t, m.P.Templates(), "cx", "", tpl)
	const key = "sk-proj-catchpen-integration-fake"
	t.Setenv("OPENAI_API_KEY", key)
	if _, err := m.Create(ctx, "cx", CreateOpts{Template: "cx"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("cx") })
	if _, err := m.Start(ctx, "cx"); err != nil {
		t.Fatal(err)
	}
	port, err := m.SSHPort("cx")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.keys("cx").Dial(ctx, port, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	guest := guestRunner(t, ctx, c)
	if v := guest("codex --version"); !strings.Contains(v, "codex") {
		t.Errorf("codex --version: %q", v)
	}
	if got := guest("stat -c %a ~/.codex/auth.json ~/.codex/config.toml | sort -u"); got != "600" {
		t.Errorf("modes: %q", got)
	}
	if got := guest("grep -c " + key + " ~/.codex/auth.json"); got != "1" {
		t.Errorf("auth.json lacks the key")
	}
	if got := guest("codex login status 2>&1"); !strings.Contains(strings.ToLower(got), "api key") {
		t.Errorf("codex login status: %q", got)
	}
	if got := guest("printenv OPENAI_API_KEY || true"); got != "" {
		t.Errorf("the key is in the session environment")
	}
	// The fake key gets codex as far as the API, through the proxy.
	out := guest("cd /tmp && echo hi | timeout 120 codex exec --json --yolo --skip-git-repo-check - 2>&1 || true")
	t.Logf("codex exec:\n%s", out)
	if got := guest("codex mcp list >/dev/null 2>&1 && echo ok || codex mcp list 2>&1"); got != "ok" {
		t.Errorf("codex rejected its config: %s", got)
	}
	if !strings.Contains(out, `"type":"turn.failed"`) {
		t.Errorf("codex exec didn't get to a turn:\n%s", out)
	}
	log, _ := os.ReadFile(filepath.Join(m.Dir("cx"), egress.LogFile))
	if !strings.Contains(string(log), `"host":"api.openai.com"`) {
		t.Errorf("no api.openai.com request in the egress log:\n%s", log)
	}
	for _, line := range []string{"check_for_update_on_startup = false", "sandbox_mode = 'danger-full-access'"} {
		if got := guest("grep -cxF \"" + line + "\" ~/.codex/config.toml"); got != "1" {
			t.Errorf("config.toml lacks %s", line)
		}
	}
}
