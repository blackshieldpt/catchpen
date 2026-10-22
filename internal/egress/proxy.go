package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/blackshieldpt/curral/internal/canary"
)

const (
	requestTimeout = 30 * time.Second
	dialTimeout    = 15 * time.Second
	maxHeaderBytes = 64 << 10
	idleTimeout    = 10 * time.Minute
	// MaxConns caps a VM's concurrent processes of one kind (QEMU spawns one
	// per connection): proxy and package cache processes each have their own.
	MaxConns = 128
)

// Proxy serves one guest connection per ServeConn call.
type Proxy struct {
	Dir     string
	VM      string
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	Now     func() time.Time
	// Idle overrides idleTimeout (tests).
	Idle time.Duration
	// OnCanary is told about a request using one of the VM's canaries
	// (which is denied), before the guest gets its answer.
	OnCanary func(c *canary.Canary, where string)
}

func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Proxy) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if p.Resolve != nil {
		return p.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

func (p *Proxy) dial(ctx context.Context, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	if p.Dial != nil {
		return p.Dial(ctx, "tcp", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// ServeConn handles a single proxied request (CONNECT or absolute-URI HTTP).
func (p *Proxy) ServeConn(ctx context.Context, c io.ReadWriteCloser) error {
	defer c.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); c.Close() }()

	release, ok := AcquireSlot(p.Dir, "egress.slots")
	if !ok {
		writeStatus(c, http.StatusServiceUnavailable, "curral: too many concurrent proxy connections\n")
		return errors.New("connection limit reached")
	}
	defer release()

	// The header phase is size-limited; the limit is lifted for bodies and tunnels.
	lr := &io.LimitedReader{R: c, N: maxHeaderBytes}
	br := bufio.NewReader(lr)
	timer := time.AfterFunc(requestTimeout, func() { c.Close() })
	req, err := http.ReadRequest(br)
	if !timer.Stop() {
		return errors.New("timed out reading request")
	}
	if err != nil {
		writeStatus(c, http.StatusBadRequest, "curral: malformed or oversized proxy request\n")
		return err
	}
	lr.N = 1<<63 - 1

	host, port, err := target(req)
	if errors.Is(err, errMethod) {
		writeStatus(c, http.StatusMethodNotAllowed, "curral: "+err.Error()+"\n")
		return nil
	}
	if err != nil {
		writeStatus(c, http.StatusBadRequest, "curral: "+err.Error()+"\n")
		return nil
	}
	entry := LogEntry{VM: p.VM, Method: req.Method, Host: host, Port: port}
	hit, where, err := p.canary(req, host, port)
	if err != nil { // fail closed: detection can't be skipped by breaking the file
		_ = p.log(entry, ActionDeny, "canaries unreadable: "+err.Error())
		p.deny(c, host, port, "not allowed")
		return nil
	}
	if hit != nil {
		_ = p.log(entry, ActionDeny, "canary ("+hit.Kind+")")
		if p.OnCanary != nil {
			p.OnCanary(hit, where)
		}
		p.deny(c, host, port, "not allowed")
		return nil
	}

	allowed, action, reason := p.decide(ctx, host, port, req.Method)
	if !allowed {
		_ = p.log(entry, action, reason)
		p.deny(c, host, port, reason)
		return nil
	}
	up, err := p.connect(ctx, host, port)
	if err != nil {
		var pe privateError
		if errors.As(err, &pe) {
			_ = p.log(entry, ActionDeny, pe.Error())
			p.deny(c, host, port, pe.Error())
			return nil
		}
		_ = p.log(entry, ActionError, err.Error())
		writeStatus(c, http.StatusBadGateway, fmt.Sprintf("curral: cannot reach %s: %v\n", hostPort(host, port), err))
		return nil
	}
	defer up.Close()
	// No audit record, no connection.
	if err := p.log(entry, action, reason); err != nil {
		writeStatus(c, http.StatusServiceUnavailable, "curral: egress log unavailable; refusing connection\n")
		return fmt.Errorf("egress log: %w", err)
	}
	idle := p.Idle
	if idle == 0 {
		idle = idleTimeout
	}
	last := new(atomic.Int64)
	stopWatch := watchIdle(idle, last, c, up)
	defer stopWatch()

	if req.Method == http.MethodConnect {
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return err
		}
		tunnel(c, br, up, last)
		return nil
	}
	return forward(c, req, up, last)
}

// canary finds one of the VM's canaries in a request: its host, or a
// value in the request line or headers (a body over plain HTTP isn't read).
func (p *Proxy) canary(req *http.Request, host string, port int) (*canary.Canary, string, error) {
	cs, err := canary.Load(p.Dir)
	if err != nil || len(cs) == 0 {
		return nil, "", err
	}
	if c := canary.Host(cs, host); c != nil {
		return c, "connection to " + hostPort(host, port), nil
	}
	if c := canary.InRequest(cs, req); c != nil {
		return c, "in a request to " + hostPort(host, port), nil
	}
	return nil, "", nil
}

var errMethod = errors.New("method not allowed")

var allowedMethods = map[string]bool{
	http.MethodConnect: true, http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// target extracts host and port from a CONNECT or absolute-URI request.
func target(req *http.Request) (string, int, error) {
	if !allowedMethods[req.Method] {
		return "", 0, errMethod
	}
	if req.Method == http.MethodConnect {
		h, ps, err := net.SplitHostPort(req.RequestURI)
		if err != nil {
			return "", 0, fmt.Errorf("invalid CONNECT target %q", req.RequestURI)
		}
		port, err := parsePort(ps)
		if err != nil {
			return "", 0, err
		}
		return checkHost(h, port)
	}
	if !req.URL.IsAbs() || req.URL.Scheme != "http" || req.URL.Hostname() == "" {
		return "", 0, errors.New("only CONNECT and absolute http:// requests are supported")
	}
	port := 80
	if ps := req.URL.Port(); ps != "" {
		var err error
		if port, err = parsePort(ps); err != nil {
			return "", 0, err
		}
	}
	return checkHost(req.URL.Hostname(), port)
}

// checkHost accepts only DNS names and IP literals, so odd bytes in the target
// can't reach matching, logs or prompts.
func checkHost(h string, port int) (string, int, error) {
	n := normalizeHost(h)
	if net.ParseIP(n) == nil && !validHostname(n) {
		return "", 0, fmt.Errorf("invalid target host %q", h)
	}
	return n, port, nil
}

// AcquireSlot takes one of MaxConns flock slots in dir/name; the lock is
// released on return or when the process exits.
func AcquireSlot(dir, name string) (func(), bool) {
	sd := filepath.Join(dir, name)
	if err := os.MkdirAll(sd, 0o700); err != nil {
		return nil, false
	}
	start := os.Getpid() % MaxConns
	for i := 0; i < MaxConns; i++ {
		f, err := os.OpenFile(filepath.Join(sd, strconv.Itoa((start+i)%MaxConns)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, false
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return func() { f.Close() }, true
		}
		f.Close()
	}
	return nil, false
}

// activity counts bytes moved through a tunnel or forward.
type activity struct {
	io.Writer
	last *atomic.Int64
}

func (a activity) Write(b []byte) (int, error) {
	a.last.Store(time.Now().UnixNano())
	return a.Writer.Write(b)
}

// watchIdle closes conns when no bytes moved (per last) for d.
func watchIdle(d time.Duration, last *atomic.Int64, conns ...io.Closer) func() {
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(min(d/4, 30*time.Second))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) > d {
					for _, c := range conns {
						c.Close()
					}
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// decide applies denylist, allowlist, then the on_deny policy.
func (p *Proxy) decide(ctx context.Context, host string, port int, method string) (bool, string, string) {
	cfg, err := ReadConfig(p.Dir)
	if err != nil {
		return false, ActionError, "config: " + err.Error()
	}
	deny, err := LoadList(filepath.Join(p.Dir, DenylistFile))
	if err != nil {
		return false, ActionError, "denylist: " + err.Error()
	}
	if Match(deny, host, port) {
		return false, ActionDeny, "denylisted"
	}
	if cfg.AllowAll {
		return true, ActionAllow, "allow_all"
	}
	allow, err := LoadList(filepath.Join(p.Dir, AllowlistFile))
	if err != nil {
		return false, ActionError, "allowlist: " + err.Error()
	}
	if Match(allow, host, port) {
		return true, ActionAllow, ""
	}
	if cfg.OnDeny != OnDenyPrompt {
		return false, ActionDeny, "not in allowlist"
	}
	d, note, err := p.ask(ctx, cfg.PromptTimeout, Request{VM: p.VM, Host: host, Port: port, Method: method})
	if err != nil {
		return false, ActionPromptDeny, "prompt unavailable: " + err.Error()
	}
	reason := string(d)
	if note != "" {
		reason += " (" + note + ")"
	}
	if d.allows() {
		return true, ActionPromptAllow, reason
	}
	return false, ActionPromptDeny, reason
}

type promptRequest struct {
	VM       string `json:"vm"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Method   string `json:"method"`
	Deadline int64  `json:"deadline"` // unix nanoseconds; the proxy denies after this
}

type promptReply struct {
	Decision Decision `json:"decision"`
	Note     string   `json:"note,omitempty"` // e.g. why the watcher denied without asking
}

// ask queries the watcher on the VM's socket; any failure is an error (deny).
func (p *Proxy) ask(ctx context.Context, timeout time.Duration, r Request) (Decision, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", filepath.Join(p.Dir, SocketFile))
	if err != nil {
		return "", "", errors.New("no watcher")
	}
	defer conn.Close()
	dl, _ := ctx.Deadline()
	conn.SetDeadline(dl)
	if err := json.NewEncoder(conn).Encode(promptRequest{r.VM, r.Host, r.Port, r.Method, dl.UnixNano()}); err != nil {
		return "", "", err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadBytes('\n')
	if err != nil {
		// The watcher gives up at the same deadline, so its hang-up can arrive
		// just before our own timeout fires: both mean nobody answered in time.
		var ne net.Error
		if (errors.As(err, &ne) && ne.Timeout()) || time.Until(dl) < 100*time.Millisecond {
			return "", "", errors.New("timeout")
		}
		return "", "", err
	}
	var rep promptReply
	if err := json.Unmarshal(line, &rep); err != nil {
		return "", "", err
	}
	if !rep.Decision.valid() {
		return "", "", fmt.Errorf("invalid decision %q", rep.Decision)
	}
	return rep.Decision, rep.Note, nil
}

type privateError struct{ ip net.IP }

func (e privateError) Error() string { return "private address " + e.ip.String() }

var reservedNets = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "10.0.2.0/24", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "::/96"} {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}()

// IsPrivate reports addresses no fetch on a guest's behalf may reach.
func IsPrivate(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range reservedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// connect resolves host, rejects private addresses (unless allowed) and dials
// the checked IPs directly so a second DNS lookup can't rebind.
func (p *Proxy) connect(ctx context.Context, host string, port int) (net.Conn, error) {
	cfg, err := ReadConfig(p.Dir)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if ips, err = p.resolve(ctx, host); err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	if len(ips) == 0 {
		return nil, errors.New("resolve: no addresses")
	}
	if !cfg.AllowPrivate {
		for _, ip := range ips {
			if IsPrivate(ip) {
				return nil, privateError{ip}
			}
		}
	}
	var lastErr error
	for _, ip := range ips {
		c, err := p.dial(ctx, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (p *Proxy) log(e LogEntry, action, reason string) error {
	e.Time, e.Action, e.Reason = p.now().UTC(), action, reason
	return appendLog(p.Dir, e)
}

func (p *Proxy) deny(c io.Writer, host string, port int, reason string) {
	writeStatus(c, http.StatusForbidden,
		fmt.Sprintf("curral: egress to %s blocked (%s); see: curral egress log %s\n", hostPort(host, port), reason, p.VM))
}

func hostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

func writeStatus(w io.Writer, code int, body string) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
}

type closeWriter interface{ CloseWrite() error }

func closeWrite(w any) bool {
	if cw, ok := w.(closeWriter); ok {
		return cw.CloseWrite() == nil
	}
	return false
}

// tunnel copies bytes both ways; br carries anything already buffered from
// the client. A side that can't half-close ends the whole tunnel.
func tunnel(c io.ReadWriteCloser, br *bufio.Reader, up net.Conn, last *atomic.Int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(activity{up, last}, br)
		if !closeWrite(up) {
			up.Close()
			c.Close()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(activity{c, last}, up)
		if !closeWrite(c) {
			up.Close()
			c.Close()
		}
	}()
	wg.Wait()
}

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// forward sends one request upstream in origin form with Connection: close
// and streams the raw response back.
func forward(c io.Writer, req *http.Request, up net.Conn, last *atomic.Int64) error {
	for _, f := range req.Header.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			req.Header.Del(strings.TrimSpace(name))
		}
	}
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	for name := range req.Header {
		if strings.HasPrefix(name, "Proxy-") {
			req.Header.Del(name)
		}
	}
	req.Close = true
	req.RequestURI = ""
	// The writer goroutine ends once ServeConn closes the client and upstream.
	go req.Write(activity{up, last})
	_, err := io.Copy(activity{c, last}, up)
	return err
}
