package vm

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blackshieldpt/catchpen/internal/creds"
	"github.com/blackshieldpt/catchpen/internal/forge"
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
