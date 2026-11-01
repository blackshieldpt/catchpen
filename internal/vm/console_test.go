package vm

import (
	"bytes"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// consoleVM fakes a running VM named name whose serial socket is served by
// serve.
func consoleVM(t *testing.T, m *Manager, name string, serve func(net.Conn)) {
	t.Helper()
	fakeQEMU(t, m, name, 512)
	writeFile(t, filepath.Join(m.Dir(name), runtimeFile), `{"ssh_port":2222}`)
	l, err := net.Listen("unix", m.SerialSocket(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		serve(c)
	}()
}

func pump(t *testing.T, conn net.Conn, in io.Reader, out io.Writer) {
	t.Helper()
	done := make(chan struct{})
	go func() { PumpConsole(conn, in, out); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PumpConsole did not return")
	}
}

// The console shows the guest's output without the sequences that act on the
// terminal, even one split across writes, and ends when the guest closes it.
func TestConsoleFiltersGuestOutput(t *testing.T) {
	m := testManager(t)
	consoleVM(t, m, "c1", func(c net.Conn) {
		c.Write([]byte("boot \x1b[1mbold\x1b[0m\x1b]52;c;"))
		time.Sleep(50 * time.Millisecond) // the rest arrives in another read
		c.Write([]byte("ZXZpbA==\x07 \x1b]0;title\x07ok\r\n"))
	})
	conn, err := m.DialConsole("c1")
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe() // input that never ends: only the guest can end this console
	defer inW.Close()
	var out bytes.Buffer
	pump(t, conn, inR, &out)
	if got, want := out.String(), "boot \x1b[1mbold\x1b[0m ok\r\n"; got != want {
		t.Errorf("console showed %q, want %q", got, want)
	}
}

// The console sends the caller's input to the guest, and the caller detaches
// by ending it while the guest stays connected.
func TestConsoleSendsInputUntilItEnds(t *testing.T) {
	m := testManager(t)
	got := make(chan string, 1)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	consoleVM(t, m, "c2", func(c net.Conn) {
		b, _ := io.ReadAll(c)
		got <- string(b)
		<-hold // the guest never hangs up
	})
	conn, err := m.DialConsole("c2")
	if err != nil {
		t.Fatal(err)
	}
	pump(t, conn, strings.NewReader("root\r"), io.Discard)
	select {
	case s := <-got:
		if s != "root\r" {
			t.Errorf("guest read %q, want %q", s, "root\r")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the console stayed open after its input ended")
	}
}

// blockedWriter holds its first Write until release is closed.
type blockedWriter struct {
	writing chan struct{}
	release chan struct{}
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	select {
	case <-w.writing:
	default:
		close(w.writing)
	}
	<-w.release
	return len(p), nil
}

// The console doesn't return while it is still writing the guest's output,
// so a caller can reuse out as soon as it returns.
func TestConsoleWritesNothingAfterReturn(t *testing.T) {
	m := testManager(t)
	consoleVM(t, m, "c3", func(c net.Conn) {
		c.Write([]byte("x"))
		io.Copy(io.Discard, c)
	})
	conn, err := m.DialConsole("c3")
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	out := &blockedWriter{writing: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { PumpConsole(conn, inR, out); close(done) }()
	<-out.writing
	inW.Close() // detach while the output is being written
	select {
	case <-done:
		t.Fatal("PumpConsole returned while still writing to out")
	case <-time.After(200 * time.Millisecond):
	}
	close(out.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PumpConsole did not return")
	}
}

// Like a terminal, the console refuses a VM that is stopped or paused by a
// canary hit, without connecting to it.
func TestDialConsoleRefuses(t *testing.T) {
	m := testManager(t)
	if _, err := m.DialConsole("off1"); err == nil || !strings.Contains(err.Error(), "is not running") {
		t.Errorf("stopped VM: %v", err)
	}
	first := make(chan string, 1)
	consoleVM(t, m, "p1", func(c net.Conn) {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		b := make([]byte, 5)
		n, _ := io.ReadFull(c, b)
		first <- string(b[:n])
	})
	writeFile(t, filepath.Join(m.Dir("p1"), pausedFile), "{}")
	if _, err := m.DialConsole("p1"); err == nil || !strings.Contains(err.Error(), "paused by a canary hit") {
		t.Errorf("paused VM: %v", err)
	}
	// QEMU serves one console client at a time, so a refused dial must not
	// have connected: the socket's first connection must be this probe.
	probe, err := net.Dial("unix", m.SerialSocket("p1"))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	probe.Write([]byte("probe"))
	if s := <-first; s != "probe" {
		t.Errorf("the refused dial connected first (its connection read %q)", s)
	}
}
