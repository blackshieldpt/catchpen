package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// promptQueue bounds the prompt messages a stream may have queued: a client
// that can't keep up is disconnected, not allowed to delay a decision.
const promptQueue = 16

// promptAnswer is a client's decision for one prompt, and who gave it.
type promptAnswer struct {
	decision egress.Decision
	client   string
}

// promptWait is one prompt awaiting an answer.
type promptWait struct {
	msg api.Prompt
	res chan promptAnswer
}

// prompts is the API's side of the daemon's egress prompts: eligible
// connected clients are asked, the first answer decides, and with none
// eligible the daemon's fallback (desktop notifications) is used.
type prompts struct {
	mu      sync.Mutex
	streams map[*promptStream]struct{}
	pending map[string]*promptWait
}

func newPrompts() *prompts {
	return &prompts{streams: map[*promptStream]struct{}{}, pending: map[string]*promptWait{}}
}

// promptStream is one connected client.
type promptStream struct {
	conn   *websocket.Conn
	caller Caller // Name is api.ClientLocal over the unix socket
	local  bool   // the unix socket: every scope
	send   chan api.Prompt
	done   chan struct{}
	once   sync.Once
}

func (st *promptStream) close() { st.once.Do(func() { close(st.done) }) }

// add registers a stream, sends it the prompts already pending, and starts
// its writer.
func (p *prompts) add(st *promptStream) {
	p.mu.Lock()
	p.streams[st] = struct{}{}
	for _, w := range p.pending {
		if !answerAllowed(st, w.msg.VM) { // it can't answer this one
			continue
		}
		select { // a prompt asked before it connected
		case st.send <- w.msg:
		default: // its queue is full: it misses this one, which the others were asked
		}
	}
	p.mu.Unlock()
	go func() {
		defer func() {
			_ = st.conn.CloseNow()
			p.remove(st)
		}()
		for {
			select {
			case <-st.done:
				return
			case msg := <-st.send:
				b, err := json.Marshal(msg)
				if err != nil {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), streamWriteTimeout)
				err = st.conn.Write(ctx, websocket.MessageText, b)
				cancel()
				if err != nil {
					st.close()
					return
				}
			}
		}
	}()
}

// remove unregisters a stream and stops its writer.
func (p *prompts) remove(st *promptStream) {
	p.mu.Lock()
	delete(p.streams, st)
	p.mu.Unlock()
	st.close()
}

// connected is how many streams are open (tests use it to wait).
func (p *prompts) connected() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.streams)
}

// pendingCount is how many prompts wait for an answer (tests use it).
func (p *prompts) pendingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

// eligibleLocked reports whether a connected client may answer a prompt for
// vmName. p.mu is held.
func (p *prompts) eligibleLocked(vmName string) bool {
	for st := range p.streams {
		if answerAllowed(st, vmName) {
			return true
		}
	}
	return false
}

// sendLocked enqueues msg to every client that may answer a prompt for vm
// (a done carries the prompt's VM too); one whose queue is full can't keep
// up and is disconnected. p.mu is held.
func (p *prompts) sendLocked(msg api.Prompt, vmName string) {
	for st := range p.streams {
		if !answerAllowed(st, vmName) {
			continue
		}
		select {
		case st.send <- msg:
		default:
			delete(p.streams, st)
			st.close()
		}
	}
}

// sendOne enqueues msg to one stream only.
func (p *prompts) sendOne(st *promptStream, msg api.Prompt) {
	select {
	case st.send <- msg:
	default:
	}
}

