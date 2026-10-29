package vm

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/catchpen/internal/forge"
)

// fakeGitea answers the deploy-key and token endpoints and records calls.
type fakeGitea struct {
	mu    sync.Mutex
	calls []string
	fail  string // path prefix that returns 403
	keys  string // the answer to adding a deploy key, if not {"id":5}
	mint  string // the answer to minting a token, if not {"id":9,...}
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if f.fail != "" && strings.HasPrefix(r.URL.Path, f.fail) {
		w.WriteHeader(403)
		return
	}
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys") && f.keys != "":
		w.Write([]byte(f.keys))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys"):
		w.Write([]byte(`{"id":5}`))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens") && f.mint != "":
		w.Write([]byte(f.mint))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
		w.Write([]byte(`{"id":9,"sha1":"minted-token"}`))
	case r.Method == "DELETE":
		w.WriteHeader(204)
	case r.URL.Path == "/api/v1/user":
		w.Write([]byte(`{"login":"tok-owner"}`))
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGitea) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func forgeTestManager(t *testing.T, api *fakeGitea) *Manager {
	t.Helper()
	needQemuImg(t)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	m := testManager(t)
	// The fake API listens on loopback; reach it under a name the loopback check accepts.
	addr := strings.TrimPrefix(srv.URL, "http://")
	m.HTTP = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	key := forge.FormatHostKey(signer.PublicKey())
	base := forge.Forge{Type: forge.Gitea, Host: "git.test", HTTPURL: "http://git.test:3000", SSHPort: 2222, HostKeys: []string{key}}
	gd, gm, gt := base, base, base
	gd.Name, gd.Auth = "gd", forge.Auth{Mode: forge.ModeDeployKey, Token: "secret:ADMIN"}
	gm.Name, gm.Auth = "gm", forge.Auth{Mode: forge.ModeMintedToken, User: "bot", Password: "secret:PW"}
	gt.Name, gt.Auth = "gt", forge.Auth{Mode: forge.ModeToken, Token: "secret:USERTOK"}
	if err := forge.Update(m.P.Forges(), func(r *forge.Registry) error { r.Forges = []forge.Forge{gd, gm, gt}; return nil }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN", "admin-token")
	t.Setenv("PW", "pw")
	t.Setenv("USERTOK", "user-token")
	r, err := m.Resolve("minimal")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", r.BasePath(m.P), "1G").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return m
}

func TestCreateProvisionsAndReleasesForges(t *testing.T) {
	api := &fakeGitea{}
	m := forgeTestManager(t, api)
	ctx := context.Background()
	s, err := m.Create(ctx, "a", CreateOpts{Template: "minimal", Repos: []string{"gd:o/app", "gm:o/lib", "gt:o/doc"}, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Repos) != 3 || s.Repos[0].DeployKeyID != 5 || s.Repos[0].Alias != "catchpen-gd-o-app" || !s.Repos[0].ReadOnly || s.ForgeUsers["gt"] != "tok-owner" {
		t.Errorf("state: %+v users %v", s.Repos, s.ForgeUsers)
	}
	if st, err := os.Stat(filepath.Join(m.vm("a").path(forgeDir), "catchpen-gd-o-app")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("deploy key file: %v", err)
	}
	b, _ := os.ReadFile(m.vm("a").path("allowlist"))
	if !strings.Contains(string(b), "git.test:3000\n") || !strings.Contains(string(b), "git.test:2222\n") {
		t.Errorf("allowlist lacks forge hosts:\n%s", b)
	}
	creds, err := m.forgeCreds(ctx, s)
	if err != nil || len(creds) != 2 {
		t.Fatalf("forgeCreds = %+v, %v", creds, err)
	}
	for _, c := range creds {
		if (c.Forge.Name == "gm" && (c.Token != "minted-token" || c.User != "bot")) || (c.Forge.Name == "gt" && (c.Token != "user-token" || c.User != "tok-owner")) {
			t.Errorf("cred %+v", c)
		}
	}
	if err := m.Destroy("a"); err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"DELETE /api/v1/repos/o/app/keys/5", "DELETE /api/v1/users/bot/tokens/9"} {
		if !api.has(call) {
			t.Errorf("destroy did not call %s; calls: %v", call, api.calls)
		}
	}
}

func TestCreateRollbackReleasesForges(t *testing.T) {
	api := &fakeGitea{fail: "/api/v1/repos/o/second"}
	m := forgeTestManager(t, api)
	_, err := m.Create(context.Background(), "a", CreateOpts{Template: "minimal", Repos: []string{"gm:o/lib", "gd:o/first", "gd:o/second"}})
	if err == nil || !strings.Contains(err.Error(), "o/second") {
		t.Fatalf("err = %v", err)
	}
	for _, call := range []string{"DELETE /api/v1/repos/o/first/keys/5", "DELETE /api/v1/users/bot/tokens/9"} {
		if !api.has(call) {
			t.Errorf("rollback did not call %s; calls: %v", call, api.calls)
		}
	}
	if _, err := os.Stat(m.dir("a")); !os.IsNotExist(err) {
		t.Error("VM dir left behind")
	}
}

// A key or token the forge created without its id, and that catchpen couldn't
// find to remove, is in the audit log; an http:// forge is warned about.
func TestCreateAuditsCredentialsLeftBehind(t *testing.T) {
	api := &fakeGitea{keys: `{}`, mint: `{}`}
	m := forgeTestManager(t, api)
	var out bytes.Buffer
	m.Out = &out
	_, err := m.Create(context.Background(), "a", CreateOpts{Template: "minimal", Repos: []string{"gd:o/app"}})
	if err == nil || !strings.Contains(err.Error(), "may remain") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(m.P.Audit()); !strings.Contains(string(b), `"forge.deploy_key.add"`) || !strings.Contains(string(b), `"repo":"gd:o/app"`) {
		t.Errorf("no audit record of the key: %s", b)
	}
	if !strings.Contains(out.String(), "warning: forge gd uses an http:// URL") {
		t.Errorf("no cleartext warning: %s", out.String())
	}
	if _, err := m.Create(context.Background(), "b", CreateOpts{Template: "minimal", Repos: []string{"gm:o/lib"}}); err == nil || !strings.Contains(err.Error(), "may remain") {
		t.Fatalf("mint: err = %v", err)
	}
	if b, _ := os.ReadFile(m.P.Audit()); !strings.Contains(string(b), `"forge.token.mint"`) || !strings.Contains(string(b), `"user":"bot"`) {
		t.Errorf("no audit record of the token: %s", b)
	}
}

func TestPlanReposErrors(t *testing.T) {
	api := &fakeGitea{}
	m := forgeTestManager(t, api)
	if _, err := m.Create(context.Background(), "a", CreateOpts{Template: "minimal", Repos: []string{"gd:o/app", "gm:other/app"}}); err == nil || !strings.Contains(err.Error(), "/workspace/app") {
		t.Errorf("clashing dirs: %v", err)
	}
	// Both map to the alias catchpen-gd-o-b-c, so one key file would replace the other.
	if _, err := m.Create(context.Background(), "b", CreateOpts{Template: "minimal", Repos: []string{"gd:o/b-c", "gd:o-b/c"}}); err == nil || !strings.Contains(err.Error(), "catchpen-gd-o-b-c") {
		t.Errorf("clashing deploy-key aliases: %v", err)
	}
	// A deploy-key repo of forge "forge" and an agent-mode forge "o-r" both name catchpen-forge-o-r.
	if err := forge.Update(m.P.Forges(), func(r *forge.Registry) error {
		dk, ag := r.Forges[0], r.Forges[0]
		dk.Name = "forge"
		ag.Name, ag.Auth = "o-r", forge.Auth{Mode: forge.ModeAgent}
		r.Forges = append(r.Forges, dk, ag)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), "c", CreateOpts{Template: "minimal", Repos: []string{"forge:o/r", "o-r:x/y"}}); err == nil || !strings.Contains(err.Error(), "catchpen-forge-o-r") {
		t.Errorf("deploy-key alias clashing with an agent-mode forge: %v", err)
	}
	if _, err := m.Create(context.Background(), "a", CreateOpts{Template: "minimal", Repos: []string{"https://nowhere.test/o/r"}}); err == nil || !strings.Contains(err.Error(), "catchpen forge add") {
		t.Errorf("unknown forge: %v", err)
	}
	if len(api.calls) != 0 {
		t.Errorf("API called before validation finished: %v", api.calls)
	}
}

func TestValidBranch(t *testing.T) {
	for _, b := range []string{"main", "feat/x", "v1.2-rc_3"} {
		if err := validBranch(b); err != nil {
			t.Errorf("%q: %v", b, err)
		}
	}
	for _, b := range []string{"", "-x", "a..b", "a/", "a//b", "x.lock", "a/.b", "a b", "a;b", "$(x)", "a.", "/a"} {
		if validBranch(b) == nil {
			t.Errorf("%q accepted", b)
		}
	}
}
