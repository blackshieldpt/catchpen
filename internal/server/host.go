package server

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/cache"
	"github.com/blackshieldpt/catchpen/internal/task"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// templates lists each category's sections, as catchpen template ls does,
// without their settings.
func (s *Server) templates(w http.ResponseWriter, r *http.Request) {
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	lib, err := m.Tpl.Library()
	if err != nil {
		s.fail(w, r, "the templates can't be loaded", err)
		return
	}
	out := make([]api.TemplateCategory, 0, len(template.Categories))
	for _, c := range template.Categories {
		def, err := lib.Default(c.Name)
		if err != nil {
			s.fail(w, r, "the templates can't be loaded", err)
			return
		}
		tc := api.TemplateCategory{Name: c.Name, About: c.About, Exclusive: c.Exclusive, Sections: []api.TemplateSection{}}
		secs, _ := lib.Sections(c.Name)
		for _, sec := range def {
			if sec.Name == "default" { // a default with settings of its own
				secs = append([]*template.Section{sec}, secs...)
			}
		}
		for _, sec := range secs {
			tc.Sections = append(tc.Sections, api.TemplateSection{
				Name: sec.Name, Description: sec.Description, Default: slices.Contains(def, sec),
				Builtin: sec.Source == "builtin", Group: api.List(sec.Group),
			})
		}
		out = append(out, tc)
	}
	writeJSON(w, http.StatusOK, out)
}

// userinfo is a URL's user and password, up to the last @ before its path:
// a repo given as a URL can carry a token, which catchpen keeps as typed (in
// task.json and the audit log).
var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/?#\s]*@`)

// redact drops the user and password of every URL in free text s.
func redact(s string) string { return userinfo.ReplaceAllString(s, "$1") }

// scheme is a URL's scheme, at the start of a string.
var scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// redactRepo is a repo as given to --repo (stored as typed, unchecked)
// without a user or password: one with an @ keeps only what follows its
// last @, after its scheme if it starts with one. Whatever lay before goes.
func redactRepo(r string) string {
	i := strings.LastIndex(r, "@")
	if i < 0 {
		return r
	}
	return scheme.FindString(r) + r[i+1:]
}

// withheld stands for the error of a task whose repo has an @ (a user or
// password): errors quote the repo, and pieces of it, in ways no rewrite
// reliably finds.
const withheld = "withheld: the task's repo has a user or password (catchpen task show prints the error)"

// taskError is a task's error without any URL's user and password.
func taskError(t *task.Task) string {
	if t.Error != "" && strings.Contains(t.Spec.Repo, "@") {
		return withheld
	}
	return redact(t.Error)
}

// redactAll is redact for every string in a decoded JSON value.
func redactAll(v any) any {
	switch v := v.(type) {
	case string:
		return redact(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = redactAll(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			if r, ok := e.(string); ok && k == "repo" { // task.submit's, as typed
				out[k] = redactRepo(r)
				continue
			}
			out[k] = redactAll(e)
		}
		return out
	}
	return v
}

// taskInfo copies, field by field, what the API may tell of a task.
func taskInfo(t *task.Task) api.TaskInfo {
	return api.TaskInfo{
		ID: t.ID, State: t.Status(), Agent: t.Spec.Agent, Template: t.Spec.Template, Repo: redactRepo(t.Spec.Repo),
		Title: t.Spec.Title, Out: t.Spec.Out, VM: t.VM, Batch: t.Spec.Batch,
		Timeout: int64(t.Spec.Timeout / time.Second), MaxBudgetUSD: t.Spec.MaxBudgetUSD,
		Created: t.Created, Started: t.Started, Ended: t.Ended, Exit: t.Exit, Error: taskError(t),
		Usage: api.TaskUsage{Turns: t.Usage.Turns, InputTokens: t.Usage.InputTokens,
			OutputTokens: t.Usage.OutputTokens, CostUSD: t.Usage.CostUSD},
		Branch: t.Branch, Commits: t.Commits, Output: t.Output,
	}
}

// listTasks is every task, newest first.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) ([]*task.Task, bool) {
	m, ok := s.manager(w, r)
	if !ok {
		return nil, false
	}
	ts, err := m.Tasks().List()
	if err != nil {
		s.fail(w, r, "the tasks can't be listed", err)
		return nil, false
	}
	return ts, true
}

