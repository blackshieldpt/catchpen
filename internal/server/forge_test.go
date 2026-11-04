package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// hostKey is a new SSH host key, as a signer and as forges.yaml pins it.
func hostKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s, forge.FormatHostKey(s.PublicKey())
}

// sshServer serves SSH on 127.0.0.1 with s's host key, and no host-bound
// auth; it returns its port.
func sshServer(t *testing.T, s ssh.Signer) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(s)
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

// forgesYAML writes the home's forges.yaml.
func (h *vmHost) forgesYAML(t *testing.T, fs ...forge.Forge) {
	t.Helper()
	if err := forge.Update(h.p.Forges(), func(r *forge.Registry) error { r.Forges = fs; return nil }); err != nil {
		t.Fatal(err)
	}
}

// The forges are listed with their secrets' names, never their values.
func TestForges(t *testing.T) {
	h := home(t, nil)
	c := h.client(t)
	ctx := context.Background()
	if st, body := h.raw(t, api.PathForges); st != http.StatusOK || body != "[]\n" {
		t.Errorf("none: %d %s", st, body)
	}
	_, key := hostKey(t)
	pk, _ := forge.ParseHostKey(key)
	fp := forge.Fingerprint(pk)
	write(t, h.p.SecretsFile(), "GH_TOKEN="+decoy+"\nACME_PW="+decoy+"\n")
	h.forgesYAML(t,
		forge.Forge{Name: "gh", Type: forge.GitHub, Host: "github.com", HostKeys: []string{key}, Auth: forge.Auth{Mode: forge.ModeDeployKey, Token: "secret:GH_TOKEN"}},
		forge.Forge{Name: "acme", Type: forge.Gitea, Host: "git.acme.test", HTTPURL: "https://git.acme.test:3000", SSHPort: 2222,
			Auth: forge.Auth{Mode: forge.ModeMintedToken, User: "bot", Password: "secret:ACME_PW", Scopes: []string{"write:repository"}}})
	fs, err := c.Forges(ctx)
	want := []api.ForgeInfo{
		{Name: "gh", Type: "github", Host: "github.com", WebURL: "https://github.com", SSHPort: 22, Mode: "deploy-key", TokenSecret: "GH_TOKEN",
			Scopes: []string{}, HostKeys: []string{fp}},
		{Name: "acme", Type: "gitea", Host: "git.acme.test", WebURL: "https://git.acme.test:3000", SSHPort: 2222, Mode: "minted-token",
			User: "bot", PasswordSecret: "ACME_PW", Scopes: []string{"write:repository"}, HostKeys: []string{}},
	}
	if err != nil || !reflect.DeepEqual(fs, want) {
		t.Errorf("forges: %+v %v", fs, err)
	}
	_, body := h.raw(t, api.PathForges)
	if strings.Contains(body, decoy) || !strings.Contains(body, `{"name":"acme","type":"gitea","host":"git.acme.test","web_url":"https://git.acme.test:3000",`+
		`"ssh_port":2222,"mode":"minted-token","user":"bot","password_secret":"ACME_PW","scopes":["write:repository"],"host_keys":[]}`) ||
		!strings.Contains(body, `"token_secret":"GH_TOKEN"`) {
		t.Errorf("on the wire: %s", body)
	}
}

