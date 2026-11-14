package server

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// promptReq is a request for vm as the daemon's watcher passes it on.
func promptReq(vm string) egress.Request {
	return egress.Request{VM: vm, Host: "example.com", Port: 443, Method: "CONNECT"}
}

// waitFor waits until cond holds, or fails the test after 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ask decides a prompt for vm in the background, with a timeout, and returns
// its decision.
func ask(t *testing.T, d egress.Decider, vm string, timeout time.Duration) chan egress.Decision {
	t.Helper()
	out := make(chan egress.Decision, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out <- d(ctx, promptReq(vm))
	}()
	return out
}

// promptConn is a prompt stream as a hand-written client opens it: the raw
// WebSocket on the home's socket, for messages a client library wouldn't send.
func promptConn(t *testing.T, h *vmHost) *websocket.Conn {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}
	c, _, err := websocket.Dial(context.Background(), "ws://catchpen"+api.PathEgressPrompts,
		&websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// answerer runs c.Prompts with answer in the background until the test ends.
func answerer(t *testing.T, c *apiclient.Client, answer func(api.Prompt) string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.Prompts(ctx, answer) }()
}

func TestEgressPromptAPI(t *testing.T) {
	h := home(t, nil)
	c := h.client(t)
	s := h.srv
	asked := make(chan api.Prompt, 4)
	answerer(t, c, func(m api.Prompt) string {
		if m.Type != api.PromptAsk {
			return ""
		}
		asked <- m
		return api.DecisionAllowOnce
	})
	waitFor(t, "the client to connect", func() bool { return s.prompts.connected() == 1 })
	fallback := func(context.Context, egress.Request) egress.Decision {
		t.Error("the fallback was used with a client connected")
		return egress.DecisionDeny
	}
	out := ask(t, s.PromptDecider(fallback), "v", 10*time.Second)
	d := <-out
	if d != egress.DecisionAllowOnce {
		t.Errorf("decision = %q, want allow-once", d)
	}
	var m api.Prompt
	select {
	case m = <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the client was never asked")
	}
	if m.Type != api.PromptAsk || m.VM != "v" || m.Host != "example.com" || m.Port != 443 || m.Method != "CONNECT" {
		t.Errorf("prompt = %+v, want an ask for v example.com:443 CONNECT", m)
	}
	if b, err := hex.DecodeString(m.ID); err != nil || len(b) != 8 {
		t.Errorf("prompt ID = %q, want 16 hex digits", m.ID)
	}
	found := false
	for _, e := range events(t, h, audit.EgressPrompt) {
		if strings.HasPrefix(e, "v ") && strings.Contains(e, "client:@local") && strings.Contains(e, "decision:allow-once") {
			found = true
		}
	}
	if !found {
		t.Errorf("no egress.prompt event for v by @local with allow-once: %v", events(t, h, audit.EgressPrompt))
	}
	if n := s.prompts.pendingCount(); n != 0 {
		t.Errorf("pending prompts = %d after the answer, want 0", n)
	}
}

