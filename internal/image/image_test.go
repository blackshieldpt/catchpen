package image

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseSums(t *testing.T) {
	sums := "aa11  other.img\nBB22 *noble.img\ncc33  noble.img.manifest\n"
	if h, err := ParseSums(strings.NewReader(sums), "noble.img"); err != nil || h != "bb22" {
		t.Fatalf("got %q, %v", h, err)
	}
	if _, err := ParseSums(strings.NewReader(sums), "missing.img"); err == nil {
		t.Error("expected error for unlisted file")
	}
	if _, err := ParseSums(strings.NewReader("zz  noble.img\n"), "noble.img"); err == nil {
		t.Error("expected error for non-hex digest")
	}
}

var (
	hex64  = strings.Repeat("ab", 32)
	hex128 = strings.Repeat("cd", 64)
)

func TestValidate(t *testing.T) {
	for _, s := range []Spec{
		{Name: "x", URL: "https://h/x.img", SHA256: hex64},
		{Name: "x", URL: "https://h/x.img", SHA512: strings.ToUpper(hex128)},
		{Name: "x", URL: "https://h/x.img", ChecksumURL: "https://h/S"},
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	for _, s := range []Spec{
		{Name: "x", URL: "http://h/x.img", SHA256: hex64},
		{Name: "x", URL: "https://h/x.img"},
		{Name: "x", URL: "https://h/x.img", SHA256: hex64, ChecksumURL: "https://h/S"},
		{Name: "../x", URL: "https://h/x.img", SHA256: hex64},
		{Name: "x", URL: "https://h/x.img", ChecksumURL: "http://h/S"},
		{Name: "x", URL: "https://h/x.img", SHA256: "ab"},
		{Name: "x", URL: "https://h/x.img", SHA256: hex128},
		{Name: "x", URL: "https://h/x.img", SHA512: hex64},
		{Name: "x", URL: "https://h/x.img", SHA256: strings.Repeat("zz", 32)},
	} {
		if s.Validate() == nil {
			t.Errorf("%+v: expected error", s)
		}
	}
}

// useServer points the package client at srv, keeping its redirect policy.
func useServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := client
	c := *client
	c.Transport = srv.Client().Transport
	client = &c
	t.Cleanup(func() { client = old })
}

type fakeUpstream struct {
	srv     *httptest.Server
	images  map[string][]byte // path -> body
	sums    string            // SHA*SUMS body
	gets    atomic.Int32
	corrupt atomic.Bool
}

