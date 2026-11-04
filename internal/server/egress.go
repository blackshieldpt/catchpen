package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

func (s *Server) egress(w http.ResponseWriter, r *http.Request) {
	if v, m, ok := s.findVM(w, r); ok {
		s.writeEgress(w, r, m, v)
	}
}

// writeEgress answers with the VM's egress.
func (s *Server) writeEgress(w http.ResponseWriter, r *http.Request, m *vm.Manager, v *vm.Info) {
	e := api.EgressInfo{Mode: v.Egress, Allow: []string{}, Deny: []string{}}
	if v.Egress == egress.ModeAllowlist {
		c, err := egress.ReadConfig(m.Dir(v.Name))
		if err != nil {
			s.fail(w, r, "the VM's egress settings can't be read", err)
			return
		}
		e.OnDeny, e.PromptTimeout, e.AllowPrivate = c.OnDeny, c.PromptTimeout.String(), c.AllowPrivate
		for file, out := range map[string]*[]string{egress.AllowlistFile: &e.Allow, egress.DenylistFile: &e.Deny} {
			rules, err := egress.LoadList(filepath.Join(m.Dir(v.Name), file))
			if err != nil {
				s.fail(w, r, "the VM's egress lists can't be read", err)
				return
			}
			for _, rule := range rules {
				*out = append(*out, rule.String())
			}
		}
	}
	writeJSON(w, http.StatusOK, e)
}

// egressVM is the VM named in the path, for a call that changes its egress,
// with its Manager auditing for the request's client; it answers a request
// for one there is none of, or whose egress can't be changed.
func (s *Server) egressVM(w http.ResponseWriter, r *http.Request) (*vm.Info, *vm.Manager, bool) {
	v, m, ok := s.findVM(w, r)
	switch {
	case !ok:
		return nil, nil, false
	case v.Pool:
		writeError(w, api.CodeConflict, "the VM is a warm pool VM: claiming it rewrites its egress")
	case v.Egress != egress.ModeAllowlist:
		writeError(w, api.CodeConflict, fmt.Sprintf("the VM uses egress mode %q; lists and settings only apply to allowlist mode", v.Egress))
	default:
		m.Client = clientOf(r)
		return v, m, true
	}
	return nil, nil, false
}

// editEgress edits list (egress.AllowlistFile or egress.DenylistFile),
// adding the entries or removing them.
func (s *Server) editEgress(list string, add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req api.EgressEntriesRequest
		if !decode(w, r, &req) {
			return
		}
		switch n := len(req.Entries); {
		case n == 0:
			writeError(w, api.CodeBadRequest, "no entries")
			return
		case n > api.MaxEgressEntries:
			writeError(w, api.CodeBadRequest, fmt.Sprintf("at most %d entries a call", api.MaxEgressEntries))
			return
		}
		for _, e := range req.Entries { // EditEgressList stops at a bad one, those before it changed
			if _, err := egress.ParseEntry(e); err != nil {
				writeError(w, api.CodeBadRequest, err.Error())
				return
			}
		}
		v, m, ok := s.egressVM(w, r)
		if !ok {
			return
		}
		if err := m.EditEgressList(v.Name, list, add, req.Entries); err != nil {
			s.fail(w, r, "the list can't be changed: read the VM's egress for what was", err)
			return
		}
		s.writeEgress(w, r, m, v)
	}
}

func (s *Server) setEgressConfig(w http.ResponseWriter, r *http.Request) {
	var req api.EgressConfigRequest
	if !decode(w, r, &req) {
		return
	}
	set := vm.EgressSettings{OnDeny: req.OnDeny, AllowPrivate: req.AllowPrivate}
	if req.PromptTimeout != nil {
		d, err := time.ParseDuration(*req.PromptTimeout)
		if err != nil {
			writeError(w, api.CodeBadRequest, "prompt_timeout is a positive duration, such as 30s")
			return
		}
		set.PromptTimeout = &d
	}
	v, m, ok := s.egressVM(w, r)
	if !ok {
		return
	}
	_, err := m.SetEgressConfig(v.Name, set)
	switch {
	case errors.Is(err, vm.ErrOnDeny), errors.Is(err, vm.ErrPromptTimeout):
		writeError(w, api.CodeBadRequest, err.Error())
	case err != nil:
		s.fail(w, r, "the settings can't be changed", err)
	default:
		s.writeEgress(w, r, m, v)
	}
}

func (s *Server) forwards(w http.ResponseWriter, r *http.Request) {
	if v, m, ok := s.findVM(w, r); ok {
		s.writeForwards(w, r, m, v.Name)
	}
}

// writeForwards answers with the VM's live forward sessions.
func (s *Server) writeForwards(w http.ResponseWriter, r *http.Request, m *vm.Manager, name string) {
	fs, err := m.Forwards(name)
	if err != nil {
		s.fail(w, r, "the forwards can't be read", err)
		return
	}
	out := make([]api.ForwardInfo, 0, len(fs))
	for _, f := range fs {
		out = append(out, api.ForwardInfo{PID: f.PID, Specs: api.List(f.Specs), Started: f.Started})
	}
	writeJSON(w, http.StatusOK, out)
}

// openForward checks the forwards before the job, so a refused one is told
// as the client sent it; the job's error can quote the guest's ssh server.
func (s *Server) openForward(w http.ResponseWriter, r *http.Request) {
	var req api.ForwardRequest
	if !decode(w, r, &req) {
		return
	}
	switch n := len(req.Local) + len(req.Remote) + len(req.Dynamic); {
	case n == 0:
		writeError(w, api.CodeBadRequest, "nothing to forward: give local, remote or dynamic")
		return
	case n > api.MaxForwards:
		writeError(w, api.CodeBadRequest, fmt.Sprintf("at most %d forwards a call", api.MaxForwards))
		return
	}
	for _, c := range []struct {
		specs []string
		spec  func(string) (string, error)
	}{{req.Local, vm.LocalSpec}, {req.Remote, vm.RemoteSpec}, {req.Dynamic, vm.DynamicSpec}} {
		for _, sp := range c.specs {
			if _, err := c.spec(sp); err != nil {
				writeError(w, api.CodeBadRequest, err.Error())
				return
			}
		}
	}
	v, m, ok := s.findVM(w, r)
	if !ok || held(w, v, true) { // the session's ssh forwards the VM's SSH agent
		return
	}
	switch {
	case v.Pool:
		writeError(w, api.CodeConflict, "the VM is a warm pool VM: it has no SSH access until claimed")
	case !v.Running:
		writeError(w, api.CodeConflict, "the VM isn't running: start it first")
	default:
		s.startJob(w, r, m, api.JobForwardOpen, v.Name, func(ctx context.Context, m *vm.Manager) error {
			f, err := m.Forward(ctx, v.Name, req.Local, req.Remote, req.Dynamic)
			if err != nil {
				return err
			}
			m.Logf("forwarding (pid %d): %s", f.PID, strings.Join(f.Specs, ", "))
			return nil
		})
	}
}

func (s *Server) stopForwards(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	m.Client = clientOf(r)
	if err := m.StopForwards(v.Name); err != nil {
		s.fail(w, r, "the forwards can't be stopped", err)
		return
	}
	s.writeForwards(w, r, m, v.Name)
}