func TestEgressPromptFallback(t *testing.T) {
	h := home(t, nil)
	s := h.srv
	calls := 0
	inner := func(context.Context, egress.Request) egress.Decision {
		calls++
		return egress.DecisionDeny
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if d := s.PromptDecider(inner)(ctx, promptReq("v")); d != egress.DecisionDeny {
		t.Errorf("decision = %q, want deny from the fallback", d)
	}
	if calls != 1 {
		t.Errorf("fallback called %d times, want 1", calls)
	}
	if e := events(t, h, audit.EgressPrompt); len(e) != 0 {
		t.Errorf("fallback answer audited: %v", e)
	}
}

func TestEgressPromptScope(t *testing.T) {
	h := home(t, nil)
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	s := a.srv
	fallbackCalls := 0
	fallback := func(context.Context, egress.Request) egress.Decision {
		fallbackCalls++
		return egress.DecisionDeny
	}

	// laptop has no scope: a task VM's prompt is not asked of it, and the
	// fallback answers.
	answerer(t, a.client(t, apiclient.Options{Cert: &a.laptop}), func(m api.Prompt) string {
		if m.Type == api.PromptAsk {
			t.Error("laptop, with no scope, was asked a task VM's prompt")
		}
		return ""
	})
	waitFor(t, "laptop to connect", func() bool { return s.prompts.connected() == 1 })
	if d := <-ask(t, s.PromptDecider(fallback), "task-a1", 2*time.Second); d != egress.DecisionDeny {
		t.Errorf("laptop-only decision = %q, want the fallback's deny", d)
	}
	if fallbackCalls != 1 {
		t.Errorf("fallback called %d times with laptop only, want 1", fallbackCalls)
	}
	if e := events(t, h, audit.EgressPrompt); len(e) != 0 {
		t.Errorf("laptop-only fallback audited: %v", e)
	}

	// cockpit has config:write: it is asked, and its answer is audited with
	// its name. laptop is still not asked.
	answerer(t, a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}}), func(m api.Prompt) string {
		if m.Type != api.PromptAsk {
			return ""
		}
		time.Sleep(200 * time.Millisecond)
		return api.DecisionDeny
	})
	waitFor(t, "cockpit to connect", func() bool { return s.prompts.connected() == 2 })
	if d := <-ask(t, s.PromptDecider(fallback), "task-a1", 10*time.Second); d != egress.DecisionDeny {
		t.Errorf("cockpit decision = %q, want deny", d)
	}
	if fallbackCalls != 1 {
		t.Errorf("fallback called again with cockpit connected (%d calls)", fallbackCalls)
	}
	found := false
	for _, e := range events(t, h, audit.EgressPrompt) {
		if strings.HasPrefix(e, "task-a1 ") && strings.Contains(e, "client:cockpit") {
			found = true
		}
	}
	if !found {
		t.Errorf("no egress.prompt event for task-a1 by cockpit: %v", events(t, h, audit.EgressPrompt))
	}

	// The socket's user has every scope: a task VM's prompt is answered
	// over it, even with a client that has no scope connected.
	h2 := home(t, nil)
	answerer(t, h2.client(t), func(m api.Prompt) string {
		if m.Type != api.PromptAsk {
			return ""
		}
		return api.DecisionDenyAlways
	})
	waitFor(t, "the socket client to connect", func() bool { return h2.srv.prompts.connected() == 1 })
	if d := <-ask(t, h2.srv.PromptDecider(fallback), "task-a1", 10*time.Second); d != egress.DecisionDenyAlways {
		t.Errorf("socket decision = %q, want deny-always", d)
	}
	if fallbackCalls != 1 {
		t.Errorf("fallback called for the socket client (%d calls)", fallbackCalls)
	}
}

