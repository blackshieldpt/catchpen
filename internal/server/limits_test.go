package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// setLimits tunes the limits for one test; the cleanup restores them after
// the server's own (cleanups run last-in-first-out).
func setLimits(t *testing.T, burst, streams int, refill, check time.Duration) {
	t.Helper()
	ob, or, os, oc := reqBurst, reqRefill, maxStreams, clientCheck
	reqBurst, reqRefill, maxStreams, clientCheck = burst, refill, streams, check
	t.Cleanup(func() { reqBurst, reqRefill, maxStreams, clientCheck = ob, or, os, oc })
}

// homeManager is home's default Manager, over h's paths.
func homeManager(h *vmHost) func() (*vm.Manager, error) {
	return func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	}
}

func streamsOf(s *Server, name string) int {
	s.limits.mu.Lock()
	defer s.limits.mu.Unlock()
	if c := s.limits.by[name]; c != nil {
		return c.streams
	}
	return 0
}

// waitStreams polls name's open streams until they are want, for at most 1 s.
func waitStreams(t *testing.T, s *Server, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for streamsOf(s, name) != want {
		if time.Now().After(deadline) {
			t.Fatalf("streams for %s = %d, want %d", name, streamsOf(s, name), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openEvents starts an Events stream as c, and returns the channel its
// result comes on.
func openEvents(ctx context.Context, c *apiclient.Client) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- c.Events(ctx, time.Time{}, func(api.Event) error { return nil })
	}()
	return done
}

// cancelOnCleanup returns a context canceled at the end of the test, which
// runs before the server's own cleanup (registered earlier).
func cancelOnCleanup(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func TestRateLimit(t *testing.T) {
	setLimits(t, 3, 8, time.Hour, time.Second)
	a := serveTCP(t)
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	laptop := a.client(t, apiclient.Options{Cert: &a.laptop})
	for i := range 3 {
		if err := info(cockpit); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if err := info(cockpit); code(err) != api.CodeRateLimited {
		t.Errorf("fourth request: %v, want rate_limited", err)
	}
	// Each client has its own bucket.
	if err := info(laptop); err != nil {
		t.Errorf("laptop: %v", err)
	}
}

func TestRateRefill(t *testing.T) {
	setLimits(t, 1, 8, 200*time.Millisecond, time.Second)
	a := serveTCP(t)
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	if err := info(cockpit); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if err := info(cockpit); code(err) != api.CodeRateLimited {
		t.Errorf("second request: %v, want rate_limited", err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := info(cockpit); err != nil {
		t.Errorf("after a refill: %v", err)
	}
}

func TestClientStreamLimit(t *testing.T) {
	setLimits(t, 100, 2, time.Hour, time.Second)
	h := home(t, nil)
	a := serveTCPManager(t, homeManager(h))
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})

	ctx1, cancel1 := context.WithCancel(cancelOnCleanup(t))
	ctx2, cancel2 := context.WithCancel(cancelOnCleanup(t))
	openEvents(ctx1, cockpit)
	openEvents(ctx2, cockpit)
	waitStreams(t, a.srv, "cockpit", 2)

	select {
	case err := <-openEvents(cancelOnCleanup(t), cockpit):
		if code(err) != api.CodeRateLimited || !strings.Contains(err.Error(), "streams open") {
			t.Errorf("third stream: %v, want rate_limited", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the third stream was accepted")
	}

	// The WebSocket route is capped too: the refusal comes before the VM lookup.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cockpit.Terminal(ctx, "nope", apiclient.TerminalOptions{}); code(err) != api.CodeRateLimited {
		t.Errorf("terminal with the slots full: %v, want rate_limited", err)
	}

	cancel1()
	cancel2()
	waitStreams(t, a.srv, "cockpit", 0)

	ctx4, cancel4 := context.WithCancel(cancelOnCleanup(t))
	done4 := openEvents(ctx4, cockpit)
	waitStreams(t, a.srv, "cockpit", 1)
	select {
	case err := <-done4:
		t.Fatalf("the fourth stream ended: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel4()
}

func TestStreamSlotReleased(t *testing.T) {
	setLimits(t, 100, 2, time.Hour, time.Second)
	h := home(t, nil)
	a := serveTCPManager(t, homeManager(h))
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})

	ctx1, cancel1 := context.WithCancel(cancelOnCleanup(t))
	openEvents(ctx1, cockpit)
	waitStreams(t, a.srv, "cockpit", 1)
	cancel1()
	waitStreams(t, a.srv, "cockpit", 0)

	ctx2, cancel2 := context.WithCancel(cancelOnCleanup(t))
	ctx3, cancel3 := context.WithCancel(cancelOnCleanup(t))
	done2 := openEvents(ctx2, cockpit)
	done3 := openEvents(ctx3, cockpit)
	waitStreams(t, a.srv, "cockpit", 2)
	for _, done := range []<-chan error{done2, done3} {
		select {
		case err := <-done:
			t.Fatalf("a stream ended early: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}

	select {
	case err := <-openEvents(cancelOnCleanup(t), cockpit):
		if code(err) != api.CodeRateLimited {
			t.Errorf("third stream: %v, want rate_limited", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the third stream was accepted")
	}
	cancel2()
	cancel3()
}

func TestRemovedClient(t *testing.T) {
	setLimits(t, 100, 8, time.Hour, 50*time.Millisecond)
	h := home(t, nil)
	a := serveTCPManager(t, homeManager(h))
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})

	done := openEvents(cancelOnCleanup(t), cockpit)
	waitStreams(t, a.srv, "cockpit", 1)
	if err := RemoveClient(a.clients, "cockpit"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the removed client's stream is still open")
	}
	if err := info(cockpit); code(err) != api.CodeUnauthorized {
		t.Errorf("removed client's next request: %v, want unauthorized", err)
	}
}

func TestSocketUnlimited(t *testing.T) {
	setLimits(t, 1, 1, time.Hour, time.Second)
	h := home(t, nil)
	sock := h.client(t)
	for range 2 {
		done := openEvents(cancelOnCleanup(t), sock)
		select {
		case err := <-done:
			t.Fatalf("a socket stream was refused: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	for i := range 3 {
		if err := info(sock); err != nil {
			t.Errorf("socket request %d: %v", i, err)
		}
	}
}
