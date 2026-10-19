package forge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

func hostKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, FormatHostKey(s.PublicKey())
}

func registry(t *testing.T) Registry {
	_, k := hostKey(t)
	return Registry{Forges: []Forge{
		{Name: "github", Type: GitHub, Host: "github.com", HostKeys: []string{k}, Auth: Auth{Mode: ModeDeployKey, Token: "secret:GH"}},
		{Name: "acme", Type: Gitea, Host: "git.acme.test", SSHPort: 2222, HostKeys: []string{k}, Auth: Auth{Mode: ModeToken, Token: "secret:ACME"}},
		{Name: "web", Type: Gitea, Host: "ssh.example.test", HTTPURL: "https://code.example.test", Auth: Auth{Mode: ModeNone}},
	}}
}

func TestResolve(t *testing.T) {
	reg := registry(t)
	for ref, want := range map[string]string{
		"github:o/r":                            "github:o/r",
		"acme:team/app.git":                     "acme:team/app",
		"https://github.com/o/r":                "github:o/r",
		"https://github.com/o/r.git/":           "github:o/r",
		"git@github.com:o/r.git":                "github:o/r",
		"ssh://git@git.acme.test:2222/team/app": "acme:team/app",
		"https://code.example.test/x/y":         "web:x/y",
		"git@ssh.example.test:x/y":              "web:x/y",
	} {
		_, r, err := reg.Resolve(ref)
		if err != nil || r.String() != want {
			t.Errorf("Resolve(%q) = %v, %v; want %s", ref, r, err, want)
		}
	}
	for ref, want := range map[string]string{
		"https://unknown.test/o/r": "curral forge add",
		"github:o":                 "owner/repo",
		"github:o/r/extra":         "owner/repo",
		"github:../r":              "owner/repo",
		"github:o/.hidden":         "owner/repo",
		"justaname":                "forge:owner/repo",
	} {
		if _, _, err := reg.Resolve(ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Resolve(%q) err = %v, want %q", ref, err, want)
		}
	}
}

