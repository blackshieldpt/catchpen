package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// post POSTs body to path, for the status and body answered.
func (h *vmHost) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}
	resp, err := hc.Post("http://catchpen"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waiter waits, with c, for the job a call started, and its lines.
func waiter(t *testing.T, c *apiclient.Client) func(*api.JobInfo, error) (*api.JobInfo, []string) {
	return func(j *api.JobInfo, err error) (*api.JobInfo, []string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var lines []string
		end, err := c.WaitJob(ctx, j.ID, func(l string) { lines = append(lines, l) })
		if err != nil {
			t.Fatal(err)
		}
		return end, lines
	}
}

// lastEvent is the last audit event named event.
func lastEvent(t *testing.T, p paths.Paths, event string) audit.Event {
	t.Helper()
	var last *audit.Event
	err := audit.Log{Path: p.Audit()}.TailEvents(context.Background(), "", false, func(e audit.Event) error {
		if e.Event == event {
			last = &e
		}
		return nil
	})
	if err != nil || last == nil {
		t.Fatalf("no %s event: %v", event, err)
	}
	return *last
}

func TestDestroyVM(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	h.disk(t, "alpha", "s1", "s2")
	c := h.client(t)
	wait := waiter(t, c)
	ctx := context.Background()
	_, err := c.DestroyVM(ctx, "alpha", api.DestroyVMRequest{Snapshots: []string{"s1", "s2"}})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConfirmRequired || e.Message != `destroying alpha deletes all data inside it, `+
		`its snapshots (s1, s2) included: send "confirm": true, with the snapshots it deletes (GET this path for all it loses)` {
		t.Fatalf("without confirm: %v", err)
	}
	if st, _ := h.post(t, api.PathVMDestroy("alpha"), ""); st != http.StatusPreconditionRequired {
		t.Errorf("no body: %d", st)
	}
	l, err := c.DestroyLoss(ctx, "alpha")
	if err != nil || !reflect.DeepEqual(*l, api.VMLoss{Disk: true, Snapshots: []string{"s1", "s2"}}) {
		t.Fatalf("loss: %+v %v", l, err)
	}
	for _, snaps := range [][]string{nil, {"s1"}, {"s2", "s1"}, {"s1", "s2", "s3"}} {
		if _, err := c.DestroyVM(ctx, "alpha", api.DestroyVMRequest{Confirm: true, Snapshots: snaps}); code(err) != api.CodeConflict {
			t.Errorf("confirmed %v: %v", snaps, err)
		}
	}
	if _, err := c.VM(ctx, "alpha"); err != nil {
		t.Fatalf("gone without its snapshots confirmed: %v", err)
	}
	j, lines := wait(c.DestroyVM(ctx, "alpha", api.DestroyVMRequest{Confirm: true, Snapshots: l.Snapshots}))
	if j.State != api.JobDone || j.Kind != api.JobVMDestroy || j.VM != "alpha" || j.Client != api.ClientLocal ||
		!slices.Contains(lines, "destroyed alpha") {
		t.Errorf("job: %+v %q", j, lines)
	}
	if _, err := c.VM(ctx, "alpha"); code(err) != api.CodeNotFound {
		t.Errorf("still there: %v", err)
	}
	if e := lastEvent(t, h.p, audit.VMDestroy); e.VM != "alpha" || e.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", e)
	}
	js, err := c.Jobs(ctx)
	if err != nil || len(js) != 1 || js[0].ID != j.ID || js[0].State != api.JobDone {
		t.Errorf("jobs: %+v %v", js, err)
	}
	for _, name := range []string{"alpha", "nope", "..", "Bad"} {
		if _, err := c.DestroyVM(ctx, name, api.DestroyVMRequest{Confirm: true}); code(err) != api.CodeNotFound {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := c.DestroyLoss(ctx, name); code(err) != api.CodeNotFound {
			t.Errorf("%s loss: %v", name, err)
		}
	}
}

// A VM's loss tells what destroying it takes from an investigation: it
// runs, a canary hit paused it, and it runs a task.
func TestDestroyLoss(t *testing.T) {
	h := home(t, nil)
	m := &vm.Manager{P: h.p}
	vmtest.FakeQEMU(t, h.p.VM("beta"), "beta", 512, func() bool { _, ok := m.Running("beta"); return ok })
	h.writeVM(t, vm.State{Name: "beta", Template: "base", Created: created, Task: "0123abcd"})
	write(t, filepath.Join(h.p.VM("beta"), "paused.json"), "{}")
	write(t, filepath.Join(h.p.VM("beta"), "canary-hit.json"), "{}")
	l, err := h.client(t).DestroyLoss(context.Background(), "beta")
	want := api.VMLoss{Disk: true, Snapshots: []string{}, Running: true, Paused: true, CanaryHit: true, Task: "0123abcd"}
	if err != nil || !reflect.DeepEqual(*l, want) {
		t.Errorf("got %+v %v, want %+v", l, err, want)
	}
	if st, body := h.raw(t, api.PathVMDestroy("beta")); st != http.StatusOK || !strings.Contains(body, `"snapshots":[]`) {
		t.Errorf("raw: %d %s", st, body)
	}
	c := h.client(t)
	for _, r := range []api.DestroyVMRequest{{Confirm: true}, {Confirm: true, Paused: true}, {Confirm: true, CanaryHit: true}} {
		if _, err := c.DestroyVM(context.Background(), "beta", r); code(err) != api.CodeConflict {
			t.Errorf("confirmed %+v: %v", r, err)
		}
	}
	r := api.DestroyVMRequest{Confirm: true, Snapshots: l.Snapshots, Paused: true, CanaryHit: true}
	if j, _ := waiter(t, c)(c.DestroyVM(context.Background(), "beta", r)); j.State != api.JobDone {
		t.Errorf("confirmed all it loses: %+v", j)
	}
}

// Over TCP, the job and its audit event name the enrolled client.
func TestDestroyVMOverTCP(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	c := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	wait := waiter(t, c)
	if j, _ := wait(c.DestroyVM(context.Background(), "alpha", api.DestroyVMRequest{Confirm: true})); j.State != api.JobDone || j.Client != "cockpit" {
		t.Errorf("job: %+v", j)
	}
	if e := lastEvent(t, h.p, audit.VMDestroy); e.Detail["client"] != "cockpit" {
		t.Errorf("audit: %+v", e)
	}
}

func TestStopVM(t *testing.T) {
	h := home(t, nil)
	m := &vm.Manager{P: h.p}
	vmtest.FakeQEMU(t, h.p.VM("beta"), "beta", 512, func() bool { _, ok := m.Running("beta"); return ok })
	c := h.client(t)
	wait := waiter(t, c)
	ctx := context.Background()
	write(t, filepath.Join(h.p.VM("beta"), "paused.json"), "{}")
	if _, err := c.StopVM(ctx, "beta", false); code(err) != api.CodeVMPaused {
		t.Errorf("a paused VM stopped without force: %v", err)
	}
	if err := os.Remove(filepath.Join(h.p.VM("beta"), "paused.json")); err != nil { // Stop forces a paused VM off
		t.Fatal(err)
	}
	vmtest.FakeQEMU(t, h.p.VM("gamma"), "gamma", 512, func() bool { _, ok := m.Running("gamma"); return ok })
	write(t, filepath.Join(h.p.VM("gamma"), "paused.json"), "{}")
	if j, _ := wait(c.StopVM(ctx, "gamma", true)); j.State != api.JobDone {
		t.Errorf("a paused VM's forced stop: %+v", j)
	}
	if j, _ := wait(c.StopVM(ctx, "beta", true)); j.State != api.JobDone || j.Kind != api.JobVMStop {
		t.Errorf("job: %+v", j)
	}
	if v, err := c.VM(ctx, "beta"); err != nil || v.State != api.VMStopped {
		t.Errorf("after stop: %+v %v", v, err)
	}
	if e := lastEvent(t, h.p, audit.VMStop); e.Detail["force"] != true || e.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", e)
	}
	if _, err := c.StopVM(ctx, "nope", false); code(err) != api.CodeNotFound {
		t.Errorf("nope: %v", err)
	}
}

