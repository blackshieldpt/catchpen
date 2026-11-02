package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// tcpAPI is a server on a TLS listener with two enrolled clients: cockpit
// (HMAC, config:write) and laptop (mTLS).
type tcpAPI struct {
	url, pin string
	clients  string
	key      []byte          // cockpit's
	laptop   tls.Certificate // laptop's certificate and key
	srv      *Server
}

func serveTCP(t *testing.T) *tcpAPI {
	t.Helper()
	dir := t.TempDir()
	a := &tcpAPI{clients: filepath.Join(dir, "clients.yaml")}
	var err error
	if a.key, err = AddHMAC(a.clients, "cockpit", []string{ScopeConfigWrite}); err != nil {
		t.Fatal(err)
	}
	if a.laptop, err = Certificate(filepath.Join(dir, "laptop.key"), filepath.Join(dir, "laptop.crt")); err != nil {
		t.Fatal(err)
	}
	if _, err := AddMTLS(a.clients, "laptop", a.laptop.Leaf, nil); err != nil {
		t.Fatal(err)
	}
	cert, err := Certificate(filepath.Join(dir, "server.key"), filepath.Join(dir, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	a.pin = api.PublicKeyPin(cert.Leaf)
	a.srv = New("1.2.3", a.clients)
	l, err := a.srv.ListenTCP("127.0.0.1:0", cert)
	if err != nil {
		t.Fatal(err)
	}
	a.url = "https://" + l.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, a.srv.Handler()) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return a
}

func (a *tcpAPI) client(t *testing.T, o apiclient.Options) *apiclient.Client {
	t.Helper()
	o.URL = a.url
	if o.ServerPin == "" {
		o.ServerPin = a.pin
	}
	c, err := apiclient.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func info(c *apiclient.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	i, err := c.Info(ctx)
	if err == nil && i.Version != "1.2.3" {
		return errors.New("wrong info")
	}
	return err
}

func code(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestTCPAuth(t *testing.T) {
	a := serveTCP(t)
	if err := info(a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})); err != nil {
		t.Errorf("HMAC client: %v", err)
	}
	if err := info(a.client(t, apiclient.Options{Cert: &a.laptop})); err != nil {
		t.Errorf("mTLS client: %v", err)
	}

	wrong := bytes.Repeat([]byte{1}, 32)
	for what, o := range map[string]apiclient.Options{
		"wrong key":                {HMAC: &apiclient.HMAC{Name: "cockpit", Key: wrong}},
		"unknown name":             {HMAC: &apiclient.HMAC{Name: "nobody", Key: a.key}},
		"an mTLS client with HMAC": {HMAC: &apiclient.HMAC{Name: "laptop", Key: wrong}},
	} {
		if err := info(a.client(t, o)); code(err) != api.CodeUnauthorized {
			t.Errorf("%s: %v, want unauthorized", what, err)
		}
	}

	// A certificate no client has is refused in the handshake.
	other, err := Certificate(filepath.Join(t.TempDir(), "k"), filepath.Join(t.TempDir(), "c"))
	if err != nil {
		t.Fatal(err)
	}
	if err := info(a.client(t, apiclient.Options{Cert: &other})); err == nil || code(err) != "" {
		t.Errorf("unenrolled certificate: %v, want a TLS error", err)
	}

	// The client checks the server's pin.
	if err := info(a.client(t, apiclient.Options{Cert: &a.laptop, ServerPin: api.PublicKeyPin(other.Leaf)})); err == nil || !strings.Contains(err.Error(), "doesn't match ServerPin") {
		t.Errorf("wrong server pin: %v", err)
	}

	// No certificate and no signature.
	resp, err := pinned(a, nil).Get(a.url + api.PathInfo)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credentials: %d", resp.StatusCode)
	}
}

// pinned is an http.Client that pins a's server and sends cert if set.
func pinned(a *tcpAPI, cert *tls.Certificate) *http.Client {
	tc := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
		VerifyConnection: func(st tls.ConnectionState) error {
			if api.PublicKeyPin(st.PeerCertificates[0]) != a.pin {
				return errors.New("pin")
			}
			return nil
		}}
	if cert != nil {
		tc.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
}

// send sends a request signed by hand.
func send(t *testing.T, hc *http.Client, r *http.Request) (int, string) {
	t.Helper()
	resp, err := hc.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var er api.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&er)
	if er.Error != nil {
		return resp.StatusCode, er.Error.Code
	}
	return resp.StatusCode, ""
}

