package apiclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// serve serves h on a unix socket in a new directory until the test ends.
func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// First in the file: net/http reads the proxy variables once per process.
func TestIgnoresProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("REQUEST_METHOD", "") // when set, net/http ignores HTTP_PROXY
	sock := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != api.PathInfo {
			http.Error(w, "request line "+r.RequestURI, http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"version":"1.2.3","api":1}`))
	})
	c, err := New(Options{Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	if i, err := c.Info(context.Background()); err != nil || i.Version != "1.2.3" || i.API != 1 {
		t.Errorf("%+v, %v", i, err)
	}
}

func TestNewRefuses(t *testing.T) {
	pin := "sha256:" + strings.Repeat("ab", 32)
	key := make([]byte, api.MinKeyBytes)
	h := &HMAC{Name: "cockpit", Key: key}
	cert := &tls.Certificate{Certificate: [][]byte{{1}}}
	for what, o := range map[string]Options{
		"nothing":           {},
		"socket and URL":    {Socket: "/s", URL: "https://h:1", ServerPin: pin, HMAC: h},
		"http":              {URL: "http://h:1", ServerPin: pin, HMAC: h},
		"a path":            {URL: "https://h:1/v1", ServerPin: pin, HMAC: h},
		"a query":           {URL: "https://h:1?x=1", ServerPin: pin, HMAC: h},
		"a user":            {URL: "https://u@h:1", ServerPin: pin, HMAC: h},
		"no pin":            {URL: "https://h:1", HMAC: h},
		"a bad pin":         {URL: "https://h:1", ServerPin: "sha256:ab", HMAC: h},
		"no credentials":    {URL: "https://h:1", ServerPin: pin},
		"cert and HMAC":     {URL: "https://h:1", ServerPin: pin, HMAC: h, Cert: cert},
		"a short key":       {URL: "https://h:1", ServerPin: pin, HMAC: &HMAC{Name: "cockpit", Key: key[1:]}},
		"a name with a dot": {URL: "https://h:1", ServerPin: pin, HMAC: &HMAC{Name: "a.b", Key: key}},
		"no port":           {URL: "https://h", ServerPin: pin, HMAC: h},
		"an empty cert":     {URL: "https://h:1", ServerPin: pin, Cert: &tls.Certificate{}},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	for what, o := range map[string]Options{
		"HMAC":   {URL: "https://h:1", ServerPin: pin, HMAC: h},
		"cert":   {URL: "https://h:1/", ServerPin: pin, Cert: cert},
		"socket": {Socket: "/s"},
	} {
		if _, err := New(o); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
}

func TestErrors(t *testing.T) {
	sock := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plain") != "" {
			http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":"forbidden","message":"no"}}`))
	})
	c, err := New(Options{Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	var ae *api.Error
	if _, err := c.Info(context.Background()); !errors.As(err, &ae) || ae.Code != api.CodeForbidden || ae.Message != "no" {
		t.Errorf("API error: %v", err)
	}

	// A body that isn't an API error: the status, as an internal error.
	c.hc.Transport = headerTransport{c.hc.Transport}
	ae = nil
	if _, err := c.Info(context.Background()); !errors.As(err, &ae) || ae.Code != api.CodeInternal || ae.Message != "502 Bad Gateway" {
		t.Errorf("plain error: %v", err)
	}
}

type headerTransport struct{ rt http.RoundTripper }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Plain", "1")
	return h.rt.RoundTrip(r)
}

// The client links nothing of catchpen's but pkg/api.
func TestImportsOnlyAPIAndStandardLibrary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(out))
	want := []string{"github.com/blackshieldpt/catchpen/pkg/api", "github.com/blackshieldpt/catchpen/pkg/apiclient"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("dependencies %v, want %v", got, want)
	}
}
