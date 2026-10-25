//go:build integration

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/canary"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
	"github.com/blackshieldpt/curral/internal/vm"
	"github.com/blackshieldpt/curral/internal/vmtest"

	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/notify"
	"github.com/blackshieldpt/curral/internal/task"
)

// TestTasks runs headless tasks end to end against Gitea with the cmd agent:
// queueing on the VM budget, branch + leftover commit, PR from the host,
// transcripts, notifications, cancel. Then a guest agent hook notifies.
func TestTasks(t *testing.T) {
	g := vmtest.StartGitea(t, false)
	r := integrationRunner(t)
	m := r.M
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	var mu sync.Mutex
	var events []notify.Event
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e notify.Event
		json.NewDecoder(r.Body).Decode(&e)
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}))
	defer hook.Close()
	eventsOf := func(kind string) []notify.Event {
		mu.Lock()
		defer mu.Unlock()
		var out []notify.Event
		for _, e := range events {
			if e.Kind == kind {
				out = append(out, e)
			}
		}
		return out
	}
	// The supervisors are separate processes: they read config.yaml.
	cfgYAML := fmt.Sprintf("tasks: { max_vms: 1 }\nnotify: { sinks: [{ webhook: { url: %q } }] }\n", hook.URL)
	if err := os.WriteFile(m.P.ConfigFile(), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(m.P.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	m.Cfg = cfg

	var tok struct {
		SHA1 string `json:"sha1"`
	}
	g.API("POST", "/users/bot/tokens", map[string]any{"name": "curral-admin", "scopes": []string{"write:repository", "read:user"}}, &tok)
	t.Setenv("GITEA_ADMIN", tok.SHA1)
	keys, err := forge.Scan(ctx, g.IP, g.SSHPort)
	if err != nil {
		t.Fatal(err)
	}
	gd := forge.Forge{Name: "gd", Type: forge.Gitea, Host: g.IP, HTTPURL: g.HTTP, SSHPort: g.SSHPort,
		HostKeys: []string{forge.FormatHostKey(keys[0])}, Auth: forge.Auth{Mode: forge.ModeDeployKey, Token: "secret:GITEA_ADMIN"}}
	if err := forge.Update(m.P.Forges(), func(r *forge.Registry) error { r.Forges = []forge.Forge{gd}; return nil }); err != nil {
		t.Fatal(err)
	}
	vmtest.Template(t, m.P.Templates(), "tk", "", "network: { allow_private: true }\n")
	host := t.TempDir()
	authURL := strings.Replace(g.HTTP, "http://", "http://bot:"+vmtest.GiteaPassword+"@", 1)
	if out, err := exec.Command("git", "clone", "-q", authURL+"/bot/app.git", host).CombinedOutput(); err != nil {
		t.Fatalf("host clone: %v\n%s", err, out)
	}

	// The agent: writes the prompt into the repo and commits it, leaves one file
	// uncommitted, a transcript and a symlink that must not be copied.
	// A pre-commit hook that prints, as husky or lint-staged do: commit counting must cope.
	agent := `set -e; cat > TASK.md; git add TASK.md; git -c user.email=t@t -c user.name=t commit -qm "task file"
printf '#!/bin/sh\necho hook says hi\necho hook warns >&2\n' > .git/hooks/pre-commit; chmod +x .git/hooks/pre-commit
echo left > left.txt
mkdir -p ~/.claude/projects/p ~/.pi/agent/sessions; echo '{"x":1}' > ~/.claude/projects/p/s.jsonl; ln -sf /etc/hostname ~/.pi/agent/sessions/link
curl -s --noproxy '*' -m 5 -X POST --data '{"agent":"cmd"}' http://10.0.2.101/notify || true
echo agent-finished`
	s1 := task.Spec{Template: "tk", Repo: "gd:bot/app", Agent: "cmd", Cmd: agent, Prompt: "write the task file", Into: host, Timeout: 10 * time.Minute}
	s2 := task.Spec{Template: "tk", Agent: "cmd", Cmd: "sleep 600", Prompt: "wait", Timeout: 20 * time.Minute}
	for _, s := range []*task.Spec{&s1, &s2} {
		if err := s.Normalize(host, host); err != nil {
			t.Fatal(err)
		}
	}
	t1, err := r.Submit(s1)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := r.Submit(s2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ts, _ := m.Tasks().List()
		for _, tk := range ts {
			_ = r.Cancel(tk.ID)
		}
		time.Sleep(5 * time.Second)
		for _, tk := range ts {
			if _, err := os.Stat(m.Dir(tk.VM)); err == nil {
				_ = m.Destroy(tk.VM)
			}
		}
	})
	waitState := func(id string, until func(*task.Task) bool) *task.Task {
		t.Helper()
		deadline := time.Now().Add(15 * time.Minute)
		for {
			tk, err := m.Tasks().Load(id)
			if err == nil && until(tk) {
				return tk
			}
			if time.Now().After(deadline) {
				b, _ := os.ReadFile(m.Tasks().Path(id, "supervisor.log"))
				t.Fatalf("task %s stuck at %+v\n%s", id, tk, b)
			}
			time.Sleep(2 * time.Second)
		}
	}
	// max_vms: 1 — while the first runs, the second waits.
	waitState(t1.ID, func(tk *task.Task) bool { return tk.State == task.Running || task.Final(tk.State) })
	if tk, _ := m.Tasks().Load(t2.ID); tk.State != task.Queued {
		t.Errorf("second task should be queued, is %s", tk.State)
	}
	r1 := waitState(t1.ID, func(tk *task.Task) bool { return task.Final(tk.State) })
	if r1.State != task.Done || r1.Commits != 2 || !strings.Contains(r1.Output, "/bot/app/pulls/") || *r1.Exit != 0 {
		b, _ := os.ReadFile(m.Tasks().Path(t1.ID, "supervisor.log"))
		t.Fatalf("task 1: %+v\n%s", r1, b)
	}
	if b, _ := os.ReadFile(m.Tasks().Path(t1.ID, "result.md")); !strings.Contains(string(b), "agent-finished") {
		t.Errorf("result.md: %q", b)
	}
	var prs []map[string]any
	g.API("GET", "/repos/bot/app/pulls?state=open", nil, &prs)
	if len(prs) != 1 || !strings.Contains(fmt.Sprint(prs[0]["body"]), "write the task file") {
		t.Errorf("PRs: %v", prs)
	}
	if _, err := os.Stat(m.Dir(t1.VM)); !os.IsNotExist(err) {
		t.Error("task VM not destroyed")
	}
	var dk []map[string]any
	g.API("GET", "/repos/bot/app/keys", nil, &dk)
	if len(dk) != 0 {
		t.Errorf("deploy key left: %v", dk)
	}
	dirs, _ := m.TranscriptDirs(t1.VM)
	if len(dirs) != 1 {
		t.Fatalf("transcripts: %v", dirs)
	}
	if b, _ := os.ReadFile(filepath.Join(dirs[0], ".claude/projects/p/s.jsonl")); !bytes.Contains(b, []byte(`"x":1`)) {
		t.Errorf("transcript file: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dirs[0], ".pi/agent/sessions/link")); err == nil {
		t.Error("symlink copied from the guest")
	}
	if d := eventsOf(notify.TaskDone); len(d) != 1 || !strings.Contains(d[0].Message, "/pulls/") {
		t.Errorf("task.done notifications: %+v", d)
	}
	if w := eventsOf(notify.AgentWaiting); len(w) != 0 {
		t.Errorf("a task VM's agent hook notified: %+v", w)
	}

	// The second task starts once the first is done; cancelling it destroys its VM.
	waitState(t2.ID, func(tk *task.Task) bool { return tk.State == task.Running })
	if err := r.Cancel(t2.ID); err != nil {
		t.Fatal(err)
	}
	r2 := waitState(t2.ID, func(tk *task.Task) bool { return task.Final(tk.State) })
	if r2.State != task.Cancelled {
		t.Errorf("task 2: %+v", r2)
	}
	if _, err := os.Stat(m.Dir(t2.VM)); !os.IsNotExist(err) {
		t.Error("cancelled task's VM not destroyed")
	}
	if f := eventsOf(notify.TaskFailed); len(f) != 1 {
		t.Errorf("task.failed notifications: %+v", f)
	}
	// An export that fails (no remote of --into points at the repo) keeps the VM
	// with the unexported commits.
	other := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", other).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	s3 := task.Spec{Template: "tk", Repo: "gd:bot/app", Agent: "cmd", Cmd: "echo change > c.txt", Prompt: "change", Into: other, Timeout: 10 * time.Minute}
	if err := s3.Normalize(other, other); err != nil {
		t.Fatal(err)
	}
	t3, err := r.Submit(s3)
	if err != nil {
		t.Fatal(err)
	}
	r3 := waitState(t3.ID, func(tk *task.Task) bool { return task.Final(tk.State) })
	if r3.State != task.Failed || r3.Commits != 1 || !strings.Contains(r3.Error, "kept "+t3.VM) {
		t.Errorf("task 3: %+v", r3)
	}
	if _, err := os.Stat(m.Dir(t3.VM)); err != nil {
		t.Error("VM with unexported commits destroyed")
	}
	if err := m.Destroy(t3.VM); err != nil {
		t.Fatal(err)
	}

	// Patches are made on the host, binary files included.
	s5 := task.Spec{Template: "tk", Repo: "gd:bot/app", Agent: "cmd", Cmd: `printf 'p\0q' > p.bin && echo change > c.txt`, Prompt: "change", Out: task.OutPatch, Timeout: 10 * time.Minute}
	if err := s5.Normalize(other, other); err != nil {
		t.Fatal(err)
	}
	t5, err := r.Submit(s5)
	if err != nil {
		t.Fatal(err)
	}
	r5 := waitState(t5.ID, func(tk *task.Task) bool { return task.Final(tk.State) })
	if b, _ := os.ReadFile(r5.Output); r5.State != task.Done || !bytes.Contains(b, []byte("Subject: [PATCH] curral task "+t5.ID)) || !bytes.Contains(b, []byte("GIT binary patch")) {
		t.Errorf("task 5: %+v\n%s", r5, b)
	}

	// A task whose supervisor was killed shows as lost; cancelling it destroys its VM.
	s4 := task.Spec{Template: "tk", Agent: "cmd", Cmd: "sleep 600", Prompt: "wait", Timeout: 20 * time.Minute}
	if err := s4.Normalize(host, host); err != nil {
		t.Fatal(err)
	}
	t4, err := r.Submit(s4)
	if err != nil {
		t.Fatal(err)
	}
	r4 := waitState(t4.ID, func(tk *task.Task) bool { return tk.State == task.Running })
	syscall.Kill(r4.PID, syscall.SIGKILL)
	waitState(t4.ID, func(tk *task.Task) bool { return tk.Status() == "lost" })
	if err := r.Cancel(t4.ID); err != nil {
		t.Fatal(err)
	}
	if r4, _ = m.Tasks().Load(t4.ID); r4.State != task.Cancelled {
		t.Errorf("lost task after cancel: %+v", r4)
	}
	if _, err := os.Stat(m.Dir(t4.VM)); !os.IsNotExist(err) {
		t.Error("lost task's VM not destroyed by cancel")
	}

	var alog bytes.Buffer
	m.Audit().Tail(ctx, t1.VM, false, &alog)
	if !strings.Contains(alog.String(), "task.submit") || !strings.Contains(alog.String(), "task.end") {
		t.Errorf("audit:\n%s", alog.String())
	}

	// A normal VM: an agent hook reaches the notify sinks, rate-limited.
	if _, err := m.Create(ctx, "nt1", vm.CreateOpts{Template: "tk"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy("nt1") })
	if _, err := m.Start(ctx, "nt1"); err != nil {
		t.Fatal(err)
	}
	ng, _, err := m.DialGuest(ctx, "nt1")
	if err != nil {
		t.Fatal(err)
	}
	defer ng.Close()
	out, err := sshx.Run(ctx, ng.Client, `for i in 1 2 3 4 5 6 7; do curl -s -o /dev/null -w '%{http_code} ' --noproxy '*' -m 5 -X POST --data '{"agent":"tester","message":"needs input"}' http://10.0.2.101/notify; done`, nil)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.Fields(string(out.Raw())); len(got) != 7 || got[0] != "202" || got[6] != "429" {
		t.Errorf("hook responses: %q", out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(eventsOf(notify.AgentWaiting)) < 5 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if w := eventsOf(notify.AgentWaiting); len(w) != 5 || w[0].Title != "curral nt1: tester is waiting" || w[0].Message != "needs input" {
		t.Errorf("agent.waiting: %+v", w)
	}
}

func integrationRunner(t *testing.T) *Runner {
	t.Helper()
	p, bin := vmtest.Home(t)
	return &Runner{M: &vm.Manager{P: p, Cfg: config.Default(), Tpl: template.Loader{UserDir: p.Templates()}, Out: vmtest.Writer{T: t}, Bin: bin}}
}

// TestCanaryTasks: a task using a decoy fails at once and its paused VM is
// kept; with canary_action log it runs on, and its VM is still kept.
func TestCanaryTasks(t *testing.T) {
	r := integrationRunner(t)
	m := r.M
	vmtest.Template(t, m.P.Templates(), "cn", "", "canaries: [aws, env, git]\ncache: [go]\n")
	// A task using a decoy fails at once, and its paused VM is kept.
	spec := task.Spec{Template: "cn", Agent: "cmd", Cmd: `. ~/.env.production; curl -s -m 120 "$INTERNAL_API_URL/x"; sleep 600`, Prompt: "x", Timeout: 20 * time.Minute}
	dir := t.TempDir()
	if err := spec.Normalize(dir, dir); err != nil {
		t.Fatal(err)
	}
	tk, err := r.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy(tk.VM) })
	until := time.Now().Add(10 * time.Minute)
	for tk.State != task.Failed && time.Now().Before(until) {
		time.Sleep(2 * time.Second)
		if tk, err = m.Tasks().Load(tk.ID); err != nil {
			t.Fatal(err)
		}
	}
	if tk.State != task.Failed || !strings.Contains(tk.Error, "a canary hit paused") || !m.Paused(tk.VM) {
		b, _ := os.ReadFile(m.Tasks().Path(tk.ID, "supervisor.log"))
		t.Fatalf("task %+v, paused %v\n%s", tk, m.Paused(tk.VM), b)
	}

	// A kept VM holds its share of the task budget until destroyed.
	if err := m.Destroy(tk.VM); err != nil {
		t.Fatal(err)
	}

	// With canary_action log the task runs on, and its VM is still kept.
	if err := os.WriteFile(m.P.ConfigFile(), []byte("canary_action: log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.Cmd = `. ~/.env.production; curl -s -m 30 "$INTERNAL_API_URL/x"; echo finished; echo "token $INTERNAL_API_TOKEN"`
	tk, err = r.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy(tk.VM) })
	until = time.Now().Add(10 * time.Minute)
	for !task.Final(tk.State) && time.Now().Before(until) {
		time.Sleep(2 * time.Second)
		if tk, err = m.Tasks().Load(tk.ID); err != nil {
			t.Fatal(err)
		}
	}
	if tk.State != task.Done || !strings.Contains(tk.Error, "decoy credentials were used: kept") || m.Paused(tk.VM) {
		b, _ := os.ReadFile(m.Tasks().Path(tk.ID, "supervisor.log"))
		t.Fatalf("task %+v, paused %v\n%s", tk, m.Paused(tk.VM), b)
	}
	if _, err := os.Stat(m.Dir(tk.VM)); err != nil {
		t.Fatalf("task VM not kept: %v", err)
	}
	// Its result, beyond the first line too, is withheld.
	cs, _ := canary.Load(m.Dir(tk.VM))
	if b, _ := os.ReadFile(m.Tasks().Path(tk.ID, "result.md")); !strings.Contains(string(b), "(withheld") || strings.Contains(string(b), cs[1].Values[2]) {
		t.Errorf("task result: %q", b)
	}
}
