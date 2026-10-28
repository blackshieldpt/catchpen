package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newWatcher(t *testing.T, decide Decider) *watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &watcher{ctx: ctx, decide: decide, pending: map[string]*pending{}, perVM: map[string]int{}}
}

// until polls cond under the watcher lock.
func until(t *testing.T, w *watcher, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		w.mu.Lock()
		ok := cond()
		w.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDecisionCoalescing(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	calls := map[string]int{}
	w := newWatcher(t, func(ctx context.Context, r Request) Decision {
		<-release
		mu.Lock()
		calls[r.VM+" "+r.Host]++
		mu.Unlock()
		if r.Host == "a.com" && r.VM == "vm1" {
			return DecisionAllowOnce
		}
		return DecisionDeny
	})
	dir := t.TempDir()
	type result struct {
		r Request
		d Decision
	}
	reqs := []Request{
		{VM: "vm1", Host: "a.com", Port: 443}, {VM: "vm1", Host: "a.com", Port: 443}, {VM: "vm1", Host: "a.com", Port: 443},
		{VM: "vm1", Host: "b.com", Port: 443}, // different target
		{VM: "vm2", Host: "a.com", Port: 443}, // different VM
	}
	results := make(chan result, len(reqs))
	for _, r := range reqs {
		go func() {
			d, _ := w.decision(context.Background(), r, dir)
			results <- result{r, d}
		}()
	}
	until(t, w, func() bool {
		p := w.pending["vm1\x00a.com\x00443"]
		return len(w.pending) == 3 && p != nil && p.waiters == 3
	})
	close(release)
	for range reqs {
		res := <-results
		want := DecisionDeny
		if res.r.VM == "vm1" && res.r.Host == "a.com" {
			want = DecisionAllowOnce
		}
		if res.d != want {
			t.Errorf("%+v: got %s, want %s", res.r, res.d, want)
		}
	}
	if calls["vm1 a.com"] != 1 || calls["vm1 b.com"] != 1 || calls["vm2 a.com"] != 1 || len(calls) != 3 {
		t.Fatalf("decide calls = %v", calls)
	}
}

func TestPendingCap(t *testing.T) {
	release := make(chan struct{})
	w := newWatcher(t, func(ctx context.Context, r Request) Decision {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return DecisionDeny
	})
	defer close(release)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range maxPending {
		go w.decision(ctx, Request{VM: "vm1", Host: string(rune('a'+i)) + ".com", Port: 443}, dir)
	}
	until(t, w, func() bool { return w.perVM["vm1"] == maxPending })
	if d, note := w.decision(ctx, Request{VM: "vm1", Host: "z.com", Port: 443}, dir); d != DecisionDeny || note != "too many pending prompts" {
		t.Fatalf("over cap: %s %q", d, note)
	}
	// An already-pending target still joins, and other VMs are unaffected.
	until(t, w, func() bool { return w.pending["vm1\x00a.com\x00443"] != nil })
	go w.decision(ctx, Request{VM: "vm1", Host: "a.com", Port: 443}, dir)
	go w.decision(ctx, Request{VM: "vm2", Host: "z.com", Port: 443}, dir)
	until(t, w, func() bool { return w.pending["vm1\x00a.com\x00443"].waiters == 2 && w.perVM["vm2"] == 1 })
}

func TestAbandonedRequestNotPrompted(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	w := newWatcher(t, func(ctx context.Context, r Request) Decision {
		mu.Lock()
		seen = append(seen, r.Host)
		mu.Unlock()
		if r.Host == "a.com" {
			<-release
		}
		return DecisionAllowOnce
	})
	dir := t.TempDir()
	done := make(chan struct{})
	go func() {
		w.decision(context.Background(), Request{VM: "vm1", Host: "a.com", Port: 443}, dir)
		close(done)
	}()
	until(t, w, func() bool { return len(w.pending) == 1 })

	// b.com queues behind a.com's prompt; its proxy gives up first.
	ctx, cancel := context.WithCancel(context.Background())
	bDone := make(chan Decision)
	go func() { d, _ := w.decision(ctx, Request{VM: "vm1", Host: "b.com", Port: 443}, dir); bDone <- d }()
	until(t, w, func() bool { return len(w.pending) == 2 })
	cancel()
	if d := <-bDone; d != DecisionDeny {
		t.Fatalf("abandoned request got %s", d)
	}
	close(release)
	<-done
	until(t, w, func() bool { return len(w.pending) == 0 && len(w.perVM) == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "a.com" {
		t.Fatalf("prompted for %v, want only a.com", seen)
	}
}

// rawAsk sends one prompt request over the watcher socket and returns the reply.
func rawAsk(t *testing.T, dir string, pr promptRequest) promptReply {
	t.Helper()
	c, err := net.Dial("unix", filepath.Join(dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(pr); err != nil {
		t.Fatal(err)
	}
	var rep promptReply
	if err := json.NewDecoder(c).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestWatcherProtocol(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	var seen atomic.Value
	startWatch(t, dir, func(_ context.Context, r Request) Decision {
		calls.Add(1)
		seen.Store(r)
		return DecisionAllowOnce
	})
	future := time.Now().Add(5 * time.Second).UnixNano()

	// The VM name comes from the socket, never the payload.
	rep := rawAsk(t, dir, promptRequest{VM: "evil", Host: "Example.COM", Port: 443, Method: "CONNECT", Deadline: future})
	if rep.Decision != DecisionAllowOnce {
		t.Fatalf("reply = %+v", rep)
	}
	if r := seen.Load().(Request); r.VM != "vm1" || r.Host != "example.com" {
		t.Fatalf("decider saw %+v", r)
	}

	for _, dl := range []int64{0, time.Now().Add(-time.Second).UnixNano()} {
		rep := rawAsk(t, dir, promptRequest{Host: "example.com", Port: 443, Method: "CONNECT", Deadline: dl})
		if rep.Decision != DecisionDeny || rep.Note != "expired" {
			t.Fatalf("deadline %d: reply = %+v", dl, rep)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("decide called %d times, want 1", calls.Load())
	}
}

// fakeWatcher answers every prompt on dir's socket with reply.
func fakeWatcher(t *testing.T, dir, reply string) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, SocketFile))
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
			bufio.NewReader(c).ReadBytes('\n')
			io.WriteString(c, reply)
			c.Close()
		}
	}()
}

func TestInvalidWatcherReplyDenies(t *testing.T) {
	for reply, reason := range map[string]string{
		`{"decision":"yes"}` + "\n": "invalid decision",
		"garbage\n":                 "prompt unavailable",
		"":                          "prompt unavailable",
	} {
		p := setup(t, Config{OnDeny: OnDenyPrompt, PromptTimeout: 2 * time.Second}, nil, nil)
		fakeWatcher(t, p.Dir, reply)
		c, _ := serve(t, p)
		expect403(t, c, "example.com:443", reason)
	}
}

func TestPromptReachesWatcherOverSocket(t *testing.T) {
	p := setup(t, Config{OnDeny: OnDenyPrompt, PromptTimeout: 2 * time.Second}, nil, nil)
	startWatch(t, p.Dir, func(_ context.Context, r Request) Decision { return DecisionDeny })
	c, _ := serve(t, p)
	expect403(t, c, "example.com:443", "deny")
	logs := readLog(t, p.Dir)
	if len(logs) != 1 || logs[0].Action != ActionPromptDeny || logs[0].Reason != "deny" {
		t.Fatalf("each decision must be logged once: %+v", logs)
	}
}

// signalWriter records output and signals each write.
type signalWriter struct {
	safeBuf
	wrote chan struct{}
}

func (s *signalWriter) Write(p []byte) (int, error) {
	n, err := s.safeBuf.Write(p)
	select {
	case s.wrote <- struct{}{}:
	default:
	}
	return n, err
}

func TestTerminalDecider(t *testing.T) {
	pr, pw := io.Pipe()
	out := &signalWriter{wrote: make(chan struct{}, 64)}
	d := TerminalDecider(pr, out)
	r := Request{VM: "vm1", Host: "example.com", Port: 443, Method: "CONNECT"}

	// Type-ahead with no prompt shown is discarded. The second write returns
	// only after the scanner has consumed the first line.
	io.WriteString(pw, "A\n")
	io.WriteString(pw, "x\n")
	answer := func(s string) {
		go func() { <-out.wrote; io.WriteString(pw, s) }()
	}
	answer("a\n")
	if got := d(context.Background(), r); got != DecisionAllowOnce {
		t.Fatalf("got %s, want allow-once (stale input must not answer)", got)
	}
	drain := func() {
		for len(out.wrote) > 0 {
			<-out.wrote
		}
	}
	drain()
	answer("D\n")
	if got := d(context.Background(), r); got != DecisionDenyAlways {
		t.Fatalf("got %s", got)
	}
	drain()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-out.wrote; cancel() }()
	if got := d(ctx, r); got != DecisionDeny {
		t.Fatalf("cancelled: got %s", got)
	}
	pw.Close()
	if got := d(context.Background(), r); got != DecisionDeny { // EOF
		t.Fatalf("EOF: got %s", got)
	}
	if !strings.Contains(out.String(), "example.com:443  [vm1 CONNECT] — (a)llow once") {
		t.Fatalf("prompt = %q", out.String())
	}
}

func TestDesktopDecider(t *testing.T) {
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("PATH", bin)
	r := Request{VM: "vm1", Host: "example.com", Port: 443, Method: "CONNECT"}
	for _, tc := range []struct {
		name, body string
		want       Decision
	}{
		{"allow-always", "echo allow-always", DecisionAllowAlways},
		{"allow-once", "echo allow-once", DecisionAllowOnce},
		{"unknown action", "echo bogus", DecisionDeny},
		{"dismissed", "exit 1", DecisionDeny},
		{"hangs", "exec /bin/sleep 30", DecisionDeny},
	} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n" + tc.body + "\n"
		if err := os.WriteFile(filepath.Join(bin, "notify-send"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		start := time.Now()
		got := DesktopDecider()(ctx, r)
		cancel()
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("%s: decider ignored ctx", tc.name)
		}
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "catchpen: example.com:443\nvm1 CONNECT\n") {
		t.Fatalf("notify-send args = %q", args)
	}
}
