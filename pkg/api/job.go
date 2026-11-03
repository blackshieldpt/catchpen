package api

import (
	"net/url"
	"strings"
	"time"
)

// A call that changes a VM runs as a job: it answers 202 with the job's
// JobInfo at once, and the job runs on in the server whether or not its
// client stays. GET PathJob follows it.

// PathVMStart is POST, with no body (or {}): start the VM. A VM with an
// SSH agent (ssh.agent other than off) is refused with CodeConflict.
func PathVMStart(name string) string { return PathVM(name) + "/start" }

// PathVMStop is POST, with a StopVMRequest or no body: shut the VM down. A
// VM a canary hit paused is refused with CodeVMPaused without Force.
func PathVMStop(name string) string { return PathVM(name) + "/stop" }

// PathVMDestroy is GET: the VMLoss destroying the VM would cause. It is
// also POST, with a DestroyVMRequest: stop the VM and delete it, its disk
// and snapshots included. Without Confirm it is refused with
// CodeConfirmRequired; with snapshots, a pause or a canary hit other than the
// VM's (what the client was shown is out of date), with CodeConflict.
func PathVMDestroy(name string) string { return PathVM(name) + "/destroy" }

// PathVMResume is POST, with no body (or {}): continue a VM a canary hit
// paused.
func PathVMResume(name string) string { return PathVM(name) + "/resume" }

// CreateVMRequest is a VM to create. Unknown fields are refused.
type CreateVMRequest struct {
	Name       string   `json:"name"`
	Template   string   `json:"template,omitempty"` // a -t spec; "": every category's default
	CPUs       int      `json:"cpus,omitempty"`
	Memory     string   `json:"memory,omitempty"` // e.g. 8G
	Disk       string   `json:"disk,omitempty"`
	Rebuild    bool     `json:"rebuild,omitempty"`    // rebuild the template's base first
	AllowOpen  bool     `json:"allow_open,omitempty"` // confirm a template with network egress open
	ClaudeAuth string   `json:"claude_auth,omitempty"`
	Repos      []string `json:"repos,omitempty"`     // forge:owner/repo or a URL, with none of @ ? # % (no user, password, query or escape)
	ReadOnly   bool     `json:"read_only,omitempty"` // deploy keys are read-only
}

// StopVMRequest is how to stop a VM.
type StopVMRequest struct {
	Force bool `json:"force,omitempty"` // power off at once, without waiting for the guest
}

// DestroyVMRequest confirms a destroy, and what it deletes.
type DestroyVMRequest struct {
	Confirm   bool     `json:"confirm"`
	Snapshots []string `json:"snapshots"`  // the VMLoss's, as read before
	Paused    bool     `json:"paused"`     // the VMLoss's
	CanaryHit bool     `json:"canary_hit"` // the VMLoss's
}

// VMLoss is what a destructive call on a VM loses.
type VMLoss struct {
	Disk      bool     `json:"disk"`           // everything on its disk
	Snapshots []string `json:"snapshots"`      // the snapshots deleted, oldest first
	Running   bool     `json:"running"`        // it runs, and is forced off
	Paused    bool     `json:"paused"`         // a canary hit paused it: what it holds frozen is lost
	CanaryHit bool     `json:"canary_hit"`     // its canaries were found in use: it is the evidence
	Task      string   `json:"task,omitempty"` // the headless task it runs or ran
}

// PathJobs is GET: the jobs running and the last ones that ended, as
// []JobInfo, newest first.
const PathJobs = Prefix + "/jobs"

// PathJob is GET: one job, as a Job, with its progress lines from the
// query's after (a count of lines; default 0) on. Its id is escaped as
// PathVM's name is.
func PathJob(id string) string {
	return PathJobs + "/" + strings.ReplaceAll(url.PathEscape(id), ".", "%2E")
}

// A job's state.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// A job's kind: what it does, named as the audit event it records.
const (
	JobVMCreate  = "vm.create"
	JobVMStart   = "vm.start"
	JobVMStop    = "vm.stop"
	JobVMDestroy = "vm.destroy"
	JobVMResume  = "vm.resume"
)

// ClientLocal is the client of a call over the unix socket, the daemon's
// own user, in a job and in the "client" of the audit events a call causes.
// No enrolled client can be named so.
const ClientLocal = "@local"

// A job keeps its last MaxJobLines progress lines, each cut to MaxJobLine
// bytes.
const (
	MaxJobLines = 500
	MaxJobLine  = 1024
)

// JobInfo is a job.
type JobInfo struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	VM      string    `json:"vm"`
	Client  string    `json:"client"` // the enrolled client that started it, or ClientLocal
	State   string    `json:"state"`  // JobRunning, JobDone or JobFailed
	Created time.Time `json:"created"`
	Ended   time.Time `json:"ended,omitzero"`
	Error   string    `json:"error,omitempty"` // why it failed; can quote the guest and name host paths
	Written int       `json:"written"`         // progress lines written so far
}

// Job is a job and its progress lines, which can quote the guest. Once its
// lines or error carry one of the VM's decoy credentials, every line and the
// error are a "(withheld: ...)" note instead, their count kept.
type Job struct {
	JobInfo
	From  int      `json:"from"` // the number of Lines' first line: above the after asked for when older lines were dropped
	Lines []string `json:"lines"`
}
