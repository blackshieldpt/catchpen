package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/blackshieldpt/catchpen/internal/record"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// A terminal's size with no cols and rows in its query.
const (
	defaultCols = 80
	defaultRows = 24
)

// webKindPrefix is the kind an API terminal is recorded and audited as:
// "web-" and the terminal's kind (web-ssh, web-run, web-attach), so the log
// says where an ssh, run or attach session came from.
const webKindPrefix = "web-"

// streamWriteTimeout bounds each write to a stream: a client that stops
// reading can't hold a session or a console pump for good.
var streamWriteTimeout = 30 * time.Second

// consolePoll is how often an open console stream looks for the canary hit
// or pause that ends it.
var consolePoll = time.Second

// sameOrigin refuses a stream whose Origin isn't this API's own: a web page
// must not open one with a certificate the browser holds. A program's client
// sends no Origin, which is allowed.
func sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
		writeError(w, api.CodeForbidden, "a stream must come from the API's own origin")
		return false
	}
	return true
}

// terminalParams is a terminal's query, checked.
type terminalParams struct {
	kind  string
	agent string
	args  []string
	size  record.Winsize
}

// terminalQuery reads a terminal's query and refuses what it can't run.
func terminalQuery(r *http.Request) (terminalParams, error) {
	var p terminalParams
	q := r.URL.Query()
	for k := range q {
		switch k {
		case api.QueryKind, api.QueryAgent, api.QueryArg, api.QueryCols, api.QueryRows:
		default:
			return p, fmt.Errorf("unknown query parameter %q", k)
		}
	}
	p.kind = api.TerminalSSH
	switch k := q.Get(api.QueryKind); k {
	case "", api.TerminalSSH:
	case api.TerminalRun, api.TerminalAttach:
		p.kind = k
	default:
		return p, fmt.Errorf("kind must be %q, %q or %q, got %q", api.TerminalSSH, api.TerminalRun, api.TerminalAttach, k)
	}
	p.agent, p.args = q.Get(api.QueryAgent), q[api.QueryArg]
	if p.kind != api.TerminalRun && (p.agent != "" || len(p.args) > 0) {
		return p, fmt.Errorf("agent and arg are only for kind=%s", api.TerminalRun)
	}
	w, h := defaultCols, defaultRows
	if q.Has(api.QueryCols) != q.Has(api.QueryRows) {
		return p, fmt.Errorf("cols and rows go together")
	}
	if q.Has(api.QueryCols) {
		var err error
		if w, err = strconv.Atoi(q.Get(api.QueryCols)); err != nil {
			return p, fmt.Errorf("cols must be a number, got %q", q.Get(api.QueryCols))
		}
		if h, err = strconv.Atoi(q.Get(api.QueryRows)); err != nil {
			return p, fmt.Errorf("rows must be a number, got %q", q.Get(api.QueryRows))
		}
	}
	if err := api.CheckSize(w, h); err != nil {
		return p, err
	}
	p.size = record.Winsize{W: w, H: h}
	return p, nil
}

// terminal is GET /v1/vms/{name}/terminal, upgraded to a WebSocket: an
// interactive session in the VM, recorded and audited as a session.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(w, r) || !taskAllowed(w, r) {
		return
	}
	p, err := terminalQuery(r)
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	if held(w, v, true) { // paused, or ssh.agent: this session would carry the keys
		return
	}
	if !v.Running {
		writeError(w, api.CodeConflict, "VM "+v.Name+" is not running (start it: catchpen start "+v.Name+")")
		return
	}
	remote := ""
	switch p.kind {
	case api.TerminalRun:
		if remote, err = vm.RunCommand(&v.State, p.agent, p.args); err != nil {
			writeError(w, api.CodeBadRequest, err.Error())
			return
		}
	case api.TerminalAttach:
		remote = vm.AttachCommand(&v.State)
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept answered
	}
	defer c.CloseNow()
	c.SetReadLimit(api.MaxStreamMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var mu sync.Mutex
	size := p.size
	resize := make(chan record.Winsize, 1)
	in := &wsIn{c: c, ctx: ctx, end: cancel, resize: func(w, h int) {
		mu.Lock()
		size = record.Winsize{W: w, H: h}
		mu.Unlock()
		select { // only the latest size matters
		case resize <- record.Winsize{W: w, H: h}:
		default:
		}
	}}
	m.Client = clientOf(r) // session.start and session.end carry it
	code, serr := m.Session(ctx, v.Name, webKindPrefix+p.kind, remote, record.IO{
		In: in, Out: &wsOut{c: c},
		Size: func() (int, int) {
			mu.Lock()
			defer mu.Unlock()
			return size.W, size.H
		},
		Resize: resize,
		Term:   api.TerminalTerm,
	})
	// The end frame first: canceling a context a Read is waiting on closes
	// the WebSocket (coder/websocket's context.AfterFunc), and the frame
	// would be lost. Then unblock the reader.
	endStream(c, api.Frame{Type: api.FrameEnd, Exit: exitOf(code), Error: sessionError(code, serr)})
	cancel()
}

