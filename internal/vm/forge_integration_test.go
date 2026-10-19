//go:build integration

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/qemu"
)

const giteaPassword = "curral-Test-pw-1"

type gitea struct {
	t       *testing.T
	ip      string
	http    string // http://<docker bridge ip>:port
	sshPort int
}

// startGitea runs a throwaway Gitea in Docker with user "bot" and repos app and lib.
func startGitea(t *testing.T) *gitea {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	image := os.Getenv("CURRAL_TEST_GITEA_IMAGE")
	if image == "" {
		image = "gitea/gitea:1.24"
	}
	// Publish on the Docker bridge address: the VM reaches it through the egress
	// proxy (allow_private), and unlike 127.0.0.1 it isn't the VM's own loopback.
	ipOut, err := exec.Command("docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil {
		t.Fatalf("docker bridge address: %v", err)
	}
	ip := strings.TrimSpace(string(ipOut))
	hp, _ := qemu.FreePort()
	sp, _ := qemu.FreePort()
	g := &gitea{t: t, ip: ip, http: fmt.Sprintf("http://%s:%d", ip, hp), sshPort: sp}
	name := fmt.Sprintf("curral-test-gitea-%d", hp)
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("%s:%d:3000", ip, hp), "-p", fmt.Sprintf("%s:%d:2222", ip, sp),
		"-e", "GITEA__security__INSTALL_LOCK=true",
		"-e", "GITEA__server__ROOT_URL="+g.http+"/",
		"-e", "GITEA__server__START_SSH_SERVER=true",
		"-e", "GITEA__server__SSH_LISTEN_PORT=2222",
		"-e", fmt.Sprintf("GITEA__server__SSH_PORT=%d", sp),
		"-e", "GITEA__server__DISABLE_SSH=false",
		image).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if resp, err := http.Get(g.http + "/api/v1/version"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("gitea did not start")
		}
		time.Sleep(time.Second)
	}
	if out, err := exec.Command("docker", "exec", "-u", "git", name, "gitea", "admin", "user", "create",
		"--username", "bot", "--password", giteaPassword, "--email", "bot@example.test", "--admin", "--must-change-password=false").CombinedOutput(); err != nil {
		t.Fatalf("create user: %v\n%s", err, out)
	}
	for _, repo := range []string{"app", "lib"} {
		g.api("POST", "/user/repos", map[string]any{"name": repo, "auto_init": true, "default_branch": "main"}, nil)
	}
	return g
}

