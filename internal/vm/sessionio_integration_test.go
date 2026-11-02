//go:build integration

package vm

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/record"
)

// pipeOut is a session's output, safe to read while the session writes it.
type pipeOut struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (p *pipeOut) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.b.Write(b)
}

func (p *pipeOut) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.b.String()
}

// pipeSession is a Session running on pipes, as the API will run them.
type pipeSession struct {
	t      *testing.T
	in     *io.PipeWriter
	out    pipeOut
	resize chan record.Winsize
	done   chan int
}

func startPipeSession(t *testing.T, ctx context.Context, m *Manager, name, kind, remote string) *pipeSession {
	t.Helper()
	inR, inW := io.Pipe()
	p := &pipeSession{t: t, in: inW, resize: make(chan record.Winsize, 1), done: make(chan int, 1)}
	s := record.IO{In: inR, Out: &p.out, Err: &p.out, Size: func() (int, int) { return 80, 24 }, Resize: p.resize, Term: "xterm-256color"}
	go func() {
		code, err := m.Session(ctx, name, kind, remote, s)
		if err != nil {
			t.Errorf("%s session: %v", kind, err)
		}
		p.done <- code
	}()
	t.Cleanup(func() { inW.Close() })
	return p
}

func (p *pipeSession) send(s string) { _, _ = io.WriteString(p.in, s) }

func (p *pipeSession) waitFor(s string) {
	p.t.Helper()
	for end := time.Now().Add(60 * time.Second); !strings.Contains(p.out.String(), s); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(end) {
			p.t.Fatalf("no %q in %q", s, p.out.String())
		}
	}
}

func (p *pipeSession) wait() int {
	p.t.Helper()
	select {
	case code := <-p.done:
		return code
	case <-time.After(60 * time.Second):
		p.t.Fatalf("session didn't end:\n%q", p.out.String())
		return -1
	}
}

// Sessions on pipes: ssh and attach get a tty of the given size, follow a
// resize, and are recorded with it; with recording off they still get one.
func TestSessionsOverPipes(t *testing.T) {
	m := integrationManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	if _, err := m.Create(ctx, "sp1", CreateOpts{Template: "minimal"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("sp1") })
	if _, err := m.Start(ctx, "sp1"); err != nil {
		t.Fatal(err)
	}
	s, err := m.Load("sp1")
	if err != nil {
		t.Fatal(err)
	}
	resized := func(kind string) bool {
		ids, _ := m.Recordings("sp1")
		for _, id := range ids {
			if b, _ := os.ReadFile(m.RecordingPath("sp1", id)); strings.Contains(id, "-"+kind+"-") && bytes.Contains(b, []byte(`"r","100x40"`)) {
				return true
			}
		}
		return false
	}

	p := startPipeSession(t, ctx, m, "sp1", KindSSH, "")
	p.send("stty size; echo mark-$((40+2))\n")
	p.waitFor("24 80")
	p.waitFor("mark-42")
	p.resize <- record.Winsize{W: 100, H: 40}
	time.Sleep(time.Second)
	p.send("stty size; exit 3\n")
	p.waitFor("40 100")
	if code := p.wait(); code != 3 {
		t.Errorf("ssh: exit %d\n%q", code, p.out.String())
	}
	if !recordingWith(t, m, "sp1", KindSSH, "mark-42") || !resized(KindSSH) {
		t.Error("ssh session over pipes not recorded with its resize")
	}

	p = startPipeSession(t, ctx, m, "sp1", KindAttach, AttachCommand(s))
	time.Sleep(2 * time.Second)
	p.resize <- record.Winsize{W: 100, H: 40}
	time.Sleep(time.Second)
	p.send("echo att-$((2+3)); stty size\n")
	p.waitFor("att-5")
	p.waitFor("39 100") // tmux's status line takes a row
	p.send("\x01d")
	p.waitFor("[detached")
	if code := p.wait(); code != 0 {
		t.Errorf("attach: exit %d", code)
	}
	if !recordingWith(t, m, "sp1", KindAttach, "att-5") || !resized(KindAttach) {
		t.Error("attach over pipes not recorded with its resize")
	}

	// A command over pipes (no tty) ends when ssh does, though its input
	// never ends.
	p = startPipeSession(t, ctx, m, "sp1", KindCommand, "echo cmd-$((3+4))")
	p.waitFor("cmd-7")
	if code := p.wait(); code != 0 {
		t.Errorf("command: exit %d", code)
	}

	off := false
	m.Cfg.Record = &off
	before, _ := m.Recordings("sp1")
	p = startPipeSession(t, ctx, m, "sp1", KindSSH, "")
	p.send("[ -t 0 ] && echo on-a-$((6*7))-tty; exit\n")
	p.waitFor("on-a-42-tty")
	if code := p.wait(); code != 0 {
		t.Errorf("unrecorded ssh: exit %d", code)
	}
	if after, _ := m.Recordings("sp1"); len(after) != len(before) {
		t.Errorf("recording off, but %d recordings became %d", len(before), len(after))
	}
}
