package cache

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blackshieldpt/curral/internal/canary"
	"github.com/blackshieldpt/curral/internal/egress"
)

const (
	upstreamIdle = 2 * time.Minute // an upstream fetch making no progress is dropped
	roomEvery    = 8 << 20         // how often a download checks the store still has room
	maxTokens    = 256             // registry tokens kept
)

var clientStall = 30 * time.Second // a guest not reading stops being served (the fetch goes on)

// Request headers passed upstream: for stored objects (the representation
// must not vary: no ranges, no compression), for indexes the cache
// rewrites, and for everything else.
var (
	storeHeaders   = []string{"Accept", "User-Agent"}
	rewriteHeaders = []string{"Accept", "User-Agent", "If-Modified-Since", "If-None-Match"}
	passHeaders    = []string{"Accept", "Accept-Encoding", "User-Agent", "Range", "If-Range", "If-Modified-Since", "If-None-Match"}
	respHeaders    = []string{"Content-Type", "Content-Length", "Content-Encoding", "Content-Range", "Accept-Ranges",
		"ETag", "Last-Modified", "Cache-Control", "Vary", "Docker-Content-Digest"}
)

// Server serves one VM's cache connections.
type Server struct {
	Store Store
	Kinds map[string]bool // the caches the VM uses
	Dir   string          // VM directory: egress log and connection slots
	VM    string
	Max   int64 // the store's size limit (cache_max)
	// OnCanary is told about a request carrying one of the VM's canaries
	// (which is refused), before the guest gets its answer.
	OnCanary func(c *canary.Canary, where string)
	// Client fetches upstream (over an *http.Transport); nil: public
	// addresses only, no proxy.
	Client *http.Client

	gzip      http.RoundTripper // Client's transport, asking for gzip (indexes the cache rewrites)
	tokens    map[string]string // registry tokens by repository
	downloads sync.WaitGroup
	canaries  []canary.Canary
	canaryErr error
	loadOnce  sync.Once
}

// ServeConn serves HTTP on a single guest connection until it closes.
func (s *Server) ServeConn(ctx context.Context, c net.Conn) error {
	release, ok := egress.AcquireSlot(s.Dir, "cache.slots")
	if !ok {
		return c.Close()
	}
	defer release()
	l := &oneConn{c: c, done: make(chan struct{})}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	err := srv.Serve(l)
	s.downloads.Wait() // fetches outlive the guests that asked for them
	if !errors.Is(err, errDone) {
		return err
	}
	return nil
}

var (
	errDone   = errors.New("connection closed")
	errDenied = errors.New("denied")
)

// oneConn is a listener yielding c once, then blocking until it's closed.
type oneConn struct {
	c     net.Conn
	taken bool
	done  chan struct{}
	once  sync.Once
}

func (l *oneConn) closed() { l.once.Do(func() { close(l.done) }) }

func (l *oneConn) Accept() (net.Conn, error) {
	if !l.taken {
		l.taken = true
		return &notifyConn{Conn: l.c, l: l}, nil
	}
	<-l.done
	return nil, errDone
}

func (l *oneConn) Close() error   { l.closed(); return nil }
func (l *oneConn) Addr() net.Addr { return l.c.LocalAddr() }

type notifyConn struct {
	net.Conn
	l *oneConn
}

func (c *notifyConn) Close() error {
	err := c.Conn.Close()
	c.l.closed()
	return err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.loadOnce.Do(func() { s.canaries, s.canaryErr = canary.Load(s.Dir) })
	if s.canaryErr != nil { // fail closed: detection can't be skipped by breaking the file
		_ = egress.Append(s.Dir, egress.LogEntry{Time: time.Now().UTC(), VM: s.VM, Method: r.Method, Host: Addr, Port: Port,
			Action: egress.ActionDeny, Reason: "canaries unreadable: " + s.canaryErr.Error()})
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)
		return
	}
	if c := canary.InRequest(s.canaries, r); c != nil {
		if s.OnCanary != nil {
			s.OnCanary(c, "in a package cache request")
		}
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	rt, code := resolve(r, s.Kinds)
	switch {
	case rt == nil:
		http.Error(w, http.StatusText(code), code)
	case rt.local && rt.kind == "go": // sumdb/sum.golang.org/supported
	case rt.local:
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	case rt.store && r.Method == http.MethodGet:
		s.serveStored(w, r, rt)
	case rt.store && s.serveHit(w, r, rt, s.Store.path(rt.kind, rt.key)):
	default:
		s.pass(w, r, rt)
	}
}

