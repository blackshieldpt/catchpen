package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// End is how a stream ended: the session's exit code, or the server's error
// (a console a canary pause ended, say). Either may be nil.
type End struct {
	Exit  *int
	Error *api.Error
}

// TerminalOptions says what a terminal runs. The zero value is an ssh login
// shell in a default 80x24.
type TerminalOptions struct {
	Kind  string   // api.TerminalSSH (default), TerminalRun or TerminalAttach
	Agent string   // Kind == api.TerminalRun: the agent, as catchpen run
	Args  []string // Kind == api.TerminalRun: its arguments
	Cols  int      // with Rows: the terminal's size (0: 80x24)
	Rows  int
}

// Terminal is an interactive session in a VM: the guest's bytes arrive from
// Read, what is written goes to it, and Resize follows the terminal. Close it
// when done; Read returns io.EOF at the server's end frame, which End then
// tells.
type Terminal struct{ wsStream }

// Console is a VM's serial console, already filtered by the server.
type Console struct{ wsStream }

// wsStream is what Terminal and Console share: the WebSocket, a partly read
// message, and the end frame.
type wsStream struct {
	c    *websocket.Conn
	mu   sync.Mutex // writes
	rest []byte
	end  *End
}

// Terminal opens an interactive session in vm.
func (c *Client) Terminal(ctx context.Context, vm string, o TerminalOptions) (*Terminal, error) {
	q := url.Values{}
	if o.Kind != "" {
		q.Set(api.QueryKind, o.Kind)
	}
	if o.Agent != "" {
		q.Set(api.QueryAgent, o.Agent)
	}
	for _, a := range o.Args {
		q.Add(api.QueryArg, a)
	}
	if o.Cols != 0 || o.Rows != 0 {
		if err := api.CheckSize(o.Cols, o.Rows); err != nil {
			return nil, err
		}
		q.Set(api.QueryCols, strconv.Itoa(o.Cols))
		q.Set(api.QueryRows, strconv.Itoa(o.Rows))
	}
	conn, err := c.stream(ctx, api.PathVMTerminal(vm), q)
	if err != nil {
		return nil, err
	}
	return &Terminal{wsStream{c: conn}}, nil
}

// Console opens vm's serial console.
func (c *Client) Console(ctx context.Context, vm string) (*Console, error) {
	conn, err := c.stream(ctx, api.PathVMConsole(vm), nil)
	if err != nil {
		return nil, err
	}
	return &Console{wsStream{c: conn}}, nil
}

// stream upgrades c's connection to path to a WebSocket; a refused handshake
// is an *api.Error.
func (c *Client) stream(ctx context.Context, path string, q url.Values) (*websocket.Conn, error) {
	u := strings.Replace(c.base, "http", "ws", 1) + path // https -> wss too
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.hc})
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			var er api.ErrorResponse
			if json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&er) == nil && er.Error != nil {
				return nil, er.Error
			}
		}
		return nil, err
	}
	conn.SetReadLimit(api.MaxStreamMessage)
	return conn, nil
}

// Read takes the stream's next bytes. It returns io.EOF at the server's end
// frame (End then tells it) or when the server closes.
func (s *wsStream) Read(p []byte) (int, error) {
	if len(s.rest) > 0 {
		n := copy(p, s.rest)
		s.rest = s.rest[n:]
		return n, nil
	}
	for {
		typ, b, err := s.c.Read(context.Background())
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) && ce.Code == websocket.StatusNormalClosure {
				return 0, io.EOF
			}
			return 0, err
		}
		switch typ {
		case websocket.MessageBinary:
			if len(b) == 0 {
				continue
			}
			n := copy(p, b)
			s.rest = b[n:]
			return n, nil
		case websocket.MessageText:
			var f api.Frame
			if json.Unmarshal(b, &f) != nil || f.Type != api.FrameEnd {
				continue
			}
			s.end = &End{Exit: f.Exit, Error: f.Error}
			return 0, io.EOF
		}
	}
}

// Write sends p to the guest.
func (s *wsStream) Write(p []byte) (int, error) {
	if err := s.write(websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// write sends one message.
func (s *wsStream) write(typ websocket.MessageType, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Write(context.Background(), typ, b)
}

// Resize tells the server the terminal's new size.
func (t *Terminal) Resize(cols, rows int) error {
	if err := api.CheckSize(cols, rows); err != nil {
		return err
	}
	b, err := json.Marshal(api.Frame{Type: api.FrameResize, W: cols, H: rows})
	if err != nil {
		return err
	}
	return t.write(websocket.MessageText, b)
}

// End is how the stream ended, once Read returned an error: nil when the
// server closed without an end frame.
func (s *wsStream) End() *End { return s.end }

// Close closes the stream normally.
func (s *wsStream) Close() error { return s.c.Close(websocket.StatusNormalClosure, "") }
