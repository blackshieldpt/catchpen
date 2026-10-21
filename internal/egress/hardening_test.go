package egress

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readStatus(t *testing.T, c net.Conn) int {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestOversizedHeaderRejected(t *testing.T) {
	p := setup(t, Config{}, []string{"example.com"}, nil)
	c, done := serve(t, p)
	go func() {
		io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\nX-Big: ")
		io.WriteString(c, strings.Repeat("a", 2*maxHeaderBytes))
	}()
	if got := readStatus(t, c); got != 400 {
		t.Fatalf("status = %d, want 400", got)
	}
	if err := <-done; err == nil {
		t.Fatal("expected error for oversized header")
	}
}

func TestInvalidTargetRejected(t *testing.T) {
	for _, target := range []string{"evil.com/.github.com:443", "evil.com?.github.com:443", "<b>x</b>.github.com:443", "a\u009b31m.github.com:443"} {
		p := setup(t, Config{}, []string{"*.github.com"}, nil)
		c, _ := serve(t, p)
		go io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: x\r\n\r\n")
		if got := readStatus(t, c); got != 400 {
			t.Errorf("%q: status = %d, want 400", target, got)
		}
		if _, err := os.Stat(filepath.Join(p.Dir, LogFile)); err == nil {
			t.Errorf("%q: invalid target must not be logged", target)
		}
	}
}

func TestAllowAllStillDeniesPrivateAndDenylist(t *testing.T) {
	host, port := echoServer(t)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	p := setup(t, Config{AllowAll: true}, nil, []string{"blocked.example"})
	c, _ := serve(t, p)
	expect403(t, c, target, "private address")

	p = setup(t, Config{AllowAll: true}, nil, []string{"blocked.example"})
	c, _ = serve(t, p)
	expect403(t, c, "blocked.example:443", "denylisted")
}

func TestIdleTunnelClosed(t *testing.T) {
	host, port := echoServer(t)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	p := setup(t, Config{AllowPrivate: true}, []string{target}, nil)
	p.Idle = 300 * time.Millisecond
	c, done := serve(t, p)
	_, resp := connect(t, c, target, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle tunnel not closed")
	}
}

func TestConnectionLimit(t *testing.T) {
	p := setup(t, Config{}, []string{"example.com"}, nil)
	sd := filepath.Join(p.Dir, "egress.slots")
	if err := os.MkdirAll(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxConns; i++ {
		f, err := os.OpenFile(filepath.Join(sd, strconv.Itoa(i)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		// flock is per open file description, so this holds against ServeConn's own open.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
	}
	c, done := serve(t, p)
	if got := readStatus(t, c); got != 503 {
		t.Fatalf("status = %d, want 503", got)
	}
	if err := <-done; err == nil {
		t.Fatal("expected limit error")
	}
}

func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LogFile)
	if err := os.WriteFile(path, make([]byte, maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendLog(dir, LogEntry{VM: "v", Host: "h", Port: 1, Action: ActionDeny}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() > 1024 {
		t.Fatalf("log not rotated: %v %v", st, err)
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != maxLogBytes+1 {
		t.Fatalf("previous generation missing: %v", err)
	}
}

func TestSecondWatcherRefused(t *testing.T) {
	dir := t.TempDir()
	startWatch(t, dir, func(_ context.Context, _ Request) Decision { return DecisionDeny })
	err := Watch(t.Context(), map[string]string{"vm1": dir}, nil)
	if err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Fatalf("second Watch = %v", err)
	}
}

// serveTCP runs ServeConn on a real TCP connection (supports half-close).
func serveTCP(t *testing.T, p *Proxy) (*net.TCPConn, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		done <- p.ServeConn(context.Background(), c)
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c.(*net.TCPConn), done
}

func TestTunnelLargeTransferAndHalfClose(t *testing.T) {
	host, port := echoServer(t)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	p := setup(t, Config{AllowPrivate: true}, []string{target}, nil)
	c, done := serveTCP(t, p)
	br, resp := connect(t, c, target, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	payload := bytes.Repeat([]byte("0123456789abcdef"), 256<<10/16) // 256 KiB, > header cap
	go func() {
		c.Write(payload)
		c.CloseWrite() // the echo server only finishes once the half-close reaches it
	}()
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echoed %d bytes, want %d", len(got), len(payload))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestForwardLargeBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, r.Body)
	}))
	defer srv.Close()
	u := srv.Listener.Addr().String()
	p := setup(t, Config{AllowPrivate: true}, []string{u}, nil)
	c, _ := serveTCP(t, p)
	c.SetDeadline(time.Now().Add(10 * time.Second))
	body := bytes.Repeat([]byte("x"), 256<<10)
	go fmt.Fprintf(c, "POST http://%s/ HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n%s", u, u, len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(got, body) {
		t.Fatalf("status %d, %d bytes back, want %d", resp.StatusCode, len(got), len(body))
	}
}

func TestActiveTunnelOutlivesIdle(t *testing.T) {
	host, port := echoServer(t)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	p := setup(t, Config{AllowPrivate: true}, []string{target}, nil)
	p.Idle = 200 * time.Millisecond
	c, done := serve(t, p)
	br, resp := connect(t, c, target, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	buf := make([]byte, 1)
	for end := time.Now().Add(4 * p.Idle); time.Now().Before(end); {
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte("x")); err != nil {
			t.Fatalf("tunnel closed while active: %v", err)
		}
		if _, err := io.ReadFull(br, buf); err != nil {
			t.Fatalf("tunnel closed while active: %v", err)
		}
		time.Sleep(p.Idle / 4)
	}
	select {
	case err := <-done:
		t.Fatalf("tunnel ended early: %v", err)
	default:
	}
}

func TestMethodNotAllowed(t *testing.T) {
	for _, m := range []string{"TRACE", "PROPFIND", "FOO"} {
		p := setup(t, Config{}, []string{"example.com"}, nil)
		c, _ := serve(t, p)
		go fmt.Fprintf(c, "%s http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n", m)
		if got := readStatus(t, c); got != 405 {
			t.Errorf("%s: status = %d, want 405", m, got)
		}
	}
}

func TestLogFailureDeniesAllowed(t *testing.T) {
	host, port := echoServer(t)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	p := setup(t, Config{AllowPrivate: true}, []string{target}, nil)
	if err := os.Mkdir(filepath.Join(p.Dir, LogFile), 0o700); err != nil { // unwritable log
		t.Fatal(err)
	}
	c, done := serve(t, p)
	if _, resp := connect(t, c, target, ""); resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "egress log") {
		t.Fatalf("ServeConn = %v", err)
	}
}

func TestCorruptDenylistDenies(t *testing.T) {
	p := setup(t, Config{}, []string{"example.com"}, nil)
	if err := os.WriteFile(filepath.Join(p.Dir, DenylistFile), []byte("not a host!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := serve(t, p)
	expect403(t, c, "example.com:443", "denylist")
	if logs := readLog(t, p.Dir); logs[0].Action != ActionError || !strings.HasPrefix(logs[0].Reason, "denylist: ") {
		t.Fatalf("log = %+v", logs)
	}
}

func TestIsPrivate(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "169.254.169.254": true,
		"100.64.0.1": true, "0.0.0.0": true, "10.0.2.2": true, "192.0.0.1": true, "198.18.0.1": true, "240.0.0.1": true,
		"224.0.0.1": true, "::1": true, "::": true, "fe80::1": true, "fc00::1": true, "ff02::1": true,
		"::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true, "64:ff9b::7f00:1": true, "2002:7f00:1::": true, "::7f00:1": true,
		"8.8.8.8": false, "93.184.215.14": false, "100.128.0.1": false, "2606:4700::1111": false, "::ffff:1.1.1.1": false,
	} {
		if got := IsPrivate(net.ParseIP(ip)); got != want {
			t.Errorf("IsPrivate(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestLogKeepsHTMLBytes(t *testing.T) {
	dir := t.TempDir()
	if err := appendLog(dir, LogEntry{VM: "v", Host: "h", Port: 1, Action: ActionError, Reason: "a<b>&c"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, LogFile))
	if !strings.Contains(string(b), `"reason":"a<b>&c"`) {
		t.Fatalf("log line = %s", b)
	}
}
