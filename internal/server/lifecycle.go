package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/blackshieldpt/catchpen/internal/canary"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// clientOf names who a request came from: its enrolled client, or
// api.ClientLocal over the unix socket.
func clientOf(r *http.Request) string {
	if c, ok := CallerOf(r.Context()); ok {
		return c.Name
	}
	return api.ClientLocal
}

// decode reads the request's JSON body into v; no body leaves v as it is.
// It answers a body it refuses, and returns false.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := d.Decode(v)
	if err == nil {
		if _, err = d.Token(); err == io.EOF { // nothing after the value
			err = nil
		} else if err == nil {
			err = errors.New("more than one value")
		}
	} else if err == io.EOF {
		err = nil
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, api.CodeTooLarge, "request body too large")
	case err != nil:
		writeError(w, api.CodeBadRequest, "invalid request body")
	default:
		return true
	}
	return false
}

// startJob runs op on m as a job of kind on VM name, for the request's
// client, and answers with the job.
func (s *Server) startJob(w http.ResponseWriter, r *http.Request, m *vm.Manager, kind, name string,
	op func(ctx context.Context, m *vm.Manager) error) {
	client := clientOf(r)
	dw := &watch{scan: decoyScan(m, name), hit: func(c *canary.Canary) { m.CanaryHit(name, c, "in an API job's output") }}
	info, err := s.jobs.start(kind, name, client, dw, func(ctx context.Context, out io.Writer) error {
		m.Out, m.In, m.Client = out, nil, client // the progress, no terminal, the audit's client
		return op(ctx, m)
	})
	switch {
	case errors.Is(err, errBusy):
		writeError(w, api.CodeRateLimited, fmt.Sprintf("%d jobs are running; wait for one to end", maxRunning))
	case err != nil:
		writeError(w, api.CodeInternal, err.Error()) // errStopping
	default:
		writeJSON(w, http.StatusAccepted, info)
	}
}

