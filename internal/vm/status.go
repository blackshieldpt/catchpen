package vm

// Status is a VM's state as catchpen ls shows it.
type Status struct {
	State     string // running, paused (by a canary hit) or stopped
	Ephemeral bool
	Task      bool
	StaleBase bool // built from template picks, and the template's base has changed since
}

func (s Status) String() string {
	out := s.State
	if s.Ephemeral {
		out += ",ephemeral"
	}
	if s.Task {
		out += ",task"
	}
	if s.StaleBase {
		out += ",stale-base"
	}
	return out
}

// Statuses returns the Status of each VM in vms, keyed by name.
func (m *Manager) Statuses(vms []Info) map[string]Status {
	current := map[string]string{} // template -> current base
	out := make(map[string]Status, len(vms))
	for _, v := range vms {
		s := Status{State: "stopped", Ephemeral: v.Ephemeral, Task: v.Task != ""}
		if v.Running {
			s.State = "running"
		}
		if v.Paused {
			s.State = "paused"
		}
		if _, ok := current[v.Template]; !ok && v.Picks {
			current[v.Template], _ = m.CurrentBase(v.Template)
		}
		if cur := current[v.Template]; v.Picks && cur != "" && cur != v.Base {
			s.StaleBase = true
		}
		out[v.Name] = s
	}
	return out
}
