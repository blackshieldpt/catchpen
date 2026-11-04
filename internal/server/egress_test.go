package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// egressHost is a home with alpha (allowlist), beta (open) and pool-1 (a
// warm pool VM with allowlist egress), all stopped.
func egressHost(t *testing.T) *vmHost {
	t.Helper()
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created, Egress: "allowlist"})
	h.writeVM(t, vm.State{Name: "beta", Template: "base", Created: created, Egress: "open"})
	h.writeVM(t, vm.State{Name: "pool-1", Template: "base", Created: created, Egress: "allowlist", Pool: true})
	return h
}

// events are the audit events named event, as "vm detail" lines.
func events(t *testing.T, h *vmHost, event string) []string {
	t.Helper()
	var out []string
	err := audit.Log{Path: h.p.Audit()}.TailEvents(context.Background(), "", false, func(e audit.Event) error {
		if e.Event == event {
			out = append(out, fmt.Sprintf("%s %v", e.VM, e.Detail))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The egress read: settings and lists in allowlist mode, the mode alone
// otherwise, lists never null.
func TestEgress(t *testing.T) {
	h := egressHost(t)
	dir := h.p.VM("alpha")
	write(t, filepath.Join(dir, "egress.json"), `{"on_deny":"prompt","prompt_timeout":"45s","allow_private":true}`)
	write(t, filepath.Join(dir, "allowlist"), "# comment\npypi.org\n*.example.com:8443\n")
	write(t, filepath.Join(dir, "denylist"), "evil.example.com\n")
	c := h.client(t)
	ctx := context.Background()
	e, err := c.Egress(ctx, "alpha")
	want := api.EgressInfo{Mode: "allowlist", OnDeny: "prompt", PromptTimeout: "45s", AllowPrivate: true,
		Allow: []string{"pypi.org", "*.example.com:8443"}, Deny: []string{"evil.example.com"}}
	if err != nil || !reflect.DeepEqual(*e, want) {
		t.Errorf("alpha: %+v %v", e, err)
	}
	if _, body := h.raw(t, api.PathEgress("alpha")); body != `{"mode":"allowlist","on_deny":"prompt","prompt_timeout":"45s",`+
		`"allow_private":true,"allow":["pypi.org","*.example.com:8443"],"deny":["evil.example.com"]}`+"\n" {
		t.Errorf("alpha on the wire: %s", body)
	}
	if st, body := h.raw(t, api.PathEgress("beta")); st != http.StatusOK || body != `{"mode":"open","allow_private":false,"allow":[],"deny":[]}`+"\n" {
		t.Errorf("beta: %d %s", st, body)
	}
	if _, err := c.Egress(ctx, "nope"); code(err) != api.CodeNotFound {
		t.Errorf("nope: %v", err)
	}
	write(t, filepath.Join(dir, "egress.json"), "{")
	if _, err := c.Egress(ctx, "alpha"); code(err) != api.CodeInternal || !strings.Contains(h.log.String(), "egress.json") {
		t.Errorf("a broken egress.json: %v, log %q", err, h.log.String())
	}
}

// Each verb edits its list and is audited with the client; a bad entry
// anywhere changes nothing.
func TestEditEgress(t *testing.T) {
	h := egressHost(t)
	c := h.client(t)
	ctx := context.Background()
	steps := []struct {
		verb    string
		entries []string
		allow   []string
		deny    []string
	}{
		{api.EgressAllow, []string{"pypi.org", "*.Example.com:8443"}, []string{"pypi.org", "*.example.com:8443"}, []string{}},
		{api.EgressUnallow, []string{"pypi.org"}, []string{"*.example.com:8443"}, []string{}},
		{api.EgressDeny, []string{"evil.example.com", "evil2.example.com"}, []string{"*.example.com:8443"}, []string{"evil.example.com", "evil2.example.com"}},
		{api.EgressUndeny, []string{"evil2.example.com"}, []string{"*.example.com:8443"}, []string{"evil.example.com"}},
	}
	for _, s := range steps {
		e, err := c.EditEgress(ctx, "alpha", s.verb, s.entries)
		if err != nil || !reflect.DeepEqual(e.Allow, s.allow) || !reflect.DeepEqual(e.Deny, s.deny) || e.Mode != "allowlist" {
			t.Fatalf("%s %v: %+v %v", s.verb, s.entries, e, err)
		}
	}
	wantAllow := []string{"alpha map[add:true client:@local entry:pypi.org]", "alpha map[add:true client:@local entry:*.Example.com:8443]",
		"alpha map[add:false client:@local entry:pypi.org]"}
	wantDeny := []string{"alpha map[add:true client:@local entry:evil.example.com]", "alpha map[add:true client:@local entry:evil2.example.com]",
		"alpha map[add:false client:@local entry:evil2.example.com]"}
	if got := events(t, h, audit.EgressAllow); !reflect.DeepEqual(got, wantAllow) {
		t.Errorf("egress.allow: %q", got)
	}
	if got := events(t, h, audit.EgressDeny); !reflect.DeepEqual(got, wantDeny) {
		t.Errorf("egress.deny: %q", got)
	}

	var e *api.Error
	if _, err := c.EditEgress(ctx, "alpha", api.EgressAllow, []string{"a.example.org", "bad host", "b.example.org"}); !errors.As(err, &e) ||
		e.Code != api.CodeBadRequest || e.Message != `invalid host in entry "bad host"` {
		t.Errorf("a bad entry: %v", err)
	}
	many := make([]string, api.MaxEgressEntries+1)
	for i := range many {
		many[i] = "h" + strconv.Itoa(i) + ".example.org"
	}
	if _, err := c.EditEgress(ctx, "alpha", api.EgressAllow, many); !errors.As(err, &e) || e.Code != api.CodeBadRequest ||
		e.Message != "at most 64 entries a call" {
		t.Errorf("too many: %v", err)
	}
	if _, err := c.EditEgress(ctx, "alpha", api.EgressAllow, many[:api.MaxEgressEntries]); err != nil {
		t.Errorf("as many as allowed: %v", err)
	}
	if _, err := c.EditEgress(ctx, "alpha", api.EgressUnallow, many[:api.MaxEgressEntries]); err != nil {
		t.Errorf("as many as allowed, removed: %v", err)
	}
	if _, err := c.EditEgress(ctx, "alpha", api.EgressDeny, nil); code(err) != api.CodeBadRequest {
		t.Errorf("no entries: %v", err)
	}
	if e, err := c.Egress(ctx, "alpha"); err != nil || !reflect.DeepEqual(e.Allow, []string{"*.example.com:8443"}) {
		t.Errorf("after the refusals: %+v %v", e, err)
	}
	if n := len(events(t, h, audit.EgressAllow)); n != 3+2*api.MaxEgressEntries {
		t.Errorf("%d egress.allow events", n)
	}

	for name, msg := range map[string]string{"pool-1": "the VM is a warm pool VM: claiming it rewrites its egress",
		"beta": `the VM uses egress mode "open"; lists and settings only apply to allowlist mode`} {
		if _, err := c.EditEgress(ctx, name, api.EgressDeny, []string{"x.example.org"}); !errors.As(err, &e) || e.Code != api.CodeConflict || e.Message != msg {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.EditEgress(ctx, "nope", api.EgressDeny, []string{"x.example.org"}); code(err) != api.CodeNotFound {
		t.Errorf("nope: %v", err)
	}
	if st, body := h.post(t, api.PathEgressEdit("alpha", api.EgressDeny), `{"entries":["x.example.org"],"add":true}`); st != http.StatusBadRequest {
		t.Errorf("an unknown field: %d %s", st, body)
	}
	if st, _ := h.post(t, api.PathEgressEdit("alpha", "allowall"), `{"entries":["x.example.org"]}`); st != http.StatusNotFound {
		t.Errorf("an unknown verb: %d", st)
	}
	for _, f := range []string{"allowlist", "denylist"} {
		if b, _ := os.ReadFile(filepath.Join(h.p.VM("pool-1"), f)); len(b) != 0 {
			t.Errorf("pool-1's %s: %q", f, b)
		}
	}
	if st, body := h.post(t, api.PathEgressEdit("alpha", api.EgressDeny), `{"entries":["x.example.org"]}`); st != http.StatusOK ||
		!strings.Contains(body, `"deny":["evil.example.com","x.example.org"]`) {
		t.Errorf("on the wire: %d %s", st, body)
	}
}

// The settings change as sent, audited with the client; a value refused
// changes nothing.
func TestSetEgressConfig(t *testing.T) {
	h := egressHost(t)
	c := h.client(t)
	ctx := context.Background()
	str := func(s string) *string { return &s }
	yes := true
	e, err := c.SetEgressConfig(ctx, "alpha", api.EgressConfigRequest{OnDeny: str("prompt"), PromptTimeout: str("45s"), AllowPrivate: &yes})
	if err != nil || e.OnDeny != "prompt" || e.PromptTimeout != "45s" || !e.AllowPrivate {
		t.Fatalf("set: %+v %v", e, err)
	}
	if got := events(t, h, audit.EgressConfig); len(got) != 1 || got[0] != "alpha map[allow_private:true allow_private_was:false client:@local "+
		"on_deny:prompt on_deny_was:log prompt_timeout:45s prompt_timeout_was:30s]" {
		t.Errorf("audit: %q", got)
	}
	if e, err := c.SetEgressConfig(ctx, "alpha", api.EgressConfigRequest{}); err != nil || e.OnDeny != "prompt" {
		t.Errorf("nothing to set: %+v %v", e, err)
	}
	var ae *api.Error
	for req, msg := range map[*api.EgressConfigRequest]string{
		{OnDeny: str("ask")}:         `on_deny must be "log" or "prompt", got "ask"`,
		{PromptTimeout: str("soon")}: "prompt_timeout is a positive duration, such as 30s",
		{PromptTimeout: str("-1s")}:  "prompt timeout must be positive, got -1s",
		{PromptTimeout: str("0s")}:   "prompt timeout must be positive, got 0s",
	} {
		if _, err := c.SetEgressConfig(ctx, "alpha", *req); !errors.As(err, &ae) || ae.Code != api.CodeBadRequest || ae.Message != msg {
			t.Errorf("%+v: %v", *req, err)
		}
	}
	if e, err := c.Egress(ctx, "alpha"); err != nil || e.OnDeny != "prompt" || e.PromptTimeout != "45s" {
		t.Errorf("after the refusals: %+v %v", e, err)
	}
	if n := len(events(t, h, audit.EgressConfig)); n != 1 {
		t.Errorf("%d egress.config events", n)
	}
	for _, name := range []string{"pool-1", "beta"} {
		if _, err := c.SetEgressConfig(ctx, name, api.EgressConfigRequest{OnDeny: str("prompt")}); code(err) != api.CodeConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.p.VM("pool-1"), "egress.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pool-1's egress.json: %v", err)
	}
	if st, body := h.post(t, api.PathEgressConfig("alpha"), `{"on_deny":"log","allow_all":true}`); st != http.StatusBadRequest {
		t.Errorf("an unknown field: %d %s", st, body)
	}
	if st, body := h.post(t, api.PathEgressConfig("alpha"), `{"on_deny":"log","prompt_timeout":"1m","allow_private":false}`); st != http.StatusOK ||
		!strings.Contains(body, `"on_deny":"log","prompt_timeout":"1m0s","allow_private":false`) {
		t.Errorf("on the wire: %d %s", st, body)
	}
}

// fakeForward is a live process forwardAlive takes for a forward session of
// the VM name.
func fakeForward(t *testing.T, name string) int {
	t.Helper()
	c := exec.Command("bash", "-c", "sleep 60; true", "ssh", "catchpen-"+name) // "; true": bash would exec a lone command
	c.Args[0] = "ssh"
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	return c.Process.Pid
}

// Forwards are listed and stopped, the stop audited with the client.
func TestForwards(t *testing.T) {
	h := egressHost(t)
	pid := fakeForward(t, "alpha")
	write(t, filepath.Join(h.p.VM("alpha"), "forwards.json"),
		fmt.Sprintf(`[{"pid":%d,"specs":["-L 127.0.0.1:3000:localhost:3000"],"started":"2026-10-01T12:00:00Z"},{"pid":1,"specs":["-D 127.0.0.1:1080"]}]`, pid))
	c := h.client(t)
	ctx := context.Background()
	fs, err := c.Forwards(ctx, "alpha")
	if want := []api.ForwardInfo{{PID: pid, Specs: []string{"-L 127.0.0.1:3000:localhost:3000"}, Started: created}}; err != nil || !reflect.DeepEqual(fs, want) {
		t.Errorf("forwards: %+v %v", fs, err)
	}
	if _, body := h.raw(t, api.PathForwards("alpha")); body != fmt.Sprintf(`[{"pid":%d,"specs":["-L 127.0.0.1:3000:localhost:3000"],`+
		`"started":"2026-10-01T12:00:00Z"}]`+"\n", pid) {
		t.Errorf("on the wire: %s", body)
	}
	if st, body := h.raw(t, api.PathForwards("beta")); st != http.StatusOK || body != "[]\n" {
		t.Errorf("beta: %d %s", st, body)
	}
	if fs, err := c.StopForwards(ctx, "alpha"); err != nil || len(fs) != 0 || fs == nil {
		t.Errorf("stop: %#v %v", fs, err)
	}
	if got := events(t, h, audit.ForwardStop); len(got) != 1 {
		t.Errorf("audit: %q", got)
	}
	if e := lastEvent(t, h.p, audit.ForwardStop); e.VM != "alpha" || e.Detail["client"] != api.ClientLocal || e.Detail["pid"] != float64(pid) {
		t.Errorf("audit: %+v", e)
	}
	if fs, err := c.Forwards(ctx, "alpha"); err != nil || len(fs) != 0 {
		t.Errorf("after stop: %+v %v", fs, err)
	}
	if st, body := h.post(t, api.PathForwardsStop("alpha"), `{"all":true}`); st != http.StatusBadRequest {
		t.Errorf("stop with a body: %d %s", st, body)
	}
	for _, err := range []error{second(c.Forwards(ctx, "nope")), second(c.StopForwards(ctx, "nope"))} {
		if code(err) != api.CodeNotFound {
			t.Errorf("nope: %v", err)
		}
	}
}

// A forward is checked before its job; the job opens the session with the
// forwards sent.
func TestOpenForward(t *testing.T) {
	h := egressHost(t)
	m := &vm.Manager{P: h.p}
	for _, name := range []string{"beta", "gamma"} {
		vmtest.FakeQEMU(t, h.p.VM(name), name, 512, func() bool { _, ok := m.Running(name); return ok })
		write(t, filepath.Join(h.p.VM(name), "runtime.json"), `{"pid":1,"ssh_port":2222}`)
	}
	h.writeVM(t, vm.State{Name: "beta", Template: "base", Created: created, Egress: "open"})
	write(t, filepath.Join(h.p.VM("beta"), "paused.json"), "{}")
	h.writeVM(t, vm.State{Name: "gamma", Template: "base", Created: created, Egress: "allowlist"})
	h.writeVM(t, vm.State{Name: "keys", Template: "base", Created: created, SSH: template.SSH{Agent: template.AgentRestricted}})
	c := h.client(t)
	ctx := context.Background()
	ok := api.ForwardRequest{Local: []string{"3000"}}
	var e *api.Error
	for _, tc := range []struct {
		vm   string
		req  api.ForwardRequest
		code string
		msg  string
	}{
		{"gamma", api.ForwardRequest{}, api.CodeBadRequest, "nothing to forward: give local, remote or dynamic"},
		{"gamma", api.ForwardRequest{Dynamic: make([]string, api.MaxForwards+1)}, api.CodeBadRequest, "at most 16 forwards a call"},
		{"gamma", api.ForwardRequest{Local: []string{"0.0.0.0:80:x:80"}}, api.CodeBadRequest, `-L "0.0.0.0:80:x:80": use PORT, HOSTPORT:GUESTPORT or HOSTPORT:HOST:PORT`},
		{"gamma", api.ForwardRequest{Remote: []string{"1:-oProxyCommand=x:2"}}, api.CodeBadRequest, `invalid host "-oProxyCommand=x"`},
		{"gamma", api.ForwardRequest{Local: []string{"3000"}, Dynamic: []string{"70000"}}, api.CodeBadRequest, `invalid port "70000"`},
		{"beta", ok, api.CodeVMPaused, ""},
		{"keys", ok, api.CodeConflict, "the VM has ssh.agent: restricted, and this call can load or forward the host's SSH keys: use the CLI"},
		{"pool-1", ok, api.CodeConflict, "the VM is a warm pool VM: it has no SSH access until claimed"},
		{"alpha", ok, api.CodeConflict, "the VM isn't running: start it first"},
		{"nope", ok, api.CodeNotFound, ""},
	} {
		_, err := c.OpenForward(ctx, tc.vm, tc.req)
		if !errors.As(err, &e) || e.Code != tc.code || tc.msg != "" && e.Message != tc.msg {
			t.Errorf("%s %+v: %v", tc.vm, tc.req, err)
		}
	}
	for _, body := range []string{`{"local":["3000"]}`, `{"remote":["5432"]}`, `{"dynamic":["1080"]}`} {
		if st, got := h.post(t, api.PathForwards("alpha"), body); st != http.StatusConflict || !strings.Contains(got, "the VM isn't running") {
			t.Errorf("%s on the wire: %d %s", body, st, got)
		}
	}
	if js, err := c.Jobs(ctx); err != nil || len(js) != 0 {
		t.Errorf("jobs after the refusals: %+v %v", js, err)
	}

	// The home has no ssh_config: the session's ssh exits at once.
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("needs ssh")
	}
	j, _ := waiter(t, c)(c.OpenForward(ctx, "gamma", api.ForwardRequest{Local: []string{"3000"}, Remote: []string{"5432"}, Dynamic: []string{"1080"}}))
	if j.State != api.JobFailed || j.Kind != api.JobForwardOpen || j.VM != "gamma" || !strings.HasPrefix(j.Error, "ssh forward exited") {
		t.Errorf("job: %+v", j)
	}
	b, _ := os.ReadFile(filepath.Join(h.p.VM("gamma"), "forward.log"))
	if !strings.Contains(string(b), " ssh -L 127.0.0.1:3000:localhost:3000 -R 127.0.0.1:5432:localhost:5432 -D 127.0.0.1:1080\n") {
		t.Errorf("forward.log: %q", b)
	}
}

// Over TCP, the audit events name the enrolled client.
func TestEgressOverTCP(t *testing.T) {
	h := egressHost(t)
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	c := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	ctx := context.Background()
	if _, err := c.EditEgress(ctx, "alpha", api.EgressDeny, []string{"evil.example.com"}); err != nil {
		t.Fatal(err)
	}
	str := "prompt"
	if _, err := c.SetEgressConfig(ctx, "alpha", api.EgressConfigRequest{OnDeny: &str}); err != nil {
		t.Fatal(err)
	}
	pid := fakeForward(t, "alpha")
	write(t, filepath.Join(h.p.VM("alpha"), "forwards.json"), fmt.Sprintf(`[{"pid":%d,"specs":["-D 127.0.0.1:1080"]}]`, pid))
	if _, err := c.StopForwards(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{audit.EgressDeny, audit.EgressConfig, audit.ForwardStop} {
		if e := lastEvent(t, h.p, ev); e.VM != "alpha" || e.Detail["client"] != "cockpit" {
			t.Errorf("%s: %+v", ev, e)
		}
	}
}