// exitOf is a session's exit code for a FrameEnd: none when it never ran.
func exitOf(code int) *int {
	if code < 0 {
		return nil
	}
	return &code
}

// sessionError is a session's error for a FrameEnd: one after the session
// started is the exit code's to tell, as the CLI prints ssh's message, and
// ssh's text can name host paths.
func sessionError(code int, err error) *api.Error {
	if err == nil || code >= 0 {
		return nil
	}
	return &api.Error{Code: api.CodeInternal, Message: err.Error()}
}

// console is GET /v1/vms/{name}/console, upgraded to a WebSocket: the VM's
// serial console, filtered. One stream per VM; a canary hit that pauses the
// VM ends it.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(w, r) || !taskAllowed(w, r) {
		return
	}
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	if held(w, v, false) { // a paused VM; a serial line loads no keys
		return
	}
	if !v.Running {
		writeError(w, api.CodeConflict, "VM "+v.Name+" is not running (start it: catchpen start "+v.Name+")")
		return
	}
	if !s.takeConsole(v.Name) {
		writeError(w, api.CodeConflict, "another console stream is open on the VM: one client reads a serial console at a time")
		return
	}
	defer s.freeConsole(v.Name)
	conn, err := m.DialConsole(v.Name)
	if err != nil {
		writeError(w, api.CodeInternal, "the console can't be opened")
		return
	}
	defer conn.Close()
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(api.MaxStreamMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var mu sync.Mutex
	var end *api.Error
	go func() { // the CLI's console survives a pause; the server's doesn't
		t := time.NewTicker(consolePoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if m.Paused(v.Name) || m.Hit(v.Name) {
					mu.Lock()
					end = &api.Error{Code: api.CodeVMPaused, Message: `a canary hit paused the VM: resume it, stop it ("force": true) or destroy it first`}
					mu.Unlock()
					conn.Close() // ends PumpConsole's copy from the console
					return
				}
			}
		}
	}()
	vm.PumpConsole(conn, &wsIn{c: c, ctx: ctx}, &wsOut{c: c})
	mu.Lock()
	f := api.Frame{Type: api.FrameEnd, Error: end}
	mu.Unlock()
	endStream(c, f) // before cancel: see the terminal's note
	cancel()
}

// takeConsole marks a VM's console as open; false: a stream has it.
func (s *Server) takeConsole(name string) bool {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	if _, ok := s.consoles[name]; ok {
		return false
	}
	s.consoles[name] = struct{}{}
	return true
}

// freeConsole releases a VM's console.
func (s *Server) freeConsole(name string) {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	delete(s.consoles, name)
}

// endStream sends a stream's last frame, then closes it normally.
func endStream(c *websocket.Conn, f api.Frame) {
	ctx, cancel := context.WithTimeout(context.Background(), streamWriteTimeout)
	defer cancel()
	if b, err := json.Marshal(f); err == nil {
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// wsIn is a stream's input: binary messages, and the client's control frames
// among them. A frame it doesn't understand, or a size it can't use, is
// ignored: the stream goes on at its last size. When the client is gone (a
// read fails), end is called: without it a detached terminal's ssh would
// outlive its client until the guest next wrote.
type wsIn struct {
	c      *websocket.Conn
	ctx    context.Context
	resize func(w, h int) // nil for a console
	end    func()         // nil for a console, which ends on the input's EOF
	rest   []byte
}

func (in *wsIn) Read(p []byte) (int, error) {
	if len(in.rest) > 0 {
		n := copy(p, in.rest)
		in.rest = in.rest[n:]
		return n, nil
	}
	for {
		typ, b, err := in.c.Read(in.ctx)
		if err != nil {
			if in.end != nil {
				in.end()
			}
			return 0, err
		}
		switch typ {
		case websocket.MessageBinary:
			if len(b) == 0 {
				continue
			}
			n := copy(p, b)
			in.rest = b[n:]
			return n, nil
		case websocket.MessageText:
			var f api.Frame
			if json.Unmarshal(b, &f) != nil || f.Type != api.FrameResize || in.resize == nil {
				continue
			}
			if api.CheckSize(f.W, f.H) == nil {
				in.resize(f.W, f.H)
			}
		}
	}
}

// wsOut is a stream's output: one binary message per Write, each bounded.
type wsOut struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (o *wsOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), streamWriteTimeout)
	defer cancel()
	if err := o.c.Write(ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
