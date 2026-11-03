package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/task"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// userTemplates writes category files over the builtins: env.yaml with two
// sections holding decoys, its default and a list section; distros.yaml
// replacing a builtin; idle.yaml with a default of its own settings.
func (h *vmHost) userTemplates(t *testing.T) {
	t.Helper()
	write(t, filepath.Join(h.p.Templates(), "env.yaml"), `default: [mine]
mine: { description: my env, env: { TOKEN: `+decoy+` } }
other: { description: other env, env: { KEY: `+decoy+` } }
both: [mine, other]
`)
	write(t, filepath.Join(h.p.Templates(), "distros.yaml"), "debian-13: { description: my debian, distro: debian-13 }\n")
	write(t, filepath.Join(h.p.Templates(), "idle.yaml"), "default: { description: Stop after 45 minutes idle, idle_stop: 45m }\n")
}

func category(t *testing.T, cs []api.TemplateCategory, name string) api.TemplateCategory {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no category %s", name)
	return api.TemplateCategory{}
}

func section(t *testing.T, c api.TemplateCategory, name string) api.TemplateSection {
	t.Helper()
	for _, s := range c.Sections {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("%s: no section %s", c.Name, name)
	return api.TemplateSection{}
}

func TestTemplates(t *testing.T) {
	h := home(t, nil)
	h.userTemplates(t)
	cs, err := h.client(t).Templates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
	}
	var want []string
	for _, c := range template.Categories {
		want = append(want, c.Name)
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("categories %v, want %v", names, want)
	}
	env := category(t, cs, "env")
	wantEnv := []api.TemplateSection{
		{Name: "both", Group: []string{"mine", "other"}},
		{Name: "mine", Description: "my env", Default: true, Group: []string{}},
		{Name: "other", Description: "other env", Group: []string{}},
	}
	if env.About != "environment for ssh sessions" || env.Exclusive || !reflect.DeepEqual(env.Sections, wantEnv) {
		t.Errorf("env: %+v", env)
	}
	distros := category(t, cs, "distros")
	if s := section(t, distros, "debian-13"); s.Builtin || s.Default || s.Description != "my debian" {
		t.Errorf("a user's section over a builtin: %+v", s)
	}
	if s := section(t, distros, "ubuntu-24.04"); !s.Builtin || !s.Default {
		t.Errorf("the builtin default: %+v", s)
	}
	if !distros.Exclusive {
		t.Errorf("distros not exclusive")
	}
	idle := category(t, cs, "idle")
	if len(idle.Sections) == 0 || !reflect.DeepEqual(idle.Sections[0], api.TemplateSection{Name: "default", Description: "Stop after 45 minutes idle", Default: true, Group: []string{}}) {
		t.Errorf("a default with settings of its own: %+v", idle.Sections)
	}
	for _, c := range cs {
		for _, s := range c.Sections {
			if s.Group == nil {
				t.Errorf("%s/%s: group null", c.Name, s.Name)
			}
		}
	}
	st, body := h.raw(t, api.PathTemplates)
	if st != http.StatusOK || strings.Contains(body, decoy) || strings.Contains(body, h.root) {
		t.Errorf("gives away a section's settings or a host path: %d %s", st, body)
	}
}

// task writes a task as the runner does, with its prompt.
func (h *vmHost) task(t *testing.T, tk task.Task) {
	t.Helper()
	if err := (task.Store{Dir: h.p.Tasks()}).Create(&tk, "prompt "+decoy); err != nil {
		t.Fatal(err)
	}
}

func exit(n int) *int { return &n }

// badRepo is a --repo URL whose user is an email and whose password holds a
// space, a / and a quote: catchpen stores it as typed.
const badRepo = "https://me@corp.com:a b/\"" + decoy + "@example.com/o/r"

