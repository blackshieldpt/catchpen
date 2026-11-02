package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = func() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}()

var otherKey = []byte("another key of thirty-two bytes!")

// Fixed vectors, computed outside Go (Python's hmac and hashlib) with the key
// 0x00..0x1f, so a client in any language can check its signing against them.
func TestCanonicalAndMACVectors(t *testing.T) {
	for _, c := range []struct {
		ts, nonce, method, target, body, canonical, mac string
	}{
		{"2026-11-02T12:00:00Z", "00000000-0000-4000-8000-000000000000", "GET", "/v1/vms", "",
			"2026-11-02T12:00:00Z:00000000-0000-4000-8000-000000000000:GET:/v1/vms:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"dbf1668f9bc6d113999ae8d0434b12d26a2cb534fa37678099d8663566bf23ff"},
		{"2026-11-02T12:00:00Z", "11111111-2222-4333-8444-555555555555", "POST", "/v1/vms/dev/stop?force=1", `{"confirm":true}`,
			"2026-11-02T12:00:00Z:11111111-2222-4333-8444-555555555555:POST:/v1/vms/dev/stop?force=1:63547eaf4a83685f619c8e5945531198eea290b70c54ca9a45db5dbd3fddee34",
			"431bbedb837d57d1d65c3abd5bfe32c458b4cba55f169d8525bc8a0d7992d00c"},
	} {
		var body []byte
		if c.body != "" {
			body = []byte(c.body)
		}
		got := Canonical(c.ts, c.nonce, c.method, c.target, body)
		if got != c.canonical {
			t.Errorf("canonical %q, want %q", got, c.canonical)
		}
		if m := MAC(testKey, got); m != c.mac {
			t.Errorf("MAC of %q: %s, want %s", got, m, c.mac)
		}
	}
}

// at is a Verifier whose clock reads now. It knows the clients "cockpit"
// (testKey) and "other" (otherKey), and two whose keys are too short to use.
func at(now time.Time) *Verifier {
	v := NewVerifier(func(name string) ([]byte, bool) {
		switch name {
		case "cockpit":
			return testKey, true
		case "other":
			return otherKey, true
		case "nokey":
			return nil, true
		case "short":
			return []byte("short"), true
		}
		return nil, false
	})
	v.now = func() time.Time { return now }
	return v
}

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// signedBy is a request as a server receives it, signed by client name with
// key at when, with nonce n.
func signedBy(t *testing.T, name string, key []byte, when time.Time, method, target, body, n string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if err := sign(r, name, key, []byte(body), when, n); err != nil {
		t.Fatal(err)
	}
	return r
}

// signed is a request signed by cockpit at t0.
func signed(t *testing.T, method, target, body, n string) *http.Request {
	t.Helper()
	return signedBy(t, "cockpit", testKey, t0, method, target, body, n)
}

const n1 = "00000000-0000-4000-8000-000000000001"

// A client's request, signed as the transport will send it, verifies on
// the server.
func TestSignVerify(t *testing.T) {
	r, err := http.NewRequest("POST", "http://unix/v1/vms/dev/stop?force=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(r, "cockpit", testKey, []byte(`{"confirm":true}`)); err != nil {
		t.Fatal(err)
	}
	if n := r.Header.Get(HeaderNonce); len(n) != 36 || n[14] != '4' {
		t.Errorf("nonce %q is not a version 4 UUID", n)
	}
	r.RequestURI = r.URL.RequestURI() // as the server reads the request line
	name, err := at(time.Now()).Verify(r, []byte(`{"confirm":true}`))
	if err != nil || name != "cockpit" {
		t.Fatalf("Verify: %q, %v", name, err)
	}
	// A request with no method is sent as a GET.
	g, _ := http.NewRequest("", "http://unix/v1/vms", nil)
	g.Method = ""
	if err := Sign(g, "cockpit", testKey, nil); err != nil {
		t.Fatal(err)
	}
	g.Method, g.RequestURI = "GET", "/v1/vms"
	if _, err := at(time.Now()).Verify(g, nil); err != nil {
		t.Errorf("no method, sent as GET: %v", err)
	}
	if err := Sign(r, "a.b", testKey, nil); err == nil {
		t.Error("signed for a client name holding a dot")
	}
	// The timestamp is written in UTC, whatever the client's zone.
	z := signedBy(t, "cockpit", testKey, t0.In(time.FixedZone("CET", 3600)), "GET", "/v1/vms", "", n1)
	if ts := z.Header.Get(HeaderTimestamp); ts != "2026-11-02T12:00:00Z" {
		t.Errorf("timestamp %q, want 2026-11-02T12:00:00Z", ts)
	}
}

// A client that doesn't sign with Go: the server checks the request target
// as sent, not as Go would escape it.
func TestVerifyRawTarget(t *testing.T) {
	const raw = "/v1/x|y?q=a%20b"
	r := httptest.NewRequest("GET", raw, nil)
	if r.URL.RequestURI() == raw {
		t.Fatalf("Go no longer escapes %q", raw)
	}
	r.Header.Set(HeaderVersion, HMACVersion)
	r.Header.Set(HeaderTimestamp, "2026-11-02T12:00:00Z")
	r.Header.Set(HeaderNonce, n1)
	r.Header.Set(HeaderHash, "cockpit."+MAC(testKey, Canonical("2026-11-02T12:00:00Z", n1, "GET", raw, nil)))
	if name, err := at(t0).Verify(r, nil); err != nil || name != "cockpit" {
		t.Errorf("%q, %v", name, err)
	}
}

// A client that writes its timestamp with an offset: the server checks the
// instant, and the signature covers the text as sent.
func TestVerifyOffsetTimestamp(t *testing.T) {
	const ts = "2026-11-02T13:00:00+01:00" // t0
	r := httptest.NewRequest("GET", "/v1/vms", nil)
	r.Header.Set(HeaderVersion, HMACVersion)
	r.Header.Set(HeaderTimestamp, ts)
	r.Header.Set(HeaderNonce, n1)
	r.Header.Set(HeaderHash, "cockpit."+MAC(testKey, Canonical(ts, n1, "GET", "/v1/vms", nil)))
	if name, err := at(t0).Verify(r, nil); err != nil || name != "cockpit" {
		t.Errorf("%q, %v", name, err)
	}
}

// Every part of the request is signed, and every header is checked.
func TestVerifyRefuses(t *testing.T) {
	body := `{"confirm":true}`
	retarget := func(r *http.Request, target string) *http.Request {
		r2 := httptest.NewRequest(r.Method, target, nil)
		r2.Header = r.Header
		return r2
	}
	for _, c := range []struct {
		name   string
		change func(r *http.Request) (*http.Request, string)
		want   error
	}{
		{"no headers", func(r *http.Request) (*http.Request, string) {
			return httptest.NewRequest("POST", "/v1/vms/dev/stop", nil), body
		}, ErrNoSignature},
		{"version 1", func(r *http.Request) (*http.Request, string) { r.Header.Set(HeaderVersion, "1"); return r, body }, ErrNoSignature},
		{"no client name", func(r *http.Request) (*http.Request, string) {
			_, mac, _ := strings.Cut(r.Header.Get(HeaderHash), ".")
			r.Header.Set(HeaderHash, mac)
			return r, body
		}, ErrBadSignature},
		{"client name in the wrong form", func(r *http.Request) (*http.Request, string) {
			_, mac, _ := strings.Cut(r.Header.Get(HeaderHash), ".")
			r.Header.Set(HeaderHash, "Cockpit."+mac)
			return r, body
		}, ErrBadSignature},
		{"unknown client", func(r *http.Request) (*http.Request, string) {
			_, mac, _ := strings.Cut(r.Header.Get(HeaderHash), ".")
			r.Header.Set(HeaderHash, "stranger."+mac)
			return r, body
		}, ErrUnknownClient},
		{"another client's name", func(r *http.Request) (*http.Request, string) {
			_, mac, _ := strings.Cut(r.Header.Get(HeaderHash), ".")
			r.Header.Set(HeaderHash, "other."+mac)
			return r, body
		}, ErrBadSignature},
		{"method", func(r *http.Request) (*http.Request, string) { r.Method = "DELETE"; return r, body }, ErrBadSignature},
		{"path", func(r *http.Request) (*http.Request, string) { return retarget(r, "/v1/vms/prod/stop?force=1"), body }, ErrBadSignature},
		{"query", func(r *http.Request) (*http.Request, string) { return retarget(r, "/v1/vms/dev/stop?force=0"), body }, ErrBadSignature},
		{"body", func(r *http.Request) (*http.Request, string) { return r, `{"confirm":false}` }, ErrBadSignature},
		{"timestamp", func(r *http.Request) (*http.Request, string) {
			r.Header.Set(HeaderTimestamp, "2026-11-02T12:00:01Z")
			return r, body
		}, ErrBadSignature},
		{"timestamp not RFC 3339", func(r *http.Request) (*http.Request, string) {
			r.Header.Set(HeaderTimestamp, "2026-11-02 12:00:00")
			return r, body
		}, ErrBadSignature},
		{"nonce", func(r *http.Request) (*http.Request, string) {
			r.Header.Set(HeaderNonce, "00000000-0000-4000-8000-000000000002")
			return r, body
		}, ErrBadSignature},
		{"MAC not hex", func(r *http.Request) (*http.Request, string) {
			r.Header.Set(HeaderHash, "cockpit.zz")
			return r, body
		}, ErrBadSignature},
	} {
		r, b := c.change(signed(t, "POST", "/v1/vms/dev/stop?force=1", body, n1))
		if name, err := at(t0).Verify(r, []byte(b)); !errors.Is(err, c.want) || name != "" {
			t.Errorf("%s: %q, %v; want %v", c.name, name, err, c.want)
		}
	}
	// A nonce must be a short token even when it is signed: the cache keeps it.
	for _, n := range []string{"a b", strings.Repeat("a", 65)} {
		if _, err := at(t0).Verify(signed(t, "GET", "/v1/vms", "", n), nil); !errors.Is(err, ErrBadSignature) {
			t.Errorf("signed nonce %q: %v, want ErrBadSignature", n, err)
		}
	}
	// A key too short to be one: anyone could sign with it.
	for name, key := range map[string][]byte{"nokey": nil, "short": []byte("short")} {
		r := signedBy(t, name, key, t0, "GET", "/v1/vms", "", n1)
		if _, err := at(t0).Verify(r, nil); !errors.Is(err, ErrUnknownClient) {
			t.Errorf("client %s with a short key: %v, want ErrUnknownClient", name, err)
		}
	}
}

// Precheck refuses what it can from the headers alone, and leaves the body
// to Verify.
func TestPrecheck(t *testing.T) {
	v := at(t0)
	if err := v.Precheck(signed(t, "POST", "/v1/vms/dev/stop", `{"confirm":true}`, n1)); err != nil {
		t.Errorf("signed request: %v", err)
	}
	stale := signedBy(t, "cockpit", testKey, t0.Add(-2*Window), "GET", "/v1/vms", "", n1)
	unknown := signedBy(t, "stranger", testKey, t0, "GET", "/v1/vms", "", n1)
	for r, want := range map[*http.Request]error{
		stale: ErrStale, unknown: ErrUnknownClient, httptest.NewRequest("GET", "/v1/vms", nil): ErrNoSignature,
	} {
		if err := v.Precheck(r); !errors.Is(err, want) {
			t.Errorf("%v, want %v", err, want)
		}
	}
}

// A timestamp may be up to Window off, either way.
func TestVerifyWindow(t *testing.T) {
	for _, c := range []struct {
		off  time.Duration
		want error
	}{
		{Window, nil}, {-Window, nil}, {Window + time.Second, ErrStale}, {-Window - time.Second, ErrStale},
	} {
		r := signed(t, "GET", "/v1/vms", "", n1)
		if _, err := at(t0.Add(c.off)).Verify(r, nil); !errors.Is(err, c.want) {
			t.Errorf("server clock %v off: %v, want %v", c.off, err, c.want)
		}
	}
}

// A signed request is accepted once while its timestamp is in the window.
// A request that fails doesn't use up its nonce, and nonces are per client.
func TestVerifyReplay(t *testing.T) {
	v := at(t0)
	bad := signed(t, "GET", "/v1/vms", "", n1)
	bad.Header.Set(HeaderTimestamp, "2026-11-02T12:00:01Z")
	if _, err := v.Verify(bad, nil); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := v.Verify(signed(t, "GET", "/v1/vms", "", n1), nil); err != nil {
		t.Fatalf("first use after a failed one: %v", err)
	}
	if _, err := v.Verify(signed(t, "GET", "/v1/vms", "", n1), nil); !errors.Is(err, ErrReplay) {
		t.Errorf("replay: %v, want ErrReplay", err)
	}
	v.now = func() time.Time { return t0.Add(Window) }
	if _, err := v.Verify(signed(t, "GET", "/v1/vms", "", n1), nil); !errors.Is(err, ErrReplay) {
		t.Errorf("replay at the window's end: %v, want ErrReplay", err)
	}
	other := signedBy(t, "other", otherKey, t0, "GET", "/v1/vms", "", n1)
	if name, err := v.Verify(other, nil); err != nil || name != "other" {
		t.Errorf("same nonce, other client: %q, %v", name, err)
	}
}

// A request dated ahead of the server's clock stays in the window longer,
// and so does its nonce.
func TestVerifyFutureReplay(t *testing.T) {
	v := at(t0)
	ahead := t0.Add(Window)
	if _, err := v.Verify(signedBy(t, "cockpit", testKey, ahead, "GET", "/v1/vms", "", n1), nil); err != nil {
		t.Fatalf("dated a window ahead: %v", err)
	}
	v.now = func() time.Time { return ahead.Add(Window / 2) }
	if _, err := v.Verify(signedBy(t, "cockpit", testKey, ahead, "GET", "/v1/vms", "", n1), nil); !errors.Is(err, ErrReplay) {
		t.Errorf("replay while its timestamp is still in the window: %v, want ErrReplay", err)
	}
}

// The cache of nonces is bounded per client: past the limit, the client's
// nonces whose window ended are dropped, and if none has, its request is
// refused. Other clients aren't affected.
func TestVerifyNonceCacheFull(t *testing.T) {
	v := at(t0)
	v.maxNonces = 2
	nonce := func(i int) string { return "00000000-0000-4000-8000-00000000001" + string(rune('0'+i)) }
	for i := range 2 {
		if _, err := v.Verify(signed(t, "GET", "/v1/vms", "", nonce(i)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.Verify(signed(t, "GET", "/v1/vms", "", nonce(2)), nil); !errors.Is(err, ErrNonceCacheFull) {
		t.Fatalf("third in the window: %v, want ErrNonceCacheFull", err)
	}
	other := signedBy(t, "other", otherKey, t0, "GET", "/v1/vms", "", nonce(2))
	if name, err := v.Verify(other, nil); err != nil || name != "other" {
		t.Errorf("another client while cockpit is over its limit: %q, %v", name, err)
	}
	// A request signed later, once the first two left the window.
	v.now = func() time.Time { return t0.Add(Window + time.Second) }
	later := signedBy(t, "cockpit", testKey, t0.Add(Window+time.Second), "GET", "/v1/vms", "", nonce(3))
	if _, err := v.Verify(later, nil); err != nil {
		t.Errorf("after the window: %v", err)
	}
}

func TestVerifyConcurrent(t *testing.T) {
	v := at(t0)
	reqs := make([]*http.Request, 8)
	for i := range reqs {
		reqs[i] = signed(t, "GET", "/v1/vms", "", n1)
	}
	var wg sync.WaitGroup
	ok := make(chan bool, len(reqs))
	for _, r := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Verify(r, nil)
			ok <- err == nil
		}()
	}
	wg.Wait()
	close(ok)
	n := 0
	for o := range ok {
		if o {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d of 8 copies accepted, want 1", n)
	}
}

func TestValidClientName(t *testing.T) {
	for name, want := range map[string]bool{
		"cockpit": true, "c": true, "cockpit-2": true,
		"": false, "a.b": false, "Cockpit": false, "-a": false, "a-": false, "2a": false,
		strings.Repeat("a", 32): true, strings.Repeat("a", 33): false,
	} {
		if ValidClientName(name) != want {
			t.Errorf("ValidClientName(%q) = %v", name, !want)
		}
	}
}
