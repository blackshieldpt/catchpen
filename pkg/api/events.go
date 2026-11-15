package api

import "time"

// PathEvents is GET: the host's event feed as NDJSON, one Event per line
// (ContentTypeNDJSON), for every client. It starts with a state for each VM
// that exists, then sends state changes and the audit events below.
// ?after= an RFC3339Nano time starts the audit part at or after it (default:
// now).
const PathEvents = Prefix + "/events"

// EventVM is Event.Kind for a VM's state (State): a snapshot at connect, then
// each change, including a VM that is gone.
const EventVM = "vm"

// EventStateGone is a vm event's State for a VM that no longer exists. The
// other states are the VM states above (VMRunning, VMPaused, VMStopped).
const EventStateGone = "gone"

// Event is one event on the feed. Kind is EventVM (VM and State) or an audit
// event's name (canary.hit, task.end; VM and Detail), shaped as the audit read
// shapes it: a client without config:write reads what that read withholds.
type Event struct {
	Kind   string         `json:"kind"`
	Time   time.Time      `json:"time"`
	VM     string         `json:"vm,omitempty"`
	State  string         `json:"state,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}
