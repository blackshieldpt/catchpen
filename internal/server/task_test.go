package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
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

// supervisor is a home whose Manager starts, as a task's supervisor, a
// script that records its arguments and a SIGTERM in the file it returns,
// and waits up to 30 s.
func supervisor(t *testing.T) (*vmHost, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "started")
	bin := filepath.Join(dir, "catchpen")
	write(t, bin, "#!/bin/sh\necho \"$@\" >> "+log+"\ntrap 'kill $!; echo term >> "+log+"; exit 0' TERM\nsleep 30 & wait\n")
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	var h *vmHost
	h = home(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}, Bin: bin}, nil
	})
	t.Cleanup(func() { // the supervisors still waiting
		ts, _ := (task.Store{Dir: h.p.Tasks()}).List()
		for _, tk := range ts {
			_ = tk.Signal(syscall.SIGTERM)
		}
	})
	return h, log
}

// started waits for log to hold want n times.
func started(t *testing.T, log, want string, n int) {
	t.Helper()
	for range 100 {
		if b, _ := os.ReadFile(log); strings.Count(string(b), want) >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(log)
	t.Fatalf("%s: %q, want %q %d times", log, b, want, n)
}

// reaped waits for process pid to be gone, not left a zombie.
func reaped(t *testing.T, pid int) {
	t.Helper()
	for range 100 {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	t.Fatalf("process %d is still there: %s", pid, b)
}

// A task is submitted as catchpen task run would: stored with every field
// sent, its prompt in prompt.md, its supervisor started, and audited.
func TestSubmitTask(t *testing.T) {
	h, log := supervisor(t)
	c := h.client(t)
	ctx := context.Background()
	tk, err := c.SubmitTask(ctx, api.SubmitTaskRequest{Agent: "cmd", Cmd: "make fix", Prompt: "fix it"})
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != task.Queued || tk.Agent != "cmd" || tk.Out != task.OutNone || tk.VM != "task-"+tk.ID || tk.Timeout != 7200 || tk.Template != "default" {
		t.Errorf("submitted: %+v", tk)
	}
	started(t, log, "task-supervise "+tk.ID, 1)
	st := task.Store{Dir: h.p.Tasks()}
	got, err := st.Load(tk.ID)
	if err != nil || !got.Alive() || got.Spec.Cmd != "make fix" || got.Spec.Timeout != 2*time.Hour {
		t.Errorf("stored: %+v %v", got, err)
	}
	if b, err := os.ReadFile(st.Path(tk.ID, "prompt.md")); err != nil || string(b) != "fix it" {
		t.Errorf("prompt.md: %q %v", b, err)
	}
	if ev := lastEvent(t, h.p, audit.TaskSubmit); ev.VM != tk.VM || ev.Detail["task"] != tk.ID || ev.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", ev)
	}

	// Every field, by its name on the wire.
	into := gitRepo(t)
	st2, body := h.post(t, api.PathTasks, `{"template":"large","repo":"gh:o/r","agent":"claude","prompt":"p","timeout_s":600,`+
		`"max_budget_usd":2.5,"out":"bundle","into":"`+into+`","title":"T","rw":true,"keep":true}`)
	if st2 != http.StatusOK || !strings.Contains(body, `"timeout_s":600,"max_budget_usd":2.5,`) {
		t.Fatalf("on the wire: %d %s", st2, body)
	}
	ts, err := st.List()
	if err != nil || len(ts) != 2 {
		t.Fatalf("tasks: %+v %v", ts, err)
	}
	want := task.Spec{Template: "large", Repo: "gh:o/r", Agent: "claude", Timeout: 10 * time.Minute, MaxBudgetUSD: 2.5,
		Out: task.OutBundle, Into: into, Title: "T", RW: true, Keep: true}
	if got := ts[0].Spec; !reflect.DeepEqual(got, want) {
		t.Errorf("spec:\n got %+v\nwant %+v", got, want)
	}
}

// What is refused stores no task and starts no supervisor.
func TestSubmitTaskRefused(t *testing.T) {
	h, log := supervisor(t)
	c := h.client(t)
	into := gitRepo(t)
	notGit := t.TempDir()
	ok := api.SubmitTaskRequest{Agent: "cmd", Cmd: "true", Prompt: "p"}
	var e *api.Error
	for _, tc := range []struct {
		name string
		edit func(*api.SubmitTaskRequest)
		code string
		msg  string
	}{
		{"a repo's password", func(r *api.SubmitTaskRequest) { r.Repo, r.Out = "https://me:"+decoy+"@example.com/o/r", "none" }, api.CodeBadRequest,
			"a repo with any of @ ? # % (a user, password, query or escape) is refused: use forge:owner/repo or a plain URL"},
		{"a repo's query", func(r *api.SubmitTaskRequest) { r.Repo, r.Out = "https://example.com/o/r?access_token="+decoy, "none" }, api.CodeBadRequest, "a repo with any of @"},
		{"a repo's fragment", func(r *api.SubmitTaskRequest) { r.Repo, r.Out = "https://example.com/o/r#"+decoy, "none" }, api.CodeBadRequest, "a repo with any of @"},
		{"a repo's escape", func(r *api.SubmitTaskRequest) { r.Repo, r.Out = "https://me%3A"+decoy+"%40example.com/o/r", "none" }, api.CodeBadRequest, "a repo with any of @"},
		{"a relative into", func(r *api.SubmitTaskRequest) { r.Repo, r.Into = "gh:o/r", "repo" }, api.CodeBadRequest,
			"into is the absolute path of a git repository on the daemon's host"},
		{"an unclean into", func(r *api.SubmitTaskRequest) { r.Repo, r.Into = "gh:o/r", into+"/" }, api.CodeBadRequest, "into is the absolute path"},
		{"no into", func(r *api.SubmitTaskRequest) { r.Repo = "gh:o/r" }, api.CodeBadRequest,
			"out pr needs into: the absolute path of a git repository on the daemon's host"},
		{"into no repo", func(r *api.SubmitTaskRequest) { r.Repo, r.Out, r.Into = "gh:o/r", "bundle", notGit }, api.CodeBadRequest,
			"--out bundle needs a host repository: " + notGit + " is not one"},
		{"an agent", func(r *api.SubmitTaskRequest) { r.Agent, r.Cmd = "nosuch", "" }, api.CodeBadRequest, `unknown agent "nosuch"`},
		{"no prompt", func(r *api.SubmitTaskRequest) { r.Prompt = " " }, api.CodeBadRequest, "a task needs a prompt"},
		{"a negative timeout", func(r *api.SubmitTaskRequest) { r.Timeout = -1 }, api.CodeBadRequest, "timeout must be between 1m and 48h"},
		{"a negative overflowing timeout", func(r *api.SubmitTaskRequest) { r.Timeout = -(1 << 55) + 3600 }, api.CodeBadRequest, "timeout must be between 1m and 48h"},
		{"a short timeout", func(r *api.SubmitTaskRequest) { r.Timeout = 59 }, api.CodeBadRequest, "timeout must be between 1m and 48h"},
		{"a long timeout", func(r *api.SubmitTaskRequest) { r.Timeout = 48*3600 + 1 }, api.CodeBadRequest, "timeout must be between 1m and 48h"},
		// In nanoseconds, it overflows to an hour.
		{"an overflowing timeout", func(r *api.SubmitTaskRequest) { r.Timeout = 1<<55 + 3600 }, api.CodeBadRequest, "timeout must be between 1m and 48h"},
		{"an SSH agent", func(r *api.SubmitTaskRequest) { r.Template = "ssh-host" }, api.CodeConflict,
			"-t ssh-host sets ssh.agent: host, which loads or forwards the host's SSH keys: run it with catchpen task run"},
		{"a template", func(r *api.SubmitTaskRequest) { r.Template = "nosuch" }, api.CodeBadRequest, `unknown section "nosuch" (catchpen template ls lists them)`},
		{"an agent not installed", func(r *api.SubmitTaskRequest) { r.Template, r.Agent, r.Cmd = "minimal", "claude", "" }, api.CodeBadRequest,
			"-t minimal doesn't install claude"},
	} {
		r := ok
		tc.edit(&r)
		if _, err := c.SubmitTask(context.Background(), r); !errors.As(err, &e) || e.Code != tc.code || !strings.HasPrefix(e.Message, tc.msg) ||
			strings.Contains(err.Error(), decoy) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	for _, body := range []string{`{"prompt":"p","prompt_file":"/etc/passwd"}`, `{"prompt":"p","timeout":"1h"}`, `{"prompt":"p","parallel":2}`} {
		if st, got := h.post(t, api.PathTasks, body); st != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, st, got)
		}
	}
	if ts, err := (task.Store{Dir: h.p.Tasks()}).List(); err != nil || len(ts) != 0 {
		t.Errorf("stored: %+v %v", ts, err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Errorf("a supervisor started")
	}
}

// heldVM writes a task's VM, with a canary's file.
func heldVM(t *testing.T, h *vmHost, name, file string) {
	t.Helper()
	h.writeVM(t, vm.State{Name: name, Template: "base", Created: created})
	write(t, filepath.Join(h.p.VM(name), file), "{}")
}

// Cancelling signals a task's supervisor, or reaps a task whose supervisor
// died; either is audited. One ended, or holding a hit VM, is refused.
func TestCancelTask(t *testing.T) {
	h, log := supervisor(t)
	c := h.client(t)
	ctx := context.Background()
	wait := waiter(t, c)
	tk, err := c.SubmitTask(ctx, api.SubmitTaskRequest{Agent: "cmd", Cmd: "true", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	started(t, log, "task-supervise "+tk.ID, 1)
	if j, _ := wait(c.CancelTask(ctx, tk.ID)); j.State != api.JobDone || j.Kind != api.JobTaskCancel || j.VM != tk.VM {
		t.Errorf("cancel: %+v", j)
	}
	started(t, log, "term", 1)
	if ev := lastEvent(t, h.p, audit.TaskCancel); ev.VM != tk.VM || ev.Detail["task"] != tk.ID || ev.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", ev)
	}
	if got, err := (task.Store{Dir: h.p.Tasks()}).Load(tk.ID); err != nil {
		t.Fatal(err)
	} else {
		reaped(t, got.PID) // the daemon outlives its supervisors
	}

	// A live supervisor keeps a hit VM itself: the cancel goes through.
	live, err := c.SubmitTask(ctx, api.SubmitTaskRequest{Agent: "cmd", Cmd: "true", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	started(t, log, "task-supervise "+live.ID, 1)
	heldVM(t, h, live.VM, "canary-hit.json")
	if j, _ := wait(c.CancelTask(ctx, live.ID)); j.State != api.JobDone {
		t.Errorf("a live task with a hit VM: %+v", j)
	}
	started(t, log, "term", 2)

	h.task(t, task.Task{ID: "aaaaaaa1", State: task.Running, VM: "task-aaaaaaa1", Created: created, PID: 1 << 30})
	if j, _ := wait(c.CancelTask(ctx, "aaaaaaa1")); j.State != api.JobDone {
		t.Errorf("a lost task: %+v", j)
	}
	if got, err := (task.Store{Dir: h.p.Tasks()}).Load("aaaaaaa1"); err != nil || got.State != task.Cancelled || got.Error != "cancelled after its supervisor died" {
		t.Errorf("a lost task, stored: %+v %v", got, err)
	}

	h.task(t, task.Task{ID: "bbbbbbb2", State: task.Done, VM: "task-bbbbbbb2", Created: created})
	h.task(t, task.Task{ID: "ccccccc3", State: task.Running, VM: "task-ccccccc3", Created: created, PID: 1 << 30})
	heldVM(t, h, "task-ccccccc3", "paused.json")
	h.task(t, task.Task{ID: "ddddddd4", State: task.Queued, VM: "task-ddddddd4", Created: created, PID: 1 << 30})
	heldVM(t, h, "task-ddddddd4", "canary-hit.json")
	var e *api.Error
	for id, want := range map[string]string{
		"bbbbbbb2": "task bbbbbbb2 is already done",
		"ccccccc3": "a canary hit task-ccccccc3, which this call can destroy: destroy it with POST /v1/vms/task-ccccccc3/destroy, which confirms the hit",
		"ddddddd4": "a canary hit task-ddddddd4, which this call can destroy",
	} {
		if _, err := c.CancelTask(ctx, id); !errors.As(err, &e) || e.Code != api.CodeConflict || !strings.HasPrefix(e.Message, want) {
			t.Errorf("%s: %v", id, err)
		}
	}
	if _, err := c.CancelTask(ctx, "eeeeeee5"); code(err) != api.CodeNotFound {
		t.Errorf("no such task: %v", err)
	}
	if st, body := h.post(t, api.PathTaskCancel("ccccccc3"), `{"confirm":true}`); st != http.StatusBadRequest {
		t.Errorf("with a body: %d %s", st, body)
	}
	for _, name := range []string{"task-ccccccc3", "task-ddddddd4"} {
		if _, err := os.Stat(h.p.VM(name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if js, err := c.Jobs(ctx); err != nil || len(js) != 3 {
		t.Errorf("jobs: %+v %v", js, err)
	}
}

// Deleting an ended or lost task removes its record, audited; one whose
// supervisor runs, or a lost one holding a hit VM, is refused.
func TestDeleteTask(t *testing.T) {
	h, log := supervisor(t)
	c := h.client(t)
	ctx := context.Background()
	wait := waiter(t, c)
	st := task.Store{Dir: h.p.Tasks()}
	h.task(t, task.Task{ID: "aaaaaaa1", State: task.Done, VM: "task-aaaaaaa1", Created: created})
	write(t, st.Path("aaaaaaa1", "output.jsonl"), "{}\n")
	if j, _ := wait(c.DeleteTask(ctx, "aaaaaaa1")); j.State != api.JobDone || j.Kind != api.JobTaskDelete || j.VM != "task-aaaaaaa1" {
		t.Errorf("delete: %+v", j)
	}
	if _, err := os.Stat(st.Path("aaaaaaa1", "")); !os.IsNotExist(err) {
		t.Errorf("still there: %v", err)
	}
	if ev := lastEvent(t, h.p, audit.TaskRemove); ev.VM != "task-aaaaaaa1" || ev.Detail["task"] != "aaaaaaa1" || ev.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", ev)
	}

	h.task(t, task.Task{ID: "bbbbbbb2", State: task.Running, VM: "task-bbbbbbb2", Created: created, PID: 1 << 30})
	if j, _ := wait(c.DeleteTask(ctx, "bbbbbbb2")); j.State != api.JobDone {
		t.Errorf("a lost task: %+v", j)
	}
	h.task(t, task.Task{ID: "ccccccc3", State: task.Failed, VM: "task-ccccccc3", Created: created})
	heldVM(t, h, "task-ccccccc3", "canary-hit.json")
	if j, _ := wait(c.DeleteTask(ctx, "ccccccc3")); j.State != api.JobDone {
		t.Errorf("an ended task with a hit VM: %+v", j)
	}
	if _, err := os.Stat(h.p.VM("task-ccccccc3")); err != nil {
		t.Errorf("its VM: %v", err)
	}

	tk, err := c.SubmitTask(ctx, api.SubmitTaskRequest{Agent: "cmd", Cmd: "true", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	started(t, log, "task-supervise "+tk.ID, 1)
	h.task(t, task.Task{ID: "ddddddd4", State: task.Running, VM: "task-ddddddd4", Created: created, PID: 1 << 30})
	heldVM(t, h, "task-ddddddd4", "paused.json")
	var e *api.Error
	for id, want := range map[string]string{
		tk.ID:      "task " + tk.ID + " is queued: cancel it first",
		"ddddddd4": "a canary hit task-ddddddd4, which this call can destroy",
	} {
		if _, err := c.DeleteTask(ctx, id); !errors.As(err, &e) || e.Code != api.CodeConflict || !strings.HasPrefix(e.Message, want) {
			t.Errorf("%s: %v", id, err)
		}
	}
	if _, err := c.DeleteTask(ctx, "eeeeeee5"); code(err) != api.CodeNotFound {
		t.Errorf("no such task: %v", err)
	}
	if st, body := h.post(t, api.PathTaskDelete(tk.ID), `{"confirm":true}`); st != http.StatusBadRequest {
		t.Errorf("with a body: %d %s", st, body)
	}
	if ts, err := st.List(); err != nil || len(ts) != 2 {
		t.Errorf("left: %+v %v", ts, err)
	}
}

// buildHome is a home with the default template's base built, whose cloud
// image can't be fetched: its lock is a directory.
func buildHome(t *testing.T) *vmHost {
	t.Helper()
	h := home(t, nil)
	m := &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}
	r, err := m.Resolve("default")
	if err != nil {
		t.Fatal(err)
	}
	write(t, r.BasePath(h.p), "base")
	if err := os.MkdirAll(filepath.Join(h.p.Images(), r.Distro.Name+".img.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

// A build is a job that builds the base only if asked to or if there is
// none; a rebuild, a job for the templates named or all.
func TestBuildTemplates(t *testing.T) {
	h := buildHome(t)
	c := h.client(t)
	ctx := context.Background()
	wait := waiter(t, c)
	if j, _ := wait(c.RebuildTemplates(ctx, api.RebuildTemplatesRequest{All: true})); j.State != api.JobDone || j.Kind != api.JobTemplateRebuild || j.VM != "" {
		t.Errorf("rebuild all of none: %+v", j)
	}
	if j, _ := wait(c.BuildTemplate(ctx, api.BuildTemplateRequest{})); j.State != api.JobDone || j.Kind != api.JobTemplateBuild || j.VM != "" {
		t.Errorf("build: %+v", j)
	}
	if j, _ := wait(c.BuildTemplate(ctx, api.BuildTemplateRequest{Rebuild: true})); j.State != api.JobFailed || !strings.Contains(j.Error, ".img.lock") {
		t.Errorf("build, rebuild: %+v", j)
	}
	if j, _ := wait(c.RebuildTemplates(ctx, api.RebuildTemplatesRequest{Templates: []string{"default,"}})); j.State != api.JobFailed || // normalized
		!strings.HasPrefix(j.Error, "default: ") || !strings.Contains(j.Error, ".img.lock") {
		t.Errorf("rebuild default: %+v", j)
	}
	// The build recorded the default's spec: all is it now.
	if j, _ := wait(c.RebuildTemplates(ctx, api.RebuildTemplatesRequest{All: true})); j.State != api.JobFailed || !strings.HasPrefix(j.Error, "default: ") {
		t.Errorf("rebuild all: %+v", j)
	}
	// On the wire, and the most templates a rebuild takes; each fails at the
	// image's lock, as above.
	most := make([]string, api.MaxRebuild)
	for i := range most {
		most[i] = "default"
	}
	if _, err := c.RebuildTemplates(ctx, api.RebuildTemplatesRequest{Templates: most}); err != nil {
		t.Errorf("%d templates: %v", len(most), err)
	}
	for path, body := range map[string]string{
		api.PathTemplateBuild:   `{"template":"default","rebuild":true}`,
		api.PathTemplateRebuild: `{"templates":["default"],"all":false}`,
	} {
		if st, got := h.post(t, path, body); st != http.StatusAccepted {
			t.Errorf("%s on the wire: %d %s", body, st, got)
		}
	}
	js, err := c.Jobs(ctx)
	if err != nil || len(js) != 8 {
		t.Fatalf("jobs: %+v %v", js, err)
	}
	for _, j := range js[:3] {
		if j, _ := wait(&j, nil); j.State != api.JobFailed || !strings.Contains(j.Error, ".img.lock") {
			t.Errorf("on the wire: %+v", j)
		}
	}
}

// What is refused starts no job.
func TestBuildTemplatesRefused(t *testing.T) {
	h := buildHome(t)
	c := h.client(t)
	ctx := context.Background()
	many := make([]string, api.MaxRebuild+1)
	for i := range many {
		many[i] = "default"
	}
	var e *api.Error
	for name, tc := range map[string]struct {
		err error
		msg string
	}{
		"build nosuch":        {second(c.BuildTemplate(ctx, api.BuildTemplateRequest{Template: "nosuch"})), `unknown section "nosuch"`},
		"rebuild nosuch":      {second(c.RebuildTemplates(ctx, api.RebuildTemplatesRequest{Templates: []string{"default", "nosuch"}})), `unknown section "nosuch"`},
		"rebuild all and one": {second(c.RebuildTemplates(ctx, api.RebuildTemplatesRequest{All: true, Templates: []string{"default"}})), "all takes no templates"},
		"rebuild none":        {second(c.RebuildTemplates(ctx, api.RebuildTemplatesRequest{})), `give templates ("default" for the defaults) or all`},
		"rebuild too many":    {second(c.RebuildTemplates(ctx, api.RebuildTemplatesRequest{Templates: many})), "at most 64 templates"},
	} {
		if !errors.As(tc.err, &e) || e.Code != api.CodeBadRequest || !strings.HasPrefix(e.Message, tc.msg) {
			t.Errorf("%s: %v", name, tc.err)
		}
	}
	if st, body := h.post(t, api.PathTemplateBuild, `{"template":"minimal","force":true}`); st != http.StatusBadRequest {
		t.Errorf("build with an unknown field: %d %s", st, body)
	}
	if js, err := c.Jobs(ctx); err != nil || len(js) != 0 {
		t.Errorf("jobs: %+v %v", js, err)
	}
}

// Over TCP, submitting needs config:write; the audit names the client.
func TestSubmitTaskScope(t *testing.T) {
	h, log := supervisor(t)
	bin := filepath.Join(filepath.Dir(log), "catchpen")
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}, Bin: bin}, nil
	})
	ctx := context.Background()
	req := api.SubmitTaskRequest{Agent: "cmd", Cmd: "true", Prompt: "p"}
	var e *api.Error
	laptop := a.client(t, apiclient.Options{Cert: &a.laptop}) // no scope
	if _, err := laptop.SubmitTask(ctx, req); !errors.As(err, &e) || e.Code != api.CodeForbidden ||
		e.Message != "this call needs the config:write scope (catchpen daemon client add --scope config:write)" {
		t.Errorf("without the scope: %v", err)
	}
	if ts, err := (task.Store{Dir: h.p.Tasks()}).List(); err != nil || len(ts) != 0 {
		t.Errorf("stored: %+v %v", ts, err)
	}
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	tk, err := cockpit.SubmitTask(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	started(t, log, "task-supervise "+tk.ID, 1)
	if ev := lastEvent(t, h.p, audit.TaskSubmit); ev.Detail["task"] != tk.ID || ev.Detail["client"] != "cockpit" {
		t.Errorf("audit: %+v", ev)
	}

	// A task's error is read only with the scope: it can quote the guest.
	h.task(t, task.Task{ID: "aaaaaaa1", State: task.Failed, VM: "task-aaaaaaa1", Created: created, Error: "the guest says " + decoy})
	if got, err := laptop.Task(ctx, "aaaaaaa1"); err != nil || got.Error != withheldError {
		t.Errorf("without the scope: %+v %v", got, err)
	}
	if ts, err := laptop.Tasks(ctx); err != nil || len(ts) != 2 || ts[1].Error != withheldError || ts[0].Error != "" {
		t.Errorf("all, without the scope: %+v %v", ts, err)
	}
	if got, err := cockpit.Task(ctx, "aaaaaaa1"); err != nil || got.Error != "the guest says "+decoy {
		t.Errorf("with the scope: %+v %v", got, err)
	}
}
