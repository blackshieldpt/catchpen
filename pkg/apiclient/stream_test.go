package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// streamServer serves the stream paths of the API on a unix socket. It sends
// each resize frame it reads on resized, and counts the upgrades it sees in
// hits.
func streamServer(t *testing.T) (c *Client, resized chan string, hits *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	resized = make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/{name}/terminal", func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.PathValue("name") == "refuse" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: &api.Error{Code: api.CodeForbidden, Message: "no"}})
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if _, b, err := conn.Read(ctx); err != nil || string(b) != "hi" {
			return
		}
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("echo:hi"))
		if _, b, err := conn.Read(ctx); err == nil {
			resized <- string(b)
		}
		exit := 7
		b, _ := json.Marshal(api.Frame{Type: api.FrameEnd, Exit: &exit})
		_ = conn.Write(ctx, websocket.MessageText, b)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	mux.HandleFunc("GET /v1/vms/{name}/console", func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("serial\r\n"))
		b, _ := json.Marshal(api.Frame{Type: api.FrameEnd, Error: &api.Error{Code: api.CodeVMPaused, Message: "paused"}})
		_ = conn.Write(ctx, websocket.MessageText, b)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(l, mux)
	t.Cleanup(func() { l.Close() })
	c, err = New(Options{Socket: l.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	return c, resized, &n
}

func TestStreamClient(t *testing.T) {
	c, resized, hits := streamServer(t)
	ctx := context.Background()

	term, err := c.Terminal(ctx, "a", TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := term.Read(buf); err != nil || string(buf[:n]) != "echo:hi" {
		t.Fatalf("read %q, %v; want echo:hi", buf[:n], err)
	}
	if err := term.Resize(100, 40); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resized:
		if got != `{"type":"resize","w":100,"h":40}` {
			t.Errorf("resize frame %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no resize frame")
	}
	if _, err := term.Read(buf); err != io.EOF {
		t.Errorf("read after the end frame: %v, want io.EOF", err)
	}
	if e := term.End(); e == nil || e.Exit == nil || *e.Exit != 7 {
		t.Errorf("end %+v, want exit 7", e)
	}
	if err := term.Close(); err != nil {
		t.Errorf("close: %v", err)
	}

	before := hits.Load()
	if _, err := c.Terminal(ctx, "a", TerminalOptions{Cols: 70000, Rows: 24}); err == nil {
		t.Error("a 70000-column terminal was accepted")
	}
	if hits.Load() != before {
		t.Error("a refused size still made a connection")
	}

	cons, err := c.Console(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, 64)
	if n, err := cons.Read(buf); err != nil || string(buf[:n]) != "serial\r\n" {
		t.Fatalf("console read %q, %v; want serial", buf[:n], err)
	}
	if _, err := cons.Read(buf); err != io.EOF {
		t.Errorf("console read after the end frame: %v, want io.EOF", err)
	}
	if e := cons.End(); e == nil || e.Error == nil || e.Error.Code != api.CodeVMPaused {
		t.Errorf("console end %+v, want %s", e, api.CodeVMPaused)
	}

	_, err = c.Terminal(ctx, "refuse", TerminalOptions{})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeForbidden {
		t.Errorf("a refused handshake: %v, want an api.Error %s", err, api.CodeForbidden)
	}
}
