//go:build integration

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/canary"
	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/egress"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/qemu"
	"github.com/blackshieldpt/curral/internal/sshx"
)

const giteaPassword = "curral-Test-pw-1"

type gitea struct {
	t       *testing.T
	ip      string
	http    string // http://<docker bridge ip>:port
	sshPort int
}

// startGitea runs a throwaway Gitea in Docker with user "bot" and repos app and lib.
// startGitea runs Gitea with its built-in SSH server, or with OpenSSH (which
// supports host-bound auth, needed for the restricted agent).
func startGitea(t *testing.T, openssh bool) *gitea {
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
	sshArgs := []string{"-p", fmt.Sprintf("%s:%d:2222", ip, sp), "-e", "GITEA__server__START_SSH_SERVER=true", "-e", "GITEA__server__SSH_LISTEN_PORT=2222"}
	if openssh {
		sshArgs = []string{"-p", fmt.Sprintf("%s:%d:22", ip, sp), "-e", "GITEA__server__START_SSH_SERVER=false"}
	}
	args := append([]string{"run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("%s:%d:3000", ip, hp),
		"-e", "GITEA__security__INSTALL_LOCK=true",
		"-e", "GITEA__server__ROOT_URL=" + g.http + "/",
		"-e", fmt.Sprintf("GITEA__server__SSH_PORT=%d", sp),
		"-e", "GITEA__server__DISABLE_SSH=false"}, sshArgs...)
	out, err := exec.Command("docker", append(args, image)...).CombinedOutput()
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
	g := startGitea(t, false)
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
	if err := os.WriteFile(filepath.Join(m.P.Templates(), "fg.yaml"), []byte("extends: minimal\nnetwork: { allow_private: true }\ncanaries: [git]\n"), 0o600); err != nil {
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
	pdir := t.TempDir()
	p, err := m.Pull(ctx, "fg1", PullOpts{Repo: "app", Into: host, Patches: pdir})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(pdir, "fg1-local.mbox")); !bytes.Contains(b, []byte("Subject: [PATCH 1/2] Add hi")) || !bytes.Contains(b, []byte("Subject: [PATCH 2/2] Local change")) {
		t.Errorf("patches:\n%s", b)
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

	// Canaries: a pull or PR carrying the VM's decoys is refused, and
	// nothing moves on the host or the forge.
	m.Cfg.CanaryAction = config.CanaryLog // keep the VM running for the rest
	cs, err := canary.Load(m.dir("fg1"))
	if err != nil || len(cs) != 1 {
		t.Fatalf("canaries %v %v", cs, err)
	}
	hostRef := func(ref string) string {
		out, _ := exec.Command("git", "-C", host, "for-each-ref", "--format=%(refname) %(objectname)", ref).Output()
		return string(out)
	}
	before := hostRef("refs/curral/")
	run(`cd /workspace/app && git checkout -q local && echo '` + cs[0].Values[0] + `' > leak.txt && git add leak.txt && git commit -qm "Add config"`)
	if _, err := m.Pull(ctx, "fg1", PullOpts{Repo: "app", Into: host}); !errors.Is(err, ErrCanary) {
		t.Errorf("pull of a leaking commit: %v", err)
	}
	if after := hostRef("refs/curral/"); after != before || hostRef("refs/curral-incoming/") != "" {
		t.Errorf("refs after a refused pull:\n%s%s\nwas\n%s", after, hostRef("refs/curral-incoming/"), before)
	}
	run(`cd /workspace/app && git checkout -q -b clean2 origin/HEAD && echo z > z.txt && git add z.txt && git commit -qm z && git branch -q "x-` + cs[0].Hosts[0] + `"`)
	if _, err := m.Pull(ctx, "fg1", PullOpts{Repo: "app", Into: host, Branch: "x-" + cs[0].Hosts[0]}); !errors.Is(err, ErrCanary) {
		t.Errorf("pull of a branch named after a canary: %v", err)
	}
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("Deployed to "+cs[0].Hosts[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PR(ctx, "fg1", PROpts{PullOpts: PullOpts{Repo: "app", Into: host, Branch: "clean2"}, BodyFile: body}); !errors.Is(err, ErrCanary) {
		t.Errorf("PR with a canary in its body: %v", err)
	}
	var branches []map[string]any
	g.api("GET", "/repos/bot/app/branches", nil, &branches)
	for _, b := range branches {
		if strings.Contains(fmt.Sprint(b["name"]), "clean2") {
			t.Errorf("refused PR pushed %v", b["name"])
		}
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

func TestAgentForwarding(t *testing.T) {
	g := startGitea(t, true)
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// A host key the user would normally have in ~/.ssh, registered on the forge account.
	kdir, err := os.MkdirTemp("", "ak")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(kdir)
	priv, pub, _ := sshx.GenerateKey("user@host")
	keyFile := filepath.Join(kdir, "id_ed25519")
	os.WriteFile(keyFile, priv, 0o600)
	g.api("POST", "/user/keys", map[string]any{"title": "host key", "key": strings.TrimSpace(string(pub))}, nil)

	// Confirmations go to a stub askpass that answers from a file.
	answer := filepath.Join(kdir, "answer")
	asked := filepath.Join(kdir, "asked")
	askpass := filepath.Join(kdir, "askpass")
	os.WriteFile(askpass, []byte("#!/bin/sh\necho \"$*\" >> "+asked+"\nexit $(cat "+answer+")\n"), 0o700)
	os.WriteFile(answer, []byte("0"), 0o600)
	t.Setenv("SSH_ASKPASS", askpass)
	t.Setenv("SSH_ASKPASS_REQUIRE", "force")
	t.Setenv("DISPLAY", ":curral-test")

	keys, err := forge.Scan(ctx, g.ip, g.sshPort)
	if err != nil {
		t.Fatal(err)
	}
	var pinned []string
	for _, k := range keys {
		pinned = append(pinned, forge.FormatHostKey(k))
	}
	ga := forge.Forge{Name: "ga", Type: forge.Gitea, Host: g.ip, HTTPURL: g.http, SSHPort: g.sshPort, HostKeys: pinned, Auth: forge.Auth{Mode: forge.ModeAgent}}
	if ok, err := forge.HostBound(ctx, ga); err != nil || !ok {
		t.Fatalf("Gitea over OpenSSH: host-bound = %v, %v", ok, err)
	}
	builtin := startGitea(t, false)
	bkeys, err := forge.Scan(ctx, builtin.ip, builtin.sshPort)
	if err != nil {
		t.Fatal(err)
	}
	bf := forge.Forge{Name: "gb", Host: builtin.ip, SSHPort: builtin.sshPort, HostKeys: []string{forge.FormatHostKey(bkeys[0])}}
	if ok, err := forge.HostBound(ctx, bf); err != nil || ok {
		t.Errorf("Gitea built-in SSH: host-bound = %v, %v; want false", ok, err)
	}
	if err := forge.Update(m.P.Forges(), func(r *forge.Registry) error { r.Forges = []forge.Forge{ga}; return nil }); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(m.P.Templates(), "ag.yaml"), []byte("extends: minimal\nnetwork: { allow_private: true }\nssh: { agent: restricted, agent_keys: ["+keyFile+"] }\n"), 0o600)

	if _, err := m.Create(ctx, "ag2", CreateOpts{Template: "ag", Repos: []string{"ga:bot/app"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("ag2") })
	ssh := func(cmd string) (string, error) {
		out, err := m.sshExec(ctx, "ag2", cmd)
		return strings.TrimSpace(string(out.Raw())), err
	}
	if _, err := m.Start(ctx, "ag2"); err != nil {
		if os.Getenv("CURRAL_TEST_DEBUG") != "" {
			for _, c := range []string{"echo $SSH_AUTH_SOCK; ssh-add -l", "ssh -v -T curral-forge-ga 2>&1 | grep -v -e load_hostkeys -e kex -e rekey -e Reading | tail -25"} {
				out, err := ssh(c)
				t.Logf("$ %s (%v)\n%s", c, err, out)
			}
			b, _ := os.ReadFile(asked)
			t.Logf("askpass log: %q", b)
		}
		t.Fatal(err)
	}
	// The clone at start used the forwarded agent, after a confirmation.
	if out, err := ssh("git -C /workspace/app log -1 --format=%s"); err != nil || out != "Initial commit" {
		t.Fatalf("clone via agent: %q %v", out, err)
	}
	if b, _ := os.ReadFile(asked); len(b) == 0 {
		t.Error("no confirmation was asked")
	}
	if out, err := ssh("ssh-add -l | grep -c SHA256"); err != nil || out != "1" {
		t.Errorf("guest sees %q keys (%v), want only the restricted one", out, err)
	}
	// The key is constrained to the forge: the guest can't use it for other hosts.
	if _, err := ssh("mkdir -p ~/.ssh && echo '" + strings.TrimSpace(string(pub)) + "' >> ~/.ssh/authorized_keys"); err != nil {
		t.Fatal(err)
	}
	if out, err := ssh("ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=no agent@localhost echo pwned"); err == nil || strings.Contains(out, "pwned") {
		t.Errorf("constrained key worked for the VM itself: %q", out)
	}
	if out, err := ssh("git ls-remote curral-forge-ga:bot/app.git HEAD"); err != nil || !strings.Contains(out, "HEAD") {
		t.Errorf("ls-remote via agent: %q %v", out, err)
	}
	// A denied confirmation fails the operation.
	os.WriteFile(answer, []byte("1"), 0o600)
	if out, err := ssh("git ls-remote curral-forge-ga:bot/app.git HEAD"); err == nil {
		t.Errorf("ls-remote succeeded though confirmation was denied: %q", out)
	}
	os.WriteFile(answer, []byte("0"), 0o600)

	// Port forwards: a guest web server to the host, a host service to the guest.
	if _, err := ssh("cd /tmp && echo guest-page > index.html && (setsid python3 -m http.server 8000 --bind 127.0.0.1 >/dev/null 2>&1 &) && sleep 1"); err != nil {
		t.Fatal(err)
	}
	hostPort, _ := qemu.FreePort()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("host-service")) })}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	svcPort := ln.Addr().(*net.TCPAddr).Port
	fs, err := m.Forward(ctx, "ag2", []string{fmt.Sprintf("%d:8000", hostPort)}, []string{fmt.Sprint(svcPort)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", hostPort))
	if err != nil {
		t.Fatalf("-L: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(body)) != "guest-page" {
		t.Errorf("-L body %q", body)
	}
	if out, err := ssh(fmt.Sprintf("curl -s --noproxy '*' http://127.0.0.1:%d/", svcPort)); err != nil || out != "host-service" {
		t.Errorf("-R: %q %v", out, err)
	}
	if list, _ := m.Forwards("ag2"); len(list) != 1 || list[0].PID != fs.PID {
		t.Errorf("forwards: %+v", list)
	}
	elog, _ := os.ReadFile(filepath.Join(m.Dir("ag2"), egress.LogFile))
	if !strings.Contains(string(elog), `"action":"forward"`) {
		t.Error("-R forward not recorded in the egress log")
	}

	// Stopping the VM ends the agent and the forwards.
	if err := m.Stop("ag2", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.agent("ag2").Running(); ok {
		t.Error("agent still running after stop")
	}
	if m.forwardAlive("ag2", fs.PID) {
		t.Error("forward still running after stop")
	}
}
