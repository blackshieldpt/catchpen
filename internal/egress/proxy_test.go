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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/canary"
)

func setup(t *testing.T, cfg Config, allow, deny []string) *Proxy {
	t.Helper()
	dir := t.TempDir()
	if err := WriteConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := WriteList(dir, AllowlistFile, allow); err != nil {
		t.Fatal(err)
	}
	if err := WriteList(dir, DenylistFile, deny); err != nil {
		t.Fatal(err)
	}
	return &Proxy{Dir: dir, VM: "vm1"}
}

// serve runs ServeConn on one end of a pipe and returns the client end.
func serve(t *testing.T, p *Proxy) (net.Conn, <-chan error) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- p.ServeConn(context.Background(), server) }()
	t.Cleanup(func() { client.Close() })
	return client, done
}

func echoServer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

func readLog(t *testing.T, dir string) []LogEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, LogFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []LogEntry
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e LogEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("bad log line %q: %v", l, err)
		}
		out = append(out, e)
	}
	return out
}

func connect(t *testing.T, c net.Conn, target string, extra string) (*bufio.Reader, *http.Response) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	go fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n%s", target, target, extra)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return br, resp
}

func TestConnectTunnelWithPipelinedBytes(t *testing.T) {
	host, port := echoServer(t)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	p := setup(t, Config{AllowPrivate: true}, []string{target}, nil)
	c, done := serve(t, p)

	br, resp := connect(t, c, target, "PIPELINED")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	buf := make([]byte, len("PIPELINED"))
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "PIPELINED" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	go c.Write([]byte("more"))
	buf = make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "more" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	c.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	logs := readLog(t, p.Dir)
	if len(logs) != 1 || logs[0].Action != ActionAllow || logs[0].Method != "CONNECT" || logs[0].Port != port || logs[0].VM != "vm1" {
		t.Fatalf("log = %+v", logs)
	}
}

func TestAbsoluteURIForward(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	u := srv.Listener.Addr().String()
	p := setup(t, Config{AllowPrivate: true}, []string{u}, nil)
	c, _ := serve(t, p)
	c.SetDeadline(time.Now().Add(5 * time.Second))

	go fmt.Fprintf(c, "GET http://%s/path?q=1 HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: secret\r\nProxy-Connection: keep-alive\r\nX-Drop: 1\r\nConnection: X-Drop\r\nX-Keep: 1\r\n\r\n", u, u)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("resp = %d %q", resp.StatusCode, body)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("connection not closed after one request: %v", err)
	}
	if got.RequestURI != "/path?q=1" || !got.Close || got.Header.Get("Proxy-Authorization") != "" ||
		got.Header.Get("Proxy-Connection") != "" || got.Header.Get("X-Drop") != "" || got.Header.Get("X-Keep") != "1" {
		t.Fatalf("upstream saw %s close=%v headers=%v", got.RequestURI, got.Close, got.Header)
	}
}

func expect403(t *testing.T, c net.Conn, target, reason string) {
	t.Helper()
	_, resp := connect(t, c, target, "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 || !strings.Contains(string(body), reason) || !strings.Contains(string(body), "curral egress log vm1") {
		t.Fatalf("resp = %d %q, want 403 with %q", resp.StatusCode, body, reason)
	}
}

func TestDenyNotAllowlisted(t *testing.T) {
	p := setup(t, Config{}, []string{"other.com"}, nil)
	c, _ := serve(t, p)
	expect403(t, c, "example.com:443", "not in allowlist")
	logs := readLog(t, p.Dir)
	if len(logs) != 1 || logs[0].Action != ActionDeny || logs[0].Host != "example.com" || logs[0].Port != 443 {
		t.Fatalf("log = %+v", logs)
	}
}

func TestDenylistWins(t *testing.T) {
	p := setup(t, Config{}, []string{"example.com"}, []string{"example.com:443"})
	c, _ := serve(t, p)
	expect403(t, c, "example.com:443", "denylisted")
}

func TestOriginFormRejected(t *testing.T) {
	p := setup(t, Config{}, nil, nil)
	c, _ := serve(t, p)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	go io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 400 {
		t.Fatalf("resp = %v, %v", resp, err)
	}
}

