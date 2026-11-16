package server

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// Per-client limits. They are constants, not configuration: a host's limits are
// the same everywhere, and the unix socket (the daemon's own user) is exempt.
var (
	reqBurst    = 30                     // requests one client may make at once
	reqRefill   = 100 * time.Millisecond // one token back, so 10 requests/s
	maxStreams  = 8                      // streams one client may have open
	clientCheck = 2 * time.Second        // how often an open stream checks its client is still enrolled
)

// limits is the per-client request and stream state, by enrolled client name.
type limits struct {
	mu sync.Mutex
	by map[string]*clientLimit
}

type clientLimit struct {
	tokens  float64
	last    time.Time
	streams int
}

func newLimits() *limits { return &limits{by: map[string]*clientLimit{}} }

func (l *limits) client(name string) *clientLimit {
	c := l.by[name]
	if c == nil {
		c = &clientLimit{tokens: float64(reqBurst), last: time.Now()}
		l.by[name] = c
	}
	return c
}

// allow spends one request token for name; false means the client must wait.
func (l *limits) allow(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.client(name)
	now := time.Now()
	if d := now.Sub(c.last); d > 0 {
		c.tokens = min(float64(reqBurst), c.tokens+d.Seconds()/reqRefill.Seconds())
	}
	c.last = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

// open takes one of name's stream slots and returns a context that is canceled
// when the client is no longer in the clients file. false: every slot is taken.
func (s *Server) open(ctx context.Context, name string) (context.Context, func(), bool) {
	l := s.limits
	l.mu.Lock()
	c := l.client(name)
	if c.streams >= maxStreams {
		l.mu.Unlock()
		return nil, nil, false
	}
	c.streams++
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	check := clientCheck // read before the goroutine: a later test must not race it
	go func() {          // a removed client's streams end within check
		t := time.NewTicker(check)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cs, err := LoadClients(s.clients)
				if err != nil {
					continue // the next request's authTCP reports a broken file
				}
				if _, ok := cs.Get(name); !ok {
					cancel()
					return
				}
			}
		}
	}()
	release := func() {
		l.mu.Lock()
		c.streams--
		l.mu.Unlock()
		cancel()
	}
	return ctx, release, true
}

// stream wraps a stream handler: an enrolled client needs a free stream slot,
// and its stream ends when it is removed. The unix socket (no Caller) is exempt.
func (s *Server) stream(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := CallerOf(r.Context())
		if !ok {
			h(w, r)
			return
		}
		ctx, release, ok := s.open(r.Context(), c.Name)
		if !ok {
			writeError(w, api.CodeRateLimited, "this client has "+strconv.Itoa(maxStreams)+" streams open: close one")
			return
		}
		defer release()
		h(w, r.WithContext(ctx))
	}
}
