package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request is an egress attempt awaiting a decision.
type Request struct {
	VM, Host, Method string
	Port             int
}

type Decision string

const (
	DecisionAllowOnce   Decision = "allow-once"
	DecisionAllowAlways Decision = "allow-always"
	DecisionDeny        Decision = "deny"
	DecisionDenyAlways  Decision = "deny-always"
)

func (d Decision) valid() bool {
	switch d {
	case DecisionAllowOnce, DecisionAllowAlways, DecisionDeny, DecisionDenyAlways:
		return true
	}
	return false
}

func (d Decision) allows() bool { return d == DecisionAllowOnce || d == DecisionAllowAlways }

// Decider returns the user's decision for r; it must return when ctx is done.
type Decider func(ctx context.Context, r Request) Decision

// maxPending caps distinct undecided targets per VM, so a guest can't queue
// an unbounded number of prompts.
const maxPending = 8

type pending struct {
	vm      string
	done    chan struct{}
	d       Decision
	note    string
	waiters int
	cancel  context.CancelFunc
}

type watcher struct {
	ctx     context.Context
	decide  Decider
	mu      sync.Mutex
	pending map[string]*pending
	perVM   map[string]int
	serial  sync.Mutex
}

// Watch serves prompt requests from each VM's egress.sock until ctx is done.
// Concurrent requests for the same vm+host+port share one decision; decisions
// are made one at a time, and requests whose proxies gave up are dropped
// without prompting.
func Watch(ctx context.Context, vms map[string]string, decide Decider) error {
	w := &watcher{ctx: ctx, decide: decide, pending: map[string]*pending{}, perVM: map[string]int{}}
	type listener struct {
		ln       net.Listener
		vm, dir  string
		sockPath string
	}
	var lns []listener
	cleanup := func() {
		for _, l := range lns {
			l.ln.Close()
			os.Remove(l.sockPath)
		}
	}
	for vm, dir := range vms {
		path := filepath.Join(dir, SocketFile)
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			cleanup()
			return fmt.Errorf("another egress watcher is already serving %s", vm)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return err
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			cleanup()
			return err
		}
		lns = append(lns, listener{ln, vm, dir, path})
		if err := os.Chmod(path, 0o600); err != nil {
			cleanup()
			return err
		}
	}

	var wg sync.WaitGroup
	for _, l := range lns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				conn, err := l.ln.Accept()
				if err != nil {
					return
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					w.handle(conn, l.vm, l.dir)
				}()
			}
		}()
	}
	<-ctx.Done()
	cleanup()
	wg.Wait()
	return nil
}

func (w *watcher) handle(conn net.Conn, vm, dir string) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadBytes('\n')
	if err != nil {
		return
	}
	var pr promptRequest
	if err := json.Unmarshal(line, &pr); err != nil || pr.Host == "" || pr.Port < 1 || pr.Port > 65535 {
		return
	}
	reply := func(d Decision, note string) {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		json.NewEncoder(conn).Encode(promptReply{d, note})
	}
	deadline := time.Unix(0, pr.Deadline)
	if pr.Deadline <= 0 || !time.Now().Before(deadline) {
		reply(DecisionDeny, "expired")
		return
	}
	// The request lives until its deadline or until the proxy hangs up.
	ctx, cancel := context.WithDeadline(w.ctx, deadline)
	defer cancel()
	conn.SetReadDeadline(time.Time{})
	go func() {
		var b [1]byte
		conn.Read(b[:])
		cancel()
	}()
	// The VM name comes from the socket, not the payload.
	r := Request{VM: vm, Host: normalizeHost(pr.Host), Port: pr.Port, Method: pr.Method}
	d, note := w.decision(ctx, r, dir)
	if ctx.Err() == nil {
		reply(d, note)
	}
}