// Start and resume run the Manager's: a pool VM isn't started, and a VM
// that isn't paused isn't resumed.
func TestStartAndResumeVM(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "pool-1", Template: "base", Created: created, Pool: true})
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	h.writeVM(t, vm.State{Name: "keys", Template: "base", Created: created, SSH: template.SSH{Agent: template.AgentRestricted}})
	c := h.client(t)
	wait := waiter(t, c)
	ctx := context.Background()
	if _, err := c.StartVM(ctx, "keys"); code(err) != api.CodeConflict {
		t.Errorf("start with an SSH agent: %v", err)
	}
	if j, _ := wait(c.StartVM(ctx, "pool-1")); j.State != api.JobFailed || j.Kind != api.JobVMStart ||
		!strings.Contains(j.Error, "pool-1 is a warm pool VM") {
		t.Errorf("start: %+v", j)
	}
	if j, _ := wait(c.ResumeVM(ctx, "alpha")); j.State != api.JobFailed || j.Kind != api.JobVMResume ||
		j.Error != "alpha was not paused by a canary hit" {
		t.Errorf("resume: %+v", j)
	}
	for _, path := range []string{api.PathVMStart("alpha"), api.PathVMResume("alpha")} {
		if st, body := h.post(t, path, `{"force":true}`); st != http.StatusBadRequest {
			t.Errorf("%s with a body: %d %s", path, st, body)
		}
	}
	for _, err := range []error{second(c.StartVM(ctx, "nope")), second(c.ResumeVM(ctx, "nope"))} {
		if code(err) != api.CodeNotFound {
			t.Errorf("nope: %v", err)
		}
	}
}

