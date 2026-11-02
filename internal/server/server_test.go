package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// serve serves the API of catchpen 1.2.3 on a socket in a new directory
// until the test ends.
func serve(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "api.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, New("1.2.3", "").Handler()) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
		if _, err := os.Lstat(sock); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("socket left behind: %v", err)
		}
	})
	return sock
}

func TestServeInfo(t *testing.T) {
	sock := serve(t)
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v", fi.Mode())
	}
	c, err := apiclient.New(apiclient.Options{Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	i, err := c.Info(context.Background())
	if err != nil || i.Version != "1.2.3" || i.API != api.Version {
		t.Errorf("info %+v, %v", i, err)
	}
}

// get sends a GET for path over the socket.
func get(t *testing.T, sock, path string) (*http.Response, *api.ErrorResponse) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	resp, err := hc.Get("http://catchpen" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var er api.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&er)
	return resp, &er
}

func TestServeHeadersAndUnknownPaths(t *testing.T) {
	sock := serve(t)
	resp, _ := get(t, sock, api.PathInfo)
	if h := resp.Header; h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", h)
	}
	for _, p := range []string{"/", "/v1/nope", "/v2/info"} {
		resp, er := get(t, sock, p)
		if resp.StatusCode != http.StatusNotFound || er.Error == nil || er.Error.Code != api.CodeNotFound {
			t.Errorf("GET %s: %d %+v", p, resp.StatusCode, er.Error)
		}
	}
}

// A connection from another user, or one whose user isn't known, is refused.
func TestRefusesOtherConnections(t *testing.T) {
	for _, uid := range []int{os.Getuid() + 1, 0, -1} {
		if uid == os.Getuid() {
			continue
		}
		r := httptest.NewRequest("GET", api.PathInfo, nil)
		r = r.WithContext(context.WithValue(r.Context(), peerUID{}, uid))
		w := httptest.NewRecorder()
		New("1.2.3", "").Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("uid %d: %d %s", uid, w.Code, w.Body)
		}
	}

	// Over TCP there is no peer to ask.
	ts := httptest.NewServer(New("1.2.3", "").Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + api.PathInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var er api.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil || resp.StatusCode != http.StatusUnauthorized || er.Error == nil || er.Error.Code != api.CodeUnauthorized {
		t.Errorf("%d %+v %v", resp.StatusCode, er.Error, err)
	}
}

func TestListenReplacesOnlySockets(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := Listen(file); err == nil {
		l.Close()
		t.Error("listened over a regular file")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "keep" {
		t.Errorf("file now %q, %v", b, err)
	}

	// A socket an earlier daemon left behind.
	sock := filepath.Join(dir, "api.sock")
	old, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	l, err := Listen(sock)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	defer l.Close()

	// A socket something answers on is left alone.
	if l2, err := Listen(sock); err == nil {
		l2.Close()
		t.Fatal("took over a live socket")
	}
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("the live socket no longer answers: %v", err)
	}
	c.Close()
}

// Serve cancelled before it got going still closes the listener before it
// returns.
func TestServeCancelledAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 20 {
		sock := filepath.Join(t.TempDir(), "api.sock")
		l, err := Listen(sock)
		if err != nil {
			t.Fatal(err)
		}
		if err := Serve(ctx, l, New("1.2.3", "").Handler()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(sock); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("try %d: socket left behind: %v", i, err)
		}
	}
}