func TestPrivateResolutionDenied(t *testing.T) {
	p := setup(t, Config{}, []string{"example.com"}, nil)
	p.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.215.14"), net.ParseIP("10.1.2.3")}, nil
	}
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		t.Errorf("dialed %s despite private address", addr)
		return nil, errors.New("no")
	}
	c, _ := serve(t, p)
	expect403(t, c, "example.com:443", "private address 10.1.2.3")
}

func TestPrivateIPLiteralDenied(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.2.2", "169.254.169.254", "100.64.0.1", "[::1]", "[fd00::1]", "0.0.0.0"} {
		p := setup(t, Config{}, []string{ip + ":443"}, nil)
		c, _ := serve(t, p)
		expect403(t, c, ip+":443", "private address")
	}
}

func TestDialsCheckedIP(t *testing.T) {
	p := setup(t, Config{}, []string{"example.com"}, nil)
	p.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.215.14")}, nil
	}
	var dialed string
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = addr
		a, b := net.Pipe()
		go func() { io.Copy(b, b); b.Close() }()
		return a, nil
	}
	c, _ := serve(t, p)
	_, resp := connect(t, c, "example.com:443", "")
	if resp.StatusCode != 200 || dialed != "93.184.215.14:443" {
		t.Fatalf("status=%d dialed=%q", resp.StatusCode, dialed)
	}
}

func startWatch(t *testing.T, dir string, decide Decider) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Watch(ctx, map[string]string{"vm1": dir}, decide); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	sock := filepath.Join(dir, SocketFile)
	for i := 0; i < 200; i++ {
		if fi, err := os.Stat(sock); err == nil && fi.Mode().Perm() == 0o600 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watcher socket not ready")
}

func TestPromptAllowOnce(t *testing.T) {
	host, port := echoServer(t)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	p := setup(t, Config{OnDeny: OnDenyPrompt, PromptTimeout: 2 * time.Second, AllowPrivate: true}, nil, nil)
	var seen Request
	startWatch(t, p.Dir, func(ctx context.Context, r Request) Decision { seen = r; return DecisionAllowOnce })

	c, _ := serve(t, p)
	if _, resp := connect(t, c, target, ""); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if seen.VM != "vm1" || seen.Host != host || seen.Port != port || seen.Method != "CONNECT" {
		t.Fatalf("decider saw %+v", seen)
	}
	if rules, _ := LoadList(filepath.Join(p.Dir, AllowlistFile)); len(rules) != 0 {
		t.Fatalf("allow-once modified allowlist: %v", rules)
	}
}

func TestPromptAllowAlwaysPersists(t *testing.T) {
	host, port := echoServer(t)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	p := setup(t, Config{OnDeny: OnDenyPrompt, PromptTimeout: 2 * time.Second, AllowPrivate: true}, nil, nil)
	var calls atomic.Int32
	startWatch(t, p.Dir, func(ctx context.Context, r Request) Decision { calls.Add(1); return DecisionAllowAlways })

	for i := 0; i < 2; i++ {
		c, _ := serve(t, p)
		if _, resp := connect(t, c, target, ""); resp.StatusCode != 200 {
			t.Fatalf("attempt %d status = %d", i, resp.StatusCode)
		}
		c.Close()
	}
	if calls.Load() != 1 {
		t.Fatalf("decide called %d times, want 1 (second attempt should hit allowlist)", calls.Load())
	}
	rules, _ := LoadList(filepath.Join(p.Dir, AllowlistFile))
	if !Match(rules, host, port) {
		t.Fatalf("allowlist not updated: %v", rules)
	}
}

func TestPromptDenyAlwaysPersists(t *testing.T) {
	p := setup(t, Config{OnDeny: OnDenyPrompt, PromptTimeout: 2 * time.Second}, nil, nil)
	startWatch(t, p.Dir, func(ctx context.Context, r Request) Decision { return DecisionDenyAlways })
	c, _ := serve(t, p)
	expect403(t, c, "example.com:443", "deny-always")
	rules, _ := LoadList(filepath.Join(p.Dir, DenylistFile))
	if !Match(rules, "example.com", 443) {
		t.Fatalf("denylist not updated: %v", rules)
	}
}

func TestPromptTimeoutDenies(t *testing.T) {
	p := setup(t, Config{OnDeny: OnDenyPrompt, PromptTimeout: 200 * time.Millisecond}, nil, nil)
	startWatch(t, p.Dir, func(ctx context.Context, r Request) Decision { <-ctx.Done(); return DecisionAllowOnce })
	start := time.Now()
	c, _ := serve(t, p)
	expect403(t, c, "example.com:443", "timeout")
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not honoured")
	}
}

