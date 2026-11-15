package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// errClientGone stops a follow whose write failed: the client is gone.
var errClientGone = errors.New("the client is gone")

// ndjson starts a follow response: one JSON value per line, flushed as each
// is written. write reports false when the client is gone; end writes an
// error line, if any, and returns.
//
// authTCP sets a read deadline and a write deadline, each requestTimeout
// (30 s) from the request, and does not clear them (tcp.go:127,167). The read
// one makes net/http's background read fail and cancel the request's context,
// and the write one fails the next write past it. A WebSocket clears both when
// it hijacks the connection; a plain HTTP stream must clear them itself. What
// is left is limitConn's per-write stall (writeStall, tcp.go:283-289).
func ndjson(w http.ResponseWriter) (write func(any) bool, end func(*api.Error)) {
	w.Header().Set("Content-Type", api.ContentTypeNDJSON)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})  // authTCP's read deadline cancels the context at 30 s
	_ = rc.SetWriteDeadline(time.Time{}) // ... and its write deadline fails the next write
	write = func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	end = func(e *api.Error) {
		if e != nil {
			write(api.ErrorResponse{Error: e})
		}
	}
	return write, end
}

// followAfter parses ?after= (RFC3339Nano), or returns now.
func followAfter(r *http.Request) (time.Time, error) {
	v := r.URL.Query().Get("after")
	if v == "" {
		return time.Now(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, errors.New("after must be an event's RFC3339Nano time")
	}
	return t, nil
}

// auditFollow is GET /v1/audit/follow: new audit events as NDJSON.
func (s *Server) auditFollow(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("vm")
	if name != "" && vm.ValidName(name) != nil {
		writeError(w, api.CodeBadRequest, "invalid VM name")
		return
	}
	after, err := followAfter(r)
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	write, end := ndjson(w)
	err = m.Audit().TailEvents(r.Context(), name, true, func(e audit.Event) error {
		if e.Time.Before(after) { // at or after: a tie may repeat, not be missed
			return nil
		}
		if !write(shownEvent(r, e)) {
			return errClientGone
		}
		return nil
	})
	if err != nil && !errors.Is(err, errClientGone) && r.Context().Err() == nil {
		end(&api.Error{Code: api.CodeInternal, Message: "the audit log can't be followed"})
	}
}

// egressLog is GET /v1/vms/{name}/egress/log: a page of the VM's log, newest
// first.
func (s *Server) egressLog(w http.ResponseWriter, r *http.Request) {
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := api.DefaultEgressLogLimit
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > api.MaxEgressLogLimit {
			writeError(w, api.CodeBadRequest, "limit is 1 to "+strconv.Itoa(api.MaxEgressLogLimit))
			return
		}
		limit = n
	}
	var before *egress.Cursor
	if b := q.Get("before"); b != "" {
		ts, skip, _ := strings.Cut(b, "~")
		t, terr := time.Parse(time.RFC3339Nano, ts)
		n, nerr := strconv.Atoi(skip)
		if terr != nil || nerr != nil || n < 0 {
			writeError(w, api.CodeBadRequest, "invalid before: pass a page's next")
			return
		}
		before = &egress.Cursor{Time: t, Skip: n}
	}
	es, next, err := egress.Page(r.Context(), filepath.Join(m.Dir(v.Name), egress.LogFile), before, limit)
	if err != nil {
		s.fail(w, r, "the egress log can't be read", err)
		return
	}
	page := api.EgressLogPage{Entries: make([]api.EgressLogEntry, 0, len(es))}
	for _, e := range es {
		page.Entries = append(page.Entries, egressEntry(e))
	}
	if next != nil {
		page.Next = next.Time.UTC().Format(time.RFC3339Nano) + "~" + strconv.Itoa(next.Skip)
	}
	writeJSON(w, http.StatusOK, page)
}

// egressEntry is e as the API returns it.
func egressEntry(e egress.LogEntry) api.EgressLogEntry {
	return api.EgressLogEntry{Time: e.Time, VM: e.VM, Method: e.Method, Host: e.Host,
		Port: e.Port, Action: e.Action, Reason: e.Reason}
}

// egressLogFollow is GET /v1/vms/{name}/egress/log/follow: new entries as
// NDJSON.
func (s *Server) egressLogFollow(w http.ResponseWriter, r *http.Request) {
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	after, err := followAfter(r)
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	write, end := ndjson(w)
	err = egress.TailEntries(r.Context(), filepath.Join(m.Dir(v.Name), egress.LogFile), true, func(e egress.LogEntry) error {
		if e.Time.Before(after) { // at or after, as the audit follow
			return nil
		}
		if !write(egressEntry(e)) {
			return errClientGone
		}
		return nil
	})
	if err != nil && !errors.Is(err, errClientGone) && r.Context().Err() == nil {
		end(&api.Error{Code: api.CodeInternal, Message: "the egress log can't be followed"})
	}
}