// decision joins (or starts) the shared decision for r's target and waits for
// it until ctx is done. The prompt is cancelled once every waiter has left.
func (w *watcher) decision(ctx context.Context, r Request, dir string) (Decision, string) {
	key := fmt.Sprintf("%s\x00%s\x00%d", r.VM, r.Host, r.Port)
	w.mu.Lock()
	p, ok := w.pending[key]
	if !ok {
		if w.perVM[r.VM] >= maxPending {
			w.mu.Unlock()
			return DecisionDeny, "too many pending prompts"
		}
		dctx, cancel := context.WithCancel(w.ctx)
		p = &pending{vm: r.VM, done: make(chan struct{}), cancel: cancel}
		w.pending[key] = p
		w.perVM[r.VM]++
		go w.run(dctx, key, p, r, dir)
	}
	p.waiters++
	w.mu.Unlock()

	select {
	case <-p.done:
		return p.d, p.note
	case <-ctx.Done():
		w.mu.Lock()
		if p.waiters--; p.waiters == 0 {
			p.cancel()
		}
		w.mu.Unlock()
		return DecisionDeny, "timeout"
	}
}

func (w *watcher) run(ctx context.Context, key string, p *pending, r Request, dir string) {
	defer p.cancel()
	d, note := DecisionDeny, ""
	w.serial.Lock()
	if ctx.Err() == nil {
		d = w.decide(ctx, r)
		if !d.valid() {
			d = DecisionDeny
		}
	}
	w.serial.Unlock()

	entry := hostPort(r.Host, r.Port)
	switch d {
	case DecisionAllowAlways:
		if err := AddEntry(dir, AllowlistFile, entry); err != nil {
			note = "allowlist update failed: " + err.Error()
		}
	case DecisionDenyAlways:
		if err := AddEntry(dir, DenylistFile, entry); err != nil {
			note = "denylist update failed: " + err.Error()
		}
	}

	w.mu.Lock()
	delete(w.pending, key)
	if w.perVM[p.vm]--; w.perVM[p.vm] == 0 {
		delete(w.perVM, p.vm)
	}
	p.d, p.note = d, note
	w.mu.Unlock()
	close(p.done)
}

// TerminalDecider prompts on out and reads answers from in. Only input typed
// while a prompt is shown counts; unknown input re-prompts; EOF or ctx done
// denies.
func TerminalDecider(in io.Reader, out io.Writer) Decider {
	lines := make(chan string, 16)
	var active atomic.Bool
	go func() {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			if !active.Load() {
				continue // type-ahead between prompts
			}
			select {
			case lines <- sc.Text():
			default:
			}
		}
		close(lines)
	}()
	return func(ctx context.Context, r Request) Decision {
		active.Store(true)
		defer active.Store(false)
		for drained := false; !drained; {
			select {
			case l, ok := <-lines:
				if !ok {
					return DecisionDeny
				}
				_ = l
			default:
				drained = true
			}
		}
		for {
			fmt.Fprintf(out, "%s  [%s %s] — (a)llow once / (A)llow always / (d)eny / (D)eny always [d]: ",
				hostPort(r.Host, r.Port), r.VM, r.Method)
			select {
			case <-ctx.Done():
				fmt.Fprintln(out)
				return DecisionDeny
			case l, ok := <-lines:
				if !ok {
					fmt.Fprintln(out)
					return DecisionDeny
				}
				switch strings.TrimSpace(l) {
				case "a":
					return DecisionAllowOnce
				case "A":
					return DecisionAllowAlways
				case "d", "":
					return DecisionDeny
				case "D":
					return DecisionDenyAlways
				}
			}
		}
	}
}

// DesktopDecider asks via a desktop notification with actions (notify-send).
// Errors, dismissal and timeouts deny.
func DesktopDecider() Decider {
	return func(ctx context.Context, r Request) Decision {
		cmd := exec.CommandContext(ctx, "notify-send", "--app-name=curral", "--wait",
			"--action=allow-once=Allow", "--action=allow-always=Always allow", "--action=deny=Deny",
			"curral: "+hostPort(r.Host, r.Port), r.VM+" "+r.Method)
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		if err != nil {
			return DecisionDeny
		}
		if d := Decision(strings.TrimSpace(string(out))); d.valid() {
			return d
		}
		return DecisionDeny
	}
}
