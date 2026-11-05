package vm

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackshieldpt/catchpen/internal/creds"
	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/notify"
)

// TestForge takes the forge's credentials from the host's secrets and calls
// the API through m.HTTP.
func TestTestForge(t *testing.T) {
	var auth []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.Write([]byte(`{"login":"bot"}`))
	}))
	defer srv.Close()
	m := testManager(t)
	m.HTTP = srv.Client() // the only client that trusts srv's certificate
	writeFile(t, m.P.SecretsFile(), "ACME_TOKEN=tok\nACME_PW=pw\n")
	ctx := context.Background()
	token := forge.Forge{Name: "acme", Type: forge.Gitea, Host: "git.acme.test", HTTPURL: srv.URL, Auth: forge.Auth{Mode: forge.ModeToken, Token: "secret:ACME_TOKEN"}}
	minted := forge.Forge{Name: "acme", Type: forge.Gitea, Host: "git.acme.test", HTTPURL: srv.URL, Auth: forge.Auth{Mode: forge.ModeMintedToken, User: "bot", Password: "secret:ACME_PW"}}
	for _, c := range []struct {
		f    forge.Forge
		auth string
	}{{token, "GET /api/v1/user token tok"}, {minted, "GET /api/v1/user Basic Ym90OnB3"}} {
		auth = nil
		r, err := m.TestForge(ctx, c.f)
		if err != nil || r != (forge.CheckResult{API: true, Login: "bot"}) || len(auth) != 1 || auth[0] != c.auth {
			t.Errorf("%s: %+v, %v; requests %q, want %q", c.f.Auth.Mode, r, err, auth, c.auth)
		}
	}
	both := minted
	both.Auth.Token = "secret:ACME_TOKEN" // the token wins, as in testForge
	auth = nil
	if r, err := m.TestForge(ctx, both); err != nil || r != (forge.CheckResult{API: true, Login: "bot"}) || len(auth) != 1 || auth[0] != "GET /api/v1/user token tok" {
		t.Errorf("token and password: %+v, %v; requests %q", r, err, auth)
	}
	missing := token
	missing.Auth.Token = "secret:ACME_MISSING"
	auth = nil
	if r, err := m.TestForge(ctx, missing); err == nil || err.Error() != "secret ACME_MISSING not found on the host" || r != (forge.CheckResult{}) || len(auth) != 0 {
		t.Errorf("missing secret: %+v, %v; requests %q", r, err, auth)
	}
}

// A host secret that is missing, or whose source fails, is a *SecretError
// with the resolver's text, from a forge's auth and from a push's env; a
// push names the missing ones on a line that starts with MissingSecrets.
func TestSecretErrors(t *testing.T) {
	m := testManager(t)
	m.Cfg.Secrets = map[string]creds.Source{"CMD_PW": {Command: []string{"false"}}} // its source fails
	var out bytes.Buffer
	m.Out = &out
	ctx := context.Background()
	for _, tc := range []struct{ ref, want string }{
		{"secret:GONE_061", "secret GONE_061 not found on the host"},
		{"secret:CMD_PW", "secret CMD_PW: false: exit status 1"},
	} {
		_, err := secretRef(ctx, m.resolver(), tc.ref)
		var se *SecretError
		if !errors.As(err, &se) || err.Error() != tc.want {
			t.Errorf("%s: %#v", tc.ref, err)
		}
	}
	// The env is resolved, sorted, before the guest is reached: A is missed,
	// B fails.
	err := m.push(ctx, &Guest{}, &State{Name: "a", User: "dev", Env: map[string]string{"A": "secret:GONE_061", "B": "secret:CMD_PW"}})
	var se *SecretError
	if !errors.As(err, &se) || err.Error() != "secret CMD_PW: false: exit status 1" {
		t.Errorf("push: %#v", err)
	}
	if !strings.HasPrefix(out.String(), MissingSecrets+"GONE_061 (") {
		t.Errorf("push's lines: %q", out.String())
	}
}

// The callers between secretRef and a job wrap its error with %w, so the API
// still finds the *SecretError: provisionForges (a create), forgeCreds (a
// push) and apiClient (a pr), each failing before any network.
func TestSecretErrorsWrapped(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	gone := "secret:GONE_061"
	tok := forge.Forge{Name: "tok", Type: forge.Gitea, Host: "git.test", Auth: forge.Auth{Mode: forge.ModeToken, Token: gone}}
	mint := forge.Forge{Name: "mint", Type: forge.Gitea, Host: "git.test", Auth: forge.Auth{Mode: forge.ModeMintedToken, User: "bot", Password: gone}}
	dk := forge.Forge{Name: "dk", Type: forge.Gitea, Host: "git.test", Auth: forge.Auth{Mode: forge.ModeDeployKey, Token: gone}}
	provision := func(f forge.Forge) error {
		return m.provisionForges(ctx, &State{Name: "a", Repos: []RepoState{{Repo: forge.Repo{Forge: f.Name, Owner: "o", Name: "r"}}},
			Forges: map[string]forge.Forge{f.Name: f}})
	}
	second := func(_ any, err error) error { return err }
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"provisionForges, minted-token", provision(mint)},
		{"provisionForges, token", provision(tok)},
		{"provisionForges, deploy-key", provision(dk)},
		{"forgeCreds, token", second(m.forgeCreds(ctx, &State{Name: "a", Forges: map[string]forge.Forge{"tok": tok}}))},
		{"apiClient, token", second(m.apiClient(ctx, tok))},
		{"apiClient, password", second(m.apiClient(ctx, mint))},
	} {
		var se *SecretError
		if !errors.As(tc.err, &se) || !strings.HasSuffix(tc.err.Error(), ": secret GONE_061 not found on the host") {
			t.Errorf("%s: %#v", tc.what, tc.err)
		}
	}
}

