// Package server serves the catchpen API (pkg/api) for catchpen daemon, on
// a unix socket only the daemon's user can use.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// Server is the API's handlers.
type Server struct {
	version string
	uid     int // the only user whose connections are served
}

// New returns the API of catchpen at version, for this process's user.
func New(version string) *Server {
	return &Server{version: version, uid: os.Getuid()}
}

// Handler is the API. A request is served only on a connection Serve
// accepted from this process's user.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathInfo, s.info)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, api.CodeNotFound, "no such endpoint")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		if uid, ok := r.Context().Value(peerUID{}).(int); !ok || uid != s.uid {
			writeError(w, api.CodeUnauthorized, "not a connection from this user")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Info{Version: s.version, API: api.Version})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code, msg string) {
	writeJSON(w, api.Status(code), api.ErrorResponse{Error: &api.Error{Code: code, Message: msg}})
}

// peerUID is the context key of the user id at the other end of a
// connection (-1: unknown).
type peerUID struct{}

func withPeerUID(ctx context.Context, c net.Conn) context.Context {
	uid := -1
	if uc, ok := c.(*net.UnixConn); ok {
		if raw, err := uc.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) {
				if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
					uid = int(cred.Uid)
				}
			})
		}
	}
	return context.WithValue(ctx, peerUID{}, uid)
}

// Listen listens on the unix socket path, mode 0600. A socket left there by
// a daemon that is gone is replaced; a socket something still answers on, or
// any other file, is an error.
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("something is already serving %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Serve serves h on l, a listener from Listen, until ctx is done; it returns
// once l is closed, which removes the socket.
func Serve(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ConnContext: withPeerUID,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := srv.Shutdown(sctx)
	<-done // srv.Serve closes l, even when it starts after Shutdown
	return err
}
