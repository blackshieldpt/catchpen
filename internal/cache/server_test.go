package cache

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/canary"
	"github.com/blackshieldpt/catchpen/internal/egress"
)

// upstream fakes every upstream host: plain HTTP on :80, TLS on :443.
type upstream struct {
	mu   sync.Mutex
	hits map[string]int // host+path -> requests
	last http.Header    // headers of the latest request
}

func (u *upstream) count(k string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[k]
}

func newUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*upstream, *http.Client) {
	t.Helper()
	u := &upstream{hits: map[string]int{}}
	wrap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits[r.Host+r.URL.Path]++
		u.last = r.Header.Clone()
		u.mu.Unlock()
		h(w, r)
	})
	plain := httptest.NewServer(wrap)
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(wrap)
	t.Cleanup(tlsSrv.Close)
	var d net.Dialer
	roots := x509.NewCertPool()
	roots.AddCert(tlsSrv.Certificate())
	return u, &http.Client{Transport: &http.Transport{
		DisableCompression: true,
		// The test certificate names example.com, whatever host is dialled.
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasSuffix(addr, ":443") {
				return d.DialContext(ctx, network, tlsSrv.Listener.Addr().String())
			}
			return d.DialContext(ctx, network, plain.Listener.Addr().String())
		},
	}}
}

func newServer(t *testing.T, store string, c *http.Client, kinds ...string) *Server {
	s := &Server{Store: Store{Dir: store}, Kinds: map[string]bool{}, Dir: t.TempDir(), VM: "vm1", Client: c}
	for _, k := range kinds {
		s.Kinds[k] = true
	}
	return s
}

// stored is how many objects st holds of kind.
func stored(t *testing.T, st Store, kind string) int {
	t.Helper()
	us, err := st.Usage()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		if u.Kind == kind {
			return u.Files
		}
	}
	t.Fatalf("no usage of %s", kind)
	return 0
}

func get(s *Server, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestStoredObjectFetchedOnce(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "tarball-bytes")
	})
	store := t.TempDir()
	s1 := newServer(t, store, c, "npm")
	if w := get(s1, "/npm/a/-/a-1.0.0.tgz"); w.Code != 200 || w.Body.String() != "tarball-bytes" {
		t.Fatalf("miss: %d %q", w.Code, w.Body)
	}
	s2 := newServer(t, store, c, "npm") // another VM
	if w := get(s2, "/npm/a/-/a-1.0.0.tgz"); w.Code != 200 || w.Body.String() != "tarball-bytes" || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("hit: %d %q %v", w.Code, w.Body, w.Header())
	}
	if w := get(s2, "/npm/a/-/a-1.0.0.tgz", "Range", "bytes=0-6"); w.Code != http.StatusPartialContent || w.Body.String() != "tarball" {
		t.Errorf("range on a hit: %d %q", w.Code, w.Body)
	}
	if n := up.count("registry.npmjs.org/a/-/a-1.0.0.tgz"); n != 1 {
		t.Errorf("upstream fetched %d times", n)
	}
	b, _ := os.ReadFile(filepath.Join(s1.Dir, egress.LogFile))
	if !strings.Contains(string(b), `"action":"cache"`) || !strings.Contains(string(b), `"host":"registry.npmjs.org"`) || !strings.Contains(string(b), "npm /a/-/a-1.0.0.tgz") {
		t.Errorf("egress log %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(s2.Dir, egress.LogFile)); len(b) != 0 {
		t.Errorf("hits are not logged: %s", b)
	}
}

func TestParallelMissesFetchOnce(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		io.WriteString(w, "wheel")
	})
	store := t.TempDir()
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 5 {
		wg.Go(func() {
			if w := get(newServer(t, store, c, "pypi"), "/pypi/files/packages/ab/cd/x-1.0-py3-none-any.whl"); w.Code != 200 || w.Body.String() != "wheel" {
				bad.Add(1)
			}
		})
	}
	wg.Wait()
	if n := up.count("files.pythonhosted.org/packages/ab/cd/x-1.0-py3-none-any.whl"); n != 1 || bad.Load() != 0 {
		t.Errorf("upstream fetched %d times, %d bad responses", n, bad.Load())
	}
}

