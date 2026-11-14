package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// upgrade asks for a WebSocket upgrade of path, with origin as its Origin
// ("": none). It returns the status, and the error a refusal names.
func (h *vmHost) upgrade(t *testing.T, path, origin string) (int, *api.Error) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "http://catchpen"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return resp.StatusCode, nil
	}
	var er api.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&er)
	return resp.StatusCode, er.Error
}

// drain reads r until its bytes contain want (want "": until a read fails),
// and returns them with the failing read's error. A read that takes longer
// than d fails the test.
func drain(t *testing.T, r io.Reader, d time.Duration, want string) (string, error) {
	t.Helper()
	type result struct {
		got string
		err error
	}
	done := make(chan result, 1)
	go func() {
		var acc []byte
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			acc = append(acc, buf[:n]...)
			if (want != "" && strings.Contains(string(acc), want)) || err != nil {
				done <- result{string(acc), err}
				return
			}
		}
	}()
	select {
	case res := <-done:
		return res.got, res.err
	case <-time.After(d):
		t.Fatalf("no read within %v", d)
		return "", nil
	}
}

// runningOf reports whether the VM name is running under m.
func runningOf(m *vm.Manager, name string) func() bool {
	return func() bool { _, ok := m.Running(name); return ok }
}

// fakeRunning is a running VM named name, with ssh_port 1 (ssh fails at
// once) and a state a stream can read.
func fakeRunning(t *testing.T, h *vmHost, name string) {
	t.Helper()
	m := &vm.Manager{P: h.p}
	vmtest.FakeQEMU(t, h.p.VM(name), name, 512, runningOf(m, name))
	write(t, filepath.Join(h.p.VM(name), "runtime.json"), `{"ssh_port":1}`)
	h.writeVM(t, vm.State{Name: name, Template: "base", Created: created})
}

func TestSameOrigin(t *testing.T) {
	h := home(t, nil)
	for _, c := range []struct {
		origin string
		status int
		code   string
		msg    string
	}{
		{"https://evil.example", http.StatusForbidden, api.CodeForbidden, "a stream must come from the API's own origin"},
		{"", http.StatusNotFound, api.CodeNotFound, "no such VM"},
		{"https://catchpen", http.StatusNotFound, api.CodeNotFound, "no such VM"},
	} {
		for _, path := range []string{api.PathVMTerminal("nope"), api.PathVMConsole("nope")} {
			status, e := h.upgrade(t, path, c.origin)
			if status != c.status || e == nil || e.Code != c.code || !strings.HasPrefix(e.Message, c.msg) {
				t.Errorf("%s from %q: %d %+v, want %d %s: %s", path, c.origin, status, e, c.status, c.code, c.msg)
			}
		}
	}
}

func TestStreamRefusals(t *testing.T) {
	h := serveVMs(t)
	h.writeVM(t, vm.State{Name: "idle", Template: "base", Created: created})
	h.writeVM(t, vm.State{Name: "term", Template: "base", Created: created, SSH: template.SSH{Agent: "host"}})
	c := h.client(t)
	ctx := context.Background()

	_, err := c.Terminal(ctx, "nope", apiclient.TerminalOptions{})
	refused(t, "terminal on a missing VM", err, api.CodeNotFound, "no such VM")
	_, err = c.Console(ctx, "nope")
	refused(t, "console on a missing VM", err, api.CodeNotFound, "no such VM")

	_, err = c.Terminal(ctx, "idle", apiclient.TerminalOptions{})
	refused(t, "terminal on a stopped VM", err, api.CodeConflict, "VM idle is not running (start it: catchpen start idle)")
	_, err = c.Console(ctx, "idle")
	refused(t, "console on a stopped VM", err, api.CodeConflict, "VM idle is not running (start it: catchpen start idle)")

	_, err = c.Terminal(ctx, "beta", apiclient.TerminalOptions{})
	refused(t, "terminal on a paused VM", err, api.CodeVMPaused, "a canary hit paused the VM")
	_, err = c.Console(ctx, "beta")
	refused(t, "console on a paused VM", err, api.CodeVMPaused, "a canary hit paused the VM")

	gamma, err := c.Terminal(ctx, "gamma", apiclient.TerminalOptions{})
	if err != nil {
		t.Fatalf("terminal on gamma: %v", err)
	}
	if _, err := drain(t, gamma, 10*time.Second, ""); err != io.EOF {
		t.Errorf("gamma: read ended with %v, want io.EOF", err)
	}
	if gamma.End() == nil {
		t.Error("gamma: no end frame")
	}
	if err := gamma.Close(); err != nil {
		t.Errorf("gamma: close: %v", err)
	}

	_, err = c.Terminal(ctx, "term", apiclient.TerminalOptions{})
	refused(t, "terminal on an ssh.agent VM", err, api.CodeConflict, "the VM has ssh.agent: host, and this call can load or forward the host's SSH keys: use the CLI")
	_, err = c.Console(ctx, "term")
	refused(t, "console on an ssh.agent VM", err, api.CodeConflict, "VM term is not running")

	for _, q := range []struct{ query, msg string }{
		{"kind=other", `kind must be "ssh", "run" or "attach", got "other"`},
		{"agent=claude", "agent and arg are only for kind=run"},
		{"kind=attach&arg=x", "agent and arg are only for kind=run"},
		{"cols=100", "cols and rows go together"},
		{"cols=x&rows=20", `cols must be a number, got "x"`},
		{"cols=20&rows=y", `rows must be a number, got "y"`},
		{"cols=5000&rows=20", "terminal size 5000x20 is outside 1x1 to 4096x4096"},
		{"extra=1", `unknown query parameter "extra"`},
	} {
		status, e := h.upgrade(t, api.PathVMTerminal("nope")+"?"+q.query, "")
		if status != http.StatusBadRequest || e == nil || e.Code != api.CodeBadRequest || !strings.HasPrefix(e.Message, q.msg) {
			t.Errorf("?%s: %d %+v, want 400 %s", q.query, status, e, q.msg)
		}
	}
}

