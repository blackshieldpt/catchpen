package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// maxConns bounds the TCP connections open at once, authenticated or not,
// and maxConnsPerAddr those from one address (an IPv6 /64), so one host
// can't take them all.
var (
	maxConns        = 64
	maxConnsPerAddr = 16
)

// requestTimeout bounds how long a TCP client has to send a request's body,
// and then to read the answer.
var requestTimeout = 30 * time.Second

// preAuthLifetime bounds a TCP connection's life until a request on it is
// authenticated: whatever a stranger does (slow headers, a slow body, TLS
// messages that make the server write), the socket is closed by then.
var preAuthLifetime = 60 * time.Second

// writeStall bounds each write to a TCP connection, whatever deadline net/http
// asked for: a peer that stops reading can't block a goroutine for good, even
// in a write net/http doesn't know of (crypto/tls answers a TLS 1.3
// KeyUpdate from inside a read).
var writeStall = 30 * time.Second

// Caller is the enrolled client a TCP request came from.
type Caller struct {
	Name   string
	Scopes []string
}

type callerKey struct{}

// CallerOf returns the enrolled client a request came from; false for one
// from this user over the unix socket.
func CallerOf(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// ListenTCP listens on addr for enrolled clients: TLS 1.3 only, with cert,
// and at most maxConns connections at once. With no client enrolled there is
// no one to serve, so it refuses.
func (s *Server) ListenTCP(addr string, cert tls.Certificate) (net.Listener, error) {
	cs, err := LoadClients(s.clients)
	if err != nil {
		return nil, err
	}
	if len(cs.Clients) == 0 {
		return nil, errors.New("no client is enrolled (catchpen daemon client add)")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(newLimitListener(l, maxConns), s.tlsConfig(cert)), nil
}

// tlsConfig takes a client certificate if one is offered, and then only one
// whose key an mtls client has; a client with none must sign its requests
// (HMAC). Go checks that the client holds the certificate's key.
func (s *Server) tlsConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequestClientCert,
		VerifyConnection: func(st tls.ConnectionState) error {
			if len(st.PeerCertificates) == 0 {
				return nil
			}
			cs, err := LoadClients(s.clients)
			if err != nil {
				return err
			}
			if _, ok := cs.byPin(api.PublicKeyPin(st.PeerCertificates[0])); !ok {
				return errors.New("client certificate not enrolled")
			}
			return nil
		},
	}
}

func (cs Clients) byPin(pin string) (Client, bool) {
	for _, c := range cs.Clients {
		if c.Method == MethodMTLS && c.Pin == pin {
			return c, true
		}
	}
	return Client{}, false
}

func (s *Server) hmacKey(name string) ([]byte, bool) {
	cs, err := LoadClients(s.clients)
	if err != nil {
		return nil, false
	}
	c, ok := cs.Get(name)
	if !ok || c.Method != MethodHMAC {
		return nil, false
	}
	k, err := hex.DecodeString(c.Key)
	return k, err == nil
}