func TestPromptNoWatcherDenies(t *testing.T) {
	p := setup(t, Config{OnDeny: OnDenyPrompt, PromptTimeout: 5 * time.Second}, nil, nil)
	c, _ := serve(t, p)
	expect403(t, c, "example.com:443", "no watcher")
	logs := readLog(t, p.Dir)
	if logs[0].Action != ActionPromptDeny {
		t.Fatalf("log = %+v", logs)
	}
}

func TestTailLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LogFile)
	ts := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	appendLog(dir, LogEntry{Time: ts, VM: "vm1", Method: "CONNECT", Host: "example.com", Port: 443, Action: ActionDeny, Reason: "not in allowlist"})
	appendLog(dir, LogEntry{Time: ts, VM: "vm1", Method: "GET", Host: "a.org", Port: 80, Action: ActionAllow})
	var out strings.Builder
	if err := TailLog(context.Background(), path, false, &out); err != nil {
		t.Fatal(err)
	}
	want := "2026-10-19T09:00:00Z DENY  vm1  CONNECT example.com:443  not in allowlist\n" +
		"2026-10-19T09:00:00Z ALLOW vm1  GET a.org:80\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestTailLogFollow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LogFile)
	entry := func(host string) LogEntry {
		return LogEntry{Time: time.Now(), VM: "vm1", Method: "GET", Host: host, Port: 80, Action: ActionAllow}
	}
	appendLog(dir, entry("a.org"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fout safeBuf
	done := make(chan error, 1)
	go func() { done <- TailLog(ctx, path, true, &fout) }()
	waitFor := func(s string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !strings.Contains(fout.String(), s) {
			if time.Now().After(deadline) {
				t.Fatalf("follow never printed %q; got %q", s, fout.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Only append once the existing content has been read, so b.org must come
	// through the follow loop.
	waitFor("a.org:80")
	appendLog(dir, entry("b.org"))
	waitFor("b.org:80")

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendLog(dir, entry("c.org"))
	waitFor("c.org:80")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(fout.String(), "b.org"); n != 1 {
		t.Fatalf("b.org printed %d times:\n%s", n, fout.String())
	}
}

type safeBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestCanaryHits(t *testing.T) {
	cs := canary.Generate([]string{"aws", "env"})
	p := setup(t, Config{AllowPrivate: true}, []string{cs[0].Hosts[0], "example.com"}, nil)
	if err := canary.Save(p.Dir, cs); err != nil {
		t.Fatal(err)
	}
	var hits []string
	p.OnCanary = func(c *canary.Canary, where string) { hits = append(hits, c.Kind+": "+where) }

	// Allowlisted or not, a canary host is never reached.
	c, _ := serve(t, p)
	expect403(t, c, cs[0].Hosts[0]+":443", "not allowed")
	c, _ = serve(t, p)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	go fmt.Fprintf(c, "GET http://example.com/x HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer %s\r\n\r\n", cs[1].Values[2])
	if resp, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil || resp.StatusCode != 403 {
		t.Fatalf("plain request with a canary value: %v %v", resp, err)
	}
	want := []string{"aws: connection to " + cs[0].Hosts[0] + ":443", "env: in a request to example.com:80"}
	if strings.Join(hits, "|") != strings.Join(want, "|") {
		t.Errorf("hits %q, want %q", hits, want)
	}
	logs := readLog(t, p.Dir)
	if len(logs) != 2 || logs[0].Action != ActionDeny || logs[0].Reason != "canary (aws)" {
		t.Errorf("log %+v", logs)
	}
	// Other requests are unaffected.
	c, _ = serve(t, p)
	expect403(t, c, "other.example:443", "not in allowlist")
	if len(hits) != 2 {
		t.Errorf("hits %q", hits)
	}
	// An unreadable canary file denies everything, without a hit.
	if err := os.WriteFile(filepath.Join(p.Dir, canary.File), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ = serve(t, p)
	expect403(t, c, "example.com:443", "not allowed")
	if logs := readLog(t, p.Dir); len(hits) != 2 || !strings.HasPrefix(logs[len(logs)-1].Reason, "canaries unreadable") {
		t.Errorf("unreadable canaries: hits %q, log %+v", hits, logs[len(logs)-1])
	}
}
