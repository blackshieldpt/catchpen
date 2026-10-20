package record

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func readCast(t *testing.T, path string) (Header, [][]any) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	var h Header
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	var evs [][]any
	for sc.Scan() {
		var ev []any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad event line %q: %v", sc.Text(), err)
		}
		evs = append(evs, ev)
	}
	return h, evs
}

func output(evs [][]any) string {
	var b strings.Builder
	for _, e := range evs {
		if e[1] == "o" {
			b.WriteString(e[2].(string))
		}
	}
	return b.String()
}

func TestCast(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r", "a.cast")
	c, err := Create(p, 100, 30, "curral ssh vm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(p, 1, 1, ""); err == nil {
		t.Error("existing recording overwritten")
	}
	c.Write([]byte("héllo \xe2\x82")) // "€" split across writes
	c.Write([]byte("\xac \x1b[31mred\x1b[0m"))
	c.Resize(120, 40)
	c.Write([]byte("bad \xff byte"))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	h, evs := readCast(t, p)
	if h.Version != 2 || h.Width != 100 || h.Height != 30 || h.Title != "curral ssh vm" {
		t.Errorf("header %+v", h)
	}
	if got := output(evs); got != "héllo € \x1b[31mred\x1b[0mbad � byte" {
		t.Errorf("output %q", got)
	}
	var resized bool
	for _, e := range evs {
		if e[1] == "r" && e[2] == "120x40" {
			resized = true
		}
	}
	if !resized {
		t.Errorf("no resize event: %v", evs)
	}
	in, err := Stat(p)
	if err != nil || in.Width != 100 || in.Size != st.Size() {
		t.Errorf("Stat = %+v, %v", in, err)
	}
}

func TestCastSizeLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.cast")
	c, err := Create(p, 80, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	c.max = 1000
	for i := 0; i < 10; i++ {
		c.Write(bytes.Repeat([]byte("\x01"), 50)) // 6 bytes each once JSON-escaped
	}
	c.Close()
	if st, _ := os.Stat(p); st.Size() > c.max+100 {
		t.Errorf("recording is %d bytes, cap %d", st.Size(), c.max)
	}
	_, evs := readCast(t, p)
	last := evs[len(evs)-1]
	if last[1] != "m" || !strings.Contains(last[2].(string), "size limit") {
		t.Errorf("last event %v", last)
	}
	if n := len(output(evs)); n == 0 || n >= 500 {
		t.Errorf("recorded %d bytes", n)
	}
}

func TestPlay(t *testing.T) {
	cast := `{"version":2,"width":80,"height":24,"timestamp":1}
[0.1,"o","a"]
[0.2,"r","100x30"]
[5.0,"o","b\u001b[0m"]
[9.0,"m","marker"]
`
	var out bytes.Buffer
	start := time.Now()
	if err := Play(strings.NewReader(cast), &out, 1, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ab\x1b[0m" {
		t.Errorf("played %q", out.String())
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("idle not capped: %v", d)
	}
	if err := Play(strings.NewReader("{\"version\":1}\n"), &out, 1, 0); err == nil {
		t.Error("v1 recording accepted")
	}
}

func TestRun(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.cast")
	c, err := Create(p, 80, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	code, err := Run(exec.Command("sh", "-c", "[ -t 1 ] && printf 'on a tty'; exit 3"), c)
	c.Close()
	if err != nil || code != 3 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	if _, evs := readCast(t, p); !strings.Contains(output(evs), "on a tty") {
		t.Errorf("recording: %q", output(evs))
	}
}

// TestRunOnTerminal runs a session on a real terminal: raw mode is restored,
// resizes reach the child and the recording, and input typed after the
// session is left for the next reader.
func TestRunOnTerminal(t *testing.T) {
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer ptm.Close()
	defer tty.Close()
	pty.Setsize(ptm, &pty.Winsize{Rows: 24, Cols: 80})
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = tty, tty
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	before, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var screen bytes.Buffer
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := ptm.Read(b)
			mu.Lock()
			screen.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	waitFor := func(s string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			mu.Lock()
			ok := strings.Contains(screen.String(), s)
			mu.Unlock()
			if ok {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no %q on screen: %q", s, screen.String())
	}

	p := filepath.Join(t.TempDir(), "a.cast")
	c, err := Create(p, 80, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan int, 1)
	go func() {
		code, err := Run(exec.Command("sh", "-c", "echo ready; read x; stty size; read y; echo got-$y"), c)
		if err != nil {
			t.Error(err)
		}
		res <- code
	}()
	waitFor("ready")
	pty.Setsize(ptm, &pty.Winsize{Rows: 40, Cols: 100})
	syscall.Kill(os.Getpid(), syscall.SIGWINCH)
	time.Sleep(300 * time.Millisecond)
	ptm.WriteString("a\r")
	waitFor("40 100")
	ptm.WriteString("b\r")
	waitFor("got-b")
	if code := <-res; code != 0 {
		t.Errorf("exit %d", code)
	}
	c.Close()

	after, _ := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if after.Lflag != before.Lflag || after.Iflag != before.Iflag || after.Oflag != before.Oflag {
		t.Errorf("terminal mode not restored: %+v -> %+v", before, after)
	}
	_, evs := readCast(t, p)
	var resized bool
	for _, e := range evs {
		resized = resized || (e[1] == "r" && e[2] == "100x40")
	}
	if !resized || !strings.Contains(output(evs), "got-b") {
		t.Errorf("recording: resized=%v output=%q", resized, output(evs))
	}
	// Input after the session must not be swallowed by a leftover reader.
	ptm.WriteString("after\n")
	line := make(chan string, 1)
	go func() {
		b := make([]byte, 64)
		n, _ := tty.Read(b)
		line <- string(b[:n])
	}()
	select {
	case l := <-line:
		if !strings.HasPrefix(l, "after") {
			t.Errorf("next reader got %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Error("input after the session was swallowed")
	}
}