func TestIncompleteOrFailedNotStored(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "chunked.deb"): // no length: can't tell it's complete
			io.WriteString(w, "part one ")
			w.(http.Flusher).Flush()
			io.WriteString(w, "part two")
		case strings.HasSuffix(r.URL.Path, "short.deb"):
			w.Header().Set("Content-Length", "100")
			io.WriteString(w, "only ten b")
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				c.Close()
			}
		default:
			http.Error(w, "gone", http.StatusNotFound)
		}
	})
	s := newServer(t, t.TempDir(), c, "apt")
	for range 2 {
		get(s, "http://deb.debian.org/debian/pool/short.deb")
		if w := get(s, "http://deb.debian.org/debian/pool/chunked.deb"); w.Body.String() != "part one part two" {
			t.Errorf("chunked: %q", w.Body)
		}
		if w := get(s, "http://deb.debian.org/debian/pool/missing.deb"); w.Code != http.StatusNotFound {
			t.Errorf("404 relayed as %d", w.Code)
		}
	}
	for _, f := range []string{"short", "missing", "chunked"} {
		if n := up.count("deb.debian.org/debian/pool/" + f + ".deb"); n != 2 {
			t.Errorf("%s fetched %d times, want 2: nothing stored", f, n)
		}
	}
	if locks, _ := filepath.Glob(filepath.Join(s.Store.Dir, "apt", "*", "*.lock")); len(locks) != 0 {
		t.Errorf("lock files left: %v", locks)
	}
	if n := stored(t, s.Store, "apt"); n != 0 {
		t.Errorf("stored %d", n)
	}
}

