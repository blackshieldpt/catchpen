// Package server serves the catchpen API (pkg/api) for catchpen daemon: on a
// unix socket only the daemon's user can use, and over TCP with TLS to
// enrolled clients.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// Server is the API's handlers.
type Server struct {
	version string
	uid     int    // the only user whose unix socket connections are served
	clients string // the clients file: who may connect over TCP
	hmac    *api.Verifier
	jobs    *jobs

	// Manager builds the Manager each request uses (nil: the VM endpoints
	// answer internal). It must build a new one each call: a job sets its
	// Out and Client.
	Manager func() (*vm.Manager, error)
	// Logf logs what a client isn't told: why a request failed (nil: nothing).
	Logf func(format string, a ...any)
}

// New returns the API of catchpen at version, for this process's user over
// the unix socket and for the clients in the clients file over TCP.
func New(version, clients string) *Server {
	s := &Server{version: version, uid: os.Getuid(), clients: clients, jobs: newJobs()}
	s.hmac = api.NewVerifier(s.hmacKey)
	return s
}

// Handler is the API. A request is served on a unix socket connection from
// this process's user, or on a TLS connection from an enrolled client (see
// ListenTCP); anything else is refused.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathInfo, s.info)
	mux.HandleFunc("GET "+api.PathVMs, s.vms)
	mux.HandleFunc("GET "+api.PathVMs+"/{name}", s.vm)
	mux.HandleFunc("GET "+api.PathVMs+"/{name}/snapshots", s.snapshots)
	mux.HandleFunc("GET "+api.PathTemplates, s.templates)
	mux.HandleFunc("GET "+api.PathTasks, s.tasks)
	mux.HandleFunc("GET "+api.PathTasks+"/{id}", s.task)
	mux.HandleFunc("POST "+api.PathTasks, s.submitTask)
	mux.HandleFunc("POST "+api.PathTasks+"/{id}/cancel", s.cancelTask)
	mux.HandleFunc("POST "+api.PathTasks+"/{id}/delete", s.deleteTask)
	mux.HandleFunc("POST "+api.PathTemplateBuild, s.buildTemplate)
	mux.HandleFunc("POST "+api.PathTemplateRebuild, s.rebuildTemplates)
	mux.HandleFunc("GET "+api.PathAudit, s.audit)
	mux.HandleFunc("GET "+api.PathCache, s.cache)
	mux.HandleFunc("POST "+api.PathVMs, s.createVM)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/start", s.startVM)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/stop", s.stopVM)
	mux.HandleFunc("GET "+api.PathVMs+"/{name}/destroy", s.vmDestroyLoss)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/destroy", s.destroyVM)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/resume", s.resumeVM)
	mux.HandleFunc("GET "+api.PathVMs+"/{name}/reset", s.vmResetLoss)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/reset", s.resetVM)
	mux.HandleFunc("GET "+api.PathVMs+"/{name}/rebase", s.vmRebaseLoss)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/rebase", s.rebaseVM)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/push", s.pushVM)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/snapshots", s.createSnapshot)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/snapshots/{snap}/restore", s.restoreSnapshot)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/snapshots/{snap}/delete", s.deleteSnapshot)
	mux.HandleFunc("GET "+api.PathVMs+"/{name}/egress", s.egress)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/egress/"+api.EgressAllow, s.editEgress(egress.AllowlistFile, true))
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/egress/"+api.EgressUnallow, s.editEgress(egress.AllowlistFile, false))
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/egress/"+api.EgressDeny, s.editEgress(egress.DenylistFile, true))
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/egress/"+api.EgressUndeny, s.editEgress(egress.DenylistFile, false))
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/egress/config", s.setEgressConfig)
	mux.HandleFunc("GET "+api.PathVMs+"/{name}/forwards", s.forwards)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/forwards", s.openForward)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/forwards/stop", s.stopForwards)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/pull", s.pullVM)
	mux.HandleFunc("POST "+api.PathVMs+"/{name}/pr", s.prVM)
	mux.HandleFunc("GET "+api.PathForges, s.forges)
	mux.HandleFunc("POST "+api.PathForges, s.addForge)
	mux.HandleFunc("POST "+api.PathForgeScan, s.scanForge)
	mux.HandleFunc("POST "+api.PathForges+"/{name}/test", s.testForge)
	mux.HandleFunc("POST "+api.PathForges+"/{name}/delete", s.deleteForge)
	mux.HandleFunc("GET "+api.PathJobs, s.listJobs)
	mux.HandleFunc("GET "+api.PathJobs+"/{id}", s.getJob)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, api.CodeNotFound, "no such endpoint")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBody)
		if r.TLS != nil {
			var ok bool
			if r, ok = s.authTCP(w, r); !ok {
				return
			}
		} else if uid, ok := r.Context().Value(peerUID{}).(int); !ok || uid != s.uid {
			writeError(w, api.CodeUnauthorized, "not a connection from this user")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// StopJobs refuses new jobs, cancels those running and returns once they
// have ended.
func (s *Server) StopJobs() { s.jobs.stop() }

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

// connContext gives a request its connection's peer uid (unix socket) or
// its limitConn (TCP, which authTCP marks authenticated).
func connContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		if lc, ok := tc.NetConn().(*limitConn); ok {
			return context.WithValue(ctx, limitConnKey{}, lc)
		}
	}
	return withPeerUID(ctx, c)
}

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

// headerTimeout bounds a request's headers, and a TLS connection's
// handshake: a client that sends nothing can't hold a connection open.
var headerTimeout = 10 * time.Second

// Serve serves h on l, a listener from Listen or ListenTCP, until ctx is
// done; it returns once l is closed (for the socket, that removes it).
func Serve(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ConnContext: connContext,
		ReadHeaderTimeout: headerTimeout, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
		// net/http reads some bodies without calling h (after a 417 for an
		// unknown Expect, say): bound those too. h may set its own deadline.
		ReadTimeout: headerTimeout + requestTimeout,
		// net/http would answer "OPTIONS *" itself, past h's checks and deadlines.
		DisableGeneralOptionsHandler: true}
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
	if err != nil {
		srv.Close() // what is still open after 5 s
	}
	<-done // srv.Serve closes l, even when it starts after Shutdown
	return err
}