func TestEgressPromptExpiry(t *testing.T) {
	h := home(t, nil)
	s := h.srv
	answerer(t, h.client(t), func(api.Prompt) string { return "" })
	waitFor(t, "the client to connect", func() bool { return s.prompts.connected() == 1 })
	fallbackCalls := 0
	fallback := func(context.Context, egress.Request) egress.Decision {
		fallbackCalls++
		return egress.DecisionAllowOnce
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if d := s.PromptDecider(fallback)(ctx, promptReq("v")); d != egress.DecisionDeny {
		t.Errorf("expired decision = %q, want deny", d)
	}
	if fallbackCalls != 0 {
		t.Errorf("fallback called for an expired prompt (%d calls)", fallbackCalls)
	}
	if n := s.prompts.pendingCount(); n != 0 {
		t.Errorf("pending prompts = %d after expiry, want 0", n)
	}
}

func TestEgressPromptReplay(t *testing.T) {
	h := home(t, nil)
	s := h.srv
	answerer(t, h.client(t), func(api.Prompt) string { return "" })
	waitFor(t, "the first client to connect", func() bool { return s.prompts.connected() == 1 })
	fallback := func(context.Context, egress.Request) egress.Decision {
		t.Error("the fallback was used with a client connected")
		return egress.DecisionDeny
	}
	out := ask(t, s.PromptDecider(fallback), "v", 10*time.Second)
	waitFor(t, "the prompt to be pending", func() bool { return s.prompts.pendingCount() == 1 })
	answerer(t, h.client(t), func(m api.Prompt) string {
		if m.Type != api.PromptAsk {
			return ""
		}
		return api.DecisionAllowAlways
	})
	if d := <-out; d != egress.DecisionAllowAlways {
		t.Errorf("replayed decision = %q, want allow-always", d)
	}
	found := false
	for _, e := range events(t, h, audit.EgressPrompt) {
		if strings.HasPrefix(e, "v ") && strings.Contains(e, "client:@local") && strings.Contains(e, "decision:allow-always") {
			found = true
		}
	}
	if !found {
		t.Errorf("no egress.prompt event for v with allow-always: %v", events(t, h, audit.EgressPrompt))
	}
}

func TestEgressPromptWatcher(t *testing.T) {
	h := home(t, nil)
	s := h.srv
	answerer(t, h.client(t), func(m api.Prompt) string {
		if m.Type != api.PromptAsk {
			return ""
		}
		return api.DecisionAllowAlways
	})
	waitFor(t, "the client to connect", func() bool { return s.prompts.connected() == 1 })
	dir := t.TempDir()
	fallback := func(context.Context, egress.Request) egress.Decision {
		t.Error("the fallback was used with a client connected")
		return egress.DecisionDeny
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- egress.Watch(ctx, map[string]string{"v": dir}, s.PromptDecider(fallback)) }()

	sock := filepath.Join(dir, egress.SocketFile)
	var conn net.Conn
	waitFor(t, "the watcher's socket", func() bool {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return false
		}
		conn = c
		return true
	})
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	req := fmt.Sprintf(`{"vm":"v","host":"example.com","port":443,"method":"CONNECT","deadline":%d}`+"\n",
		time.Now().Add(10*time.Second).UnixNano())
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal(line, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Decision != "allow-always" {
		t.Errorf("proxy got %+v, want allow-always", rep)
	}
	list, err := egress.LoadList(filepath.Join(dir, egress.AllowlistFile))
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, r := range list {
		entries = append(entries, r.String())
	}
	if !strings.Contains(strings.Join(entries, " "), "example.com:443") {
		t.Errorf("allowlist = %v, want example.com:443", entries)
	}
	found := false
	for _, e := range events(t, h, audit.EgressPrompt) {
		if strings.HasPrefix(e, "v ") && strings.Contains(e, "client:@local") && strings.Contains(e, "decision:allow-always") {
			found = true
		}
	}
	if !found {
		t.Errorf("no egress.prompt event for v with allow-always: %v", events(t, h, audit.EgressPrompt))
	}
	cancel()
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Error("egress.Watch did not return after its context was cancelled")
	}
}

func TestEgressPromptBadDecision(t *testing.T) {
	h := home(t, nil)
	s := h.srv
	raw := promptConn(t, h)
	waitFor(t, "the raw client to connect", func() bool { return s.prompts.connected() == 1 })
	refused := make(chan struct{})
	answerer(t, h.client(t), func(m api.Prompt) string {
		if m.Type != api.PromptAsk {
			return ""
		}
		<-refused
		return api.DecisionAllowOnce
	})
	waitFor(t, "the real client to connect", func() bool { return s.prompts.connected() == 2 })
	out := ask(t, s.PromptDecider(func(context.Context, egress.Request) egress.Decision {
		t.Error("the fallback was used with clients connected")
		return egress.DecisionDeny
	}), "v", 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var asked api.Prompt
	for asked.Type != api.PromptAsk {
		_, b, err := raw.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &asked); err != nil {
			t.Fatal(err)
		}
	}
	bad, _ := json.Marshal(api.Prompt{Type: api.PromptAnswer, ID: asked.ID, Decision: "maybe"})
	if err := raw.Write(ctx, websocket.MessageText, bad); err != nil {
		t.Fatal(err)
	}
	var refusal api.Prompt
	for refusal.Type != api.PromptDone {
		_, b, err := raw.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &refusal); err != nil {
			t.Fatal(err)
		}
	}
	if refusal.ID != asked.ID || refusal.Error == nil || refusal.Error.Code != api.CodeBadRequest {
		t.Errorf("refusal = %+v, want bad_request for %s", refusal, asked.ID)
	}
	close(refused)

	if d := <-out; d != egress.DecisionAllowOnce {
		t.Errorf("decision = %q, want allow-once from the valid answer", d)
	}
}

func TestPromptOrigin(t *testing.T) {
	h := home(t, nil)
	if status, e := h.upgrade(t, api.PathEgressPrompts, "https://evil.example"); status != http.StatusForbidden ||
		e == nil || e.Message != "a stream must come from the API's own origin" {
		t.Errorf("foreign origin: status %d, error %+v, want 403", status, e)
	}
	if status, _ := h.upgrade(t, api.PathEgressPrompts, ""); status != http.StatusSwitchingProtocols {
		t.Errorf("no origin: status %d, want 101", status)
	}
}