func TestIndexesPassAndRewrite(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host == "registry.npmjs.org":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"e1"`)
			body := `{"versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/a/-/a-1.0.0.tgz"}}}}`
			if r.Header.Get("Accept-Encoding") != "gzip" {
				io.WriteString(w, body)
				return
			}
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			io.WriteString(zw, body)
			zw.Close()
		case r.Header.Get("If-Modified-Since") != "":
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("Content-Encoding", "gzip")
			io.WriteString(w, "compressed")
		}
	})
	s := newServer(t, t.TempDir(), c, "npm", "apt")
	w := get(s, "/npm/a", "Accept-Encoding", "gzip", "Accept", "application/vnd.npm.install-v1+json")
	want := `{"versions":{"1.0.0":{"dist":{"tarball":"http://10.0.2.102/npm/a/-/a-1.0.0.tgz"}}}}`
	if w.Code != 200 || w.Body.String() != want || w.Header().Get("Content-Length") != "" || w.Header().Get("Content-Encoding") != "" || w.Header().Get("ETag") != `"e1"` {
		t.Errorf("packument: %d %q %v", w.Code, w.Body, w.Header())
	}
	if up.last.Get("Accept-Encoding") != "gzip" || up.last.Get("Accept") != "application/vnd.npm.install-v1+json" {
		t.Errorf("rewritten index fetched with %v", up.last)
	}
	if w := get(s, "http://archive.ubuntu.com/ubuntu/dists/noble/InRelease", "If-Modified-Since", "Mon, 01 Jan 2024 00:00:00 GMT"); w.Code != http.StatusNotModified {
		t.Errorf("conditional index: %d", w.Code)
	}
	if w := get(s, "http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages.gz", "Accept-Encoding", "gzip"); w.Body.String() != "compressed" ||
		w.Header().Get("Content-Encoding") != "gzip" || up.last.Get("Accept-Encoding") != "gzip" {
		t.Errorf("pass-through: %q %v %v", w.Body, w.Header(), up.last)
	}
	if n := stored(t, s.Store, "apt") + stored(t, s.Store, "npm"); n != 0 {
		t.Errorf("indexes stored: %d", n)
	}
}

func TestRegistry(t *testing.T) {
	layer := "layer-bytes"
	sum := sha256.Sum256([]byte(layer))
	good := hex.EncodeToString(sum[:])
	bad := strings.Repeat("0", 64)
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "auth.docker.io" {
			if r.URL.Query().Get("scope") != "repository:library/alpine:pull" || r.URL.Query().Get("service") != "registry.docker.io" {
				http.Error(w, "bad scope", 400)
				return
			}
			io.WriteString(w, `{"token":"tok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("Www-Authenticate", `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		}
		io.WriteString(w, layer)
	})
	store := t.TempDir()
	s := newServer(t, store, c, "docker")
	if w := get(s, "/v2/"); w.Code != 200 || w.Header().Get("Docker-Distribution-Api-Version") != "registry/2.0" {
		t.Errorf("/v2/: %d %v", w.Code, w.Header())
	}
	for range 2 {
		if w := get(newServer(t, store, c, "docker"), "/v2/library/alpine/blobs/sha256:"+good+"?ns=docker.io"); w.Code != 200 || w.Body.String() != layer {
			t.Errorf("blob: %d %q %v", w.Code, w.Body, w.Header())
		}
		get(s, "/v2/library/alpine/blobs/sha256:"+bad)
		get(s, "/v2/library/alpine/manifests/sha256:"+good)
	}
	if n := up.count("registry-1.docker.io/v2/library/alpine/blobs/sha256:" + good); n != 2 { // a 401, then the fetch
		t.Errorf("good blob fetched %d times", n)
	}
	if n := up.count("registry-1.docker.io/v2/library/alpine/blobs/sha256:" + bad); n < 3 {
		t.Errorf("a blob not matching its digest must not be stored (%d fetches)", n)
	}
	w := get(s, "/v2/library/alpine/manifests/sha256:"+good)
	if w.Header().Get("Content-Type") != "application/vnd.oci.image.index.v1+json" || w.Header().Get("Docker-Content-Digest") != "sha256:"+good {
		t.Errorf("stored manifest headers %v", w.Header())
	}
	// Another repository with the same layer: stored already.
	if w := get(s, "/v2/me/app/blobs/sha256:"+good); w.Body.String() != layer || w.Header().Get("Docker-Content-Digest") != "sha256:"+good ||
		up.count("registry-1.docker.io/v2/me/app/blobs/sha256:"+good) != 0 {
		t.Errorf("shared blob: %q %v", w.Body, w.Header())
	}
	if n := up.count("auth.docker.io/token"); n != 2 { // once per Server (VM connection) that fetched
		t.Errorf("token fetched %d times", n)
	}
}