// secretOut is an Out that keeps apart the text written to it as naming
// host secrets (SecretLines), as an API job does.
type secretOut struct{ plain, secret strings.Builder }

func (o *secretOut) Write(p []byte) (int, error)       { return o.plain.Write(p) }
func (o *secretOut) WriteSecret(p []byte) (int, error) { return o.secret.Write(p) }

// Each line that names, or can name, a host secret is written through
// SecretLines, in the text the CLI prints: releaseForges' warnings when a
// forge's secret is missing (not when its forge fails), the notification
// warning, the env line and the missing-secrets line.
func TestSecretLines(t *testing.T) {
	ctx := context.Background()
	gone := "secret:GONE_063"
	check := func(what string, o *secretOut, want string) {
		t.Helper()
		if o.secret.String() != want || o.plain.String() != "" {
			t.Errorf("%s: secret %q, plain %q; want secret %q", what, o.secret.String(), o.plain.String(), want)
		}
	}

	m := testManager(t)
	var o secretOut
	m.Out = &o
	// releaseForges: dk's and mint's secrets are missing; ok's and mintok's are
	// found, and their forge answers 500, an error that names no secret.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }))
	defer srv.Close()
	writeFile(t, m.P.SecretsFile(), "OK_063=x\n")
	writeFile(t, filepath.Join(m.P.VM("a"), "forge", "tokens.json"), `{"mint":{"id":3,"user":"bot","token":"t"},"mintok":{"id":4,"user":"bot","token":"t"}}`)
	m.releaseForges(&State{Name: "a", Repos: []RepoState{{Repo: forge.Repo{Forge: "dk", Owner: "o", Name: "r"}, DeployKeyID: 7},
		{Repo: forge.Repo{Forge: "ok", Owner: "o", Name: "r"}, DeployKeyID: 8}},
		Forges: map[string]forge.Forge{
			"dk":     {Name: "dk", Type: forge.Gitea, Host: "git.test", Auth: forge.Auth{Mode: forge.ModeDeployKey, Token: gone}},
			"ok":     {Name: "ok", Type: forge.Gitea, Host: "git.test", HTTPURL: srv.URL, Auth: forge.Auth{Mode: forge.ModeDeployKey, Token: "secret:OK_063"}},
			"mint":   {Name: "mint", Type: forge.Gitea, Host: "git.test", Auth: forge.Auth{Mode: forge.ModeMintedToken, User: "bot", Password: gone + "P"}},
			"mintok": {Name: "mintok", Type: forge.Gitea, Host: "git.test", HTTPURL: srv.URL, Auth: forge.Auth{Mode: forge.ModeMintedToken, User: "bot", Password: "secret:OK_063"}},
		}})
	if want := "warning: could not remove deploy key 7 from dk:o/r: secret GONE_063 not found on the host (remove it in the forge's repo settings)\n" +
		"warning: could not delete mint token 3 for bot: secret GONE_063P not found on the host (delete it in the user's settings)\n"; o.secret.String() != want {
		t.Errorf("releaseForges, secrets missing: %q, want %q", o.secret.String(), want)
	}
	if want := "warning: could not remove deploy key 8 from ok:o/r: DELETE ok/repos/o/r/keys/8: HTTP 500: Internal Server Error (remove it in the forge's repo settings)\n" +
		"warning: could not delete mintok token 4 for bot: DELETE mintok/users/bot/tokens/4: HTTP 500: Internal Server Error (delete it in the user's settings)\n"; o.plain.String() != want {
		t.Errorf("releaseForges, forge errors: %q, want %q", o.plain.String(), want)
	}

	o = secretOut{}
	m.Cfg.Notify = notify.Config{Sinks: []notify.Sink{{Webhook: &notify.Webhook{URL: "http://127.0.0.1:1/", Headers: map[string]string{"Authorization": gone}}}}}
	m.notify(notify.Event{Kind: notify.VMIdleStop, VM: "a", Title: "t", Message: "m"})
	check("notify", &o, "warning: notification: webhook header Authorization: secret GONE_063 not found\n")

	o = secretOut{}
	m.Cfg.Secrets = map[string]creds.Source{"CMD_PW": {Command: []string{"false"}}} // B's source fails: push stops before the guest
	err := m.push(ctx, &Guest{}, &State{Name: "a", User: "dev", Env: map[string]string{"A": gone, "B": "secret:CMD_PW"}})
	if err == nil {
		t.Fatal("push: no error")
	}
	check("push", &o, MissingSecrets+"GONE_063 (set them in the environment, "+m.P.SecretsFile()+", or config.yaml secrets)\n")

	e := envVM(t, nil, "env: { GH_TOKEN: \"secret:GH_TOKEN\" }\n")
	o = secretOut{}
	e.Out = &o
	if err := e.refreshEnv("dev"); err != nil {
		t.Fatal(err)
	}
	// Its skills line, which names no secret, is written as before.
	if want := "dev: env from template envtpl: +GH_TOKEN\n"; o.secret.String() != want || strings.Contains(o.plain.String(), "GH_TOKEN") {
		t.Errorf("refreshEnv: secret %q, plain %q; want secret %q", o.secret.String(), o.plain.String(), want)
	}
}
