package forge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
)

// sshServer serves SSH with the signers' host keys and returns its port. It
// supports no host-bound auth.
func sshServer(t *testing.T, signers ...ssh.Signer) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	for _, s := range signers {
		cfg.AddHostKey(s)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { ssh.NewServerConn(c, cfg); c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// toServer sends every request to srv, whatever its URL's host (here
// api.github.com).
type toServer struct{ srv *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.srv.Scheme, t.srv.Host
	return http.DefaultTransport.RoundTrip(r)
}

// keysAPI is a forge API: GET /meta lists ssh_keys, GET <prefix>/user
// answers "bot" to the token tok or the basic auth bot:pw, and 401 otherwise.
func keysAPI(t *testing.T, sshKeys ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		switch {
		case r.URL.Path == "/meta" && len(sshKeys) > 0:
			w.Write([]byte(`{"ssh_keys":["` + strings.Join(sshKeys, `","`) + `"]}`))
		case strings.HasSuffix(r.URL.Path, "/user") && (r.Header.Get("Authorization") == "token tok" || r.Header.Get("Authorization") == "Bearer tok"):
			w.Write([]byte(`{"login":"bot"}`))
		case strings.HasSuffix(r.URL.Path, "/user"):
			if u, p, ok := r.BasicAuth(); ok && u == "bot" && p == "pw" {
				w.Write([]byte(`{"login":"bot"}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func githubClient(t *testing.T, srv *httptest.Server) *Client {
	u, _ := url.Parse(srv.URL)
	return &Client{F: Forge{Name: "github", Type: GitHub, Host: "github.com"}, HTTP: &http.Client{Transport: toServer{u}}}
}

func needSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
}

// Forges other than github.com, GitHub Enterprise and a Gitea at github.com
// included, are scanned over SSH and need confirming. Every key type the
// server offers is returned, each with its own fingerprint.
func TestScanHostKeys(t *testing.T) {
	signer, k := hostKey(t)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecSigner, err := ssh.NewSignerFromKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	port := sshServer(t, signer, ecSigner)
	for _, typ := range []string{Gitea, GitHub} {
		c := &Client{F: Forge{Name: "a", Type: typ, Host: "127.0.0.1", SSHPort: port}}
		s, err := c.ScanHostKeys(context.Background())
		want := HostKeyScan{Keys: []string{k, FormatHostKey(ecSigner.PublicKey())}, Fingerprints: []string{Fingerprint(signer.PublicKey()), Fingerprint(ecSigner.PublicKey())}}
		if err != nil || !reflect.DeepEqual(s, want) {
			t.Errorf("%s: %+v, %v; want %+v", typ, s, err, want)
		}
	}
	c := &Client{F: Forge{Name: "a", Type: Gitea, Host: "127.0.0.1", SSHPort: 1}}
	if _, err := c.ScanHostKeys(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "dial tcp 127.0.0.1:1: ") {
		t.Errorf("closed port: %v", err)
	}
	// A Gitea at github.com isn't GitHub: it is scanned (here with a canceled
	// context, so nothing leaves the host), and GitHub's API isn't asked.
	srv, reqs := keysAPI(t, k)
	gc := githubClient(t, srv)
	gc.F.Type = Gitea
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gc.ScanHostKeys(ctx); err == nil || strings.HasPrefix(err.Error(), "fetching GitHub host keys") || len(*reqs) != 0 {
		t.Errorf("gitea at github.com: %v; requests %q", err, *reqs)
	}
}

// github.com's keys come from GitHub's API, already trusted.
func TestScanHostKeysGitHub(t *testing.T) {
	s1, k1 := hostKey(t)
	s2, k2 := hostKey(t)
	srv, reqs := keysAPI(t, k1+" comment", k2)
	s, err := githubClient(t, srv).ScanHostKeys(context.Background())
	want := HostKeyScan{Keys: []string{k1, k2}, Fingerprints: []string{Fingerprint(s1.PublicKey()), Fingerprint(s2.PublicKey())}, Published: true}
	if err != nil || !reflect.DeepEqual(s, want) {
		t.Errorf("%+v, %v; want %+v", s, err, want)
	}
	if !reflect.DeepEqual(*reqs, []string{"GET /meta "}) {
		t.Errorf("requests %q", *reqs)
	}
	bad, _ := keysAPI(t)
	if _, err := githubClient(t, bad).ScanHostKeys(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "fetching GitHub host keys: ") {
		t.Errorf("API error: %v", err)
	}
}

func TestAdd(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "forges.yaml")
	_, k := hostKey(t)
	f := Forge{Name: "acme", Type: Gitea, Host: "git.acme.test", HostKeys: []string{k}, Auth: Auth{Mode: ModeToken, Token: "secret:ACME"}}
	if err := Add(ctx, path, f); err != nil {
		t.Fatal(err)
	}
	other := f
	other.Host = "other.test"
	if err := Add(ctx, path, other); err == nil || err.Error() != "forge acme exists (catchpen forge rm acme first)" {
		t.Errorf("same name: %v", err)
	}
	bad := f
	bad.Name = "Bad"
	if err := Add(ctx, path, bad); err == nil || err.Error() != `forge: invalid name "Bad"` {
		t.Errorf("invalid forge: %v", err)
	}
	reg, err := Load(path)
	if err != nil || len(reg.Forges) != 1 || !reflect.DeepEqual(reg.Forges[0], f) {
		t.Errorf("registry %+v, %v", reg, err)
	}
}

// Concurrent Adds of one name save it once (a duplicate makes the registry
// unloadable).
func TestAddConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forges.yaml")
	f := Forge{Name: "web", Type: Gitea, Host: "git.test", Auth: Auth{Mode: ModeNone}}
	errs := make(chan error, 10)
	for range 10 {
		go func() { errs <- Add(context.Background(), path, f) }()
	}
	ok := 0
	for range 10 {
		if err := <-errs; err == nil {
			ok++
		} else if err.Error() != "forge web exists (catchpen forge rm web first)" {
			t.Error(err)
		}
	}
	if reg, err := Load(path); err != nil || ok != 1 || len(reg.Forges) != 1 {
		t.Errorf("%d Adds succeeded; registry %+v, %v", ok, reg, err)
	}
}

// The name is checked under the registry's file lock, which other processes
// take too: a forge saved by another process while Add waits is seen.
func TestAddChecksUnderFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forges.yaml")
	f := Forge{Name: "web", Type: Gitea, Host: "git.test", Auth: Auth{Mode: ModeNone}}
	unlock, err := fsutil.Lock(path + ".lock") // another holder: flock conflicts between open files, even in one process
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Add(context.Background(), path, f) }()
	time.Sleep(200 * time.Millisecond)
	b, err := yaml.Marshal(Registry{Forges: []Forge{f}}) // what the other process saves
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err == nil || err.Error() != "forge web exists (catchpen forge rm web first)" {
		t.Errorf("Add: %v", err)
	}
	if reg, err := Load(path); err != nil || len(reg.Forges) != 1 {
		t.Errorf("registry %+v, %v", reg, err)
	}
}

// An agent-mode forge needs host-bound auth, checked before anything is saved.
func TestAddAgentNeedsHostBound(t *testing.T) {
	needSSH(t)
	signer, k := hostKey(t)
	path := filepath.Join(t.TempDir(), "forges.yaml")
	f := Forge{Name: "ag", Type: Gitea, Host: "127.0.0.1", SSHPort: sshServer(t, signer), HostKeys: []string{k}, Auth: Auth{Mode: ModeAgent}}
	if err := Add(context.Background(), path, f); err == nil || !strings.HasPrefix(err.Error(), "ag's SSH server does not support host-bound auth") {
		t.Errorf("%v", err)
	}
	if reg, err := Load(path); err != nil || len(reg.Forges) != 0 {
		t.Errorf("registry %+v, %v", reg, err)
	}
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	signer, k := hostKey(t)
	_, other := hostKey(t)
	port := sshServer(t, signer)
	srv, reqs := keysAPI(t)
	gitea := Forge{Name: "acme", Type: Gitea, Host: "127.0.0.1", SSHPort: port, HTTPURL: srv.URL, HostKeys: []string{k}, Auth: Auth{Mode: ModeDeployKey, Token: "secret:T"}}
	fp := Fingerprint(signer.PublicKey())
	changed := gitea
	changed.HostKeys = []string{other}
	closed := gitea
	closed.SSHPort = 1
	ghe := Forge{Name: "ghe", Type: GitHub, Host: "127.0.0.1", SSHPort: port, HTTPURL: srv.URL, HostKeys: []string{k}, Auth: Auth{Mode: ModeDeployKey, Token: "secret:T"}}
	anon := Forge{Name: "anon", Type: Gitea, Host: "127.0.0.1", HTTPURL: srv.URL, Auth: Auth{Mode: ModeNone}}
	for _, c := range []struct {
		name    string
		c       *Client
		want    CheckResult
		err     string // the error, or its start when it ends in "…"
		request string // the one API request, if any
	}{
		{"token", &Client{F: gitea, Token: "tok"}, CheckResult{API: true, Login: "bot", HostKeys: true}, "", "GET /api/v1/user token tok"},
		{"basic", &Client{F: gitea, User: "bot", Password: "pw", Basic: true}, CheckResult{API: true, Login: "bot", HostKeys: true}, "", "GET /api/v1/user Basic Ym90OnB3"},
		{"GitHub Enterprise", &Client{F: ghe, Token: "tok"}, CheckResult{API: true, Login: "bot", HostKeys: true}, "", "GET /api/v3/user Bearer tok"},
		{"no credentials or keys", &Client{F: anon}, CheckResult{}, "", ""},
		{"bad token", &Client{F: gitea, Token: "nope"}, CheckResult{}, "API: …", "GET /api/v1/user token nope"},
		{"key changed", &Client{F: changed, Token: "tok"}, CheckResult{API: true, Login: "bot"},
			"SSH host key changed: " + fp + " is not pinned (if expected: catchpen forge rm acme && catchpen forge add ...)", "GET /api/v1/user token tok"},
		{"SSH unreachable", &Client{F: closed, Token: "tok"}, CheckResult{API: true, Login: "bot"}, "SSH: dial tcp 127.0.0.1:1: …", "GET /api/v1/user token tok"},
	} {
		*reqs = nil
		r, err := c.c.Check(ctx)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if pre, ok := strings.CutSuffix(c.err, "…"); ok && strings.HasPrefix(got, pre) {
			got = c.err
		}
		if r != c.want || got != c.err {
			t.Errorf("%s: %+v, %q; want %+v, %q", c.name, r, got, c.want, c.err)
		}
		if want := []string{c.request}; c.request == "" && len(*reqs) != 0 || c.request != "" && !reflect.DeepEqual(*reqs, want) {
			t.Errorf("%s: requests %q, want %q", c.name, *reqs, c.request)
		}
	}
}

// A Gitea at github.com isn't GitHub: Check scans its pinned keys (here with
// a canceled context, so nothing leaves the host).
func TestCheckGiteaAtGitHubCom(t *testing.T) {
	_, k := hostKey(t)
	f := Forge{Name: "g", Type: Gitea, Host: "github.com", HostKeys: []string{k}, Auth: Auth{Mode: ModeNone}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := (&Client{F: f}).Check(ctx); err == nil || !strings.HasPrefix(err.Error(), "SSH: ") || r != (CheckResult{}) {
		t.Errorf("%+v, %v", r, err)
	}
}

// github.com's pinned keys come from its API, so Check doesn't scan them.
func TestCheckGitHub(t *testing.T) {
	_, k := hostKey(t)
	srv, reqs := keysAPI(t)
	c := githubClient(t, srv)
	c.F.HostKeys, c.F.Auth, c.Token = []string{k}, Auth{Mode: ModeDeployKey, Token: "secret:GH"}, "tok"
	r, err := c.Check(context.Background())
	if err != nil || r != (CheckResult{API: true, Login: "bot"}) || !reflect.DeepEqual(*reqs, []string{"GET /user Bearer tok"}) {
		t.Errorf("%+v, %v; requests %q", r, err, *reqs)
	}
}

// In agent mode Check also needs host-bound auth, after the host keys.
func TestCheckAgent(t *testing.T) {
	needSSH(t)
	signer, k := hostKey(t)
	f := Forge{Name: "ag", Type: Gitea, Host: "127.0.0.1", SSHPort: sshServer(t, signer), HostKeys: []string{k}, Auth: Auth{Mode: ModeAgent}}
	r, err := (&Client{F: f}).Check(context.Background())
	if r != (CheckResult{HostKeys: true}) || err == nil || !strings.HasPrefix(err.Error(), "ag's SSH server does not support host-bound auth") {
		t.Errorf("%+v, %v", r, err)
	}
}