func TestServeConn(t *testing.T) {
	_, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "{}") })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := newServer(t, t.TempDir(), c, "go")
	if err := canary.Save(s.Dir, canary.Generate([]string{"git"})); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		done <- s.ServeConn(context.Background(), conn)
	}()
	var dials atomic.Int32
	guest := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", l.Addr().String())
	}}}
	for i := range 2 { // keep-alive: one connection, as QEMU spawns one process per connection
		resp, err := guest.Get("http://10.0.2.102/go/x/@v/list")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("request %d: status %d", i, resp.StatusCode)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if i == 0 { // canaries are read once per connection: breaking the file now changes nothing
			if err := os.WriteFile(filepath.Join(s.Dir, canary.File), []byte("{broken"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	guest.CloseIdleConnections()
	select {
	case err := <-done:
		if err != nil || dials.Load() != 1 {
			t.Errorf("ServeConn: %v, %d dials", err, dials.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeConn didn't return after the guest closed")
	}
}

func TestDialPublicRefusesPrivate(t *testing.T) {
	for _, a := range []string{"127.0.0.1:80", "10.0.2.2:80", "192.168.1.1:443", "localhost:80"} {
		if _, err := dialPublic(context.Background(), "tcp", a); err == nil || !strings.Contains(err.Error(), "private") {
			t.Errorf("%s: %v", a, err)
		}
	}
}

// stalled is a guest that stops reading after the first write.
type stalled struct {
	h       http.Header
	wrote   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stalled) Header() http.Header { return s.h }
func (s *stalled) WriteHeader(int)     {}
func (s *stalled) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.wrote) })
	<-s.release
	return len(p), nil
}

func TestSlowGuestDoesntHoldOthers(t *testing.T) {
	body := strings.Repeat("x", 4<<20)
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		io.WriteString(w, body)
	})
	store := t.TempDir()
	slow := newServer(t, store, c, "go")
	g := &stalled{h: http.Header{}, wrote: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		slow.ServeHTTP(g, httptest.NewRequest(http.MethodGet, "/go/x/@v/v1.0.0.zip", nil))
		close(done)
	}()
	<-g.wrote
	fast := make(chan *httptest.ResponseRecorder)
	go func() { fast <- get(newServer(t, store, c, "go"), "/go/x/@v/v1.0.0.zip") }()
	select {
	case w := <-fast:
		if w.Body.Len() != len(body) || up.count("proxy.golang.org/x/@v/v1.0.0.zip") != 1 {
			t.Errorf("other guest: %d bytes, %d fetches", w.Body.Len(), up.count("proxy.golang.org/x/@v/v1.0.0.zip"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled guest holds the object from others")
	}
	close(g.release)
	<-done
	slow.downloads.Wait()
}

func TestDenylistApplies(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") })
	s := newServer(t, t.TempDir(), c, "go")
	if err := egress.WriteList(s.Dir, egress.DenylistFile, []string{"proxy.golang.org"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/go/x/@v/list", "/go/x/@v/v1.0.0.zip"} {
		if w := get(s, p); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if up.count("proxy.golang.org/x/@v/list") != 0 {
		t.Error("denied host fetched")
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, egress.LogFile)); !strings.Contains(string(b), `"action":"deny"`) {
		t.Errorf("denial not logged: %s", b)
	}
	if w := get(s, "/go/sumdb/sum.golang.org/supported"); w.Code != 200 { // answered here, nothing fetched
		t.Errorf("sumdb probe: %d", w.Code)
	}
}

func TestHeadAndHeaders(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"up"`)
		w.Header().Set("Cache-Control", "max-age=300")
		if r.Host == "pypi.org" {
			io.WriteString(w, `<a href="https://files.pythonhosted.org/packages/x.whl">x</a>`)
			return
		}
		w.Header().Set("Content-Length", "5")
		if r.Method == http.MethodGet {
			io.WriteString(w, "wheel")
		}
	})
	s := newServer(t, t.TempDir(), c, "pypi")
	head := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodHead, target, nil))
		return w
	}
	const file = "/pypi/files/packages/x.whl"
	if w := head(file); w.Code != 200 || w.Header().Get("Content-Length") != "5" || w.Body.Len() != 0 || up.count("files.pythonhosted.org/packages/x.whl") != 1 {
		t.Errorf("HEAD miss: %d %v %q", w.Code, w.Header(), w.Body)
	}
	if stored(t, s.Store, "pypi") != 0 {
		t.Error("HEAD stored the object")
	}
	if w := get(s, file, "Authorization", "Basic c2VjcmV0", "Cookie", "c=1"); w.Body.String() != "wheel" ||
		up.last.Get("Authorization") != "" || up.last.Get("Cookie") != "" {
		t.Errorf("GET: %q, upstream saw %v", w.Body, up.last)
	}
	before := up.count("files.pythonhosted.org/packages/x.whl")
	w := head(file)
	if w.Code != 200 || w.Header().Get("Content-Length") != "5" || w.Header().Get("ETag") != `"up"` || w.Header().Get("Last-Modified") != "" ||
		up.count("files.pythonhosted.org/packages/x.whl") != before {
		t.Errorf("HEAD hit: %d %v", w.Code, w.Header())
	}
	if w := head("/pypi/simple/x/"); w.Header().Get("Content-Length") != "" || w.Header().Get("Cache-Control") != "max-age=300" {
		t.Errorf("HEAD of a rewritten index: %v", w.Header())
	}
}

func TestFetchesTrimTheStore(t *testing.T) {
	_, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("n", 100)) })
	store := t.TempDir()
	old := put(t, Store{Dir: store}, "npm", "old", 100, time.Now().Add(-time.Hour))
	s := newServer(t, store, c, "npm")
	s.Max = 150
	get(s, "/npm/a/-/a-1.0.0.tgz")
	s.downloads.Wait()
	if _, err := os.Stat(old); err == nil {
		t.Error("the store wasn't trimmed to its limit")
	}
	if n := stored(t, s.Store, "npm"); n != 1 {
		t.Errorf("%d stored", n)
	}
}

func TestRedirectsChecked(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/@v/v1.0.0.zip":
			http.Redirect(w, r, "https://cdn.example/blob", http.StatusTemporaryRedirect)
		case "/x/@v/v2.0.0.zip":
			http.Redirect(w, r, "http://cdn.example/blob", http.StatusTemporaryRedirect)
		case "/x/@v/v3.0.0.zip":
			http.Redirect(w, r, "https://cdn.example/hop", http.StatusTemporaryRedirect)
		case "/hop":
			http.Redirect(w, r, "https://denied.example/blob", http.StatusTemporaryRedirect)
		default:
			io.WriteString(w, "blob")
		}
	})
	s := newServer(t, t.TempDir(), c, "go")
	if w := get(s, "/go/x/@v/v1.0.0.zip"); w.Body.String() != "blob" {
		t.Errorf("redirect: %d %q", w.Code, w.Body)
	}
	if w := get(s, "/go/x/@v/v2.0.0.zip"); w.Code != http.StatusBadGateway {
		t.Errorf("redirect to http: %d", w.Code)
	}
	if err := egress.WriteList(s.Dir, egress.DenylistFile, []string{"denied.example"}); err != nil {
		t.Fatal(err)
	}
	if w := get(s, "/go/x/@v/v3.0.0.zip"); w.Code != http.StatusForbidden || up.count("cdn.example/hop") != 1 || up.count("denied.example/blob") != 0 {
		t.Errorf("second hop to a denied host: %d, %d requests to it", w.Code, up.count("denied.example/blob"))
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, egress.LogFile)); !strings.Contains(string(b), `"host":"denied.example","port":443,"action":"deny","reason":"redirect /blob"`) {
		t.Errorf("denied hop not logged: %s", b)
	}
	if err := egress.WriteList(s.Dir, egress.DenylistFile, []string{"cdn.example"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Clear(); err != nil {
		t.Fatal(err)
	}
	before := up.count("cdn.example/blob")
	if w := get(s, "/go/x/@v/v1.0.0.zip"); w.Code != http.StatusForbidden || up.count("cdn.example/blob") != before {
		t.Errorf("redirect to a denied host: %d", w.Code)
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, egress.LogFile)); !strings.Contains(string(b), `"host":"cdn.example","port":443,"action":"cache","reason":"redirect /blob"`) {
		t.Errorf("redirect hop not logged: %s", b)
	}
}

func TestCanaryInRequest(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") })
	s := newServer(t, t.TempDir(), c, "go")
	cs := canary.Generate([]string{"git"})
	if err := canary.Save(s.Dir, cs); err != nil {
		t.Fatal(err)
	}
	var hit string
	s.OnCanary = func(c *canary.Canary, where string) { hit = c.Kind + ": " + where }
	if w := get(s, "/go/example.com/"+cs[0].Values[0]+"/@v/list"); w.Code != http.StatusNotFound || hit != "git: in a package cache request" {
		t.Errorf("%d %q", w.Code, hit)
	}
	if up.count("proxy.golang.org/example.com/"+cs[0].Values[0]+"/@v/list") != 0 {
		t.Error("fetched upstream")
	}
	hit = ""
	if w := get(s, "/go/x/@v/list"); w.Code != 200 || hit != "" {
		t.Errorf("clean request: %d %q", w.Code, hit)
	}
}

func TestCanaryInHeader(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") })
	s := newServer(t, t.TempDir(), c, "go")
	cs := canary.Generate([]string{"git"})
	if err := canary.Save(s.Dir, cs); err != nil {
		t.Fatal(err)
	}
	var hit *canary.Canary
	s.OnCanary = func(c *canary.Canary, where string) { hit = c }
	if w := get(s, "/go/x/@v/list", "Authorization", "Bearer "+cs[0].Values[0]); w.Code != http.StatusNotFound || hit == nil {
		t.Errorf("%d %v", w.Code, hit)
	}
	if up.count("proxy.golang.org/x/@v/list") != 0 {
		t.Error("fetched upstream")
	}
}

func TestCanariesUnreadableRefused(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") })
	s := newServer(t, t.TempDir(), c, "go")
	if err := os.WriteFile(filepath.Join(s.Dir, canary.File), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	s.OnCanary = func(*canary.Canary, string) { called = true }
	if w := get(s, "/go/x/@v/list"); w.Code != http.StatusServiceUnavailable || called {
		t.Errorf("%d, OnCanary called: %v", w.Code, called)
	}
	if up.count("proxy.golang.org/x/@v/list") != 0 {
		t.Error("fetched upstream")
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, egress.LogFile)); !strings.Contains(string(b), `"action":"deny","reason":"canaries unreadable`) {
		t.Errorf("refusal not logged: %s", b)
	}
}

func TestStoredFetchHeaders(t *testing.T) {
	const body = "tarball-bytes"
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") == "gzip" {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			io.WriteString(zw, body)
			zw.Close()
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
	})
	s := newServer(t, t.TempDir(), c, "npm")
	const tgz = "/npm/a/-/a-1.0.0.tgz"
	if w := get(s, tgz, "Accept-Encoding", "gzip", "Range", "bytes=0-6"); w.Code != 200 || w.Body.String() != body {
		t.Errorf("miss: %d %q", w.Code, w.Body)
	}
	if up.last.Get("Accept-Encoding") != "" || up.last.Get("Range") != "" {
		t.Errorf("upstream saw %v", up.last)
	}
	s.downloads.Wait()
	if w := get(s, tgz); w.Body.String() != body || w.Header().Get("Content-Encoding") != "" || up.count("registry.npmjs.org"+strings.TrimPrefix(tgz, "/npm")) != 1 {
		t.Errorf("hit: %q %v", w.Body, w.Header())
	}
}