// noBases makes the home's bases directory a file: a create that gets as
// far as building a base fails there, without downloading an image.
func noBases(t *testing.T, h *vmHost) {
	t.Helper()
	if err := os.RemoveAll(h.p.Bases()); err != nil {
		t.Fatal(err)
	}
	write(t, h.p.Bases(), "not a directory")
}

// CreateVM passes each option to Create: these fail before any image is
// needed, each on the option it sets.
func TestCreateVMOptions(t *testing.T) {
	h := home(t, nil)
	noBases(t, h)
	c := h.client(t)
	wait := waiter(t, c)
	ctx := context.Background()
	for _, tc := range []struct {
		req  api.CreateVMRequest
		want string
	}{
		{api.CreateVMRequest{Name: "c1", Template: "open"}, "-t open has network egress open"},
		{api.CreateVMRequest{Name: "c2", Template: "open", AllowOpen: true, Repos: []string{"nope:o/r"}}, `repo "nope:o/r": no forge for host nope`},
		{api.CreateVMRequest{Name: "c3", CPUs: 999}, "invalid CPU count 999"},
		{api.CreateVMRequest{Name: "c4", Memory: "lots"}, `"lots"`},
		{api.CreateVMRequest{Name: "c5", Disk: "many"}, `"many"`},
		{api.CreateVMRequest{Name: "c6", ClaudeAuth: "bogus"}, "agents.claude.auth must be oauth-token or api-key"},
		{api.CreateVMRequest{Name: "c7", Template: "ssh-host"}, "sets ssh.agent: host, which loads or forwards the host's SSH keys"},
	} {
		j, _ := wait(c.CreateVM(ctx, tc.req))
		if j.State != api.JobFailed || j.Kind != api.JobVMCreate || j.VM != tc.req.Name || !strings.Contains(j.Error, tc.want) {
			t.Errorf("%+v: %+v, want an error with %s", tc.req, j, tc.want)
		}
		if _, err := os.Stat(h.p.VM(tc.req.Name)); err == nil {
			t.Errorf("%s: left a directory", tc.req.Name)
		}
	}
}

