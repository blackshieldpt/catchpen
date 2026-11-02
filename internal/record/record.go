// Package record runs interactive sessions through a host pty and records
// their output as asciicast v2, on the host where the guest can't alter it.
package record

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// MaxBytes caps one recording; later output is shown but not recorded.
const MaxBytes = 256 << 20

// Truncated is the marker event that ends a recording which reached MaxBytes.
const Truncated = "recording stopped: size limit reached"

// Header is the asciicast v2 header line.
type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Cast is an asciicast v2 file being written.
type Cast struct {
	mu    sync.Mutex
	f     *os.File
	w     *bufio.Writer
	start time.Time
	n     int64
	max   int64
	full  bool
	pend  []byte // incomplete UTF-8 sequence from the last write
	err   error
}

// Create starts a recording at path (0600, never overwriting).
func Create(path string, width, height int, title string) (*Cast, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	c := &Cast{f: f, w: bufio.NewWriterSize(f, 64<<10), start: time.Now(), max: MaxBytes}
	h := Header{Version: 2, Width: width, Height: height, Timestamp: c.start.Unix(), Title: title,
		Env: map[string]string{"TERM": os.Getenv("TERM"), "SHELL": os.Getenv("SHELL")}}
	c.line(h)
	if c.err != nil {
		f.Close()
		return nil, c.err
	}
	return c, nil
}

func (c *Cast) line(v any) {
	if c.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		c.err = err
		return
	}
	c.write(b)
}

func (c *Cast) write(b []byte) {
	b = append(b, '\n')
	c.n += int64(len(b))
	_, c.err = c.w.Write(b)
}

func (c *Cast) event(kind, data string) {
	if c.full || c.err != nil {
		return
	}
	t := float64(time.Since(c.start).Microseconds()) / 1e6
	b, err := json.Marshal([]any{t, kind, data})
	if err != nil {
		c.err = err
		return
	}
	// The cap applies to the encoded size: escaping can make a line ~6x its input.
	if c.n+int64(len(b))+1 > c.max {
		c.full = true
		c.line([]any{t, "m", Truncated})
		if c.err == nil { // on disk now, not at Close: the session may go on for hours
			c.err = c.w.Flush()
		}
		return
	}
	c.write(b)
}

// Write records terminal output. Multi-byte characters split across writes
// are kept whole.
func (c *Cast) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := append(c.pend, p...)
	cut := len(b)
	// Hold back a trailing incomplete UTF-8 sequence (at most 3 bytes).
	for i := len(b) - 1; i >= 0 && i >= len(b)-3; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				cut = i
			}
			break
		}
	}
	c.pend = append([]byte(nil), b[cut:]...)
	if cut > 0 {
		c.event("o", string(bytes.ToValidUTF8(b[:cut], []byte("�"))))
	}
	return len(p), nil
}

// Resize records a terminal size change.
func (c *Cast) Resize(width, height int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.event("r", fmt.Sprintf("%dx%d", width, height))
}

// Close flushes and closes the file.
func (c *Cast) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pend) > 0 {
		c.event("o", string(bytes.ToValidUTF8(c.pend, []byte("�"))))
		c.pend = nil
	}
	if c.err == nil {
		c.err = c.w.Flush()
	}
	if err := c.f.Close(); c.err == nil {
		c.err = err
	}
	return c.err
}

// Winsize is a terminal's size in columns and rows.
type Winsize struct{ W, H int }

// IO is what a session runs on; In and Out are required. With Size nil
// there is no terminal: the session gets no pty and isn't recorded. When In
// is this process's own terminal (an *os.File), Run puts it in raw mode and
// stops reading it as soon as the session ends. Any other In is read by a
// goroutine nobody waits for: it ends when In returns from a Read, so the
// caller must end In when the session ends. Out and Err may be written at
// the same time, from different goroutines, and a write that blocks stalls
// the session. All of them must be usable values, not typed nils.
type IO struct {
	In     io.Reader
	Out    io.Writer
	Err    io.Writer         // ssh's own messages, not recorded (nil: discarded)
	Size   func() (w, h int) // the size at the start
	Resize <-chan Winsize    // later sizes (nil: it never changes)
	Term   string            // TERM for the session ("": this process's)
}