func TestDefaultClient(t *testing.T) {
	var hits atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer local.Close()
	s := &Server{}
	if resp, err := s.client(false).Get(local.URL); err == nil || !strings.Contains(err.Error(), "private") || hits.Load() != 0 {
		if err == nil {
			resp.Body.Close()
		}
		t.Errorf("dialled a private address: %v", err)
	}
	if !s.client(false).Transport.(*http.Transport).DisableCompression {
		t.Error("stored and pass-through fetches would ask for gzip")
	}
	if s.client(true).Transport.(*http.Transport).DisableCompression {
		t.Error("rewritten indexes fetched uncompressed")
	}
}

func TestTokenRealmChecked(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"token":"tok"}`) })
	s := newServer(t, t.TempDir(), c, "docker")
	if _, err := s.token(context.Background(), `Bearer realm="http://auth.docker.io/token",service="registry.docker.io"`); err == nil {
		t.Error("http:// realm used")
	}
	if err := egress.WriteList(s.Dir, egress.DenylistFile, []string{"auth.docker.io"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.token(context.Background(), `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`); !errors.Is(err, errDenied) {
		t.Errorf("denied realm: %v", err)
	}
	if n := up.count("auth.docker.io/token"); n != 0 {
		t.Errorf("realm fetched %d times", n)
	}
}

// eofBody ends cleanly, short of the length its response declares.
type eofBody struct{ io.Reader }

func (eofBody) Close() error { return nil }

func TestShortBodyNotCommitted(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	rt := &route{kind: "go", url: "https://proxy.golang.org/x/@v/v1.0.0.zip"}
	path := st.path(rt.kind, rt.url)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPfx+"*")
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: 200, ContentLength: 100, Header: http.Header{}, Body: eofBody{strings.NewReader("only ten b")}}
	d := &download{}
	d.cond.L = &d.mu
	d.run(st, resp, tmp, rt, path)
	if _, err := os.Stat(path); err == nil || !d.done || d.n != 10 {
		t.Errorf("committed: %v, done %v, %d bytes", err, d.done, d.n)
	}
	if _, err := os.Stat(tmp.Name()); err == nil {
		t.Error("temp file left")
	}
}

func TestNoRoomRelayedNotStored(t *testing.T) {
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "wheel") })
	s := newServer(t, t.TempDir(), c, "pypi")
	s.Store.statfs = func(string) (int64, int64, error) { return 5 << 20, 100 << 20, nil } // at the floor
	const file = "/pypi/files/packages/x.whl"
	for range 2 {
		if w := get(s, file); w.Code != 200 || w.Body.String() != "wheel" {
			t.Errorf("%d %q", w.Code, w.Body)
		}
	}
	if n := stored(t, s.Store, "pypi"); n != 0 || up.count("files.pythonhosted.org/packages/x.whl") != 2 {
		t.Errorf("stored %d with no room", n)
	}
}

func TestRoomRecheckedWhileDownloading(t *testing.T) {
	const size = 20 << 20
	_, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		io.WriteString(w, strings.Repeat("x", size))
	})
	s := newServer(t, t.TempDir(), c, "go")
	var calls atomic.Int32
	s.Store.statfs = func(string) (int64, int64, error) {
		if calls.Add(1) == 1 { // the check before fetching
			return 100 << 30, 200 << 30, nil
		}
		return 10 << 30, 200 << 30, nil // other downloads filled it
	}
	w := get(s, "/go/x/@v/v1.0.0.zip")
	s.downloads.Wait()
	if w.Header().Get("Content-Length") != strconv.Itoa(size) || w.Body.Len() >= size || calls.Load() < 2 {
		t.Errorf("guest got %d of %s bytes, %d checks", w.Body.Len(), w.Header().Get("Content-Length"), calls.Load())
	}
	if n := stored(t, s.Store, "go"); n != 0 {
		t.Errorf("stored %d", n)
	}
	if tmps, _ := filepath.Glob(filepath.Join(s.Store.Dir, "go", "*", tmpPfx+"*")); len(tmps) != 0 {
		t.Errorf("temp files left: %v", tmps)
	}
}

func TestSlowSteadyGuestServed(t *testing.T) {
	old := clientStall
	clientStall = time.Second
	t.Cleanup(func() { clientStall = old })
	const size = 4 << 20 // over 3s to read
	_, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		io.WriteString(w, strings.Repeat("x", size))
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := newServer(t, t.TempDir(), c, "go")
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		conn.(*net.TCPConn).SetWriteBuffer(16 << 10) // the backlog stays in the temp file
		done <- s.ServeConn(context.Background(), conn)
	}()
	// A small receive window, set before connecting: the backlog can't hide in it.
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 64<<10) })
	}}
	conn, err := d.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /go/x/@v/v1.0.0.zip HTTP/1.1\r\nHost: 10.0.2.102\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	var n int
	for n < size { // 64 KiB every 50ms: steady, slower than upstream
		time.Sleep(50 * time.Millisecond)
		k, err := io.ReadFull(resp.Body, buf)
		n += k
		if err != nil {
			break
		}
	}
	if n != size {
		t.Errorf("guest got %d of %d bytes", n, size)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func TestStalledGuestDroppedFromRelays(t *testing.T) {
	old := clientStall
	clientStall = time.Second
	t.Cleanup(func() { clientStall = old })
	_, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("x", 32<<10)
		for { // no Content-Length: not storable either
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	})
	for _, target := range []string{"/go/x/@v/list", "/go/x/@v/v1.0.0.zip"} {
		stalledGuest(t, newServer(t, t.TempDir(), c, "go"), target)
	}
}

func TestStalledGuestDroppedFromHits(t *testing.T) {
	old := clientStall
	clientStall = time.Second
	t.Cleanup(func() { clientStall = old })
	body := strings.Repeat("x", 16<<20)
	_, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
	})
	s := newServer(t, t.TempDir(), c, "go")
	const zip = "/go/x/@v/v1.0.0.zip"
	if w := get(s, zip); w.Code != 200 {
		t.Fatalf("miss: %d", w.Code)
	}
	s.downloads.Wait()
	stalledGuest(t, s, zip)
}

// stalledGuest requests target from s over a connection it never reads, and
// fails unless s gives up on it.
func stalledGuest(t *testing.T, s *Server, target string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		conn.(*net.TCPConn).SetWriteBuffer(16 << 10)
		done <- s.ServeConn(context.Background(), conn)
	}()
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 64<<10) })
	}}
	conn, err := d.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET "+target+" HTTP/1.1\r\nHost: 10.0.2.102\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select { // the guest never reads
	case err := <-done:
		if err != nil {
			t.Errorf("%s: %v", target, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: a guest not reading holds the connection", target)
	}
}

func TestTokensCapped(t *testing.T) {
	_, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "auth.docker.io" {
			io.WriteString(w, `{"token":"tok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("Www-Authenticate", `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "{}")
	})
	s := newServer(t, t.TempDir(), c, "docker")
	for i := range maxTokens + 10 {
		if w := get(s, "/v2/r"+strconv.Itoa(i)+"/app/manifests/latest"); w.Code != 200 {
			t.Fatalf("repository %d: %d", i, w.Code)
		}
	}
	if len(s.tokens) > maxTokens || s.tokens["r"+strconv.Itoa(maxTokens+9)+"/app"] != "tok" {
		t.Errorf("%d tokens kept", len(s.tokens))
	}
}

