package forge

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recorder serves fixed responses by "METHOD path" and records every request.
type recorder struct {
	mu   sync.Mutex
	reqs []string
	h    map[string]http.HandlerFunc
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	rc.reqs = append(rc.reqs, r.Method+" "+r.URL.Path)
	h := rc.h[r.Method+" "+r.URL.Path]
	rc.mu.Unlock()
	if h == nil {
		w.WriteHeader(404)
		return
	}
	h(w, r)
}

func (rc *recorder) got(req string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, r := range rc.reqs {
		if r == req {
			return true
		}
	}
	return false
}

func reply(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		w.Write([]byte(body))
	}
}

func redirect(code int, to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, code) }
}

func TestClientRedirects(t *testing.T) {
	ctx := context.Background()
	other := &recorder{h: map[string]http.HandlerFunc{"GET /api/v1/user": reply(200, `{"login":"x"}`)}}
	otherSrv := httptest.NewServer(other)
	defer otherSrv.Close()
	rc := &recorder{h: map[string]http.HandlerFunc{
		"GET /api/v1/user":                  redirect(302, otherSrv.URL+"/api/v1/user"),
		"POST /api/v1/repos/o/r/keys":       redirect(301, "/api/v1/repos/o/moved/keys"),
		"GET /api/v1/repos/o/moved/keys":    reply(200, `[]`),
		"POST /api/v1/repos/o/s/keys":       redirect(307, "/api/v1/repos/o/s2/keys"),
		"POST /api/v1/repos/o/s2/keys":      reply(201, `{"id":3}`),
		"DELETE /api/v1/repos/o/old/keys/5": redirect(301, "/api/v1/repos/o/new/keys/5"),
		"GET /api/v1/repos/o/new/keys/5":    reply(200, `{"id":5}`),
		"POST /api/v1/repos/o/esc/keys":     redirect(301, "/x%1b[2J"),
	}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := &Client{F: Forge{Name: "g", Type: Gitea, Host: "h", HTTPURL: srv.URL}, Token: "tok"}

	// Another host (here another port) never gets the token.
	if _, err := c.Login(ctx); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("cross-host redirect: %v", err)
	}
	if len(other.reqs) != 0 {
		t.Errorf("the other host was called: %v", other.reqs)
	}
	// A 301 on a POST or DELETE would become a GET: an error naming where it points.
	if id, err := c.AddDeployKey(ctx, Repo{Owner: "o", Name: "r"}, "t", "ssh-ed25519 AAA", true); err == nil || id != 0 || !strings.Contains(err.Error(), "/repos/o/moved/keys") {
		t.Errorf("POST 301: %d, %v", id, err)
	}
	if rc.got("GET /api/v1/repos/o/moved/keys") {
		t.Error("the POST was replayed as a GET")
	}
	if err := c.DeleteDeployKey(ctx, Repo{Owner: "o", Name: "old"}, 5); err == nil {
		t.Error("DELETE 301: the key counted as removed after a GET")
	}
	// A 307 keeps the method and body: followed.
	if id, err := c.AddDeployKey(ctx, Repo{Owner: "o", Name: "s"}, "t", "ssh-ed25519 AAA", true); err != nil || id != 3 {
		t.Errorf("POST 307: %d, %v", id, err)
	}
	// The forge's Location doesn't reach the terminal raw.
	if _, err := c.AddDeployKey(ctx, Repo{Owner: "o", Name: "esc"}, "t", "ssh-ed25519 AAA", true); err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("escape in Location: %q", err)
	}
}

func TestClientRefusesSchemeDowngrade(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/api/v1/user2", http.StatusFound)
	}))
	defer srv.Close()
	// A caller's client without a redirect policy gets the forge one.
	hc := &http.Client{Transport: srv.Client().Transport}
	c := &Client{F: Forge{Name: "g", Type: Gitea, Host: "h", HTTPURL: srv.URL}, Token: "tok", HTTP: hc}
	if _, err := c.Login(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("https to http redirect: %v", err)
	}
}

// An http forge that moved to https on the same host keeps working, POSTs included.
func TestClientFollowsSchemeUpgrade(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(&recorder{h: map[string]http.HandlerFunc{"POST /api/v1/repos/o/r/keys": reply(201, `{"id":4}`)}})
	defer tlsSrv.Close()
	plain := httptest.NewServer(redirect(http.StatusTemporaryRedirect, "https://example.com/api/v1/repos/o/r/keys"))
	defer plain.Close()
	tr := tlsSrv.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		to := plain.Listener.Addr().String()
		if strings.HasSuffix(addr, ":443") {
			to = tlsSrv.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, to)
	}
	c := &Client{F: Forge{Name: "g", Type: Gitea, Host: "example.com", HTTPURL: "http://example.com"}, Token: "tok", HTTP: &http.Client{Transport: tr}}
	if id, err := c.AddDeployKey(context.Background(), Repo{Owner: "o", Name: "r"}, "t", "ssh-ed25519 AAA", true); err != nil || id != 4 {
		t.Errorf("http to https 307: %d, %v", id, err)
	}
}