func TestTCPReplayStaleAndLarge(t *testing.T) {
	a := serveTCP(t)
	hc := pinned(a, nil)
	r, _ := http.NewRequest("GET", a.url+api.PathInfo, nil)
	if err := api.Sign(r, "cockpit", a.key, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := send(t, hc, r); st != http.StatusOK {
		t.Fatalf("first: %d", st)
	}
	r2, _ := http.NewRequest("GET", a.url+api.PathInfo, nil)
	r2.Header = r.Header.Clone()
	if st, c := send(t, hc, r2); st != http.StatusUnauthorized || c != api.CodeUnauthorized {
		t.Errorf("replay: %d %s", st, c)
	}

	// Signed two minutes ago.
	ts := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	const nonce = "00000000-0000-4000-8000-000000000001"
	r3, _ := http.NewRequest("GET", a.url+api.PathInfo, nil)
	r3.Header.Set(api.HeaderVersion, api.HMACVersion)
	r3.Header.Set(api.HeaderTimestamp, ts)
	r3.Header.Set(api.HeaderNonce, nonce)
	r3.Header.Set(api.HeaderHash, "cockpit."+api.MAC(a.key, api.Canonical(ts, nonce, "GET", api.PathInfo, nil)))
	if st, c := send(t, hc, r3); st != http.StatusUnauthorized || c != api.CodeUnauthorized {
		t.Errorf("stale: %d %s", st, c)
	}

	// A signed body over the limit.
	big := make([]byte, api.MaxRequestBody+1)
	r4, _ := http.NewRequest("POST", a.url+api.PathInfo, bytes.NewReader(big))
	if err := api.Sign(r4, "cockpit", a.key, big); err != nil {
		t.Fatal(err)
	}
	if st, c := send(t, hc, r4); st != http.StatusRequestEntityTooLarge || c != api.CodeTooLarge {
		t.Errorf("large body: %d %s", st, c)
	}
	// The same answer without a signature: it says nothing about the name.
	r5, _ := http.NewRequest("POST", a.url+api.PathInfo, bytes.NewReader(big))
	if st, c := send(t, hc, r5); st != http.StatusRequestEntityTooLarge || c != api.CodeTooLarge {
		t.Errorf("large unsigned body: %d %s", st, c)
	}
	// Chunked, so its size is known only once read: refused all the same.
	r6, _ := http.NewRequest("POST", a.url+api.PathInfo, io.MultiReader(bytes.NewReader(big)))
	if err := api.Sign(r6, "cockpit", a.key, big); err != nil {
		t.Fatal(err)
	}
	if st, c := send(t, hc, r6); st != http.StatusUnauthorized || c != api.CodeUnauthorized {
		t.Errorf("chunked large body: %d %s", st, c)
	}
}

// A removed client is refused from its next request, even on a connection
// it already has.
func TestTCPRevocation(t *testing.T) {
	a := serveTCP(t)
	hm := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	mt := a.client(t, apiclient.Options{Cert: &a.laptop})
	for _, c := range []*apiclient.Client{hm, mt} {
		if err := info(c); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"cockpit", "laptop"} {
		if err := RemoveClient(a.clients, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := info(hm); code(err) != api.CodeUnauthorized {
		t.Errorf("removed HMAC client: %v", err)
	}
	if err := info(mt); code(err) != api.CodeUnauthorized {
		t.Errorf("removed mTLS client, same connection: %v", err)
	}
	if err := info(a.client(t, apiclient.Options{Cert: &a.laptop})); err == nil {
		t.Error("removed mTLS client, new connection: served")
	}
}

func TestTCPRefusesTLS12(t *testing.T) {
	a := serveTCP(t)
	c, err := tls.Dial("tcp", strings.TrimPrefix(a.url, "https://"), &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
	if err == nil {
		c.Close()
		t.Error("TLS 1.2 handshake succeeded")
	}
}

func TestListenTCPNeedsClients(t *testing.T) {
	dir := t.TempDir()
	cert, err := Certificate(filepath.Join(dir, "k"), filepath.Join(dir, "c"))
	if err != nil {
		t.Fatal(err)
	}
	s := New("1.2.3", filepath.Join(dir, "clients.yaml"))
	if l, err := s.ListenTCP("127.0.0.1:0", cert); err == nil || !strings.Contains(err.Error(), "no client is enrolled") {
		if l != nil {
			l.Close()
		}
		t.Errorf("no clients: %v", err)
	}
}

// Connections past the limit wait, handshake or not, until one closes.
func TestTCPConnectionLimit(t *testing.T) {
	old := maxConns
	maxConns = 2
	t.Cleanup(func() { maxConns = old })
	a := serveTCP(t)
	addr := strings.TrimPrefix(a.url, "https://")
	var idle []net.Conn
	for range 2 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}}
	if conn, err := d.DialContext(ctx, "tcp", addr); err == nil {
		conn.Close()
		t.Error("served past the limit")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("past the limit: %v, want it to wait", err)
	}
	c := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	idle[0].Close()
	if err := info(c); err != nil {
		t.Errorf("after a connection closed: %v", err)
	}
	idle[1].Close()
}

// The handler passes on who called: the client, with its scopes.
func TestCallerOf(t *testing.T) {
	a := serveTCP(t)
	var got []Caller
	h := func(w http.ResponseWriter, r *http.Request) {
		r, ok := a.srv.authTCP(w, r)
		if ok {
			c, _ := CallerOf(r.Context())
			got = append(got, c)
		}
	}
	r := httptest.NewRequest("GET", api.PathInfo, nil)
	r.TLS = &tls.ConnectionState{}
	if err := api.Sign(r, "cockpit", a.key, nil); err != nil {
		t.Fatal(err)
	}
	h(httptest.NewRecorder(), r)
	r = httptest.NewRequest("GET", api.PathInfo, nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{a.laptop.Leaf}}
	h(httptest.NewRecorder(), r)
	if len(got) != 2 || got[0].Name != "cockpit" || !slices.Equal(got[0].Scopes, []string{ScopeConfigWrite}) || got[1].Name != "laptop" || got[1].Scopes != nil {
		t.Errorf("callers %+v", got)
	}
	if _, ok := CallerOf(context.Background()); ok {
		t.Error("a caller with no client")
	}
}

// A client past its nonce limit in the window is told to slow down, not
// that it isn't enrolled.
func TestTCPNonceCacheFull(t *testing.T) {
	a := serveTCP(t)
	a.srv.hmac.SetNonceLimit(2)
	c := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	for i := range 2 {
		if err := info(c); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if err := info(c); code(err) != api.CodeRateLimited {
		t.Errorf("third request: %v, want rate_limited", err)
	}
}

// halfRequest sends a request's headers, as signed by name with a wrong
// MAC, and none of its body; it returns the answer's status line, read
// within 3 s, and whether the connection then closed.
func halfRequest(t *testing.T, a *tcpAPI, framing, name string) (string, bool) {
	t.Helper()
	conn, err := tls.Dial("tcp", strings.TrimPrefix(a.url, "https://"), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\n%s\r\n%s: %s\r\n%s: %s.00\r\n%s: %s\r\n%s: %s\r\n\r\n",
		api.PathInfo, framing, api.HeaderVersion, api.HMACVersion, api.HeaderHash, name, api.HeaderTimestamp, ts, api.HeaderNonce, rand.Text())
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return err.Error(), false
	}
	_, err = io.Copy(io.Discard, br)
	return line, err == nil
}

// A request that can't be valid is refused, and its connection closed,
// without waiting for its body: here the body never comes.
func TestTCPPrecheckFirst(t *testing.T) {
	a := serveTCP(t)
	for _, framing := range []string{"Content-Length: 1000000", "Content-Length: 1000", "Transfer-Encoding: chunked"} {
		if line, closed := halfRequest(t, a, framing, "nobody"); !strings.HasPrefix(line, "HTTP/1.1 401 ") || !closed {
			t.Errorf("%s: status line %q, closed %v; want 401 before the body", framing, line, closed)
		}
	}
}

// An enrolled name passes Precheck, so its body is read: a client that
// never sends it is cut off after requestTimeout.
func TestTCPRequestTimeout(t *testing.T) {
	old := requestTimeout
	requestTimeout = 300 * time.Millisecond
	t.Cleanup(func() { requestTimeout = old })
	a := serveTCP(t)
	if line, closed := halfRequest(t, a, "Content-Length: 1000", "cockpit"); !strings.HasPrefix(line, "HTTP/1.1 401 ") || !closed {
		t.Errorf("status line %q, closed %v; want 401 after requestTimeout", line, closed)
	}
}

// One address gets at most maxConnsPerAddr connections; the next is closed
// at once.
func TestTCPPerAddrLimit(t *testing.T) {
	old := maxConnsPerAddr
	maxConnsPerAddr = 1
	t.Cleanup(func() { maxConnsPerAddr = old })
	a := serveTCP(t)
	addr := strings.TrimPrefix(a.url, "https://")
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("second connection from one address still open: %v", err)
	}
	first.Close()
	// The server sees the close a moment later.
	c := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	err = info(c)
	for start := time.Now(); err != nil && time.Since(start) < 3*time.Second; err = info(c) {
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("after the first closed: %v", err)
	}
}

// A connection that never handshakes is closed after headerTimeout, and so
// gives its slot back.
func TestTCPHandshakeTimeout(t *testing.T) {
	old := headerTimeout
	headerTimeout = time.Second
	t.Cleanup(func() { headerTimeout = old })
	a := serveTCP(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(a.url, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); err == nil || ok && ne.Timeout() {
		t.Errorf("silent connection still open after 3s: %v", err)
	}
}

// The slot of a connection that never handshook is given back.
func TestTCPHandshakeTimeoutFreesSlot(t *testing.T) {
	oldH, oldM := headerTimeout, maxConns
	headerTimeout, maxConns = time.Second, 1
	t.Cleanup(func() { headerTimeout, maxConns = oldH, oldM })
	a := serveTCP(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(a.url, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Info(ctx); err != nil {
		t.Errorf("after the silent connection timed out: %v", err)
	}
}

func TestHMACKey(t *testing.T) {
	a := serveTCP(t)
	if k, ok := a.srv.hmacKey("cockpit"); !ok || !bytes.Equal(k, a.key) {
		t.Errorf("cockpit: %v", ok)
	}
	for _, name := range []string{"laptop", "nobody", ""} {
		if _, ok := a.srv.hmacKey(name); ok {
			t.Errorf("%q has an HMAC key", name)
		}
	}
}

// Closing the listener ends an Accept waiting for a free slot.
func TestLimitListenerClose(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := newLimitListener(inner, 1)
	c, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	held, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	l.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept after Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept still waiting for a slot after Close")
	}
}

// rawTLS sends req over a new TLS connection and returns the answer's
// status line, read within 3 s, and whether the connection then closed.
func rawTLS(t *testing.T, a *tcpAPI, req string) (string, bool) {
	t.Helper()
	conn, err := tls.Dial("tcp", strings.TrimPrefix(a.url, "https://"), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, req)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return err.Error(), false
	}
	_, err = io.Copy(io.Discard, br)
	return line, err == nil
}

// Every refusal closes its connection, a request with no body included,
// and "OPTIONS *" is no way around authTCP.
func TestTCPRefusalCloses(t *testing.T) {
	oldH, oldR := headerTimeout, requestTimeout
	headerTimeout, requestTimeout = time.Second, 300*time.Millisecond
	t.Cleanup(func() { headerTimeout, requestTimeout = oldH, oldR })
	a := serveTCP(t)
	for _, c := range []struct{ req, status string }{
		{"GET " + api.PathInfo + " HTTP/1.1\r\nHost: x\r\n\r\n", "401"},
		{"OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n", "401"},
		{"OPTIONS * HTTP/1.1\r\nHost: x\r\nContent-Length: 1\r\n\r\n", "401"},
		// net/http answers this itself, then reads the body: ReadTimeout ends it.
		{"POST " + api.PathInfo + " HTTP/1.1\r\nHost: x\r\nExpect: x\r\nContent-Length: 1\r\n\r\n", "417"},
	} {
		if line, closed := rawTLS(t, a, c.req); !strings.HasPrefix(line, "HTTP/1.1 "+c.status+" ") || !closed {
			t.Errorf("%q: status line %q, closed %v; want %s and closed", c.req, line, closed, c.status)
		}
	}
}

// A write to a peer that never reads fails after writeStall: with no
// deadline asked for, and when the one asked for is cleared during it.
func TestLimitConnWriteStall(t *testing.T) {
	old := writeStall
	writeStall = 300 * time.Millisecond
	t.Cleanup(func() { writeStall = old })
	for _, clear := range []bool{false, true} {
		first, next := stalledWrite(t, clear)
		if !errors.Is(first, os.ErrDeadlineExceeded) {
			t.Errorf("deadline cleared %v: write: %v", clear, first)
		}
		// The socket is closed: no later write waits writeStall again.
		if !errors.Is(next, net.ErrClosed) {
			t.Errorf("deadline cleared %v: next write: %v", clear, next)
		}
	}
}

// stalledWrite writes to a peer that never reads until the write fails, or
// for 3 s, and then writes once more.
func stalledWrite(t *testing.T, clear bool) (first, next error) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	l := newLimitListener(inner, 1)
	peer, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1<<20)
		for {
			if _, err := c.Write(buf); err != nil {
				done <- err
				return
			}
		}
	}()
	if clear {
		time.Sleep(100 * time.Millisecond)
		c.SetWriteDeadline(time.Time{})
	}
	select {
	case err := <-done:
		_, next := c.Write([]byte{1})
		return err, next
	case <-time.After(3 * time.Second):
		return errors.New("still blocked after 3s"), nil
	}
}