// A forge is scanned, then added with the fingerprints confirmed, which the
// server checks against a scan of its own; whatever is refused adds nothing.
func TestScanAndAddForge(t *testing.T) {
	h := home(t, nil)
	c := h.client(t)
	ctx := context.Background()
	signer, key := hostKey(t)
	port := sshServer(t, signer)
	fp := forge.Fingerprint(signer.PublicKey())
	scan, err := c.ScanForge(ctx, api.ForgeScanRequest{Type: "gitea", Host: "127.0.0.1", SSHPort: port})
	if err != nil || !reflect.DeepEqual(*scan, api.ForgeScan{Fingerprints: []string{fp}}) {
		t.Fatalf("scan: %+v %v", scan, err)
	}
	if st, body := h.post(t, api.PathForgeScan, `{"type":"gitea","host":"127.0.0.1","http_url":"http://127.0.0.1:1","ssh_port":`+strconv.Itoa(port)+`}`); st != http.StatusOK ||
		body != `{"fingerprints":["`+fp+`"],"published":false}`+"\n" {
		t.Errorf("scan on the wire: %d %s", st, body)
	}
	add := api.AddForgeRequest{Name: "acme", Type: "gitea", Host: "127.0.0.1", HTTPURL: "http://127.0.0.1:1", SSHPort: port,
		Mode: "deploy-key", TokenSecret: "ACME_TOKEN", Fingerprints: []string{fp}}
	var e *api.Error
	for _, tc := range []struct {
		name string
		edit func(*api.AddForgeRequest)
		code string
		msg  string
	}{
		{"no fingerprints", func(r *api.AddForgeRequest) { r.Fingerprints = nil }, api.CodeBadRequest,
			"auth mode deploy-key pins the forge's SSH host keys: scan them, and send the fingerprints you confirmed"},
		{"other fingerprints", func(r *api.AddForgeRequest) { r.Fingerprints = []string{"ssh-ed25519 SHA256:other"} }, api.CodeConflict,
			"the forge's host keys are not those confirmed: scan them again"},
		{"more fingerprints", func(r *api.AddForgeRequest) { r.Fingerprints = []string{fp, "ssh-ed25519 SHA256:other"} }, api.CodeConflict,
			"the forge's host keys are not those confirmed: scan them again"},
		{"bad host", func(r *api.AddForgeRequest) { r.Host = "bad host" }, api.CodeBadRequest, `forge acme: invalid host "bad host"`},
		{"bad name", func(r *api.AddForgeRequest) { r.Name = "Acme" }, api.CodeBadRequest, `forge: invalid name "Acme"`},
		{"bad secret", func(r *api.AddForgeRequest) { r.TokenSecret = "acme" }, api.CodeBadRequest, "forge acme: auth.token must be secret:NAME"},
		{"no secret", func(r *api.AddForgeRequest) { r.TokenSecret = "" }, api.CodeBadRequest, "forge acme: auth.mode deploy-key needs auth.token"},
		{"nothing listening", func(r *api.AddForgeRequest) { r.SSHPort = 1 }, api.CodeBadRequest, "the forge's host keys can't be read: "},
	} {
		r := add
		tc.edit(&r)
		if _, err := c.AddForge(ctx, r); !errors.As(err, &e) || e.Code != tc.code || !strings.HasPrefix(e.Message, tc.msg) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if fs, err := c.Forges(ctx); err != nil || len(fs) != 0 {
		t.Fatalf("after the refusals: %+v %v", fs, err)
	}

	f, err := c.AddForge(ctx, add)
	want := api.ForgeInfo{Name: "acme", Type: "gitea", Host: "127.0.0.1", WebURL: "http://127.0.0.1:1", SSHPort: port, Mode: "deploy-key",
		TokenSecret: "ACME_TOKEN", Scopes: []string{"write:repository", "read:user"}, HostKeys: []string{fp}}
	if err != nil || !reflect.DeepEqual(*f, want) {
		t.Fatalf("add: %+v %v", f, err)
	}
	reg, err := forge.Load(h.p.Forges())
	if g, ok := reg.Get("acme"); err != nil || !ok || !reflect.DeepEqual(g.HostKeys, []string{key}) || g.Auth.Token != "secret:ACME_TOKEN" {
		t.Errorf("forges.yaml: %+v %v", reg, err)
	}
	if ev := lastEvent(t, h.p, audit.ForgeAdd); ev.VM != "" || ev.Detail["forge"] != "acme" || ev.Detail["mode"] != "deploy-key" ||
		ev.Detail["client"] != api.ClientLocal || !reflect.DeepEqual(ev.Detail["host_keys"], []any{fp}) || ev.Detail["token_secret"] != "ACME_TOKEN" {
		t.Errorf("audit: %+v", ev)
	}
	again := add
	again.SSHPort = 1 // refused before a scan, which would fail here
	if _, err := c.AddForge(ctx, again); !errors.As(err, &e) || e.Code != api.CodeConflict || e.Message != "forge acme exists (catchpen forge rm acme first)" {
		t.Errorf("again: %v", err)
	}

	// A mode that doesn't use SSH needs no keys, and is added without a scan.
	none, err := c.AddForge(ctx, api.AddForgeRequest{Name: "anon", Type: "gitea", Host: "127.0.0.1", SSHPort: 1, Mode: "none"})
	if err != nil || len(none.HostKeys) != 0 || none.Mode != "none" {
		t.Errorf("mode none: %+v %v", none, err)
	}
	if _, err := c.ScanForge(ctx, api.ForgeScanRequest{Type: "gitlab", Host: "127.0.0.1", SSHPort: 1}); !errors.As(err, &e) || e.Code != api.CodeBadRequest ||
		e.Message != "forge scan: type must be github or gitea" {
		t.Errorf("scan of a bad type: %v", err)
	}
	for _, body := range []string{`{"type":"gitea","host":"127.0.0.1","fingerprints":[]}`} {
		if st, got := h.post(t, api.PathForgeScan, body); st != http.StatusBadRequest {
			t.Errorf("scan with an unknown field: %d %s", st, got)
		}
	}
	if st, got := h.post(t, api.PathForges, `{"name":"x","type":"gitea","host":"127.0.0.1","mode":"none","host_keys":["k"]}`); st != http.StatusBadRequest {
		t.Errorf("add with host_keys: %d %s", st, got)
	}
	if st, got := h.post(t, api.PathForges, `{"name":"wire","type":"gitea","host":"127.0.0.1","http_url":"http://127.0.0.1:1","ssh_port":`+
		strconv.Itoa(port)+`,"mode":"minted-token","user":"bot","password_secret":"PW","scopes":["read:user"],"fingerprints":["`+fp+`"]}`); st != http.StatusOK ||
		!strings.Contains(got, `"user":"bot","password_secret":"PW","scopes":["read:user"],"host_keys":["`+fp+`"]`) {
		t.Errorf("add on the wire: %d %s", st, got)
	}
}

// An agent-mode forge needs host-bound auth, which the test SSH server
// hasn't.
func TestAddForgeAgent(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("needs ssh")
	}
	h := home(t, nil)
	c := h.client(t)
	signer, _ := hostKey(t)
	port := sshServer(t, signer)
	_, err := c.AddForge(context.Background(), api.AddForgeRequest{Name: "acme", Type: "gitea", Host: "127.0.0.1", SSHPort: port,
		Mode: "agent", Fingerprints: []string{forge.Fingerprint(signer.PublicKey())}})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConflict || !strings.Contains(e.Message, "does not support host-bound auth") {
		t.Errorf("agent: %v", err)
	}
	if fs, err := c.Forges(context.Background()); err != nil || len(fs) != 0 {
		t.Errorf("added: %+v %v", fs, err)
	}
}