func TestClientResponseLimits(t *testing.T) {
	ctx := context.Background()
	big := `{"login":"` + strings.Repeat("a", maxResponse) + `"}`
	rc := &recorder{h: map[string]http.HandlerFunc{
		"GET /api/v1/user":         reply(200, big),
		"GET /api/v1/repos/o/html": reply(200, `<html>ok</html>`),
	}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := &Client{F: Forge{Name: "g", Type: Gitea, Host: "h", HTTPURL: srv.URL}, Token: "tok"}
	if login, err := c.Login(ctx); err == nil || login != "" {
		t.Errorf("over 4 MiB: %d bytes, %v", len(login), err)
	}
	if b, err := c.DefaultBranch(ctx, Repo{Owner: "o", Name: "html"}); err == nil || b != "" {
		t.Errorf("non-JSON 2xx: %q, %v", b, err)
	}
}

// A credential the forge created but didn't report fully is found and removed.
func TestClientRemovesHalfReportedCredentials(t *testing.T) {
	ctx := context.Background()
	minted := map[string]string{"n1": `{"id":8}`, "n2": `{"sha1":"s"}`, "n3": `<html>`}
	rc := &recorder{h: map[string]http.HandlerFunc{
		"POST /api/v1/repos/o/e/keys":        reply(201, ``),
		"GET /api/v1/repos/o/e/keys":         reply(200, `[{"id":11,"key":"ssh-ed25519 OTHER"},{"id":12,"key":"ssh-ed25519 AAA"}]`),
		"DELETE /api/v1/repos/o/e/keys/12":   reply(204, ``),
		"POST /api/v1/repos/o/u/keys":        reply(201, `<html>`),
		"GET /api/v1/repos/o/u/keys":         reply(200, `[{"id":13,"key":"ssh-ed25519 BBB"}]`),
		"DELETE /api/v1/repos/o/u/keys/13":   reply(204, ``),
		"POST /api/v1/repos/o/f/keys":        reply(201, ``),
		"GET /api/v1/repos/o/f/keys":         reply(500, ``),
		"POST /api/v1/repos/o/g/keys":        reply(201, ``),
		"GET /api/v1/repos/o/g/keys":         reply(200, `[{"id":14,"key":"ssh-ed25519 OTHER"}]`),
		"GET /api/v1/users/bot/tokens":       reply(200, `[{"id":8,"name":"n1"},{"id":9,"name":"n2"},{"id":10,"name":"n3"}]`),
		"DELETE /api/v1/users/bot/tokens/8":  reply(204, ``),
		"DELETE /api/v1/users/bot/tokens/9":  reply(204, ``),
		"DELETE /api/v1/users/bot/tokens/10": reply(204, ``),
		"POST /api/v1/users/bot/tokens": func(w http.ResponseWriter, r *http.Request) {
			var b struct{ Name string }
			data, _ := io.ReadAll(r.Body)
			json.Unmarshal(data, &b)
			w.WriteHeader(201)
			w.Write([]byte(minted[b.Name]))
		},
	}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := &Client{F: Forge{Name: "g", Type: Gitea, Host: "h", HTTPURL: srv.URL}, Token: "tok", User: "bot", Password: "pw"}

	for _, k := range []struct{ repo, pub, del string }{
		{"e", "ssh-ed25519 AAA catchpen@vm", "DELETE /api/v1/repos/o/e/keys/12"}, // 2xx without an id
		{"u", "ssh-ed25519 BBB catchpen@vm", "DELETE /api/v1/repos/o/u/keys/13"}, // unreadable 2xx
	} {
		if id, err := c.AddDeployKey(ctx, Repo{Owner: "o", Name: k.repo}, "catchpen vm", k.pub, true); err == nil || id != 0 {
			t.Errorf("%s: %d, %v", k.repo, id, err)
		}
		if !rc.got(k.del) {
			t.Errorf("%s: the half-reported key was left: %v", k.repo, rc.reqs)
		}
	}
	if rc.got("DELETE /api/v1/repos/o/e/keys/11") {
		t.Error("removed another key")
	}
	for _, name := range []string{"f", "g"} { // the lookup fails; the key isn't listed
		if _, err := c.AddDeployKey(ctx, Repo{Owner: "o", Name: name}, "catchpen vm", "ssh-ed25519 CCC", true); err == nil || !strings.Contains(err.Error(), `"catchpen vm" may remain`) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, id := range map[string]string{"n1": "8", "n2": "9", "n3": "10"} {
		if gotID, tok, err := c.MintToken(ctx, name, nil); err == nil || tok != "" || gotID != 0 {
			t.Errorf("%s (%s): %d %q %v", name, minted[name], gotID, tok, err)
		}
		if !rc.got("DELETE /api/v1/users/bot/tokens/" + id) {
			t.Errorf("%s (%s): the half-reported token was left", name, minted[name])
		}
	}
}

func TestDeleteDeployKeyNotFound(t *testing.T) {
	rc := &recorder{h: map[string]http.HandlerFunc{
		"DELETE /api/v1/repos/o/gone/keys/5":    reply(404, `{"message":"Not Found"}`),
		"GET /api/v1/repos/o/gone":              reply(200, `{"permissions":{"admin":true}}`),
		"DELETE /api/v1/repos/o/demoted/keys/5": reply(404, `{"message":"Not Found"}`),
		"GET /api/v1/repos/o/demoted":           reply(200, `{"permissions":{"admin":false,"pull":true}}`),
		"DELETE /api/v1/repos/o/hidden/keys/5":  reply(404, `{"message":"Not Found"}`),
	}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := &Client{F: Forge{Name: "g", Type: Gitea, Host: "h", HTTPURL: srv.URL}, Token: "tok"}
	ctx := context.Background()
	if err := c.DeleteDeployKey(ctx, Repo{Owner: "o", Name: "gone"}, 5); err != nil {
		t.Errorf("key already removed: %v", err)
	}
	for _, name := range []string{"demoted", "hidden"} {
		if err := c.DeleteDeployKey(ctx, Repo{Owner: "o", Name: name}, 5); err == nil || IsNotFound(err) {
			t.Errorf("%s: a 404 the token can't explain counted as removed (or reads as not found): %v", name, err)
		}
	}
}
