package egress

import (
	"context"
	"errors"
	"net"
	"time"
)

// fallbackDelay is how long an attempt runs before the next address is tried too
// (RFC 8305's connection attempt delay).
var fallbackDelay = 250 * time.Millisecond

// DialIPs connects to port on the first of ips to answer, Happy Eyeballs style:
// address families interleaved, a new attempt every fallbackDelay or as soon as one
// fails. A host whose IPv6 resolves but goes nowhere then still connects over IPv4.
// The callers pass addresses they have already checked, so no second lookup can rebind.
func DialIPs(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), ips []net.IP, port string) (net.Conn, error) {
	ips = interleave(ips)
	if len(ips) == 0 {
		return nil, errors.New("no addresses")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		c   net.Conn
		err error
	}
	results := make(chan result, len(ips))
	next, pending := 0, 0
	start := func() {
		addr := net.JoinHostPort(ips[next].String(), port)
		next, pending = next+1, pending+1
		go func() {
			c, err := dial(ctx, "tcp", addr)
			results <- result{c, err}
		}()
	}
	start()
	t := time.NewTimer(fallbackDelay)
	defer t.Stop()
	var lastErr error
	for pending > 0 {
		select {
		case r := <-results:
			pending--
			if r.err == nil {
				go func(n int) { // close connections that lose the race
					for ; n > 0; n-- {
						if r := <-results; r.c != nil {
							r.c.Close()
						}
					}
				}(pending)
				return r.c, nil
			}
			lastErr = r.err
		case <-t.C:
		}
		if next < len(ips) {
			start()
			t.Reset(fallbackDelay)
		}
	}
	return nil, lastErr
}

// interleave alternates address families, starting with the first address's.
func interleave(ips []net.IP) []net.IP {
	if len(ips) == 0 {
		return nil
	}
	var first, other []net.IP
	v4 := ips[0].To4() != nil
	for _, ip := range ips {
		if (ip.To4() != nil) == v4 {
			first = append(first, ip)
		} else {
			other = append(other, ip)
		}
	}
	out := make([]net.IP, 0, len(ips))
	for i := 0; i < len(first) || i < len(other); i++ {
		if i < len(first) {
			out = append(out, first[i])
		}
		if i < len(other) {
			out = append(out, other[i])
		}
	}
	return out
}
