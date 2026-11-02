package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// HMAC version 2 headers. The names and the "<client>.<hex>" hash come from
// Blueprint's hmacprovider; version 2 also signs the method, the request
// target and the body's hash, so a signature only authorises its own request.
const (
	HeaderHash      = "X-HMAC-Hash"      // <client name>.<hex HMAC-SHA256>
	HeaderTimestamp = "X-HMAC-Timestamp" // RFC 3339, UTC
	HeaderNonce     = "X-HMAC-Nonce"     // a random UUID
	HeaderVersion   = "X-HMAC-Version"   // HMACVersion
	HMACVersion     = "2"
)

// Window is how far a request's timestamp may be from the server's clock. A
// nonce is remembered for as long as its timestamp is in the window.
const Window = 60 * time.Second

// MinKeyBytes is the shortest key Verify accepts. A client's key is 32
// random bytes.
const MinKeyBytes = 32

// Why Verify refused a request. A server answers all but ErrNonceCacheFull
// the same way (CodeUnauthorized), so they tell a client nothing; they are
// for its logs.
var (
	ErrNoSignature    = errors.New("no HMAC version 2 signature")
	ErrUnknownClient  = errors.New("unknown client")
	ErrStale          = errors.New("timestamp outside the window")
	ErrBadSignature   = errors.New("bad signature")
	ErrReplay         = errors.New("nonce already used")
	ErrNonceCacheFull = errors.New("too many requests from this client in the window")
)

var (
	clientNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$|^[a-z]$`)
	nonceRe      = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// ValidClientName reports whether name can name an enrolled client: the same
// rule as VM names, so it never holds the "." that ends it in HeaderHash.
func ValidClientName(name string) bool { return clientNameRe.MatchString(name) }

// Canonical is what a version 2 signature covers. target is the request
// target as sent on the request line: the path and query, escaped as the
// client wrote them.
func Canonical(timestamp, nonce, method, target string, body []byte) string {
	sum := sha256.Sum256(body)
	return timestamp + ":" + nonce + ":" + method + ":" + target + ":" + hex.EncodeToString(sum[:])
}

// MAC is the hex HMAC-SHA256 of canonical under key.
func MAC(key []byte, canonical string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(canonical))
	return hex.EncodeToString(m.Sum(nil))
}

// target is r's request target: the request line as received on a server,
// the URL's path and query (what the transport sends) on a client.
func target(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

// Sign sets r's version 2 headers for client name, whose key is key. body is
// r's whole body (nil: none).
func Sign(r *http.Request, name string, key, body []byte) error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	b[6] = b[6]&0x0f | 0x40 // a version 4 UUID
	b[8] = b[8]&0x3f | 0x80
	nonce := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	return sign(r, name, key, body, time.Now(), nonce)
}

func sign(r *http.Request, name string, key, body []byte, now time.Time, nonce string) error {
	if !ValidClientName(name) {
		return fmt.Errorf("invalid client name %q", name)
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet // what the transport sends
	}
	ts := now.UTC().Format(time.RFC3339)
	r.Header.Set(HeaderTimestamp, ts)
	r.Header.Set(HeaderNonce, nonce)
	r.Header.Set(HeaderVersion, HMACVersion)
	r.Header.Set(HeaderHash, name+"."+MAC(key, Canonical(ts, nonce, method, target(r), body)))
	return nil
}

// Verifier checks version 2 signatures, and remembers the nonces it has
// accepted, up to a number per client within the window. It is safe for
// concurrent use if key is.
type Verifier struct {
	key       func(name string) ([]byte, bool)
	now       func() time.Time
	maxNonces int // per client

	mu   sync.Mutex
	seen map[string]map[string]time.Time // client name, nonce: the last moment its timestamp is in the window
}

// NewVerifier returns a Verifier that finds a client's key with key. A key
// shorter than MinKeyBytes counts as no key.
func NewVerifier(key func(name string) ([]byte, bool)) *Verifier {
	return &Verifier{key: key, now: time.Now, maxNonces: 1 << 13, seen: map[string]map[string]time.Time{}}
}

// SetNonceLimit sets how many nonces a client may use in a window (default
// 8192); past it, Verify returns ErrNonceCacheFull. With n <= 0 it returns
// that for every request.
func (v *Verifier) SetNonceLimit(n int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.maxNonces = n
}

// signature is what a request's headers say.
type signature struct {
	name, mac, ts, nonce string
	t                    time.Time
	key                  []byte
}

// Precheck checks what Verify can without the body: the headers' form, that
// the client is known, and the timestamp. A server calls it before reading
// the body, so a request that can't be valid costs no read.
func (v *Verifier) Precheck(r *http.Request) error {
	_, err := v.precheck(r, v.now())
	return err
}

func (v *Verifier) precheck(r *http.Request, now time.Time) (signature, error) {
	var s signature
	hash := r.Header.Get(HeaderHash)
	s.ts, s.nonce = r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderNonce)
	if r.Header.Get(HeaderVersion) != HMACVersion || hash == "" || s.ts == "" || s.nonce == "" {
		return s, ErrNoSignature
	}
	var ok bool
	s.name, s.mac, ok = strings.Cut(hash, ".")
	if !ok || !ValidClientName(s.name) || !nonceRe.MatchString(s.nonce) {
		return s, ErrBadSignature
	}
	if s.key, ok = v.key(s.name); !ok || len(s.key) < MinKeyBytes {
		return s, ErrUnknownClient
	}
	var err error
	if s.t, err = time.Parse(time.RFC3339, s.ts); err != nil {
		return s, ErrBadSignature
	}
	if d := now.Sub(s.t); d > Window || d < -Window {
		return s, ErrStale
	}
	return s, nil
}

// Verify checks r's signature against body, r's whole body, and returns the
// client's name. A nonce is remembered only once the signature checks out,
// so requests that fail can't fill the cache, and a client that sends too
// many in the window is refused without affecting the others.
func (v *Verifier) Verify(r *http.Request, body []byte) (string, error) {
	now := v.now()
	s, err := v.precheck(r, now)
	if err != nil {
		return "", err
	}
	want, _ := hex.DecodeString(MAC(s.key, Canonical(s.ts, s.nonce, r.Method, target(r), body)))
	got, err := hex.DecodeString(s.mac)
	if err != nil || !hmac.Equal(got, want) {
		return "", ErrBadSignature
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	seen := v.seen[s.name]
	if seen == nil {
		seen = map[string]time.Time{}
		v.seen[s.name] = seen
	}
	if until, ok := seen[s.nonce]; ok && !now.After(until) {
		return "", ErrReplay
	}
	if len(seen) >= v.maxNonces {
		for k, until := range seen {
			if now.After(until) {
				delete(seen, k)
			}
		}
		if len(seen) >= v.maxNonces {
			return "", ErrNonceCacheFull
		}
	}
	seen[s.nonce] = s.t.Add(Window)
	return s.name, nil
}