// OnTerminal reports whether s.In is a terminal of this process.
func (s IO) OnTerminal() bool {
	f, ok := s.In.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Stdio is the IO of this process's stdin, stdout and stderr. On a
// terminal it has the terminal's size, and its resizes (SIGWINCH) until
// stop is called; otherwise Size is nil.
func Stdio() (s IO, stop func()) {
	s = IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
	if !IsTerminal() {
		return s, func() {}
	}
	s.Size = Size
	ch := make(chan Winsize, 1)
	s.Resize = ch
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-quit:
				return
			case <-winch:
				w, h, err := term.GetSize(int(os.Stdout.Fd()))
				if err != nil {
					continue
				}
				select { // only the latest size matters
				case <-ch:
				default:
				}
				ch <- Winsize{w, h}
			}
		}
	}()
	return s, func() {
		signal.Stop(winch)
		close(quit)
		<-done
	}
}

// IsTerminal reports whether stdin and stdout are terminals.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// Size returns the terminal size of stdout.
func Size() (width, height int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80, 24
	}
	return w, h
}

// Run runs cmd on a pty of s's size, with s.In as its input and its output
// written to s.Out and teed to rec (nil: not recorded). s.Size must be set.
// It returns cmd's exit status.
func Run(cmd *exec.Cmd, rec *Cast, s IO) (int, error) {
	w, h := s.Size()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	if err != nil {
		return -1, err
	}
	defer ptmx.Close()
	stopIn := func() {}
	if s.OnTerminal() {
		fd := int(s.In.(*os.File).Fd())
		if st, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, st)
		}
		stopIn = pumpStdin(fd, ptmx)
	} else {
		go func() { _, _ = io.Copy(ptmx, s.In) }() // ends when In does, or at the next write
	}
	stopResize, resized := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(resized)
		for {
			select {
			case <-stopResize:
				return
			case sz, ok := <-s.Resize:
				if !ok {
					return
				}
				if err := pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(sz.W), Rows: uint16(sz.H)}); err == nil && rec != nil {
					rec.Resize(sz.W, sz.H)
				}
			}
		}
	}()
	out := s.Out
	if rec != nil {
		out = io.MultiWriter(s.Out, rec)
	}
	_, _ = io.Copy(out, ptmx) // ends with EIO when the child exits, or on a write error
	stopIn()
	close(stopResize)
	<-resized        // done with ptmx before it is closed
	_ = ptmx.Close() // a child still writing (Out failed) gets SIGHUP instead of blocking
	// One that ignores SIGHUP (started under nohup) is killed.
	kill := time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
	defer kill.Stop()
	return ExitStatus(cmd.Wait())
}

// pumpStdin copies the terminal fd's input to w until the returned func is
// called, which returns only once nothing reads fd any more, so input typed
// after the session goes to whatever runs next. It polls instead of making
// fd non-blocking, which would also affect stdout (same open file).
func pumpStdin(fd int, w io.Writer) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, err := unix.Poll(fds, 100)
			if err == unix.EINTR {
				continue
			}
			if err != nil || (n > 0 && fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0) {
				return
			}
			if n == 0 {
				continue
			}
			r, err := unix.Read(fd, buf)
			if r > 0 {
				if _, werr := w.Write(buf[:r]); werr != nil {
					return
				}
			}
			if r == 0 || (err != nil && err != unix.EINTR && err != unix.EAGAIN) {
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// ExitStatus turns a Wait error into a shell-style exit status (128+signal
// for a killed process).
func ExitStatus(err error) (int, error) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}