// A test checks the forge from the registry with the host's secret, and
// tells what failed rather than failing.
func TestTestForge(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "token "+decoy {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"login":"bot"}`))
	}))
	defer srv.Close()
	var h *vmHost
	h = home(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}, HTTP: srv.Client()}, nil
	})
	write(t, h.p.SecretsFile(), "ACME_TOKEN="+decoy+"\n")
	h.forgesYAML(t,
		forge.Forge{Name: "acme", Type: forge.Gitea, Host: "127.0.0.1", HTTPURL: srv.URL, Auth: forge.Auth{Mode: forge.ModeToken, Token: "secret:ACME_TOKEN"}},
		forge.Forge{Name: "gone", Type: forge.Gitea, Host: "127.0.0.1", HTTPURL: srv.URL, Auth: forge.Auth{Mode: forge.ModeToken, Token: "secret:GONE"}})
	c := h.client(t)
	ctx := context.Background()
	if r, err := c.TestForge(ctx, "acme"); err != nil || *r != (api.ForgeCheck{API: true, Login: "bot"}) ||
		!reflect.DeepEqual(auth, []string{"GET /api/v1/user token " + decoy}) {
		t.Errorf("acme: %+v %v; requests %q", r, err, auth)
	}
	auth = nil
	if r, err := c.TestForge(ctx, "gone"); err != nil || *r != (api.ForgeCheck{Error: "secret GONE not found on the host"}) || len(auth) != 0 {
		t.Errorf("gone: %+v %v; requests %q", r, err, auth)
	}
	if st, body := h.post(t, api.PathForgeTest("acme"), ""); st != http.StatusOK || strings.Contains(body, decoy) ||
		body != `{"api":true,"login":"bot","host_keys":false,"host_bound":false}`+"\n" {
		t.Errorf("on the wire: %d %s", st, body)
	}
	if st, body := h.post(t, api.PathForgeTest("acme"), `{"token":"x"}`); st != http.StatusBadRequest {
		t.Errorf("with a body: %d %s", st, body)
	}
	if _, err := c.TestForge(ctx, "nope"); code(err) != api.CodeNotFound {
		t.Errorf("nope: %v", err)
	}
}

// A forge's removal needs confirm, and is audited with the client.
func TestDeleteForge(t *testing.T) {
	h := home(t, nil)
	h.forgesYAML(t, forge.Forge{Name: "a", Type: forge.Gitea, Host: "a.test", Auth: forge.Auth{Mode: forge.ModeNone}},
		forge.Forge{Name: "b", Type: forge.Gitea, Host: "b.test", Auth: forge.Auth{Mode: forge.ModeNone}})
	c := h.client(t)
	ctx := context.Background()
	var e *api.Error
	if _, err := c.DeleteForge(ctx, "a", false); !errors.As(err, &e) || e.Code != api.CodeConfirmRequired ||
		e.Message != `removing forge a can't be undone (VMs keep their copy of it): send "confirm": true` {
		t.Errorf("without confirm: %v", err)
	}
	for _, confirm := range []bool{false, true} {
		if _, err := c.DeleteForge(ctx, "nope", confirm); code(err) != api.CodeNotFound {
			t.Errorf("nope (confirm %v): %v", confirm, err)
		}
	}
	if fs, err := c.Forges(ctx); err != nil || len(fs) != 2 {
		t.Fatalf("after the refusals: %+v %v", fs, err)
	}
	fs, err := c.DeleteForge(ctx, "a", true)
	if err != nil || len(fs) != 1 || fs[0].Name != "b" {
		t.Errorf("delete: %+v %v", fs, err)
	}
	if reg, err := forge.Load(h.p.Forges()); err != nil || len(reg.Forges) != 1 || reg.Forges[0].Name != "b" {
		t.Errorf("forges.yaml: %+v %v", reg, err)
	}
	if ev := lastEvent(t, h.p, audit.ForgeRemove); ev.Detail["forge"] != "a" || ev.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", ev)
	}
	if st, body := h.post(t, api.PathForgeDelete("b"), `{"confirm":true}`); st != http.StatusOK || body != "[]\n" {
		t.Errorf("on the wire: %d %s", st, body)
	}
}