// serveTasks is a home with three tasks, a task.json that doesn't load and a
// directory that isn't a task's.
func serveTasks(t *testing.T) *vmHost {
	t.Helper()
	h := home(t, nil)
	h.task(t, task.Task{ID: "aaaaaaa1", State: task.Done, VM: "task-aaaaaaa1", Created: created,
		Started: created.Add(time.Minute), Ended: created.Add(5 * time.Minute), PID: 4242, MemoryMB: 4096, Exit: exit(0),
		Spec: task.Spec{Agent: "claude", Template: "minimal", Repo: "gh:o/r", Title: "fix it", Out: task.OutPR,
			Into: "/" + decoy, Timeout: 30 * time.Minute, MaxBudgetUSD: 5, Batch: "b1", Parallel: 2},
		Usage:  task.Usage{Turns: 7, InputTokens: 1000, OutputTokens: 200, CostUSD: 0.5},
		Branch: "catchpen/fix-it", Commits: 2, Output: "https://example.com/o/r/pull/1",
		Error: "export: https://me@corp.com:" + decoy + "@example.com/x: push refused"})
	h.task(t, task.Task{ID: "bbbbbbb2", State: task.Running, VM: "task-bbbbbbb2", Created: created.Add(time.Hour),
		Started: created.Add(time.Hour), PID: 1 << 30,
		Spec: task.Spec{Agent: "cmd", Cmd: "deploy --token=" + decoy, Repo: "me:" + decoy + "@github.com/o/r", Out: task.OutNone, Timeout: time.Hour}})
	h.task(t, task.Task{ID: "ccccccc3", State: task.Failed, VM: "task-ccccccc3", Created: created.Add(30 * time.Minute),
		Error: "invalid repo URL " + strconv.Quote(badRepo) + `: expected owner/repo, got "` + decoy + `@example.com/o/r"`,
		Spec:  task.Spec{Agent: "codex", Out: task.OutPatch, Repo: badRepo, Into: "/" + decoy}})
	write(t, filepath.Join(h.p.Tasks(), "aaaaaaa1", "output.jsonl"), decoy+"\n")
	write(t, filepath.Join(h.p.Tasks(), "ddddddd4", "task.json"), "{")
	write(t, filepath.Join(h.p.Tasks(), "notes", "task.json"), `{"id":"notes"}`)
	return h
}

var wantTasks = []api.TaskInfo{
	{ID: "bbbbbbb2", State: "lost", Agent: "cmd", Repo: "github.com/o/r", Out: "none", VM: "task-bbbbbbb2", Timeout: 3600,
		Created: created.Add(time.Hour), Started: created.Add(time.Hour)},
	{ID: "ccccccc3", State: "failed", Agent: "codex", Repo: "https://example.com/o/r", Out: "patch", VM: "task-ccccccc3",
		Created: created.Add(30 * time.Minute), Error: withheld},
	{ID: "aaaaaaa1", State: "done", Agent: "claude", Template: "minimal", Repo: "gh:o/r", Title: "fix it", Out: "pr",
		VM: "task-aaaaaaa1", Batch: "b1", Timeout: 1800, MaxBudgetUSD: 5, Created: created,
		Started: created.Add(time.Minute), Ended: created.Add(5 * time.Minute), Exit: exit(0),
		Usage:  api.TaskUsage{Turns: 7, InputTokens: 1000, OutputTokens: 200, CostUSD: 0.5},
		Branch: "catchpen/fix-it", Commits: 2, Output: "https://example.com/o/r/pull/1",
		Error: "export: https://example.com/x: push refused"},
}

func TestTasks(t *testing.T) {
	h := serveTasks(t)
	got, err := h.client(t).Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(wantTasks) {
		t.Fatalf("got %d tasks, want %d: %+v", len(got), len(wantTasks), got)
	}
	for i := range wantTasks {
		if !reflect.DeepEqual(got[i], wantTasks[i]) {
			t.Errorf("task %d:\n got %+v\nwant %+v", i, got[i], wantTasks[i])
		}
	}
}