func newUpstream(t *testing.T, images map[string][]byte) *fakeUpstream {
	u := &fakeUpstream{images: images}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SUMS" {
			_, _ = w.Write([]byte(u.sums))
			return
		}
		body, ok := u.images[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		u.gets.Add(1)
		if u.corrupt.Load() {
			body = []byte("tampered")
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(u.srv.Close)
	useServer(t, u.srv)
	return u
}

func sha256hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func sha512hex(b []byte) string { s := sha512.Sum512(b); return hex.EncodeToString(s[:]) }

func TestEnsureDownloadsVerifiesAndCaches(t *testing.T) {
	body := []byte("fake image contents")
	u := newUpstream(t, map[string][]byte{"/disk.img": body})
	u.sums = sha256hex(body) + " *disk.img\n"
	dir := t.TempDir()
	s := Spec{Name: "test", URL: u.srv.URL + "/disk.img", ChecksumURL: u.srv.URL + "/SUMS"}
	ctx := context.Background()
	p, d, err := Ensure(ctx, dir, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(body) || d != Digest("sha256:"+sha256hex(body)) {
		t.Fatalf("bad result %s %s", got, d)
	}
	if fi, err := os.Stat(p + ".json"); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("metadata: %v %v", fi, err)
	}
	u.sums = "" // the fast path must not consult upstream
	if _, d2, err := Ensure(ctx, dir, s, nil); err != nil || d2 != d || u.gets.Load() != 1 {
		t.Fatalf("second Ensure: gets=%d, %s, %v", u.gets.Load(), d2, err)
	}

	u.sums = sha256hex(body) + " *disk.img\n"
	u.corrupt.Store(true)
	dir2 := t.TempDir()
	if _, _, err := Ensure(ctx, dir2, s, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	for _, f := range []string{"test.img", "test.img.json", "test.img.part"} {
		if _, err := os.Stat(filepath.Join(dir2, f)); err == nil {
			t.Errorf("%s left behind after a failed download", f)
		}
	}
}

func TestEnsureRefetchesOnSpecChange(t *testing.T) {
	a, b := []byte("image A"), []byte("image B")
	u := newUpstream(t, map[string][]byte{"/a.img": a, "/b.img": b})
	dir := t.TempDir()
	ctx := context.Background()
	spec := func(path string, body []byte) Spec {
		return Spec{Name: "test", URL: u.srv.URL + path, SHA256: sha256hex(body)}
	}
	if _, _, err := Ensure(ctx, dir, spec("/a.img", a), nil); err != nil {
		t.Fatal(err)
	}
	// URL changed in config.yaml: the old image must not be reused.
	p, d, err := Ensure(ctx, dir, spec("/b.img", b), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(b) || d != Digest("sha256:"+sha256hex(b)) || u.gets.Load() != 2 {
		t.Fatalf("URL change: %q %s gets=%d", got, d, u.gets.Load())
	}
	// Pin changed for the same URL: re-verified (and here, rejected).
	u.images["/b.img"] = []byte("image B v2")
	if _, _, err := Ensure(ctx, dir, Spec{Name: "test", URL: u.srv.URL + "/b.img", SHA256: sha256hex([]byte("other"))}, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("pin change: %v", err)
	}
	// SHA-512 pin.
	if _, d, err := Ensure(ctx, dir, Spec{Name: "test", URL: u.srv.URL + "/a.img", SHA512: sha512hex(a)}, nil); err != nil || d != Digest("sha512:"+sha512hex(a)) {
		t.Fatalf("sha512 pin: %s %v", d, err)
	}
}

func TestEnsureSHA512Sums(t *testing.T) {
	body := []byte("debian")
	u := newUpstream(t, map[string][]byte{"/d.qcow2": body})
	u.sums = sha512hex(body) + "  d.qcow2\n"
	s := Spec{Name: "deb", URL: u.srv.URL + "/d.qcow2", ChecksumURL: u.srv.URL + "/SUMS"}
	if _, d, err := Ensure(context.Background(), t.TempDir(), s, nil); err != nil || d != Digest("sha512:"+sha512hex(body)) {
		t.Fatalf("%s %v", d, err)
	}
	u.sums = "abcd  d.qcow2\n"
	if _, _, err := Ensure(context.Background(), t.TempDir(), s, nil); err == nil || !strings.Contains(err.Error(), "neither sha256 nor sha512") {
		t.Fatalf("short digest accepted: %v", err)
	}
}

func TestEnsureAdoptsUnrecordedImage(t *testing.T) {
	body := []byte("manually fetched")
	u := newUpstream(t, map[string][]byte{"/m.img": body})
	dir := t.TempDir()
	dst := filepath.Join(dir, "test.img")
	_ = os.WriteFile(dst, body, 0o600)
	_ = os.WriteFile(dst+".digest", []byte("sha256:"+sha256hex(body)+"\n"), 0o600)
	if _, _, err := Ensure(context.Background(), dir, Spec{Name: "test", URL: u.srv.URL + "/m.img", SHA256: sha256hex(body)}, nil); err != nil {
		t.Fatal(err)
	}
	if u.gets.Load() != 0 {
		t.Error("matching local image re-downloaded")
	}
	if _, err := os.Stat(dst + ".json"); err != nil {
		t.Error("metadata not written")
	}
	if _, err := os.Stat(dst + ".digest"); err == nil {
		t.Error("legacy .digest not removed")
	}
}

func TestRedirectPolicy(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer plain.Close()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/insecure":
			http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/loop/"):
			var n int
			fmt.Sscanf(r.URL.Path, "/loop/%d", &n)
			http.Redirect(w, r, fmt.Sprintf("%s/loop/%d", srv.URL, n+1), http.StatusFound)
		}
	}))
	defer srv.Close()
	useServer(t, srv)
	ctx := context.Background()
	if _, err := get(ctx, srv.URL+"/insecure"); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("https->http redirect: %v", err)
	}
	if _, err := get(ctx, srv.URL+"/loop/0"); err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("redirect loop: %v", err)
	}
}