// serveHit serves a stored object, if there is one.
func (s *Server) serveHit(w http.ResponseWriter, r *http.Request, rt *route, path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	now := time.Now()
	_ = os.Chtimes(path, now, now) // for trimming; never shown to guests
	m := readMeta(path)
	w.Header().Set("Content-Type", cmp.Or(m.Type, "application/octet-stream"))
	for k, v := range map[string]string{"ETag": m.ETag, "Last-Modified": m.LastModified} {
		if v != "" {
			w.Header().Set(k, v)
		}
	}
	if rt.kind == "docker" {
		w.Header().Set("Docker-Content-Digest", "sha256:"+rt.digest)
	}
	http.ServeContent(stallResponse{w, stallWriter{w, http.NewResponseController(w)}}, r, "", time.Time{}, f)
	return true
}

// serveStored serves an object that never changes, fetching and storing it
// first if needed. One process fetches; others wanting it wait for it. The
// fetch runs on its own, so a guest reading slowly only slows itself.
func (s *Server) serveStored(w http.ResponseWriter, r *http.Request, rt *route) {
	path := s.Store.path(rt.kind, rt.key)
	if s.serveHit(w, r, rt, path) {
		return
	}
	release, err := s.Store.lock(path)
	if err != nil {
		http.Error(w, "cache unavailable", http.StatusInternalServerError)
		return
	}
	if s.serveHit(w, r, rt, path) {
		release()
		return
	}
	// The fetch completes for the store even if the guest goes away.
	resp, err := s.fetch(context.WithoutCancel(r.Context()), r, rt, storeHeaders)
	if err != nil {
		release()
		upstreamError(w, err)
		return
	}
	var tmp, rd *os.File
	enc := strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ","))
	if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 && (enc == "" || strings.EqualFold(enc, "identity")) &&
		s.Store.room(resp.ContentLength) {
		if tmp, err = os.CreateTemp(filepath.Dir(path), tmpPfx+"*"); err == nil {
			if rd, err = os.Open(tmp.Name()); err != nil {
				tmp.Close()
				os.Remove(tmp.Name())
				tmp = nil
			}
		}
	}
	if tmp == nil { // not storable: an error, no length, encoded (the store keeps no Content-Encoding), no room
		release()
		defer resp.Body.Close()
		relay(w, resp, "", "")
		return
	}
	defer rd.Close()
	d := &download{}
	d.cond.L = &d.mu
	s.downloads.Go(func() {
		defer resp.Body.Close()
		d.run(s.Store, resp, tmp, rt, path)
		release() // before trimming: others waiting on the object needn't wait for the walk
		s.Store.trimSometimes(s.Max)
	})
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	var sent int64
	for {
		d.mu.Lock()
		for d.n == sent && !d.done {
			d.cond.Wait()
		}
		n, done := d.n, d.done
		d.mu.Unlock()
		for sent < n { // in slices: clientStall is how long one may take
			k := min(n-sent, 256<<10)
			_ = rc.SetWriteDeadline(time.Now().Add(clientStall))
			if _, err := io.CopyN(w, rd, k); err != nil {
				return // the guest went away or stalled; the fetch goes on
			}
			sent += k
		}
		if done {
			return // short of Content-Length if the fetch failed or the store filled: the guest sees it
		}
	}
}

// download copies an upstream object into a temp file, then commits it if
// complete and matching its digest. It stops once the rest would no longer
// fit in st (downloads in other processes share the free space).
type download struct {
	mu   sync.Mutex
	cond sync.Cond
	n    int64 // bytes in the temp file
	done bool
}