// decoyScan finds name's decoys in a text: those it has when the job
// starts, else (Create plants them) those it has by then.
func decoyScan(m *vm.Manager, name string) func(string) (*canary.Canary, error) {
	cs, err := m.Decoys(name)
	return func(s string) (*canary.Canary, error) {
		if err != nil || len(cs) == 0 {
			if cs, err = m.Decoys(name); err != nil {
				return nil, err
			}
		}
		return vm.DecoyIn(cs, s), nil
	}
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var req api.CreateVMRequest
	if !decode(w, r, &req) {
		return
	}
	if err := vm.ValidName(req.Name); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	// A URL's user, password or query token would be kept as typed in
	// vm.json, and quoted in errors any client can read.
	for _, repo := range req.Repos {
		if strings.ContainsAny(repo, "@?#%") {
			writeError(w, api.CodeBadRequest, "a repo with any of @ ? # % (a user, password, query or escape) is refused: use forge:owner/repo or a plain URL")
			return
		}
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	if _, err := os.Lstat(m.Dir(req.Name)); err == nil {
		writeError(w, api.CodeConflict, "a VM of that name exists")
		return
	}
	o := vm.CreateOpts{Template: req.Template, CPUs: req.CPUs, Memory: req.Memory, Disk: req.Disk,
		Rebuild: req.Rebuild, AllowOpen: req.AllowOpen, ClaudeAuth: req.ClaudeAuth,
		Repos: slices.Clone(req.Repos), ReadOnly: req.ReadOnly, NoAgent: true}
	s.startJob(w, r, m, api.JobVMCreate, req.Name, func(ctx context.Context, m *vm.Manager) error {
		_, err := m.Create(ctx, req.Name, o)
		return err
	})
}

// vmJob runs op as a job on the VM named in the path, once it is found.
func (s *Server) vmJob(w http.ResponseWriter, r *http.Request, kind string, op func(ctx context.Context, m *vm.Manager, name string) error) {
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	s.startJob(w, r, m, kind, v.Name, func(ctx context.Context, m *vm.Manager) error { return op(ctx, m, v.Name) })
}

// startVM refuses a VM with an SSH agent: its start loads the host's keys
// (asking for passphrases on the host's desktop) or forwards its agent.
func (s *Server) startVM(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	if mode := v.SSH.Mode(); mode != template.AgentOff {
		writeError(w, api.CodeConflict, "the VM has ssh.agent: "+mode+", which loads or forwards the host's SSH keys: start it with catchpen start")
		return
	}
	s.startJob(w, r, m, api.JobVMStart, v.Name, func(ctx context.Context, m *vm.Manager) error {
		_, err := m.Start(ctx, v.Name)
		return err
	})
}

func (s *Server) stopVM(w http.ResponseWriter, r *http.Request) {
	var req api.StopVMRequest
	if !decode(w, r, &req) {
		return
	}
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	if v.Paused && !req.Force { // Stop forces it off, and what it holds frozen is lost
		writeError(w, api.CodeVMPaused, "a canary hit paused the VM: stopping it forces it off and loses its memory; send \"force\": true")
		return
	}
	s.startJob(w, r, m, api.JobVMStop, v.Name, func(_ context.Context, m *vm.Manager) error {
		return m.Stop(v.Name, req.Force)
	})
}

func (s *Server) resumeVM(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	s.vmJob(w, r, api.JobVMResume, func(ctx context.Context, m *vm.Manager, name string) error {
		return m.Resume(ctx, name)
	})
}

// destroyVM needs confirm, and the snapshots, pause and canary hit the
// client was told it loses. A snapshot taken after this check and before
// Destroy locks the VM is deleted too.
func (s *Server) destroyVM(w http.ResponseWriter, r *http.Request) {
	var req api.DestroyVMRequest
	if !decode(w, r, &req) {
		return
	}
	v, m, l, ok := s.loss(w, r, (*vm.Manager).DestroyLoss)
	if !ok {
		return
	}
	switch {
	case !req.Confirm:
		writeError(w, api.CodeConfirmRequired, confirmMessage("destroying "+v.Name+" deletes all data inside it", "forces it off", l))
	case !slices.Equal(api.List(req.Snapshots), l.Snapshots) || req.Paused != l.Paused || req.CanaryHit != l.CanaryHit:
		writeError(w, api.CodeConflict, "the VM's snapshots, pause or canary hit are not those confirmed: read what destroying it loses again")
	default:
		s.startJob(w, r, m, api.JobVMDestroy, v.Name, func(_ context.Context, m *vm.Manager) error { return m.Destroy(v.Name) })
	}
}

// loss is what a call on the VM named in the path loses, as of tells (the
// Manager's DestroyLoss, ResetLoss or RebaseLoss); it answers a request it
// can't.
func (s *Server) loss(w http.ResponseWriter, r *http.Request, of func(*vm.Manager, string) (vm.Loss, error)) (*vm.Info, *vm.Manager, api.VMLoss, bool) {
	v, m, ok := s.findVM(w, r)
	if !ok {
		return nil, nil, api.VMLoss{}, false
	}
	l, err := of(m, v.Name)
	if err != nil {
		s.fail(w, r, "what the call deletes can't be read", err)
		return nil, nil, api.VMLoss{}, false
	}
	return v, m, api.VMLoss{Disk: l.Disk, Snapshots: api.List(l.Snapshots), Running: l.Running,
		Paused: v.Paused, CanaryHit: v.Hit, Task: v.Task}, true
}

func (s *Server) vmDestroyLoss(w http.ResponseWriter, r *http.Request) {
	if _, _, l, ok := s.loss(w, r, (*vm.Manager).DestroyLoss); ok {
		writeJSON(w, http.StatusOK, l)
	}
}

// confirmMessage says what a call deletes (what, then the snapshots) and
// does to a running VM (running), as the confirm prompt.
func confirmMessage(what, running string, l api.VMLoss) string {
	msg := what
	if len(l.Snapshots) > 0 {
		msg += ", its snapshots (" + strings.Join(l.Snapshots, ", ") + ") included"
	}
	if l.Running {
		msg += ", and " + running
	}
	return msg + `: send "confirm": true, with the snapshots it deletes (GET this path for all it loses)`
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.jobs.list())
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	after := 0
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, api.CodeBadRequest, "after is a count of lines (0 or more)")
			return
		}
		after = n
	}
	j, ok := s.jobs.find(r.PathValue("id"))
	if !ok {
		writeError(w, api.CodeNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, j.from(after))
}
