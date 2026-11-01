package vm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