func TestStreamActions(t *testing.T) {
	h := home(t, nil)
	m := &vm.Manager{P: h.p}
	fakeRunning(t, h, "term")
	fakeRunning(t, h, "con")
	c := h.client(t)
	ctx := context.Background()

	term, err := c.Terminal(ctx, "term", apiclient.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drain(t, term, 10*time.Second, ""); err != io.EOF {
		t.Errorf("terminal: read ended with %v, want io.EOF", err)
	}
	if term.End() == nil {
		t.Error("terminal: no end frame")
	}
	if err := term.Close(); err != nil {
		t.Errorf("terminal: close: %v", err)
	}
	var started bool
	for _, e := range events(t, h, audit.SessionStart) {
		if strings.HasPrefix(e, "term ") && strings.Contains(e, "kind:web-ssh") && strings.Contains(e, "client:@local") {
			started = true
		}
	}
	if !started {
		t.Errorf("no session.start for term with kind:web-ssh and client:@local: %v", events(t, h, audit.SessionStart))
	}

	l, err := net.Listen("unix", m.SerialSocket("con"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("BOOT\r\n"))
		io.Copy(io.Discard, conn)
	}()
	cons, err := c.Console(ctx, "con")
	if err != nil {
		t.Fatal(err)
	}
	defer cons.Close()
	if got, err := drain(t, cons, 10*time.Second, "BOOT\r\n"); err != nil || got != "BOOT\r\n" {
		t.Errorf("console: read %q, %v; want BOOT", got, err)
	}
	_, err = c.Console(ctx, "con")
	refused(t, "second console", err, api.CodeConflict, "another console stream is open on the VM: one client reads a serial console at a time")

	pausedFile := filepath.Join(h.p.VM("con"), "paused.json")
	write(t, pausedFile, "{}")
	defer os.Remove(pausedFile)
	if _, err := drain(t, cons, 3*consolePoll+2*time.Second, ""); err != io.EOF {
		t.Errorf("console after a pause: read ended with %v, want io.EOF", err)
	}
	if e := cons.End(); e == nil || e.Error == nil || e.Error.Code != api.CodeVMPaused {
		t.Errorf("console after a pause: end %+v, want %s", e, api.CodeVMPaused)
	}
}

func TestStreamLimit(t *testing.T) {
	h := home(t, nil)
	m := &vm.Manager{P: h.p}
	fakeRunning(t, h, "lim")
	l, err := net.Listen("unix", m.SerialSocket("lim"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		seen, sent := 0, false
		buf := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buf)
			seen += n
			if !sent && seen >= 256<<10 {
				if _, err := conn.Write([]byte("OK")); err != nil {
					return
				}
				sent = true
			}
			if err != nil {
				return
			}
		}
	}()
	cons, err := h.client(t).Console(context.Background(), "lim")
	if err != nil {
		t.Fatal(err)
	}
	defer cons.Close()
	if _, err := cons.Write(make([]byte, 256<<10)); err != nil {
		t.Fatalf("paste: %v", err)
	}
	if got, err := drain(t, cons, 5*time.Second, "OK"); err != nil || !strings.Contains(got, "OK") {
		t.Errorf("after a 256 KiB paste: read %q, %v; want OK", got, err)
	}
}

func TestStreamTaskVMs(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "task-a1", Template: "base", Created: created})
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	ctx := context.Background()
	const msg = "this call needs the config:write scope (catchpen daemon client add --scope config:write)"
	laptop := a.client(t, apiclient.Options{Cert: &a.laptop}) // no scope
	_, err := laptop.Terminal(ctx, "task-a1", apiclient.TerminalOptions{})
	refused(t, "laptop's terminal on a task VM", err, api.CodeForbidden, msg)
	_, err = laptop.Console(ctx, "task-a1")
	refused(t, "laptop's console on a task VM", err, api.CodeForbidden, msg)

	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}}) // config:write
	_, err = cockpit.Terminal(ctx, "task-a1", apiclient.TerminalOptions{})
	refused(t, "cockpit's terminal on a task VM", err, api.CodeConflict, "VM task-a1 is not running")
	_, err = cockpit.Console(ctx, "task-a1")
	refused(t, "cockpit's console on a task VM", err, api.CodeConflict, "VM task-a1 is not running")
}

func TestStreamTCP(t *testing.T) {
	h := home(t, nil)
	fakeRunning(t, h, "term")
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	ctx := context.Background()
	for _, c := range []*apiclient.Client{
		a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}}),
		a.client(t, apiclient.Options{Cert: &a.laptop}),
	} {
		term, err := c.Terminal(ctx, "term", apiclient.TerminalOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := drain(t, term, 10*time.Second, ""); err != io.EOF {
			t.Errorf("read ended with %v, want io.EOF", err)
		}
		if term.End() == nil {
			t.Error("no end frame")
		}
		if err := term.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}
}
