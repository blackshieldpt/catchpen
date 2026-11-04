package api

// PathTasks is also POST, with a SubmitTaskRequest: queue one headless task,
// as catchpen task run does. It answers with the task as queued, a TaskInfo.
// Its supervisor is a process of the daemon's, which outlives a restart. It
// needs the client's config:write scope (CodeForbidden otherwise): a task
// runs any command in a guest that holds the template's secrets.

// PathTaskCancel is POST, with no body (or {}): cancel the task, as catchpen
// task cancel does, as a job. Its VM is destroyed, with any commits not yet
// exported, unless its supervisor keeps it (Keep, or a canary hit); a lost
// task's VM is destroyed even with Keep. A task already ended is refused
// with CodeConflict.
func PathTaskCancel(id string) string { return PathTask(id) + "/cancel" }

// PathTaskDelete is POST, with no body (or {}): delete an ended task's
// record, output and transcripts, as catchpen task rm does, as a job. A task
// whose supervisor runs is refused with CodeConflict (cancel it first); one
// whose supervisor died (lost) has its VM destroyed too.
//
// Cancel and delete are refused with CodeConflict on a lost task (not ended,
// its supervisor gone) whose VM a canary hit paused, or whose decoys were
// used: either call would destroy that VM. Destroy it with PathVMDestroy,
// which confirms the hit.
func PathTaskDelete(id string) string { return PathTask(id) + "/delete" }

// PathTemplateBuild is POST, with a BuildTemplateRequest: build a
// template's base, as catchpen template build does, as a job.
const PathTemplateBuild = PathTemplates + "/build"

// PathTemplateRebuild is POST, with a RebuildTemplatesRequest: build new
// base generations from a fresh cloud image, as catchpen template rebuild
// does, as a job. Existing VMs keep their bases.
const PathTemplateRebuild = PathTemplates + "/rebuild"

// SubmitTaskRequest is a task to run: catchpen task run's flags, but
// --prompt-file (the prompt is sent as text), --file, --parallel and
// --follow. A repo with a user, password, query or escape (any of @ ? # %)
// is refused, as for a VM. A template whose ssh.agent isn't off is refused
// with CodeConflict: the task's VM would load or forward the host's SSH keys.
type SubmitTaskRequest struct {
	Template     string  `json:"template,omitempty"` // a -t spec; default the defaults
	Repo         string  `json:"repo,omitempty"`     // forge:owner/repo or a URL
	Agent        string  `json:"agent,omitempty"`    // default claude; cmd runs Cmd
	Cmd          string  `json:"cmd,omitempty"`      // with agent cmd: the guest command, prompt on stdin
	Prompt       string  `json:"prompt"`
	Timeout      int64   `json:"timeout_s,omitempty"` // 60 to 172800; default 7200
	MaxBudgetUSD float64 `json:"max_budget_usd,omitempty"`
	Out          string  `json:"out,omitempty"`  // pr, bundle, patch or none; default pr with a repo, else none
	Into         string  `json:"into,omitempty"` // pr and bundle: the absolute path of a git repository on the daemon's host
	Title        string  `json:"title,omitempty"`
	RW           bool    `json:"rw,omitempty"` // a read-write deploy key
	Keep         bool    `json:"keep,omitempty"`
}

// BuildTemplateRequest is the template whose base to build.
type BuildTemplateRequest struct {
	Template string `json:"template,omitempty"` // a -t spec; default the defaults
	Rebuild  bool   `json:"rebuild,omitempty"`  // even if the base exists, re-checking the cloud image upstream
}

// RebuildTemplatesRequest is the templates to rebuild: some, or All.
type RebuildTemplatesRequest struct {
	Templates []string `json:"templates,omitempty"` // -t specs ("default" for the defaults), at most MaxRebuild
	All       bool     `json:"all,omitempty"`       // every template with a base or a VM
}

// MaxRebuild bounds RebuildTemplatesRequest.Templates.
const MaxRebuild = 64
