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
		c.line([]any{t, "m", "recording stopped: size limit reached"})
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

// Run runs cmd on a pty connected to the terminal (stdin in raw mode),
// teeing its output to rec. It returns cmd's exit status.
func Run(cmd *exec.Cmd, rec *Cast) (int, error) {
	w, h := Size()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	if err != nil {
		return -1, err
	}
	defer ptmx.Close()
	if st, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), st)
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-winch:
				if err := pty.InheritSize(os.Stdout, ptmx); err == nil {
					w, h := Size()
					rec.Resize(w, h)
				}
			}
		}
	}()
	stopIn := pumpStdin(ptmx)
	_, _ = io.Copy(io.MultiWriter(os.Stdout, rec), ptmx) // ends with EIO when the child exits
	stopIn()
	return ExitStatus(cmd.Wait())
}

// pumpStdin copies the terminal's input to w until the returned func is
// called, which returns only once nothing reads stdin any more, so input typed
// after the session goes to whatever runs next. It polls instead of making
// stdin non-blocking, which would also affect stdout (same open file).
func pumpStdin(w io.Writer) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	fd := int(os.Stdin.Fd())
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