// withheldError stands for a task's error to a client without config:write:
// the error can quote the task's guest, which holds the template's secrets.
const withheldError = "withheld: a task's error can quote its guest, which holds the template's secrets: reading it needs the config:write scope"

// scoped reports whether the request's client has scope (over the unix
// socket, every scope).
func scoped(r *http.Request, scope string) bool {
	c, ok := CallerOf(r.Context())
	return !ok || slices.Contains(c.Scopes, scope)
}

// shownTask is what r's client may read of t.
func shownTask(r *http.Request, t *task.Task) api.TaskInfo {
	i := taskInfo(t)
	if i.Error != "" && !scoped(r, ScopeConfigWrite) {
		i.Error = withheldError
	}
	return i
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.listTasks(w, r)
	if !ok {
		return
	}
	out := make([]api.TaskInfo, 0, len(ts))
	for _, t := range ts {
		out = append(out, shownTask(r, t))
	}
	writeJSON(w, http.StatusOK, out)
}

// task finds the task in the list, as findVM does a VM: a task.json that
// doesn't load is no task.
func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.listTasks(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	for _, t := range ts {
		if t.ID == id {
			writeJSON(w, http.StatusOK, shownTask(r, t))
			return
		}
	}
	writeError(w, api.CodeNotFound, "no such task")
}

// cursor is how an audit.Cursor travels as AuditPage.Next and before.
func cursor(c audit.Cursor) string {
	return c.Time.UTC().Format(time.RFC3339Nano) + "~" + strconv.Itoa(c.Skip)
}

func parseCursor(s string) (*audit.Cursor, error) {
	ts, skip, _ := strings.Cut(s, "~")
	t, err := time.Parse(time.RFC3339Nano, ts)
	n, nerr := strconv.Atoi(skip)
	if err != nil || nerr != nil || n < 0 {
		return nil, fmt.Errorf("invalid cursor")
	}
	return &audit.Cursor{Time: t, Skip: n}, nil
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("vm")
	if name != "" && vm.ValidName(name) != nil {
		writeError(w, api.CodeBadRequest, "invalid VM name")
		return
	}
	limit := api.DefaultAuditLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > api.MaxAuditLimit {
			writeError(w, api.CodeBadRequest, fmt.Sprintf("limit is 1 to %d", api.MaxAuditLimit))
			return
		}
		limit = n
	}
	var before *audit.Cursor
	if v := q.Get("before"); v != "" {
		var err error
		if before, err = parseCursor(v); err != nil {
			writeError(w, api.CodeBadRequest, "invalid before: pass a page's next")
			return
		}
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	es, next, err := m.Audit().Page(r.Context(), name, before, limit)
	if err != nil {
		s.fail(w, r, "the audit log can't be read", err)
		return
	}
	page := api.AuditPage{Events: make([]api.AuditEvent, 0, len(es))}
	for _, e := range es {
		d, _ := redactAll(e.Detail).(map[string]any)
		if e.Event == audit.ConfigWrite && !scoped(r, ScopeConfigWrite) {
			// A hash of a file mostly known confirms a guess of a secret in it.
			delete(d, "old_hash")
			delete(d, "new_hash")
		}
		page.Events = append(page.Events, api.AuditEvent{Time: e.Time, Event: e.Event, VM: e.VM, Detail: d})
	}
	if next != nil {
		page.Next = cursor(*next)
	}
	writeJSON(w, http.StatusOK, page)
}

// cache tells what the package caches hold, as catchpen cache does, without
// where they are.
func (s *Server) cache(w http.ResponseWriter, r *http.Request) {
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	us, err := cache.Store{Dir: m.P.Pkg()}.Usage()
	if err != nil {
		s.fail(w, r, "the caches can't be read", err)
		return
	}
	out := api.CacheInfo{Kinds: make([]api.CacheKind, 0, len(us)), MaxBytes: m.Cfg.CacheMaxBytes()}
	for _, u := range us {
		out.Kinds = append(out.Kinds, api.CacheKind{Kind: u.Kind, Files: u.Files, Bytes: u.Bytes})
		out.Bytes += u.Bytes
	}
	writeJSON(w, http.StatusOK, out)
}
