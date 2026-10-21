//go:build integration

package vm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOMP builds a base with oh-my-pi and checks it is installed, set not to
// update itself, and gets its API keys and notification extension at start.
func TestOMP(t *testing.T) {
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	tpl := "extends: minimal\nagents: { omp: { keys: [OPENROUTER_API_KEY] } }\n"
	if err := os.WriteFile(filepath.Join(m.P.Templates(), "om.yaml"), []byte(tpl), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-test")
	if _, err := m.Create(ctx, "om", CreateOpts{Template: "om"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("om") })
	if _, err := m.Start(ctx, "om"); err != nil {
		t.Fatal(err)
	}
	port, _ := m.SSHPort("om")
	c, err := m.keys("om").Dial(ctx, port, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	guest := guestRunner(t, ctx, c)
	if v := guest("omp --version"); v == "" {
		t.Error("omp --version printed nothing")
	}
	if got := guest("omp config get startup.checkUpdate; omp config get marketplace.autoUpdate"); !strings.Contains(got, "false") || !strings.Contains(got, "off") {
		t.Errorf("omp settings: %q", got)
	}
	if got := guest("stat -c %a ~/.omp/agent/.env && cat ~/.omp/agent/.env"); got != "600\nOPENROUTER_API_KEY=sk-or-v1-test" {
		t.Errorf("omp keys: %q", got)
	}
	if got := guest(`grep -c 'pi.on("agent_end"' ~/.omp/agent/extensions/curral-notify.ts`); got != "1" {
		t.Errorf("omp extension: %q", got)
	}
}
