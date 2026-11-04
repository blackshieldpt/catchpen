package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/blackshieldpt/catchpen/internal/runner"
	"github.com/blackshieldpt/catchpen/internal/task"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// maxTimeout is task.Spec.Normalize's bound, in seconds: a larger value
// would overflow a time.Duration before Normalize sees it.
const maxTimeout = int64(48 * time.Hour / time.Second)

func (s *Server) submitTask(w http.ResponseWriter, r *http.Request) {
	// A task runs what it is told in a guest that holds the template's
	// secrets, which it can write to its error, result or commits.
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	var req api.SubmitTaskRequest
	if !decode(w, r, &req) {
		return
	}
	// As for a VM: a URL's user, password or query token would be kept in
	// task.json, and quoted in errors.
	if strings.ContainsAny(req.Repo, "@?#%") {
		writeError(w, api.CodeBadRequest, "a repo with any of @ ? # % (a user, password, query or escape) is refused: use forge:owner/repo or a plain URL")
		return
	}
	if req.Into != "" && (!filepath.IsAbs(req.Into) || filepath.Clean(req.Into) != req.Into) {
		writeError(w, api.CodeBadRequest, "into is the absolute path of a git repository on the daemon's host")
		return
	}
	if req.Timeout < 0 || req.Timeout > maxTimeout {
		writeError(w, api.CodeBadRequest, "timeout must be between 1m and 48h")
		return
	}
	spec := task.Spec{Template: req.Template, Repo: req.Repo, Agent: req.Agent, Cmd: req.Cmd, Prompt: req.Prompt,
		Timeout: time.Duration(req.Timeout) * time.Second, MaxBudgetUSD: req.MaxBudgetUSD, Out: req.Out,
		Into: req.Into, Title: req.Title, RW: req.RW, Keep: req.Keep}
	// No prompt file, and no working directory for into to default to.
	if err := spec.Normalize("", ""); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if spec.Into != "" && !filepath.IsAbs(spec.Into) {
		writeError(w, api.CodeBadRequest, "out "+spec.Out+" needs into: the absolute path of a git repository on the daemon's host")
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	res, err := m.Resolve(template.NormalizeSpec(spec.Template))
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if mode := res.T.SSH.Mode(); mode != template.AgentOff {
		writeError(w, api.CodeConflict, fmt.Sprintf("-t %s sets ssh.agent: %s, which loads or forwards the host's SSH keys: run it with catchpen task run", res.T.Name, mode))
		return
	}
	m.Client = clientOf(r)
	t, err := (&runner.Runner{M: m}).Submit(spec)
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskInfo(t))
}

// findTask finds the task named in the path, as task does.
func (s *Server) findTask(w http.ResponseWriter, r *http.Request) (*task.Task, *vm.Manager, bool) {
	m, ok := s.manager(w, r)
	if !ok {
		return nil, nil, false
	}
	ts, err := m.Tasks().List()
	if err != nil {
		s.fail(w, r, "the tasks can't be listed", err)
		return nil, nil, false
	}
	for _, t := range ts {
		if t.ID == r.PathValue("id") {
			return t, m, true
		}
	}
	writeError(w, api.CodeNotFound, "no such task")
	return nil, nil, false
}

// taskHeld answers a lost task (not ended, its supervisor gone) whose VM a
// canary hit paused, or whose decoys were used: cancelling or deleting it
// destroys that VM, the evidence. A live supervisor keeps a hit VM itself.
func taskHeld(w http.ResponseWriter, m *vm.Manager, t *task.Task) bool {
	if task.Final(t.State) || t.Alive() || (!m.Paused(t.VM) && !m.Hit(t.VM)) {
		return false
	}
	writeError(w, api.CodeConflict, "a canary hit "+t.VM+", which this call can destroy: destroy it with POST "+api.PathVMDestroy(t.VM)+", which confirms the hit")
	return true
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	t, m, ok := s.findTask(w, r)
	if !ok {
		return
	}
	if task.Final(t.State) {
		writeError(w, api.CodeConflict, "task "+t.ID+" is already "+t.State)
		return
	}
	if taskHeld(w, m, t) {
		return
	}
	s.startJob(w, r, m, api.JobTaskCancel, t.VM, func(ctx context.Context, m *vm.Manager) error {
		return (&runner.Runner{M: m}).Cancel(t.ID)
	})
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	t, m, ok := s.findTask(w, r)
	if !ok {
		return
	}
	if !task.Final(t.State) && t.Alive() {
		writeError(w, api.CodeConflict, "task "+t.ID+" is "+t.State+": cancel it first")
		return
	}
	if taskHeld(w, m, t) {
		return
	}
	s.startJob(w, r, m, api.JobTaskDelete, t.VM, func(ctx context.Context, m *vm.Manager) error {
		return (&runner.Runner{M: m}).Remove(t.ID)
	})
}

func (s *Server) buildTemplate(w http.ResponseWriter, r *http.Request) {
	var req api.BuildTemplateRequest
	if !decode(w, r, &req) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	res, err := m.Resolve(req.Template)
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	s.startJob(w, r, m, api.JobTemplateBuild, "", func(ctx context.Context, m *vm.Manager) error {
		_, err := m.EnsureBase(ctx, res, req.Rebuild)
		return err
	})
}

func (s *Server) rebuildTemplates(w http.ResponseWriter, r *http.Request) {
	var req api.RebuildTemplatesRequest
	if !decode(w, r, &req) {
		return
	}
	switch {
	case req.All && len(req.Templates) > 0:
		writeError(w, api.CodeBadRequest, "all takes no templates")
		return
	case !req.All && len(req.Templates) == 0:
		writeError(w, api.CodeBadRequest, `give templates ("default" for the defaults) or all`)
		return
	case len(req.Templates) > api.MaxRebuild:
		writeError(w, api.CodeBadRequest, fmt.Sprintf("at most %d templates", api.MaxRebuild))
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	var targets []string
	for _, spec := range req.Templates {
		spec = template.NormalizeSpec(spec)
		if _, err := m.Resolve(spec); err != nil {
			writeError(w, api.CodeBadRequest, err.Error())
			return
		}
		targets = append(targets, spec)
	}
	s.startJob(w, r, m, api.JobTemplateRebuild, "", func(ctx context.Context, m *vm.Manager) error {
		if req.All {
			var err error
			if targets, err = m.RebuildTargets(); err != nil {
				return err
			}
		}
		return m.Rebuild(ctx, targets)
	})
}