func (d *download) run(st Store, resp *http.Response, tmp *os.File, rt *route, path string) {
	defer os.Remove(tmp.Name()) // nothing left after a commit
	h := sha256.New()
	buf := make([]byte, 256<<10)
	var n int64
	var err error
	next := int64(roomEvery)
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			if _, err = tmp.Write(buf[:k]); err != nil {
				break
			}
			h.Write(buf[:k])
			n += int64(k)
			d.mu.Lock()
			d.n = n
			d.cond.Broadcast()
			d.mu.Unlock()
			if n >= next {
				if !st.room(resp.ContentLength - n) {
					err = errors.New("no room left in the store")
					break
				}
				next += roomEvery
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				err = rerr
			}
			break
		}
	}
	if err == nil && n == resp.ContentLength && (rt.digest == "" || hex.EncodeToString(h.Sum(nil)) == rt.digest) {
		if err = tmp.Sync(); err == nil {
			err = tmp.Close()
		}
		if err == nil {
			_ = commit(tmp.Name(), path, meta{URL: rt.url, Type: resp.Header.Get("Content-Type"),
				ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")})
		}
	}
	tmp.Close()
	d.mu.Lock()
	d.done = true
	d.cond.Broadcast()
	d.mu.Unlock()
}

// pass relays an index (rewriting links to the cache) or anything else not stored.
func (s *Server) pass(w http.ResponseWriter, r *http.Request, rt *route) {
	hdrs := passHeaders
	if rt.from != "" {
		hdrs = rewriteHeaders
	}
	resp, err := s.fetch(r.Context(), r, rt, hdrs)
	if err != nil {
		upstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "" {
		relay(w, resp, "", "") // not rewritable; clients rewrite npm's links themselves
		return
	}
	relay(w, resp, rt.from, rt.to)
}

func upstreamError(w http.ResponseWriter, err error) {
	if errors.Is(err, errDenied) {
		http.Error(w, "denied by the VM's egress denylist", http.StatusForbidden)
		return
	}
	http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
}

func copyHeaders(dst, src http.Header) {
	for _, k := range respHeaders {
		if v := src.Values(k); len(v) > 0 {
			dst[http.CanonicalHeaderKey(k)] = v
		}
	}
}

// relay copies resp to the guest, replacing from with to as it streams.
func relay(w http.ResponseWriter, resp *http.Response, from, to string) {
	copyHeaders(w.Header(), resp.Header)
	sw := stallWriter{w, http.NewResponseController(w)}
	if from == "" {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(sw, resp.Body)
		return
	}
	w.Header().Del("Content-Length") // the body changes length
	w.WriteHeader(resp.StatusCode)
	_ = replace(sw, resp.Body, []byte(from), []byte(to))
}

// stallWriter gives each write to the guest clientStall to complete.
type stallWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (s stallWriter) Write(p []byte) (int, error) {
	_ = s.rc.SetWriteDeadline(time.Now().Add(clientStall))
	return s.w.Write(p)
}

// stallResponse writes through a stallWriter; it hides the ResponseWriter's
// ReadFrom, so a copy is made in small writes, each with its deadline.
type stallResponse struct {
	http.ResponseWriter
	sw stallWriter
}

func (s stallResponse) Write(p []byte) (int, error) { return s.sw.Write(p) }

// replace copies src to dst with every from replaced by to, holding back
// only what may be the start of a from split across reads.
func replace(dst io.Writer, src io.Reader, from, to []byte) error {
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := src.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := bytes.Index(buf, from)
			if i < 0 {
				break
			}
			if _, err := dst.Write(buf[:i]); err != nil {
				return err
			}
			if _, err := dst.Write(to); err != nil {
				return err
			}
			buf = buf[i+len(from):]
		}
		keep := min(len(buf), len(from)-1)
		if rerr != nil {
			keep = 0
		}
		if _, err := dst.Write(buf[:len(buf)-keep]); err != nil {
			return err
		}
		buf = append(buf[:0], buf[len(buf)-keep:]...)
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// fetch sends r upstream, logging it, with Docker Hub's anonymous token when
// the registry asks for one. The body is closed by the caller; a fetch
// making no progress for upstreamIdle is cancelled.
func (s *Server) fetch(ctx context.Context, r *http.Request, rt *route, hdrs []string) (*http.Response, error) {
	u, err := url.Parse(rt.url)
	if err != nil {
		return nil, err
	}
	if !s.allowed(r.Method, rt.kind, u) {
		return nil, errDenied
	}
	repo := ""
	if m := registryRe.FindStringSubmatch(u.Path); rt.kind == "docker" && m != nil {
		repo = m[1]
	}
	for attempt := 0; ; attempt++ {
		rctx, cancel := context.WithCancel(ctx)
		t := time.AfterFunc(upstreamIdle, cancel)
		req, err := http.NewRequestWithContext(rctx, r.Method, rt.url, nil)
		if err != nil {
			t.Stop()
			cancel()
			return nil, err
		}
		for _, k := range hdrs {
			for _, v := range r.Header.Values(k) {
				req.Header.Add(k, v)
			}
		}
		if tok := s.tokens[repo]; repo != "" && tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := s.client(rt.from != "").Do(req)
		if err != nil {
			t.Stop()
			cancel()
			return nil, err
		}
		resp.Body = &watchedBody{ReadCloser: resp.Body, t: t, cancel: cancel}
		if repo == "" || resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		challenge := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()
		tok, err := s.token(ctx, challenge)
		if err != nil {
			return nil, fmt.Errorf("registry token: %w", err)
		}
		if s.tokens == nil {
			s.tokens = map[string]string{}
		}
		if len(s.tokens) >= maxTokens { // repositories are the guest's to name
			clear(s.tokens)
		}
		s.tokens[repo] = tok
	}
}

type watchedBody struct {
	io.ReadCloser
	t      *time.Timer
	cancel context.CancelFunc
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.t.Reset(upstreamIdle)
	return n, err
}

func (b *watchedBody) Close() error {
	b.t.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

var challengeRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// token gets an anonymous pull token for a Bearer challenge.
func (s *Server) token(ctx context.Context, challenge string) (string, error) {
	params := map[string]string{}
	for _, m := range challengeRe.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	u, err := url.Parse(params["realm"])
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("unexpected challenge %q", challenge)
	}
	q := u.Query()
	for _, k := range []string{"service", "scope"} {
		if v := params[k]; v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	if !s.allowed(http.MethodGet, "docker token", u) {
		return "", errDenied
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamIdle)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client(false).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", u.Host, resp.Status)
	}
	var t struct{ Token, AccessToken string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", err
	}
	return cmp.Or(t.Token, t.AccessToken), nil
}

// allowed logs a fetch of u (what: the cache kind, or why it's fetched) in
// the VM's egress log, refusing it when curral egress deny names its host.
// (The allowlist doesn't apply: cached hosts need no entry.)
func (s *Server) allowed(method, what string, u *url.URL) bool {
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = map[string]int{"http": 80, "https": 443}[u.Scheme]
	}
	action := egress.ActionCache
	if deny, err := egress.LoadList(filepath.Join(s.Dir, egress.DenylistFile)); err != nil || egress.Match(deny, u.Hostname(), port) {
		action = egress.ActionDeny
	}
	p := u.EscapedPath()
	if len(p) > 200 {
		p = p[:200] + "..."
	}
	_ = egress.Append(s.Dir, egress.LogEntry{Time: time.Now().UTC(), VM: s.VM, Method: method, Host: u.Hostname(), Port: port,
		Action: action, Reason: what + " " + p})
	return action == egress.ActionCache
}

// client is the upstream client: redirects (to CDNs) are followed over
// HTTPS only, at most 10, each logged and checked against the denylist.
// With gzip, Go asks for gzip and decodes it (the body is identity, its
// Content-Encoding removed); stored objects and pass-through keep theirs.
func (s *Server) client(gzip bool) *http.Client {
	if s.Client == nil {
		s.Client = &http.Client{Transport: &http.Transport{
			DialContext: dialPublic, ForceAttemptHTTP2: true, DisableCompression: true,
			TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: upstreamIdle, MaxIdleConnsPerHost: 4,
		}}
	}
	c := *s.Client
	if gzip {
		if s.gzip == nil {
			t := s.Client.Transport.(*http.Transport).Clone()
			t.DisableCompression = false
			s.gzip = t
		}
		c.Transport = s.gzip
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		switch {
		case len(via) >= 10:
			return errors.New("stopped after 10 redirects")
		case req.URL.Scheme != "https":
			return fmt.Errorf("redirect to %s:// refused", req.URL.Scheme)
		case !s.allowed(req.Method, "redirect", req.URL):
			return errDenied
		}
		return nil
	}
	return &c
}

// dialPublic dials a host's public addresses only (redirects included).
func dialPublic(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if egress.IsPrivate(ip) {
			return nil, fmt.Errorf("%s resolves to private address %s", host, ip)
		}
	}
	d := net.Dialer{Timeout: 30 * time.Second}
	var lastErr error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, cmp.Or(lastErr, fmt.Errorf("%s: no addresses", host))
}
