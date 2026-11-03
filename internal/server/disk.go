package server

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/blackshieldpt/catchpen/internal/qemu"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// held answers, and reports, a call the VM's state refuses: any on a VM a
// canary hit paused, which stays so until resumed, and one that can start
// the VM or clone through its agent (keys) on a VM with an SSH agent, which
// loads or forwards the host's keys.
func held(w http.ResponseWriter, v *vm.Info, keys bool) bool {
	if v.Paused {
		writeError(w, api.CodeVMPaused, `a canary hit paused the VM: resume it, stop it ("force": true) or destroy it first`)
		return true
	}
	if mode := v.SSH.Mode(); keys && mode != template.AgentOff {
		writeError(w, api.CodeConflict, "the VM has ssh.agent: "+mode+", and this call can load or forward the host's SSH keys: use the CLI")
		return true
	}
	return false
}

func (s *Server) vmResetLoss(w http.ResponseWriter, r *http.Request) {
	if _, _, l, ok := s.loss(w, r, (*vm.Manager).ResetLoss); ok {
		writeJSON(w, http.StatusOK, l)
	}
}

// resetVM needs confirm, and the snapshots and canary hit the client was
// told it loses.
func (s *Server) resetVM(w http.ResponseWriter, r *http.Request) {
	var req api.ResetVMRequest
	if !decode(w, r, &req) {
		return
	}
	v, m, l, ok := s.loss(w, r, (*vm.Manager).ResetLoss)
	if !ok || held(w, v, true) {
		return
	}
	switch {
	case !req.Confirm:
		writeError(w, api.CodeConfirmRequired, confirmMessage("resetting "+v.Name+" deletes all data on its disk", "starts it again", l))
	case !slices.Equal(api.List(req.Snapshots), l.Snapshots) || req.CanaryHit != l.CanaryHit:
		writeError(w, api.CodeConflict, "the VM's snapshots or canary hit are not those confirmed: read what resetting it loses again")
	default:
		s.startJob(w, r, m, api.JobVMReset, v.Name, func(ctx context.Context, m *vm.Manager) error {
			return m.Reset(ctx, v.Name, req.Latest)
		})
	}
}

func (s *Server) vmRebaseLoss(w http.ResponseWriter, r *http.Request) {
	if _, _, l, ok := s.loss(w, r, (*vm.Manager).RebaseLoss); ok {
		writeJSON(w, http.StatusOK, l)
	}
}

// rebaseVM needs confirm, and the snapshots it deletes, when the VM has any.
func (s *Server) rebaseVM(w http.ResponseWriter, r *http.Request) {
	var req api.RebaseVMRequest
	if !decode(w, r, &req) {
		return
	}
	v, m, l, ok := s.loss(w, r, (*vm.Manager).RebaseLoss)
	if !ok || held(w, v, true) {
		return
	}
	switch {
	case len(l.Snapshots) > 0 && !req.Confirm:
		writeError(w, api.CodeConfirmRequired, "rebasing "+v.Name+" deletes its snapshots ("+strings.Join(l.Snapshots, ", ")+
			`): send "confirm": true, with the snapshots it deletes (GET this path for all it loses)`)
	case req.Confirm && !slices.Equal(api.List(req.Snapshots), l.Snapshots):
		writeError(w, api.CodeConflict, "the VM's snapshots are not those confirmed: read what rebasing it loses again")
	default:
		s.startJob(w, r, m, api.JobVMRebase, v.Name, func(ctx context.Context, m *vm.Manager) error {
			return m.Rebase(ctx, v.Name)
		})
	}
}

func (s *Server) pushVM(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	v, m, ok := s.findVM(w, r)
	if !ok || held(w, v, true) { // a repo whose clone is gone is cloned again through the agent
		return
	}
	s.startJob(w, r, m, api.JobCredsPush, v.Name, func(ctx context.Context, m *vm.Manager) error {
		return m.Push(ctx, v.Name)
	})
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSnapshotRequest
	if !decode(w, r, &req) {
		return
	}
	if err := vm.ValidSnap(req.Name); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	v, m, ss, ok := s.vmSnaps(w, r)
	if !ok || held(w, v, false) {
		return
	}
	if slices.ContainsFunc(ss, func(sn qemu.Snapshot) bool { return sn.Name == req.Name }) {
		writeError(w, api.CodeConflict, "the VM has a snapshot of that name")
		return
	}
	s.startJob(w, r, m, api.JobVMSnapshot, v.Name, func(ctx context.Context, m *vm.Manager) error {
		_, err := m.Snapshot(ctx, v.Name, req.Name, req.DiskOnly)
		return err
	})
}

func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	var req api.RestoreSnapshotRequest
	if !decode(w, r, &req) {
		return
	}
	v, m, snap, ok := s.findSnap(w, r)
	if !ok || held(w, v, true) {
		return
	}
	if !req.Confirm {
		writeError(w, api.CodeConfirmRequired, "restoring "+v.Name+" to "+snap+` loses what changed since it was saved: send "confirm": true`)
		return
	}
	s.startJob(w, r, m, api.JobVMRestore, v.Name, func(ctx context.Context, m *vm.Manager) error {
		return m.Restore(ctx, v.Name, snap, req.DiskOnly)
	})
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	var req api.DeleteSnapshotRequest
	if !decode(w, r, &req) {
		return
	}
	v, m, snap, ok := s.findSnap(w, r)
	if !ok || held(w, v, false) {
		return
	}
	if !req.Confirm {
		writeError(w, api.CodeConfirmRequired, "deleting "+v.Name+"'s snapshot "+snap+` can't be undone: send "confirm": true`)
		return
	}
	s.startJob(w, r, m, api.JobVMSnapshotRm, v.Name, func(_ context.Context, m *vm.Manager) error {
		return m.DeleteSnapshot(v.Name, snap)
	})
}

// vmSnaps is the VM named in the path and its snapshots; it answers a
// request it can't.
func (s *Server) vmSnaps(w http.ResponseWriter, r *http.Request) (*vm.Info, *vm.Manager, []qemu.Snapshot, bool) {
	v, m, ok := s.findVM(w, r)
	if !ok {
		return nil, nil, nil, false
	}
	ss, err := m.Snapshots(v.Name)
	if err != nil {
		s.fail(w, r, "the snapshots can't be read", err)
		return nil, nil, nil, false
	}
	return v, m, ss, true
}

// findSnap is the VM and snapshot named in the path; it answers a request
// for one there is none of.
func (s *Server) findSnap(w http.ResponseWriter, r *http.Request) (*vm.Info, *vm.Manager, string, bool) {
	v, m, ss, ok := s.vmSnaps(w, r)
	if !ok {
		return nil, nil, "", false
	}
	snap := r.PathValue("snap")
	if !slices.ContainsFunc(ss, func(sn qemu.Snapshot) bool { return sn.Name == snap }) {
		writeError(w, api.CodeNotFound, "no such snapshot")
		return nil, nil, "", false
	}
	return v, m, snap, true
}
