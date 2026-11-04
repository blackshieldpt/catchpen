package api

import "time"

// PathEgress is GET: the VM's egress mode and, in allowlist mode, its
// settings and lists, as EgressInfo.
func PathEgress(name string) string { return PathVM(name) + "/egress" }

// The calls that change a VM's egress (POST) answer with its EgressInfo
// after the change. They are refused with CodeConflict on a warm pool VM
// (claiming it rewrites its lists and settings) and on a VM whose egress
// mode isn't allowlist.

// PathEgressEdit is POST, with an EgressEntriesRequest: edit one of the
// VM's lists as catchpen egress <verb> does, verb being EgressAllow,
// EgressUnallow, EgressDeny or EgressUndeny. Every entry is checked before
// any is applied: one that isn't a host[:port] is refused with
// CodeBadRequest, nothing changed.
func PathEgressEdit(name, verb string) string { return PathEgress(name) + "/" + verb }

// The verbs of PathEgressEdit.
const (
	EgressAllow   = "allow"   // add to the allowlist
	EgressUnallow = "unallow" // remove from the allowlist
	EgressDeny    = "deny"    // add to the denylist, which wins over the allowlist
	EgressUndeny  = "undeny"  // remove from the denylist
)

// PathEgressConfig is POST, with an EgressConfigRequest: change the VM's
// egress settings, as catchpen egress config does.
func PathEgressConfig(name string) string { return PathEgress(name) + "/config" }

// PathForwards is GET: the VM's live forward sessions, as []ForwardInfo.
// It is also POST, with a ForwardRequest: open one forward session, as
// catchpen forward does, as a job; it is refused with CodeVMPaused on a VM a
// canary hit paused, and with CodeConflict on a warm pool VM, a VM that isn't
// running and a VM with an SSH agent (ssh.agent other than off), which the
// session's ssh would forward.
func PathForwards(name string) string { return PathVM(name) + "/forwards" }

// PathForwardsStop is POST, with no body (or {}): end all the VM's forward
// sessions, as catchpen forward --stop does. It answers with those left,
// []ForwardInfo.
func PathForwardsStop(name string) string { return PathForwards(name) + "/stop" }

// MaxEgressEntries is the most entries an EgressEntriesRequest takes, and
// MaxForwards the most forwards a ForwardRequest does.
const (
	MaxEgressEntries = 64
	MaxForwards      = 16
)

// EgressInfo is a VM's egress.
type EgressInfo struct {
	Mode          string   `json:"mode"`                     // allowlist, open or none; the rest is set for allowlist only
	OnDeny        string   `json:"on_deny,omitempty"`        // log or prompt
	PromptTimeout string   `json:"prompt_timeout,omitempty"` // a duration
	AllowPrivate  bool     `json:"allow_private"`            // allowlisted names may resolve to private or loopback addresses
	Allow         []string `json:"allow"`                    // the allowlist, in its file's order
	Deny          []string `json:"deny"`                     // the denylist
}

// EgressEntriesRequest is the entries to add or remove: host, *.host or an
// IP address, each with an optional :port.
type EgressEntriesRequest struct {
	Entries []string `json:"entries"`
}

// EgressConfigRequest is the settings to change; one left out stays as it
// is.
type EgressConfigRequest struct {
	OnDeny        *string `json:"on_deny,omitempty"`        // log or prompt
	PromptTimeout *string `json:"prompt_timeout,omitempty"` // a positive duration, such as 30s
	AllowPrivate  *bool   `json:"allow_private,omitempty"`
}

// ForwardRequest is the forwards of one session, in catchpen forward's
// forms. Listeners on the host bind to the daemon host's 127.0.0.1.
type ForwardRequest struct {
	Local   []string `json:"local,omitempty"`   // -L: PORT, HOSTPORT:GUESTPORT or HOSTPORT:HOST:PORT (HOST as the guest sees it)
	Remote  []string `json:"remote,omitempty"`  // -R: PORT, GUESTPORT:HOSTPORT or GUESTPORT:HOST:PORT (HOST as the daemon's host sees it); bypasses egress
	Dynamic []string `json:"dynamic,omitempty"` // -D: PORT, a SOCKS proxy on the host into the guest
}

// ForwardInfo is a live forward session: a background ssh on the daemon's
// host.
type ForwardInfo struct {
	PID     int       `json:"pid"`
	Specs   []string  `json:"specs"` // such as "-L 127.0.0.1:3000:localhost:3000"
	Started time.Time `json:"started"`
}