// CreateVM refuses at once a bad name, a name in use, a repo with an @ and
// a body it doesn't know.
func TestCreateVMRefused(t *testing.T) {
	h := home(t, nil)
	noBases(t, h)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	c := h.client(t)
	for _, tc := range []struct {
		req  api.CreateVMRequest
		code string
	}{
		{api.CreateVMRequest{Name: "Bad"}, api.CodeBadRequest},
		{api.CreateVMRequest{Name: ""}, api.CodeBadRequest},
		{api.CreateVMRequest{Name: "alpha"}, api.CodeConflict},
		{api.CreateVMRequest{Name: "new", Repos: []string{"gh:o/r", "https://me:" + decoy + "@example.com/o/r"}}, api.CodeBadRequest},
		{api.CreateVMRequest{Name: "new", Repos: []string{"git@github.com:o/r"}}, api.CodeBadRequest},
		{api.CreateVMRequest{Name: "new", Repos: []string{"https://example.com/o/r?access_token=" + decoy}}, api.CodeBadRequest},
		{api.CreateVMRequest{Name: "new", Repos: []string{"https://example.com/o/r#" + decoy}}, api.CodeBadRequest},
		{api.CreateVMRequest{Name: "new", Repos: []string{"https://me%3A" + decoy + "%40example.com/o/r"}}, api.CodeBadRequest},
	} {
		_, err := c.CreateVM(context.Background(), tc.req)
		if code(err) != tc.code || strings.Contains(err.Error(), decoy) {
			t.Errorf("%+v: %v, want %s", tc.req, err, tc.code)
		}
	}
	for body, st := range map[string]int{
		`{"name":"new","ssh_agent":"host"}`:       http.StatusBadRequest, // a field the API doesn't take
		`{"name":"new"} {"name":"other"}`:         http.StatusBadRequest,
		`{"name":"new"`:                           http.StatusBadRequest,
		strings.Repeat(" ", api.MaxRequestBody+1): http.StatusRequestEntityTooLarge,
	} {
		if got, b := h.post(t, api.PathVMs, body); got != st {
			t.Errorf("%.40q: %d %s, want %d", body, got, b, st)
		}
	}
	if js, err := c.Jobs(context.Background()); err != nil || len(js) != 0 {
		t.Errorf("jobs: %+v %v", js, err)
	}
	if _, err := os.Stat(h.p.VM("new")); err == nil {
		t.Errorf("created")
	}
}

func TestJobRoutes(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	c := h.client(t)
	wait := waiter(t, c)
	ctx := context.Background()
	if st, body := h.raw(t, api.PathJobs); st != http.StatusOK || body != "[]\n" {
		t.Errorf("no jobs: %d %q", st, body)
	}
	j, lines := wait(c.DestroyVM(ctx, "alpha", api.DestroyVMRequest{Confirm: true}))
	got, err := c.Job(ctx, j.ID, 1)
	if err != nil || got.From != 1 || len(got.Lines) != len(lines)-1 || got.Written != len(lines) {
		t.Errorf("after 1: %+v %v (%q)", got, err, lines)
	}
	for _, id := range []string{"nope", ".", "..", ""} {
		if _, err := c.Job(ctx, id, 0); code(err) != api.CodeNotFound {
			t.Errorf("%q: %v", id, err)
		}
	}
	for _, after := range []string{"-1", "x"} {
		if st, _ := h.raw(t, api.PathJob(j.ID)+"?after="+after); st != http.StatusBadRequest {
			t.Errorf("after=%s: %d", after, st)
		}
	}
}

// A job past the most that may run is refused, with rate_limited.
func TestJobsBusy(t *testing.T) {
	defer func(n int) { maxRunning = n }(maxRunning)
	maxRunning = 0
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created})
	if _, err := h.client(t).StartVM(context.Background(), "alpha"); code(err) != api.CodeRateLimited {
		t.Errorf("got %v, want rate_limited", err)
	}
}

// WaitJob reads each line once, across polls, and returns once the job ends.
func TestWaitJob(t *testing.T) {
	h := home(t, nil)
	release := make(chan struct{})
	i, err := h.srv.jobs.start("k", "a", "me", nil, func(_ context.Context, out io.Writer) error {
		for _, l := range []string{"1", "2", "3"} {
			io.WriteString(out, l+"\n")
			<-release
		}
		return errors.New("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range 3 {
			time.Sleep(700 * time.Millisecond) // past a poll
			release <- struct{}{}
		}
	}()
	var lines []string
	j, err := h.client(t).WaitJob(context.Background(), i.ID, func(l string) { lines = append(lines, l) })
	if err != nil || j.State != api.JobFailed || j.Error != "boom" || !slices.Equal(lines, []string{"1", "2", "3"}) {
		t.Errorf("%+v %v %q", j, err, lines)
	}
}
