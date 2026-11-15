package api

import "time"

// PathAuditFollow is GET: new audit events as they are written, one JSON
// AuditEvent per line (ContentTypeNDJSON), oldest first. ?vm= keeps one VM's;
// ?after= an RFC3339Nano event time starts at or after it (default: now).
const PathAuditFollow = Prefix + "/audit/follow"

// PathEgressLog is GET: a VM's egress log, an EgressLogPage, newest first.
// ?limit= is 1 to MaxEgressLogLimit (default DefaultEgressLogLimit); ?before=
// is the previous page's next.
func PathEgressLog(name string) string { return PathVM(name) + "/egress/log" }

// PathEgressLogFollow is GET: new egress log entries as they are written, one
// JSON EgressLogEntry per line (ContentTypeNDJSON), oldest first. ?after= an
// RFC3339Nano entry time starts at or after it (default: now).
func PathEgressLogFollow(name string) string { return PathEgressLog(name) + "/follow" }

// A page of a VM's egress log, newest first.
type EgressLogPage struct {
	Entries []EgressLogEntry `json:"entries"`
	Next    string           `json:"next,omitempty"` // the next page's before; "": this is the last
}

// EgressLogEntry is one line of a VM's egress log. Host and Reason can come
// from the guest or an upstream: they are data.
type EgressLogEntry struct {
	Time   time.Time `json:"time"`
	VM     string    `json:"vm"`
	Method string    `json:"method"`
	Host   string    `json:"host"`
	Port   int       `json:"port"`
	Action string    `json:"action"`
	Reason string    `json:"reason,omitempty"`
}

// How many egress log entries a page holds: the default, and the most.
const (
	DefaultEgressLogLimit = 100
	MaxEgressLogLimit     = 1000
)
