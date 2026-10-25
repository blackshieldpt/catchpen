//go:build integration

package vm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/vmtest"

	"github.com/blackshieldpt/curral/internal/cache"
	"github.com/blackshieldpt/curral/internal/egress"
)

// TestCaches installs packages with each client in a VM whose allowlist
// has none of the package hosts: they can only come through the caches. A
// second VM then gets the stored objects without fetching them again.
func TestCaches(t *testing.T) {
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	tpl := `docker: true
runtimes: { node: "24", python: uv, go: "1.27" }
network: { presets: [docker-apt] }
cache: [apt, go, npm, pypi, docker]
`
	vmtest.Template(t, m.P.Templates(), "ch", "", tpl)
	install := strings.Join([]string{
		"sudo apt-get -q update >/dev/null && sudo apt-get -qy install --no-install-recommends cowsay >/dev/null",
		"cd $(mktemp -d) && npm install --no-audit --no-fund --loglevel=error left-pad@1.3.0 >/dev/null",
		"uv venv -q /tmp/venv && VIRTUAL_ENV=/tmp/venv uv pip install -q six==1.16.0",
		"cd $(mktemp -d) && go mod init x >/dev/null 2>&1 && go get golang.org/x/text@v0.20.0 2>&1",
		"sudo docker pull -q hello-world >/dev/null",
		"echo ok",
	}, " && ")

	run := func(name string) string {
		t.Helper()
		if _, err := m.Create(ctx, name, CreateOpts{Template: "ch"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Destroy(name) })
		if _, err := m.Start(ctx, name); err != nil {
			t.Fatal(err)
		}
		port, err := m.SSHPort(name)
		if err != nil {
			t.Fatal(err)
		}
		c, err := m.keys(name).Dial(ctx, port, "agent")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		start := time.Now()
		if got := guestRunner(t, ctx, c)("bash -lc '" + install + "'"); !strings.HasSuffix(got, "ok") {
			t.Fatalf("%s: install: %q", name, got)
		}
		t.Logf("%s: installs took %s", name, time.Since(start).Round(time.Second))
		b, _ := os.ReadFile(filepath.Join(m.dir(name), egress.LogFile))
		return string(b)
	}

	first := run("c1")
	if logs, _ := filepath.Glob(filepath.Join(m.P.Bases(), "*.egress.log")); len(logs) != 1 {
		t.Errorf("build egress logs %v", logs)
	} else if b, _ := os.ReadFile(logs[0]); !strings.Contains(string(b), `"action":"cache"`) {
		t.Error("the base build didn't use the apt cache")
	}
	us, err := cache.Store{Dir: m.P.Pkg()}.Usage()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		if u.Files == 0 {
			t.Errorf("nothing stored for %s", u.Kind)
		}
	}
	for _, want := range []string{"left-pad-1.3.0.tgz", "six-1.16.0", "golang.org/x/text/@v/v0.20.0.zip", "/blobs/sha256:", "cowsay"} {
		if !strings.Contains(first, want) {
			t.Errorf("c1's egress log doesn't show %s fetched upstream", want)
		}
	}

	second := run("c2")
	for _, stored := range []string{"left-pad-1.3.0.tgz", "six-1.16.0", "golang.org/x/text/@v/v0.20.0.zip", "/blobs/sha256:", "cowsay"} {
		if strings.Contains(second, stored) {
			t.Errorf("c2 fetched %s again", stored)
		}
	}
}