// A connection no request on has authenticated is closed after
// preAuthLifetime, however slowly it sends; an authenticated one isn't.
func TestTCPPreAuthLifetime(t *testing.T) {
	oldL, oldM := preAuthLifetime, maxConns
	preAuthLifetime, maxConns = 1500*time.Millisecond, 1 // so the client below needs the slot back
	t.Cleanup(func() { preAuthLifetime, maxConns = oldL, oldM })
	a := serveTCP(t)
	conn, err := tls.Dial("tcp", strings.TrimPrefix(a.url, "https://"), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET "+api.PathInfo+" HTTP/1.1\r\n") // and no more
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("unauthenticated connection still open after 5s: %v", err)
	}

	hc := pinned(a, &a.laptop)
	reused := false
	for i := range 2 {
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(ci httptrace.GotConnInfo) { reused = ci.Reused },
		})
		r, _ := http.NewRequestWithContext(ctx, "GET", a.url+api.PathInfo, nil)
		if st, _ := send(t, hc, r); st != http.StatusOK {
			t.Fatalf("request %d: %d", i, st)
		}
		time.Sleep(time.Duration(1-i) * 2 * time.Second) // past preAuthLifetime, after the first

	}
	if !reused {
		t.Error("authenticated connection closed after preAuthLifetime")
	}
}

