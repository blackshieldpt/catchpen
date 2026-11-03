package api

import (
	"net/url"
	"strings"
	"time"
)

// PathTemplates is GET: the template categories, as []TemplateCategory in
// the order their sections are merged.
const PathTemplates = Prefix + "/templates"

// TemplateCategory is one category file: what its sections set, and its
// sections. A section's settings are not told: they can hold environment
// values and MCP headers.
type TemplateCategory struct {
	Name      string            `json:"name"`
	About     string            `json:"about"`
	Exclusive bool              `json:"exclusive"` // a pick replaces the default, not adds to it
	Sections  []TemplateSection `json:"sections"`  // by name; "default" first when the default has settings of its own
}

// TemplateSection is a section of a category file.
type TemplateSection struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Default     bool     `json:"default"` // part of its category's default
	Builtin     bool     `json:"builtin"` // catchpen's own, not from the user's file
	Group       []string `json:"group"`   // the picks a list section names; [] for settings
}

// PathTasks is GET: the headless tasks, as []TaskInfo, newest first.
const PathTasks = Prefix + "/tasks"

// PathTask is GET: one task's TaskInfo.
func PathTask(id string) string {
	return PathTasks + "/" + strings.ReplaceAll(url.PathEscape(id), ".", "%2E")
}

// TaskInfo is what the API tells of a headless task. It never carries its
// prompt or its command (though Error can quote the guest's stderr, which
// may echo it), and drops the user and password of a URL in Repo or Error.
// Error and Output can quote the guest and name host paths.
type TaskInfo struct {
	ID           string    `json:"id"`
	State        string    `json:"state"` // queued, starting, running, exporting, done, failed, cancelled, or lost (its supervisor died)
	Agent        string    `json:"agent"`
	Template     string    `json:"template,omitempty"`
	Repo         string    `json:"repo,omitempty"`
	Title        string    `json:"title,omitempty"`
	Out          string    `json:"out"` // pr, bundle, patch or none
	VM           string    `json:"vm"`
	Batch        string    `json:"batch,omitempty"`
	Timeout      int64     `json:"timeout_s"`
	MaxBudgetUSD float64   `json:"max_budget_usd,omitempty"`
	Created      time.Time `json:"created"`
	Started      time.Time `json:"started,omitzero"`
	Ended        time.Time `json:"ended,omitzero"`
	Exit         *int      `json:"exit,omitempty"` // the agent's exit status
	Error        string    `json:"error,omitempty"`
	Usage        TaskUsage `json:"usage"`
	Branch       string    `json:"branch,omitempty"`
	Commits      int       `json:"commits,omitempty"`
	Output       string    `json:"output,omitempty"` // the PR's URL, the ref and host repo, or the patch file
}

// TaskUsage is what the agent reported spending.
type TaskUsage struct {
	Turns        int     `json:"turns"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// PathAudit is GET: a page of the host audit log, as an AuditPage, newest
// first. Its query takes vm (one VM's events), limit (1 to MaxAuditLimit,
// default DefaultAuditLimit) and before (an AuditPage's Next: the page after
// it). Pages walk the log as it was: an event written meanwhile with a time
// at or before the cursor's can be missed (or, at its time, displace one),
// and a rotation drops the oldest events.
const PathAudit = Prefix + "/audit"

const (
	DefaultAuditLimit = 100
	MaxAuditLimit     = 1000
)

// AuditPage is a page of the audit log.
type AuditPage struct {
	Events []AuditEvent `json:"events"`
	Next   string       `json:"next,omitempty"` // the next page's before; "": this is the last
}

// AuditEvent is an audit log line. Detail can quote the guest and name host
// paths; a URL's user and password are dropped.
type AuditEvent struct {
	Time   time.Time      `json:"time"`
	Event  string         `json:"event"`
	VM     string         `json:"vm,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}

// PathCache is GET: what the host's package caches hold, as a CacheInfo.
const PathCache = Prefix + "/cache"

// CacheInfo is what the package caches hold, and the most they may.
type CacheInfo struct {
	Kinds    []CacheKind `json:"kinds"`
	Bytes    int64       `json:"bytes"`     // all kinds
	MaxBytes int64       `json:"max_bytes"` // config.yaml cache_max
}

// CacheKind is what one kind of package cache (apt, go, ...) holds.
type CacheKind struct {
	Kind  string `json:"kind"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}