// Over TCP, scanning, adding and removing a forge need config:write;
// listing and testing don't. The audit names the client.
func TestForgeScopes(t *testing.T) {
	h := home(t, nil)
	h.forgesYAML(t, forge.Forge{Name: "a", Type: forge.Gitea, Host: "a.test", Auth: forge.Auth{Mode: forge.ModeNone}})
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	laptop := a.client(t, apiclient.Options{Cert: &a.laptop}) // no scope
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	ctx := context.Background()
	add := api.AddForgeRequest{Name: "b", Type: "gitea", Host: "b.test", Mode: "none"}
	var e *api.Error
	for name, err := range map[string]error{
		"scan":   second(laptop.ScanForge(ctx, api.ForgeScanRequest{Type: "gitea", Host: "127.0.0.1", SSHPort: 1})),
		"add":    second(laptop.AddForge(ctx, add)),
		"delete": second(laptop.DeleteForge(ctx, "a", true)),
	} {
		if !errors.As(err, &e) || e.Code != api.CodeForbidden || e.Message != "this call needs the config:write scope (catchpen daemon client add --scope config:write)" {
			t.Errorf("%s without the scope: %v", name, err)
		}
	}
	if fs, err := laptop.Forges(ctx); err != nil || len(fs) != 1 {
		t.Errorf("list without the scope: %+v %v", fs, err)
	}
	if r, err := laptop.TestForge(ctx, "a"); err != nil || *r != (api.ForgeCheck{}) {
		t.Errorf("test without the scope: %+v %v", r, err)
	}
	if _, err := cockpit.AddForge(ctx, add); err != nil {
		t.Fatal(err)
	}
	if _, err := cockpit.DeleteForge(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{audit.ForgeAdd, audit.ForgeRemove} {
		if e := lastEvent(t, h.p, ev); e.Detail["client"] != "cockpit" {
			t.Errorf("%s: %+v", ev, e)
		}
	}
}

// gitRepo is a new git repository on the host.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// Pull and pr take an absolute host repository and no host file paths; each
// is a job that runs the Manager's.
func TestPullAndPR(t *testing.T) {
	h := home(t, nil)
	m := &vm.Manager{P: h.p}
	repos := []vm.RepoState{{Repo: forge.Repo{Forge: "gh", Owner: "o", Name: "app"}}}
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created, Repos: repos})
	vmtest.FakeQEMU(t, h.p.VM("beta"), "beta", 512, func() bool { _, ok := m.Running("beta"); return ok })
	h.writeVM(t, vm.State{Name: "beta", Template: "base", Created: created, Repos: repos})
	write(t, filepath.Join(h.p.VM("beta"), "paused.json"), "{}")
	into := gitRepo(t)
	c := h.client(t)
	ctx := context.Background()
	var e *api.Error
	for _, bad := range []string{"", "repo", "./repo", into + "/", into + "/../" + filepath.Base(into)} {
		for name, err := range map[string]error{
			"pull": second(c.PullVM(ctx, "alpha", api.PullVMRequest{Into: bad})),
			"pr":   second(c.PRVM(ctx, "alpha", api.PRVMRequest{PullVMRequest: api.PullVMRequest{Into: bad}})),
		} {
			if !errors.As(err, &e) || e.Code != api.CodeBadRequest || e.Message != "into is the absolute path of a git repository on the daemon's host" {
				t.Errorf("%s into %q: %v", name, bad, err)
			}
		}
	}
	for path, body := range map[string]string{
		api.PathVMPull("alpha"): `{"into":"` + into + `","patches":"/tmp"}`,
		api.PathVMPR("alpha"):   `{"into":"` + into + `","body_file":"/etc/passwd"}`,
	} {
		if st, got := h.post(t, path, body); st != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, st, got)
		}
	}
	for _, head := range []string{"main", "catchpenx", "refs/heads/catchpen/x"} {
		_, err := c.PRVM(ctx, "alpha", api.PRVMRequest{PullVMRequest: api.PullVMRequest{Into: into}, Head: head})
		if !errors.As(err, &e) || e.Code != api.CodeBadRequest || e.Message != "head is a branch under catchpen/ (default catchpen/<vm>/<branch>)" {
			t.Errorf("pr to %s: %v", head, err)
		}
	}
	if _, err := c.PullVM(ctx, "beta", api.PullVMRequest{Into: into}); code(err) != api.CodeVMPaused {
		t.Errorf("pull of a paused VM: %v", err)
	}
	if _, err := c.PRVM(ctx, "beta", api.PRVMRequest{PullVMRequest: api.PullVMRequest{Into: into}}); code(err) != api.CodeVMPaused {
		t.Errorf("pr of a paused VM: %v", err)
	}
	if _, err := c.PullVM(ctx, "nope", api.PullVMRequest{Into: into}); code(err) != api.CodeNotFound {
		t.Errorf("nope: %v", err)
	}
	if js, err := c.Jobs(ctx); err != nil || len(js) != 0 {
		t.Errorf("jobs after the refusals: %+v %v", js, err)
	}

	wait := waiter(t, c)
	notGit := t.TempDir()
	if j, _ := wait(c.PullVM(ctx, "alpha", api.PullVMRequest{Into: notGit})); j.State != api.JobFailed || j.Kind != api.JobExportPull ||
		!strings.HasPrefix(j.Error, notGit+" is not a git repository") {
		t.Errorf("pull into a directory: %+v", j)
	}
	notRunning := "VM alpha is not running (start it: catchpen start alpha)"
	if j, _ := wait(c.PullVM(ctx, "alpha", api.PullVMRequest{Repo: "app", Into: into})); j.State != api.JobFailed || j.Kind != api.JobExportPull ||
		j.Error != notRunning {
		t.Errorf("pull: %+v", j)
	}
	if j, _ := wait(c.PRVM(ctx, "alpha", api.PRVMRequest{PullVMRequest: api.PullVMRequest{Repo: "nope", Into: into}})); j.State != api.JobFailed ||
		j.Kind != api.JobExportPR || j.Error != `alpha has no repo "nope"` {
		t.Errorf("pr: %+v", j)
	}
	for path, body := range map[string]string{
		api.PathVMPull("alpha"): `{"repo":"app","branch":"main","into":"` + into + `"}`,
		api.PathVMPR("alpha"): `{"repo":"app","branch":"main","into":"` + into + `","title":"T","body":"B","draft":true,` +
			`"base":"main","head":"catchpen/h"}`,
	} {
		if st, got := h.post(t, path, body); st != http.StatusAccepted {
			t.Errorf("%s: %d %s", body, st, got)
		}
	}
}

// Every field of a pull and pr request reaches the Manager's options.
func TestExportOpts(t *testing.T) {
	req := api.PRVMRequest{PullVMRequest: api.PullVMRequest{Repo: "app", Branch: "main", Into: "/src/app"},
		Title: "T", Body: "B", Draft: true, Base: "dev", Head: "h"}
	o, ok := pullOpts(httptest.NewRecorder(), req.PullVMRequest)
	if want := (vm.PullOpts{Repo: "app", Branch: "main", Into: "/src/app"}); !ok || o != want {
		t.Errorf("pull: %+v %v", o, ok)
	}
	if got, want := prOpts(o, req), (vm.PROpts{PullOpts: o, Title: "T", Body: "B", Draft: true, Base: "dev", Head: "h"}); got != want {
		t.Errorf("pr: %+v", got)
	}
}