func TestValidate(t *testing.T) {
	_, k := hostKey(t)
	ok := Forge{Name: "g", Type: Gitea, Host: "h.test", Auth: Auth{Mode: ModeMintedToken, User: "u", Password: "secret:PW", Scopes: []string{"write:repository"}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Forge){
		"must be github or gitea":      func(f *Forge) { f.Type = "gitlab" },
		"invalid host":                 func(f *Forge) { f.Host = "h test" },
		"invalid http_url":             func(f *Forge) { f.HTTPURL = "https://u:p@h.test" },
		"auth.password must be secret": func(f *Forge) { f.Auth.Password = "hunter2" },
		"minted-token is Gitea only":   func(f *Forge) { f.Type = GitHub },
		"needs auth.user":              func(f *Forge) { f.Auth.User = "" },
		"invalid scope":                func(f *Forge) { f.Auth.Scopes = []string{"all; drop"} },
		"needs auth.token":             func(f *Forge) { f.Auth = Auth{Mode: ModeToken} },
		"no pinned host keys":          func(f *Forge) { f.Auth = Auth{Mode: ModeDeployKey, Token: "secret:T"}; f.HostKeys = nil },
		"auth.mode must be":            func(f *Forge) { f.Auth.Mode = "password" },
		"host key":                     func(f *Forge) { f.HostKeys = []string{"ssh-ed25519 garbage"} },
		"invalid name":                 func(f *Forge) { f.Name = "Bad Name" },
	}
	for want, mut := range cases {
		f := ok
		f.HostKeys = []string{k}
		mut(&f)
		if err := f.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestHostsAndURLs(t *testing.T) {
	reg := registry(t)
	gh, _ := reg.Get("github")
	if gh.APIURL() != "https://api.github.com" || strings.Join(gh.Hosts(), ",") != "api.github.com:443,codeload.github.com:443,github.com:22,github.com:443,objects.githubusercontent.com:443" {
		t.Errorf("github: %s %v", gh.APIURL(), gh.Hosts())
	}
	acme, _ := reg.Get("acme")
	if acme.APIURL() != "https://git.acme.test/api/v1" || strings.Join(acme.Hosts(), ",") != "git.acme.test:443" {
		t.Errorf("acme (token mode, no ssh): %s %v", acme.APIURL(), acme.Hosts())
	}
	ghe := Forge{Type: GitHub, Host: "ghe.test", HTTPURL: "http://ghe.test:8080/"}
	if ghe.APIURL() != "http://ghe.test:8080/api/v3" || ghe.Hosts()[0] != "ghe.test:8080" {
		t.Errorf("ghe: %s %v", ghe.APIURL(), ghe.Hosts())
	}
	if !strings.HasPrefix(acme.KnownHosts(), "[git.acme.test]:2222 ssh-ed25519 ") || !strings.HasPrefix(gh.KnownHosts(), "github.com ssh-ed25519 ") {
		t.Errorf("known_hosts: %q %q", acme.KnownHosts(), gh.KnownHosts())
	}
}

func TestRegistryUpdate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "forges.yaml")
	reg := registry(t)
	if err := Update(p, func(r *Registry) error { r.Forges = reg.Forges; return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || len(got.Forges) != 3 {
		t.Fatalf("Load = %v, %v", got, err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if err := Update(p, func(r *Registry) error { r.Forges[0].Type = "bad"; return nil }); err == nil {
		t.Error("invalid forge saved")
	}
	os.WriteFile(p, []byte("forges:\n  - {name: a, type: gitea, host: h, auth: {mode: none}}\n  - {name: a, type: gitea, host: h, auth: {mode: none}}\n"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate: %v", err)
	}
}

func TestGuestRendering(t *testing.T) {
	reg := registry(t)
	gh, _ := reg.Get("github")
	acme, _ := reg.Get("acme")
	repos := []GuestRepo{
		{Repo: Repo{Forge: "github", Owner: "O", Name: "R.js"}, Forge: gh, Alias: Alias(Repo{Forge: "github", Owner: "O", Name: "R.js"})},
		{Repo: Repo{Forge: "acme", Owner: "team", Name: "app"}, Forge: acme},
	}
	if repos[0].Alias != "curral-github-o-r-js" || repos[0].CloneURL() != "curral-github-o-r-js:O/R.js.git" || repos[1].CloneURL() != "https://git.acme.test/team/app.git" || repos[0].Dir() != "/workspace/R.js" {
		t.Errorf("alias/url: %s %s %s", repos[0].Alias, repos[0].CloneURL(), repos[1].CloneURL())
	}
	cfg := SSHConfig(repos, "10.0.2.100:3128")
	for _, want := range []string{"Host curral-github-o-r-js\n", "HostName github.com\n", "Port 22\n", "IdentitiesOnly yes", "StrictHostKeyChecking yes", "ProxyCommand nc -X connect -x 10.0.2.100:3128 %h %p"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("ssh config lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "acme") {
		t.Error("token-mode forge got an SSH block")
	}
	if kh := KnownHosts(repos); !strings.HasPrefix(kh, "github.com ") || strings.Contains(kh, "acme") {
		t.Errorf("known hosts: %q", kh)
	}
	creds := []GuestCred{{Forge: acme, User: "bot", Token: "t0k/en@"}, {Forge: gh, User: "x-access-token", Token: "ghp"}}
	gc := GitCredentials(creds)
	if gc != "https://bot:t0k%2Fen%40@git.acme.test\nhttps://x-access-token:ghp@github.com\n" {
		t.Errorf("git credentials: %q", gc)
	}
	tea := TeaConfig(creds)
	if !strings.Contains(tea, `token: "t0k/en@"`) || strings.Contains(tea, "github") {
		t.Errorf("tea: %q", tea)
	}
	env := GitHubEnv(append(creds, GuestCred{Forge: Forge{Type: GitHub, Host: "ghe.test"}, Token: "e"}))
	if env["GH_TOKEN"] != "ghp" || env["GH_HOST"] != "ghe.test" || env["GH_ENTERPRISE_TOKEN"] != "e" {
		t.Errorf("gh env: %v", env)
	}
}

// fakeAPI records requests and answers like GitHub/Gitea.
type fakeAPI struct {
	mu   sync.Mutex
	reqs []string
	body map[string]map[string]any
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if u, p, ok := r.BasicAuth(); ok {
			auth = "basic " + u + ":" + p
		}
		key := r.Method + " " + r.URL.Path
		f.reqs = append(f.reqs, key+" "+auth)
		var b map[string]any
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &b)
		if f.body == nil {
			f.body = map[string]map[string]any{}
		}
		f.body[key] = b
		switch {
		case key == "POST /api/v1/repos/o/r/keys":
			w.Write([]byte(`{"id":41}`))
		case key == "DELETE /api/v1/repos/o/r/keys/41":
			w.WriteHeader(204)
		case key == "DELETE /api/v1/repos/o/r/keys/404":
			w.WriteHeader(404)
		case key == "POST /api/v1/users/bot/tokens":
			w.WriteHeader(201)
			w.Write([]byte(`{"id":7,"name":"n","sha1":"minted"}`))
		case key == "DELETE /api/v1/users/bot/tokens/7":
			w.WriteHeader(204)
		case key == "POST /api/v1/repos/o/r/pulls" || key == "POST /api/v3/repos/o/r/pulls":
			w.WriteHeader(201)
			w.Write([]byte(`{"html_url":"https://x/pr/1"}`))
		case key == "GET /api/v1/user":
			w.Write([]byte(`{"login":"bot"}`))
		case key == "GET /api/v3/meta":
			k := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(mustSigner().PublicKey())))
			w.Write([]byte(`{"ssh_keys":["` + k + `"]}`))
		default:
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"no\u001b[2J way"}`))
		}
	})
}

var signerOnce struct {
	sync.Once
	s ssh.Signer
}

func mustSigner() ssh.Signer {
	signerOnce.Do(func() {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		signerOnce.s, _ = ssh.NewSignerFromKey(priv)
	})
	return signerOnce.s
}

func TestClient(t *testing.T) {
	api := &fakeAPI{}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	ctx := context.Background()
	gitea := Forge{Name: "g", Type: Gitea, Host: "h", HTTPURL: srv.URL}
	c := &Client{F: gitea, Token: "tok", User: "bot", Password: "pw"}
	repo := Repo{Owner: "o", Name: "r"}

	if id, err := c.AddDeployKey(ctx, repo, "curral vm", "ssh-ed25519 AAA", true); err != nil || id != 41 {
		t.Fatalf("AddDeployKey = %d, %v", id, err)
	}
	if b := api.body["POST /api/v1/repos/o/r/keys"]; b["read_only"] != true || b["key"] != "ssh-ed25519 AAA" {
		t.Errorf("deploy key body: %v", b)
	}
	if err := c.DeleteDeployKey(ctx, repo, 41); err != nil {
		t.Error(err)
	}
	if err := c.DeleteDeployKey(ctx, repo, 404); err != nil {
		t.Errorf("deleting a missing key: %v", err)
	}
	id, tok, err := c.MintToken(ctx, "curral-vm", []string{"write:repository"})
	if err != nil || id != 7 || tok != "minted" {
		t.Fatalf("MintToken = %d %q %v", id, tok, err)
	}
	if err := c.DeleteToken(ctx, 7); err != nil {
		t.Error(err)
	}
	if login, err := c.Login(ctx); err != nil || login != "bot" {
		t.Errorf("Login = %q, %v", login, err)
	}
	if u, err := c.CreatePR(ctx, repo, "curral/vm/b", "main", "Title", "Body", true); err != nil || u != "https://x/pr/1" {
		t.Errorf("CreatePR = %q, %v", u, err)
	}
	if api.body["POST /api/v1/repos/o/r/pulls"]["title"] != "WIP: Title" {
		t.Errorf("gitea draft title: %v", api.body["POST /api/v1/repos/o/r/pulls"])
	}
	_, err = c.DefaultBranch(ctx, repo)
	if err == nil || !IsNotFound(err) && !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("error: %v", err)
	}
	reqs := strings.Join(api.reqs, "\n")
	for _, want := range []string{"POST /api/v1/repos/o/r/keys token tok", "POST /api/v1/users/bot/tokens basic bot:pw", "DELETE /api/v1/users/bot/tokens/7 basic bot:pw"} {
		if !strings.Contains(reqs, want) {
			t.Errorf("requests lack %q:\n%s", want, reqs)
		}
	}

	gh := &Client{F: Forge{Name: "e", Type: GitHub, Host: "ghe", HTTPURL: srv.URL}, Token: "ghp"}
	if _, err := gh.CreatePR(ctx, repo, "h", "main", "T", "B", true); err != nil {
		t.Fatal(err)
	}
	if b := api.body["POST /api/v3/repos/o/r/pulls"]; b["draft"] != true || b["title"] != "T" {
		t.Errorf("github PR body: %v", b)
	}
	if !strings.Contains(strings.Join(api.reqs, "\n"), "POST /api/v3/repos/o/r/pulls Bearer ghp") {
		t.Error("github auth header")
	}
	keys, err := gh.GitHubKeys(ctx)
	if err != nil || len(keys) != 1 || !strings.HasPrefix(keys[0], "ssh-ed25519 ") {
		t.Errorf("GitHubKeys = %v, %v", keys, err)
	}
}

func TestScan(t *testing.T) {
	signer, want := hostKey(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { ssh.NewServerConn(c, cfg); c.Close() }()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	keys, err := Scan(context.Background(), "127.0.0.1", port)
	if err != nil || len(keys) != 1 || FormatHostKey(keys[0]) != want {
		t.Fatalf("Scan = %v, %v", keys, err)
	}
	if !strings.HasPrefix(Fingerprint(keys[0]), "ssh-ed25519 SHA256:") {
		t.Error(Fingerprint(keys[0]))
	}
}

func TestAgentModeGuest(t *testing.T) {
	_, k := hostKey(t)
	f := Forge{Name: "acme", Type: Gitea, Host: "git.acme.test", SSHPort: 2222, HostKeys: []string{k}, Auth: Auth{Mode: ModeAgent}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.Hosts(), ","), "git.acme.test:2222") {
		t.Errorf("hosts: %v", f.Hosts())
	}
	repos := []GuestRepo{{Repo: Repo{Forge: "acme", Owner: "t", Name: "a"}, Forge: f}, {Repo: Repo{Forge: "acme", Owner: "t", Name: "b"}, Forge: f}}
	if repos[0].CloneURL() != "curral-forge-acme:t/a.git" {
		t.Errorf("clone url %s", repos[0].CloneURL())
	}
	cfg := SSHConfig(repos, "10.0.2.100:3128")
	if strings.Count(cfg, "Host curral-forge-acme\n") != 1 || strings.Contains(cfg, "IdentityFile") || !strings.Contains(cfg, "Port 2222") || !strings.Contains(cfg, "ProxyCommand nc -X connect") {
		t.Errorf("agent-mode ssh config:\n%s", cfg)
	}
	if !strings.HasPrefix(KnownHosts(repos), "[git.acme.test]:2222 ") {
		t.Errorf("known hosts: %q", KnownHosts(repos))
	}
	f.HostKeys = nil
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "no pinned host keys") {
		t.Errorf("agent mode without host keys: %v", err)
	}
}