// authTCP finds the enrolled client a TLS request came from: by its
// certificate if it has one, else by its signature, read from the clients
// file each time so a removed client is refused at once. It answers a
// request it refuses, closing the connection without reading the rest of
// the request, and returns false.
func (s *Server) authTCP(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(requestTimeout))
	refuse := func(code, msg string) (*http.Request, bool) {
		w.Header().Set("Connection", "close")
		rc.SetReadDeadline(time.Now()) // net/http would otherwise wait for the body
		rc.SetWriteDeadline(time.Now().Add(requestTimeout))
		writeError(w, code, msg)
		return nil, false
	}
	// Before any name is looked at, so the answer says nothing about it.
	if r.ContentLength > api.MaxRequestBody {
		return refuse(api.CodeTooLarge, "request body too large")
	}
	cs, err := LoadClients(s.clients)
	if err != nil {
		return refuse(api.CodeInternal, "the clients file can't be read")
	}
	var c Client
	if certs := r.TLS.PeerCertificates; len(certs) > 0 {
		var ok bool
		if c, ok = cs.byPin(api.PublicKeyPin(certs[0])); !ok {
			return refuse(api.CodeUnauthorized, "not an enrolled client")
		}
	} else {
		if err := s.hmac.Precheck(r); err != nil {
			return refuse(api.CodeUnauthorized, "not an enrolled client")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return refuse(api.CodeUnauthorized, "not an enrolled client")
		}
		name, err := s.hmac.Verify(r, body)
		if errors.Is(err, api.ErrNonceCacheFull) {
			return refuse(api.CodeRateLimited, err.Error())
		}
		var ok bool
		if c, ok = cs.Get(name); err != nil || !ok {
			return refuse(api.CodeUnauthorized, "not an enrolled client")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	rc.SetWriteDeadline(time.Now().Add(requestTimeout))
	if lc, ok := r.Context().Value(limitConnKey{}).(*limitConn); ok {
		lc.authenticated()
	}
	return r.WithContext(context.WithValue(r.Context(), callerKey{}, Caller{Name: c.Name, Scopes: c.Scopes})), true
}

// limitListener accepts at most cap(sem) connections at once, and at most
// maxConnsPerAddr from one address: it closes the others at once. Each
// connection is closed after preAuthLifetime unless authenticated by then.
type limitListener struct {
	net.Listener
	sem     chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	perAddr map[string]int
	conns   map[*limitConn]struct{}
}

func newLimitListener(l net.Listener, n int) *limitListener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n), done: make(chan struct{}),
		perAddr: map[string]int{}, conns: map[*limitConn]struct{}{}}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		select {
		case l.sem <- struct{}{}:
		case <-l.done:
			return nil, net.ErrClosed
		}
		c, err := l.Listener.Accept()
		if err != nil {
			<-l.sem
			return nil, err
		}
		addr := addrKey(c.RemoteAddr())
		l.mu.Lock()
		if l.perAddr[addr] >= maxConnsPerAddr {
			l.mu.Unlock()
			c.Close()
			<-l.sem
			continue
		}
		select {
		case <-l.done: // Close swept the connections before this one came
			l.mu.Unlock()
			c.Close()
			<-l.sem
			return nil, net.ErrClosed
		default:
		}
		l.perAddr[addr]++
		lc := &limitConn{Conn: c}
		lc.timer = time.AfterFunc(preAuthLifetime, func() { c.Close() })
		lc.release = sync.OnceFunc(func() {
			lc.timer.Stop()
			l.mu.Lock()
			if l.perAddr[addr]--; l.perAddr[addr] == 0 {
				delete(l.perAddr, addr)
			}
			delete(l.conns, lc)
			l.mu.Unlock()
			<-l.sem
		})
		l.conns[lc] = struct{}{}
		l.mu.Unlock()
		return lc, nil
	}
}

// addrKey is what the per-address cap counts by: an IPv4 address, or an
// IPv6 /64, which one host usually has whole.
func addrKey(a net.Addr) string {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return "?" // shared, so an address it can't read is still capped
	}
	ip := ta.AddrPort().Addr().Unmap().WithZone("")
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// Close also ends an Accept waiting for a free slot, and closes the
// connections not yet authenticated: the daemon is stopping.
func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	l.mu.Lock()
	for c := range l.conns {
		if c.timer.Stop() {
			c.Conn.Close()
		}
	}
	l.mu.Unlock()
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	release func()
	timer   *time.Timer // closes the socket at preAuthLifetime
	mu      sync.Mutex
	wd      time.Time // the write deadline asked for
}

type limitConnKey struct{}

// authenticated lets the connection outlive preAuthLifetime.
func (c *limitConn) authenticated() { c.timer.Stop() }

// writeDeadline sets the socket's write deadline to the one asked for, or
// writeStall from now if that is sooner.
func (c *limitConn) writeDeadline(t time.Time) error {
	d := time.Now().Add(writeStall)
	if !t.IsZero() && t.Before(d) {
		d = t
	}
	return c.Conn.SetWriteDeadline(d)
}

func (c *limitConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wd = t
	return c.writeDeadline(t)
}

func (c *limitConn) SetDeadline(t time.Time) error {
	if err := c.Conn.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *limitConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	err := c.writeDeadline(c.wd)
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(b)
	if err != nil {
		// A TLS stream is broken by a failed write: close the socket, so the
		// writes crypto/tls may still try (alerts, KeyUpdates) fail at once.
		c.Conn.Close()
	}
	return n, err
}

func (c *limitConn) Close() error {
	c.release()
	return c.Conn.Close()
}