func TestEncodedNotStored(t *testing.T) {
	var gz strings.Builder
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, "wheel")
	zw.Close()
	up, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip") // unasked
		w.Header().Set("Content-Length", strconv.Itoa(gz.Len()))
		io.WriteString(w, gz.String())
	})
	store := t.TempDir()
	for i := range 2 {
		w := get(newServer(t, store, c, "pypi"), "/pypi/files/packages/x.whl")
		if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || w.Body.String() != gz.String() {
			t.Errorf("request %d: %d %v", i, w.Code, w.Header())
		}
	}
	if n := up.count("files.pythonhosted.org/packages/x.whl"); n != 2 {
		t.Errorf("fetched %d times: an encoded object was stored", n)
	}
}

// With every slot taken, a new guest connection is closed unserved.
func TestConnectionLimit(t *testing.T) {
	_, c := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	dir := t.TempDir()
	s := newServer(t, dir, c, "go")
	sd := filepath.Join(s.Dir, "cache.slots")
	if err := os.MkdirAll(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < egress.MaxConns; i++ {
		f, err := os.OpenFile(filepath.Join(sd, strconv.Itoa(i)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
	}
	guest, host := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- s.ServeConn(context.Background(), host) }()
	go io.WriteString(guest, "GET /go/x/@v/list HTTP/1.1\r\nHost: 10.0.2.102\r\n\r\n")
	if n, err := guest.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("served past the limit: %d %v", n, err)
	}
	<-done
}