func TestRedactRepo(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"gh:o/r", "gh:o/r"},
		{"https://example.com/o/r", "https://example.com/o/r"},
		{badRepo, "https://example.com/o/r"},
		{"me:" + decoy + "@github.com/o/r", "github.com/o/r"},
		{"git@github.com:o/r", "github.com:o/r"},
		{"me:" + decoy + "@https://example.com/o/r", "https://example.com/o/r"}, // an @ before the scheme
		{"me:pa://" + decoy + "@example.com:o/r", "example.com:o/r"},            // a :// in the password
	} {
		if got := redactRepo(c.in); got != c.want || strings.Contains(got, decoy) {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTasksEmpty(t *testing.T) {
	if st, body := home(t, nil).raw(t, api.PathTasks); st != http.StatusOK || body != "[]\n" {
		t.Errorf("no tasks: %d %q", st, body)
	}
}

func TestTask(t *testing.T) {
	h := serveTasks(t)
	c := h.client(t)
	for _, want := range wantTasks {
		if got, err := c.Task(context.Background(), want.ID); err != nil || !reflect.DeepEqual(*got, want) {
			t.Errorf("%s:\n got %+v, %v\nwant %+v", want.ID, got, err, want)
		}
	}
	for _, id := range []string{"eeeeeee5", "ddddddd4", "notes", "../aaaaaaa1", "", ".", ".."} {
		if _, err := c.Task(context.Background(), id); code(err) != api.CodeNotFound {
			t.Errorf("%q: %v, want not_found", id, err)
		}
	}
}

// No response carries a task's prompt, command, host repo field, files, or a
// URL's credentials.
func TestTasksHideSecrets(t *testing.T) {
	h := serveTasks(t)
	for _, path := range []string{api.PathTasks, api.PathTask("aaaaaaa1"), api.PathTask("bbbbbbb2"), api.PathTask("ccccccc3")} {
		st, body := h.raw(t, path)
		if st != http.StatusOK || strings.Contains(body, decoy) {
			t.Errorf("%s: %d %s", path, st, body)
		}
	}
}

func TestAudit(t *testing.T) {
	h := home(t, nil)
	if st, body := h.raw(t, api.PathAudit); st != http.StatusOK || body != `{"events":[]}`+"\n" {
		t.Errorf("no log: %d %q", st, body)
	}
	// Within one second, so a cursor that drops the nanoseconds loses events.
	var lines []byte
	for i, e := range []audit.Event{{Event: audit.VMCreate, VM: "alpha"},
		{Event: audit.CredsPush, VM: "beta", Detail: map[string]any{"secrets": []string{"K1"},
			"repo": badRepo, "args": []any{"x", map[string]any{"url": "ssh://git:" + decoy + "@example.com/r"}}}},
		{Event: audit.VMStart, VM: "alpha"}} {
		e.Time = created.Add(time.Duration(i+1) * 100 * time.Millisecond)
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(append(lines, b...), '\n')
	}
	write(t, h.p.Audit(), string(lines))
	c := h.client(t)
	ctx := context.Background()
	p, err := c.Audit(ctx, apiclient.AuditQuery{})
	if err != nil || len(p.Events) != 3 || p.Events[0].Event != audit.VMStart || p.Events[2].Event != audit.VMCreate ||
		p.Next != "" || !p.Events[0].Time.Equal(created.Add(300*time.Millisecond)) {
		t.Errorf("all: %+v, %v", p, err)
	}
	p, err = c.Audit(ctx, apiclient.AuditQuery{Limit: 2})
	if err != nil || len(p.Events) != 2 || p.Next == "" {
		t.Fatalf("limit 2: %+v, %v", p, err)
	}
	p, err = c.Audit(ctx, apiclient.AuditQuery{Limit: 2, Before: p.Next})
	if err != nil || len(p.Events) != 1 || p.Events[0].Event != audit.VMCreate || p.Next != "" {
		t.Errorf("second page: %+v, %v", p, err)
	}
	p, err = c.Audit(ctx, apiclient.AuditQuery{VM: "beta"})
	if err != nil || len(p.Events) != 1 || p.Events[0].VM != "beta" ||
		!reflect.DeepEqual(p.Events[0].Detail, map[string]any{"secrets": []any{"K1"}, "repo": "https://example.com/o/r",
			"args": []any{"x", map[string]any{"url": "ssh://example.com/r"}}}) {
		t.Errorf("beta: %+v, %v", p, err)
	}
	for _, q := range []apiclient.AuditQuery{{VM: "../alpha"}, {Limit: -1}, {Limit: api.MaxAuditLimit + 1},
		{Before: "yesterday"}, {Before: "2026-10-01T12:00:00Z~-1"}, {Before: "2026-10-01T12:00:00Z"}} {
		if _, err := c.Audit(ctx, q); code(err) != api.CodeBadRequest {
			t.Errorf("%+v: %v, want bad_request", q, err)
		}
	}
	if st, body := h.raw(t, api.PathAudit); st != http.StatusOK || strings.Contains(body, decoy) || strings.Contains(body, `"detail":{}`) {
		t.Errorf("gives away a URL's credentials, or an empty detail: %d %s", st, body)
	}
	if st, _ := h.raw(t, api.PathAudit+"?limit=x"); st != http.StatusBadRequest {
		t.Errorf("limit=x: %d", st)
	}
	if p, err := c.Audit(ctx, apiclient.AuditQuery{Limit: api.MaxAuditLimit}); err != nil || len(p.Events) != 3 {
		t.Errorf("the largest limit: %+v, %v", p, err)
	}
}

func TestAuditDefaultLimit(t *testing.T) {
	h := home(t, nil)
	l := audit.Log{Path: h.p.Audit()}
	for range api.DefaultAuditLimit + 1 {
		if err := l.Record(audit.VMStart, "alpha", nil); err != nil {
			t.Fatal(err)
		}
	}
	p, err := h.client(t).Audit(context.Background(), apiclient.AuditQuery{})
	if err != nil || len(p.Events) != api.DefaultAuditLimit || p.Next == "" {
		t.Errorf("%d events, next %q, %v", len(p.Events), p.Next, err)
	}
}

func TestCache(t *testing.T) {
	h := home(t, nil)
	write(t, filepath.Join(h.p.Pkg(), "apt", "ab", "pkg.deb"), strings.Repeat("x", 100))
	write(t, filepath.Join(h.p.Pkg(), "apt", "ab", "pkg.deb.meta"), `{"url":"https://deb.debian.org/x"}`)
	write(t, filepath.Join(h.p.Pkg(), "apt", "ab", "next.deb.lock"), "")
	write(t, filepath.Join(h.p.Pkg(), "go", "cd", ".tmp-123"), strings.Repeat("x", 50))
	write(t, filepath.Join(h.p.Pkg(), "npm", "ef", "a.tgz"), strings.Repeat("x", 10))
	ci, err := h.client(t).Cache(context.Background())
	want := &api.CacheInfo{Kinds: []api.CacheKind{
		{Kind: "apt", Files: 1, Bytes: 100}, {Kind: "go", Files: 0, Bytes: 0}, {Kind: "npm", Files: 1, Bytes: 10}, {Kind: "pypi", Files: 0, Bytes: 0}, {Kind: "docker", Files: 0, Bytes: 0}},
		Bytes: 110, MaxBytes: config.Default().CacheMaxBytes()}
	if err != nil || !reflect.DeepEqual(ci, want) {
		t.Errorf("got %+v, %v\nwant %+v", ci, err, want)
	}
	if st, body := h.raw(t, api.PathCache); st != http.StatusOK || strings.Contains(body, h.root) {
		t.Errorf("gives away a host path: %d %s", st, body)
	}
}

// Each endpoint whose source can't be read answers internal with a fixed
// message, and logs why.
func TestHostReadsFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory it may not")
	}
	for _, c := range []struct {
		path, pattern, msg string
		break_             func(h *vmHost)
	}{
		{api.PathTemplates, "GET /v1/templates", "the templates can't be loaded", func(h *vmHost) {
			write(t, filepath.Join(h.p.Templates(), "env.yaml"), "mine: [\n")
		}},
		{api.PathTemplates, "GET /v1/templates", "the templates can't be loaded", func(h *vmHost) {
			write(t, filepath.Join(h.p.Templates(), "env.yaml"), "default: [nope]\n")
		}},
		{api.PathTasks, "GET /v1/tasks", "the tasks can't be listed", func(h *vmHost) {
			write(t, h.p.Tasks(), "not a directory")
		}},
		{api.PathTask("aaaaaaa1"), "GET /v1/tasks/{id}", "the tasks can't be listed", func(h *vmHost) {
			write(t, h.p.Tasks(), "not a directory")
		}},
		{api.PathAudit, "GET /v1/audit", "the audit log can't be read", func(h *vmHost) {
			if err := os.MkdirAll(h.p.Audit(), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{api.PathCache, "GET /v1/cache", "the caches can't be read", func(h *vmHost) {
			apt := filepath.Join(h.p.Pkg(), "apt")
			if err := os.MkdirAll(apt, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(apt, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(apt, 0o700) })
		}},
	} {
		h := home(t, nil)
		c.break_(h)
		st, body := h.raw(t, c.path)
		if st != http.StatusInternalServerError || !strings.Contains(body, c.msg) || strings.Contains(body, h.root) {
			t.Errorf("%s: %d %s", c.path, st, body)
		}
		h.mu.Lock()
		if !strings.Contains(h.log.String(), "api: "+c.pattern+": ") {
			t.Errorf("%s: log %q", c.path, h.log)
		}
		h.mu.Unlock()
	}
}

// A config.yaml that doesn't load fails every endpoint, as it does the VMs'.
func TestHostReadsBadConfig(t *testing.T) {
	h := home(t, func() (*vm.Manager, error) { return nil, errors.New("config.yaml: " + decoy) })
	for _, path := range []string{api.PathTemplates, api.PathTasks, api.PathTask("aaaaaaa1"), api.PathAudit, api.PathCache} {
		if st, body := h.raw(t, path); st != http.StatusInternalServerError || strings.Contains(body, decoy) {
			t.Errorf("%s: %d %s", path, st, body)
		}
	}
}