// Closing the listener closes the connections not yet authenticated, at
// once; the others are left to finish.
func TestLimitListenerCloseEndsPreAuth(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := newLimitListener(inner, 2)
	var peers []net.Conn
	var conns []net.Conn
	for range 2 {
		p, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		peers, conns = append(peers, p), append(conns, c)
	}
	conns[1].(*limitConn).authenticated()
	l.Close()
	peers[0].SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := peers[0].Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("unauthenticated connection open after Close: %v", err)
	}
	peers[1].SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := peers[1].Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("authenticated connection closed by Close: %v", err)
	}
	for _, c := range conns {
		c.Close()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.conns) != 0 || len(l.perAddr) != 0 {
		t.Errorf("closed connections still tracked: %d, %v", len(l.conns), l.perAddr)
	}
}

func TestAddrKey(t *testing.T) {
	key := func(s string) string { return addrKey(net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s))) }
	for _, same := range [][2]string{
		{"192.0.2.1:1", "192.0.2.1:2"},
		{"192.0.2.1:1", "[::ffff:192.0.2.1]:2"},
		{"[2001:db8:1:2::1]:1", "[2001:db8:1:2:ffff::9]:2"},
	} {
		if key(same[0]) != key(same[1]) {
			t.Errorf("%s and %s counted apart: %s, %s", same[0], same[1], key(same[0]), key(same[1]))
		}
	}
	if addrKey(&net.UnixAddr{Name: "a"}) != addrKey(&net.UnixAddr{Name: "b"}) {
		t.Error("addresses it can't read counted apart")
	}
	for _, apart := range [][2]string{
		{"192.0.2.1:1", "192.0.2.2:1"},
		{"[2001:db8:1:2::1]:1", "[2001:db8:1:3::1]:1"},
	} {
		if key(apart[0]) == key(apart[1]) {
			t.Errorf("%s and %s counted together: %s", apart[0], apart[1], key(apart[0]))
		}
	}
}