// ask sends r's prompt to every client that may answer it and waits for the
// first valid answer or ctx's end. false means no client is eligible: the
// caller falls back (the desktop). A timeout answers deny (with no client to
// audit); an answer names the client that gave it.
func (p *prompts) ask(ctx context.Context, r egress.Request) (promptAnswer, bool) {
	p.mu.Lock()
	if !p.eligibleLocked(r.VM) {
		p.mu.Unlock()
		return promptAnswer{}, false
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	w := &promptWait{msg: api.Prompt{Type: api.PromptAsk, ID: hex.EncodeToString(id),
		VM: r.VM, Host: r.Host, Port: r.Port, Method: r.Method}, res: make(chan promptAnswer, 1)}
	p.pending[w.msg.ID] = w
	p.sendLocked(w.msg, r.VM)
	p.mu.Unlock()
	select {
	case a := <-w.res:
		return a, true
	case <-ctx.Done():
		p.mu.Lock()
		if _, waiting := p.pending[w.msg.ID]; !waiting {
			// An answer won the deadline race; it is already in res, sent
			// under this lock (see answer).
			p.mu.Unlock()
			select {
			case a := <-w.res:
				return a, true
			default:
				return promptAnswer{decision: egress.DecisionDeny}, true
			}
		}
		delete(p.pending, w.msg.ID)
		p.sendLocked(api.Prompt{Type: api.PromptDone, ID: w.msg.ID,
			Error: &api.Error{Code: api.CodeConflict, Message: "the prompt expired"}}, w.msg.VM)
		p.mu.Unlock()
		return promptAnswer{decision: egress.DecisionDeny}, true
	}
}

// answer takes one client's answer. A refusal goes only to that client; the
// prompt stays pending for the others.
func (p *prompts) answer(st *promptStream, id, decision string) {
	refuse := func(code, msg string) {
		p.sendOne(st, api.Prompt{Type: api.PromptDone, ID: id, Error: &api.Error{Code: code, Message: msg}})
	}
	if !api.ValidDecision(decision) {
		refuse(api.CodeBadRequest, "decision must be allow-once, allow-always, deny or deny-always")
		return
	}
	p.mu.Lock()
	w, ok := p.pending[id]
	if !ok {
		p.mu.Unlock()
		refuse(api.CodeNotFound, "no such prompt: it may have expired")
		return
	}
	if !answerAllowed(st, w.msg.VM) {
		p.mu.Unlock()
		refuse(api.CodeForbidden, "answering a task VM's egress prompt needs the config:write scope")
		return
	}
	delete(p.pending, id)
	// The decision goes into res before the lock is dropped, so an ask that
	// sees its prompt gone (the deadline branch) finds the answer waiting.
	select {
	case w.res <- promptAnswer{decision: egress.Decision(decision), client: st.caller.Name}:
	default: // the ask gave up first
	}
	p.sendLocked(api.Prompt{Type: api.PromptDone, ID: id, Decision: decision}, w.msg.VM)
	p.mu.Unlock()
}

// answerAllowed is whether st may answer a prompt for vmName: a task's VM
// needs config:write, unless the client is the unix socket's user.
func answerAllowed(st *promptStream, vmName string) bool {
	return !strings.HasPrefix(vmName, taskVMPrefix) || st.local || slices.Contains(st.caller.Scopes, ScopeConfigWrite)
}

// PromptDecider wraps inner, the daemon's fallback (desktop notifications),
// with the API's streams: with none eligible inner answers; an answer from a
// client is audited with its name.
func (s *Server) PromptDecider(inner egress.Decider) egress.Decider {
	return func(ctx context.Context, r egress.Request) egress.Decision {
		a, ok := s.prompts.ask(ctx, r)
		if !ok {
			return inner(ctx, r)
		}
		if a.client != "" { // an answer, not the prompt's expiry
			if s.Manager == nil {
				if s.Logf != nil {
					s.Logf("egress prompt: no manager to audit the answer from %s", a.client)
				}
				return a.decision
			}
			if m, err := s.Manager(); err != nil {
				if s.Logf != nil {
					s.Logf("egress prompt: audit: %v", err)
				}
			} else {
				m.Client = a.client
				m.AuditEvent(audit.EgressPrompt, r.VM, map[string]any{"host": r.Host,
					"port": r.Port, "method": r.Method, "decision": string(a.decision)})
			}
		}
		return a.decision
	}
}

// egressPrompts is GET /v1/egress/prompts, upgraded to a WebSocket: the
// prompts, and the client's answers.
func (s *Server) egressPrompts(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(w, r) {
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept answered
	}
	defer c.CloseNow()
	c.SetReadLimit(api.MaxStreamMessage)
	caller, ok := CallerOf(r.Context())
	st := &promptStream{conn: c, caller: caller, local: !ok,
		send: make(chan api.Prompt, promptQueue), done: make(chan struct{})}
	if !ok {
		st.caller.Name = api.ClientLocal
	}
	s.prompts.add(st)
	defer s.prompts.remove(st)
	for {
		typ, b, err := c.Read(r.Context())
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg api.Prompt
		if json.Unmarshal(b, &msg) != nil || msg.Type != api.PromptAnswer {
			continue
		}
		s.prompts.answer(st, msg.ID, msg.Decision)
	}
}