func (g *gitea) api(method, path string, body, out any) {
	g.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, g.http+"/api/v1"+path, rd)
	req.SetBasicAuth("bot", giteaPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode > 299 {
		g.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			g.t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
}

func TestForges(t *testing.T) {
	g := startGitea(t)
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// An admin API token (host side only) for managing deploy keys and opening PRs.
	var tok struct {
		SHA1 string `json:"sha1"`
	}
	g.api("POST", "/users/bot/tokens", map[string]any{"name": "curral-admin", "scopes": []string{"write:repository", "read:user"}}, &tok)
	t.Setenv("GITEA_ADMIN", tok.SHA1)
	t.Setenv("GITEA_PASSWORD", giteaPassword)

	keys, err := forge.Scan(ctx, g.ip, g.sshPort)
	if err != nil {
		t.Fatal(err)
	}
	var pinned []string
	for _, k := range keys {
		pinned = append(pinned, forge.FormatHostKey(k))
	}
	base := forge.Forge{Type: forge.Gitea, Host: g.ip, HTTPURL: g.http, SSHPort: g.sshPort, HostKeys: pinned}
	gd, gm := base, base
	gd.Name, gd.Auth = "gd", forge.Auth{Mode: forge.ModeDeployKey, Token: "secret:GITEA_ADMIN"}
	gm.Name, gm.Auth = "gm", forge.Auth{Mode: forge.ModeMintedToken, User: "bot", Password: "secret:GITEA_PASSWORD", Scopes: []string{"write:repository", "read:user"}}
	if err := forge.Update(m.P.Forges(), func(r *forge.Registry) error { r.Forges = []forge.Forge{gd, gm}; return nil }); err != nil {
		t.Fatal(err)
	}
	// Gitea is on a private address, which the proxy only allows with allow_private.
	if err := os.WriteFile(filepath.Join(m.P.Templates(), "fg.yaml"), []byte("extends: minimal\nnetwork: { allow_private: true }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(ctx, "fg1", CreateOpts{Template: "fg", Repos: []string{"gd:bot/app", "gm:bot/lib"}}); err != nil {
		t.Fatal(err)
	}
	destroyed := false
	t.Cleanup(func() {
		if !destroyed {
			_ = m.Destroy("fg1")
		}
	})
	var dk []map[string]any
	g.api("GET", "/repos/bot/app/keys", nil, &dk)
	if len(dk) != 1 || dk[0]["read_only"] != false {
		t.Fatalf("deploy keys after create: %v", dk)
	}
	rt, err := m.Start(ctx, "fg1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.keys("fg1").Dial(ctx, rt.SSHPort, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	run := guestRunner(t, ctx, c)

	// Both repos were cloned: app over SSH (deploy key, via the proxy), lib over HTTPS (minted token).
	if got := run("git -C /workspace/app remote get-url origin; git -C /workspace/lib remote get-url origin"); got != "curral-gd-bot-app:bot/app.git\n"+g.http+"/bot/lib.git" {
		t.Errorf("remotes: %q", got)
	}
	run(`git config --global user.email a@b.test; git config --global user.name agent
cd /workspace/app && git checkout -q -b feat && echo hi > hi.txt && git add hi.txt && git commit -qm "Add hi" && git push -q origin feat
cd /workspace/lib && echo x > x.txt && git add x.txt && git commit -qm "Add x" && git push -q origin main`)
	var br map[string]any
	g.api("GET", "/repos/bot/app/branches/feat", nil, &br)
	g.api("GET", "/repos/bot/lib/commits?limit=1", nil, nil)

	// Pull an unpushed branch into a host clone, then open a PR from it.
	host := t.TempDir()
	authURL := strings.Replace(g.http, "http://", "http://bot:"+giteaPassword+"@", 1)
	if out, err := exec.Command("git", "clone", "-q", authURL+"/bot/app.git", host).CombinedOutput(); err != nil {
		t.Fatalf("host clone: %v\n%s", err, out)
	}
	run(`cd /workspace/app && git checkout -q -b local && echo y > y.txt && git add y.txt && git commit -qm "Local change" && mkdir -p .git/hooks`)
	p, err := m.Pull(ctx, "fg1", PullOpts{Repo: "app", Into: host})
	if err != nil {
		t.Fatal(err)
	}
	if p.Ref != "refs/curral/fg1/local" {
		t.Errorf("ref = %s", p.Ref)
	}
	if out, _ := exec.Command("git", "-C", host, "log", "-1", "--format=%s", p.Ref).Output(); strings.TrimSpace(string(out)) != "Local change" {
		t.Errorf("pulled commit: %q", out)
	}
	url, err := m.PR(ctx, "fg1", PROpts{PullOpts: PullOpts{Repo: "app", Into: host}})
	if err != nil {
		t.Fatal(err)
	}
	var prs []map[string]any
	g.api("GET", "/repos/bot/app/pulls?state=open", nil, &prs)
	if len(prs) != 1 || prs[0]["title"] != "Local change" || prs[0]["html_url"] != url {
		t.Errorf("pulls: %v (url %s)", prs, url)
	}

	// Destroy removes the deploy key and the minted token.
	if err := m.Destroy("fg1"); err != nil {
		t.Fatal(err)
	}
	destroyed = true
	g.api("GET", "/repos/bot/app/keys", nil, &dk)
	if len(dk) != 0 {
		t.Errorf("deploy keys after destroy: %v", dk)
	}
	var toks []map[string]any
	g.api("GET", "/users/bot/tokens", nil, &toks)
	for _, tk := range toks {
		if strings.HasPrefix(fmt.Sprint(tk["name"]), "curral-fg1-") {
			t.Errorf("minted token left: %v", tk)
		}
	}
}
