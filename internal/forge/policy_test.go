package forge

import (
	"net/http"
	"net/url"
	"testing"
)

func TestCheckRedirect(t *testing.T) {
	req := func(method, u string) *http.Request {
		r, _ := http.NewRequest(method, u, nil)
		return r
	}
	for _, tc := range []struct {
		name     string
		from, to *http.Request
		ok       bool
	}{
		{"same host", req("GET", "https://h/a"), req("GET", "https://h/b"), true},
		{"http to https", req("POST", "http://h/a"), req("POST", "https://h/a"), true},
		{"https to http", req("GET", "https://h/a"), req("GET", "http://h/a"), false},
		{"another host", req("GET", "https://h/a"), req("GET", "https://evil/a"), false},
		{"another port", req("GET", "https://h/a"), req("GET", "https://h:8443/a"), false},
		{"POST becomes GET", req("POST", "https://h/a"), req("GET", "https://h/b"), false},
	} {
		if err := checkRedirect(tc.to, []*http.Request{tc.from}); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	via := []*http.Request{req("GET", "https://h/1"), req("GET", "https://h/2"), req("GET", "https://h/3"), req("GET", "https://h/4"), req("GET", "https://h/5")}
	if checkRedirect(req("GET", "https://h/6"), via) == nil {
		t.Error("a sixth hop was allowed")
	}
}

func TestCleartextCredentials(t *testing.T) {
	tok, pw := Auth{Mode: ModeDeployKey, Token: "secret:T"}, Auth{Mode: ModeMintedToken, User: "u", Password: "secret:P"}
	for _, tc := range []struct {
		f    Forge
		want bool
	}{
		{Forge{Type: Gitea, Host: "h", HTTPURL: "http://h:3000", Auth: tok}, true},
		{Forge{Type: Gitea, Host: "h", HTTPURL: "http://h:3000", Auth: pw}, true},
		{Forge{Type: Gitea, Host: "h", HTTPURL: "HTTP://h", Auth: Auth{Mode: ModeToken, Token: "secret:T"}}, true},
		{Forge{Type: Gitea, Host: "h", HTTPURL: "http://h", Auth: Auth{Mode: ModeNone}}, false},
		{Forge{Type: Gitea, Host: "h", HTTPURL: "https://h", Auth: Auth{Mode: ModeToken, Token: "secret:T"}}, false},
		{Forge{Type: Gitea, Host: "h", Auth: Auth{Mode: ModeToken, Token: "secret:T"}}, false},
		// github.com's API is always https; a deploy-keyed VM clones over SSH.
		{Forge{Type: GitHub, Host: "github.com", HTTPURL: "http://github.com", Auth: tok}, false},
		{Forge{Type: GitHub, Host: "github.com", HTTPURL: "http://github.com", Auth: Auth{Mode: ModeToken, Token: "secret:T"}}, true},
	} {
		if got := tc.f.CleartextCredentials(); got != tc.want {
			u, _ := url.Parse(tc.f.WebURL())
			t.Errorf("%s %s %s: %v", tc.f.Host, u, tc.f.Auth.Mode, got)
		}
	}
}
