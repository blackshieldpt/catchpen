package server

import (
	"net/http"

	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// fail answers internal with msg, which says nothing of the host, and logs
// err, which may.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, msg string, err error) {
	if s.Logf != nil {
		s.Logf("api: %s: %v", r.Pattern, err) // the route, not the client's own path
	}
	writeError(w, api.CodeInternal, msg)
}

// manager builds the Manager for a request; it answers a request it can't
// serve, and returns false.
func (s *Server) manager(w http.ResponseWriter, r *http.Request) (*vm.Manager, bool) {
	if s.Manager == nil {
		writeError(w, api.CodeInternal, "this server has no VMs")
		return nil, false
	}
	m, err := s.Manager()
	if err != nil {
		s.fail(w, r, "the configuration can't be loaded", err)
		return nil, false
	}
	return m, true
}

// list is every VM.
func (s *Server) list(w http.ResponseWriter, r *http.Request) ([]vm.Info, *vm.Manager, bool) {
	m, ok := s.manager(w, r)
	if !ok {
		return nil, nil, false
	}
	vms, err := m.List()
	if err != nil {
		s.fail(w, r, "the VMs can't be listed", err)
		return nil, nil, false
	}
	return vms, m, true
}

// vmInfo copies, field by field, what the API may tell of a VM: never its
// environment, forges, agents' settings or canaries.
func vmInfo(v vm.Info, st vm.Status) api.VMInfo {
	i := api.VMInfo{
		Name: v.Name, State: st.State, Pool: v.Pool, Ephemeral: v.Ephemeral, Task: v.Task,
		StaleBase: st.StaleBase, CanaryHit: v.Hit, Template: v.Template, Distro: v.Distro,
		User: v.User, CPUs: v.CPUs, MemoryMB: v.MemoryMB, DiskMB: v.DiskMB, Egress: v.Egress,
		Docker: v.Docker, IdleStop: v.IdleStop, Created: v.Created, Booted: v.Booted,
		Ports: make([]api.PortInfo, 0, len(v.Ports)), Forwards: []api.PortInfo{},
	}
	for _, p := range v.Ports {
		i.Ports = append(i.Ports, api.PortInfo{Host: p.Host, Guest: p.Guest})
	}
	if v.Runtime != nil { // set while it runs
		i.SSHPort = v.Runtime.SSHPort
		for _, f := range v.Runtime.Forwards {
			i.Forwards = append(i.Forwards, api.PortInfo{Host: f.Host, Guest: f.Guest})
		}
	}
	return i
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	vms, m, ok := s.list(w, r)
	if !ok {
		return
	}
	st := m.Statuses(vms)
	out := make([]api.VMInfo, 0, len(vms))
	for _, v := range vms {
		out = append(out, vmInfo(v, st[v.Name]))
	}
	writeJSON(w, http.StatusOK, out)
}

// findVM is the VM named in the request's path; it answers a request for
// one there is none of.
func (s *Server) findVM(w http.ResponseWriter, r *http.Request) (*vm.Info, *vm.Manager, bool) {
	vms, m, ok := s.list(w, r)
	if !ok {
		return nil, nil, false
	}
	name := r.PathValue("name")
	for i := range vms {
		if vms[i].Name == name {
			return &vms[i], m, true
		}
	}
	writeError(w, api.CodeNotFound, "no such VM")
	return nil, nil, false
}

func (s *Server) vm(w http.ResponseWriter, r *http.Request) {
	if v, m, ok := s.findVM(w, r); ok {
		writeJSON(w, http.StatusOK, vmInfo(*v, m.Statuses([]vm.Info{*v})[v.Name]))
	}
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	v, m, ok := s.findVM(w, r)
	if !ok {
		return
	}
	ss, err := m.Snapshots(v.Name)
	if err != nil {
		s.fail(w, r, "the snapshots can't be read", err)
		return
	}
	out := make([]api.SnapshotInfo, 0, len(ss))
	for _, sn := range ss {
		out = append(out, api.SnapshotInfo{Name: sn.Name, Created: sn.Created.UTC(), MemoryMB: sn.VMStateMB})
	}
	writeJSON(w, http.StatusOK, out)
}
