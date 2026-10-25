package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// listen returns a local port that accepts connections.
func listen(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// blackholing dials for real, except that connections to hole hang until cancelled,
// like a host whose IPv6 route drops packets.
func blackholing(hole string, cancelled *atomic.Bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h, _, _ := net.SplitHostPort(addr); h == hole {
			<-ctx.Done()
			cancelled.Store(true)
			return nil, ctx.Err()
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

func TestDialIPsFallsBackFromAHangingAddress(t *testing.T) {
	port := listen(t)
	var cancelled atomic.Bool
	ips := []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("127.0.0.1")}
	start := time.Now()
	c, err := DialIPs(context.Background(), blackholing("2001:db8::1", &cancelled), ips, port)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
	if got := c.RemoteAddr().(*net.TCPAddr).IP.String(); got != "127.0.0.1" {
		t.Fatalf("connected to %s", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cancelled.Load() {
		t.Fatal("the hanging attempt was not cancelled")
	}
}

func TestDialIPsTriesTheNextAddressAsSoonAsOneFails(t *testing.T) {
	port := listen(t)
	old := fallbackDelay
	fallbackDelay = time.Hour
	t.Cleanup(func() { fallbackDelay = old })
	refused := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h, _, _ := net.SplitHostPort(addr); h == "127.0.0.2" {
			return nil, errors.New("refused")
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialIPs(ctx, refused, []net.IP{net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.1")}, port)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestDialIPsReturnsTheLastError(t *testing.T) {
	fail := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("refused " + addr)
	}
	_, err := DialIPs(context.Background(), fail, []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.2")}, "80")
	if err == nil || err.Error() != "refused 192.0.2.2:80" {
		t.Fatalf("err %v", err)
	}
	if _, err := DialIPs(context.Background(), fail, nil, "80"); err == nil {
		t.Fatal("no addresses: no error")
	}
}

func TestInterleave(t *testing.T) {
	in := []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("2001:db8::2"), net.ParseIP("2001:db8::3"), net.ParseIP("192.0.2.1")}
	var got []string
	for _, ip := range interleave(in) {
		got = append(got, ip.String())
	}
	want := "[2001:db8::1 192.0.2.1 2001:db8::2 2001:db8::3]"
	if s := fmt.Sprint(got); s != want {
		t.Fatalf("got %s, want %s", s, want)
	}
}
