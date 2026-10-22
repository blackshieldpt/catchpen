package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/curral/internal/agents"
	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/fsutil"
	"github.com/blackshieldpt/curral/internal/notify"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/task"
	"github.com/blackshieldpt/curral/internal/template"
)

const maxTaskOutput = 512 << 20

// Tasks is the task store.
func (m *Manager) Tasks() task.Store { return task.Store{Dir: m.P.Tasks()} }

// SubmitTask records a task and starts its supervisor (curral task-supervise)
// in the background. The spec must be normalized.
func (m *Manager) SubmitTask(spec task.Spec) (*task.Task, error) {
	if spec.Template == "" {
		spec.Template = m.Cfg.Defaults.Template
	}
	r, err := m.Resolve(spec.Template, "")
	if err != nil {
		return nil, err
	}
	if spec.Agent != "cmd" {
		a := map[string]*template.Agent{"claude": r.T.Agents.Claude, "opencode": r.T.Agents.OpenCode, "pi": r.T.Agents.Pi, "omp": r.T.Agents.OMP}[spec.Agent]
		if a == nil {
			return nil, fmt.Errorf("template %s doesn't install %s", r.T.Name, spec.Agent)
		}
	}
	if spec.Into != "" {
		if _, err := hostGitCmd(context.Background(), spec.Into, "rev-parse", "--git-dir"); err != nil {
			return nil, fmt.Errorf("--out %s needs a host repository: %s is not one (use --into)", spec.Out, spec.Into)
		}
	}
	mem, err := template.ParseSize(m.Cfg.Defaults.Resources.Override(r.T.Resources).Memory)
	if err != nil {
		return nil, err
	}
	id := task.NewID()
	t := &task.Task{ID: id, Spec: spec, State: task.Queued, VM: "task-" + id, Created: time.Now().UTC(), MemoryMB: int(mem)}
	st := m.Tasks()
	if err := st.Create(t, spec.Prompt); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(st.Path(id, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(m.Bin, "task-supervise", id)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_, _ = st.Update(id, func(t *task.Task) error {
			t.State, t.Error, t.Ended = task.Failed, "starting the supervisor: "+err.Error(), time.Now().UTC()
			return nil
		})
		return nil, err
	}
	t, err = st.Update(id, func(t *task.Task) error { t.PID = cmd.Process.Pid; return nil })
	_ = cmd.Process.Release()
	if err != nil {
		return nil, err
	}
	m.audit(audit.TaskSubmit, t.VM, map[string]any{"task": id, "agent": spec.Agent, "repo": spec.Repo, "out": spec.Out})
	return t, nil
}

// claimSlot moves a queued task to starting if the budget allows.
func (m *Manager) claimSlot(id string) (bool, error) {
	unlock, err := fsutil.Lock(m.P.Tasks() + "/.schedule.lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	st := m.Tasks()
	t, err := st.Load(id)
	if err != nil {
		return false, err
	}
	vms, err := m.List()
	if err != nil {
		return false, err
	}
	n, mem := 0, int64(0)
	running := map[string]bool{}
	for _, v := range vms {
		if v.Running {
			n++
			mem += int64(v.MemoryMB)
			running[v.Name] = true
		}
	}
	all, err := st.List()
	if err != nil {
		return false, err
	}
	batch := 0
	for _, o := range all {
		if o.ID == id || task.Final(o.State) || o.State == task.Queued || !o.Alive() {
			continue
		}
		if !running[o.VM] { // claimed but its VM isn't up yet
			n++
			mem += int64(o.MemoryMB)
		}
		if t.Spec.Batch != "" && o.Spec.Batch == t.Spec.Batch {
			batch++
		}
	}
	lim := m.Cfg.Tasks
	if n > 0 && (n+1 > lim.MaxVMs || (lim.MaxMemoryMB() > 0 && mem+int64(t.MemoryMB) > lim.MaxMemoryMB())) {
		return false, nil
	}
	if t.Spec.Parallel > 0 && batch >= t.Spec.Parallel {
		return false, nil
	}
	_, err = st.Update(id, func(t *task.Task) error {
		if t.State != task.Queued {
			return fmt.Errorf("task %s is %s", id, t.State)
		}
		t.State, t.Started = task.Starting, time.Now().UTC()
		return nil
	})
	return err == nil, err
}

// RunTask is the supervisor body: wait for budget, create the VM, run the
// agent, commit, export, copy transcripts, destroy.
func (m *Manager) RunTask(ctx context.Context, id string) (err error) {
	st := m.Tasks()
	t, err := st.Update(id, func(t *task.Task) error {
		if t.State != task.Queued {
			return fmt.Errorf("task %s is %s", id, t.State)
		}
		t.PID = os.Getpid()
		return nil
	})
	if err != nil {
		return err
	}
	set := func(fn func(*task.Task)) {
		if nt, err := st.Update(id, func(t *task.Task) error { fn(t); return nil }); err == nil {
			t = nt
		}
	}
	created := false
	unexported := false // commits exist only in the VM
	stopWatch := func() {}
	defer func() {
		defer stopWatch()
		// The guest's words go to the task record and notification sinks.
		result := m.guestText(t.VM, firstLine(readFile(st.Path(id, "result.md"))), "in the task's result")
		if err != nil {
			if g := m.guestText(t.VM, err.Error(), "in the task's error"); g != err.Error() {
				err = errors.New(g)
			}
		}
		state, msg := task.Done, ""
		paused := created && m.Paused(t.VM)
		switch {
		case paused:
			state, msg = task.Failed, fmt.Sprintf("a canary hit paused %s; kept it (curral resume %[1]s, or curral destroy %[1]s)", t.VM)
		case ctx.Err() != nil:
			state, msg = task.Cancelled, "cancelled"
		case err != nil:
			// Agent-reported errors come from the guest.
			state, msg = task.Failed, sshx.Printable(err.Error())
		}
		// Whatever canary_action did, the VM is the evidence.
		hit := created && fileExists(m.file(t.VM, hitFile))
		keep := t.Spec.Keep || (unexported && state == task.Failed) || hit
		if unexported && state == task.Failed && !t.Spec.Keep && !paused {
			msg += fmt.Sprintf("; kept %s because its %d commit(s) weren't exported (curral pull %s --branch %s, then curral destroy %s)", t.VM, t.Commits, t.VM, t.Branch, t.VM)
		}
		if hit && !paused {
			msg = strings.TrimPrefix(msg+fmt.Sprintf("; its decoy credentials were used: kept %s (curral destroy %[1]s)", t.VM), "; ")
		}
		if created && !paused { // a paused guest doesn't answer
			// Copy transcripts and tear down without the signal's context.
			bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			if _, terr := m.CopyTranscripts(bg, t.VM); terr != nil {
				m.logf("warning: %v", terr)
			}
			cancel()
			if !keep {
				if derr := m.destroy(t.VM, false); derr != nil {
					m.logf("warning: destroying %s: %v", t.VM, derr)
				}
			}
		}
		set(func(t *task.Task) { t.State, t.Error, t.Ended = state, msg, time.Now().UTC() })
		m.audit(audit.TaskEnd, t.VM, map[string]any{"task": id, "state": state, "output": t.Output, "commits": t.Commits})
		kind := notify.TaskDone
		text := t.Output
		if state != task.Done {
			kind, text = notify.TaskFailed, msg
		} else if text == "" {
			text = result
		}
		m.notify(notify.Event{Kind: kind, VM: t.VM, Title: fmt.Sprintf("curral task %s %s", id, state), Message: text})
		m.logf("task %s %s %s", id, state, msg)
	}()

	for {
		ok, err := m.claimSlot(id)
		if err != nil {
			return err
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	m.logf("task %s: starting %s", id, t.VM)
	o := CreateOpts{Template: t.Spec.Template, ReadOnly: !t.Spec.RW, Task: id}
	if t.Spec.Repo != "" {
		o.Repos = []string{t.Spec.Repo}
	}
	if _, err := m.Create(ctx, t.VM, o); err != nil {
		return err
	}
	created = true
	ctx, stopWatch = m.cancelOnPause(ctx, t.VM)
	if _, err := m.Start(ctx, t.VM); err != nil {
		return err
	}
	set(func(t *task.Task) { t.State = task.Running })
	s, err := m.Load(t.VM)
	if err != nil {
		return err
	}
	port, err := m.SSHPort(t.VM)
	if err != nil {
		return err
	}
	c, err := m.keys(t.VM).Dial(ctx, port, s.User)
	if err != nil {
		return err
	}
	defer c.Close()
	// Closing a session doesn't end a command a paused guest never answers;
	// closing the connection does.
	defer context.AfterFunc(ctx, func() { c.Close() })()
	run := func(cmd string) (string, error) {
		out, err := sshx.Run(ctx, c, cmd, nil)
		return strings.TrimSpace(string(out)), err
	}
	prompt, err := os.ReadFile(st.Path(id, "prompt.md"))
	if err != nil {
		return err
	}
	if err := agents.WriteFile(ctx, sshRemote{c}, task.GuestPrompt, prompt); err != nil {
		return err
	}
	wd := WorkDir(s)
	cd := ""
	if wd != "" {
		cd = "cd " + shQuote(wd) + " && "
	}
	branch, base := "", ""
	if t.Spec.Repo != "" {
		branch = "curral/" + id
		if base, err = run(cd + "git rev-parse HEAD && git checkout -q -b " + shQuote(branch)); err != nil {
			return fmt.Errorf("preparing the repo: %w: %s", err, sshx.Printable(base))
		}
		set(func(t *task.Task) { t.Branch = branch })
	}

	exit, aerr := m.runAgent(ctx, c, t, cd)
	set(func(t *task.Task) { t.Exit = &exit })
	f, err := os.Open(st.Path(id, "output.jsonl"))
	if err != nil {
		return err
	}
	sum := task.Summarize(t.Spec.Agent, f)
	f.Close()
	if err := os.WriteFile(st.Path(id, "result.md"), []byte(sum.Result), 0o600); err != nil {
		return err
	}
	set(func(t *task.Task) { t.Usage = sum.Usage })
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if branch != "" {
		// Commit whatever the agent left uncommitted, and point the export
		// branch at HEAD (wherever the agent left it).
		base = strings.SplitN(base, "\n", 2)[0]
		commit := cd + `git add -A && if ! git diff --cached --quiet; then ` +
			`git config user.email >/dev/null || git config user.email curral@localhost; ` +
			`git config user.name >/dev/null || git config user.name curral; ` +
			`git commit -q -m ` + shQuote("curral task "+id+": uncommitted changes") + `; fi && ` +
			`git checkout -q -B ` + shQuote(branch)
		if out, err := run(commit); err != nil {
			unexported = true // there may be commits; don't destroy them
			return fmt.Errorf("committing: %w: %s", err, sshx.Printable(out))
		}
		// Only git's output here: hooks and warnings above could add lines.
		out, err := run(cd + "git rev-list --count " + shQuote(base) + ".." + shQuote(branch))
		n, cerr := strconv.Atoi(lastLine(out))
		if err != nil || cerr != nil {
			unexported = true
			return fmt.Errorf("counting commits: %v %v: %s", err, cerr, sshx.Printable(out))
		}
		set(func(t *task.Task) { t.Commits = n })
		if n > 0 && t.Spec.Out != task.OutNone {
			unexported = true
			set(func(t *task.Task) { t.State = task.Exporting })
			out, err := m.exportTask(ctx, c, t, cd, base)
			if err != nil {
				return fmt.Errorf("export: %w", err)
			}
			unexported = false
			set(func(t *task.Task) { t.Output = out })
		} else if n == 0 {
			set(func(t *task.Task) { t.Output = "no changes" })
		}
	}
	switch {
	case aerr != nil:
		return aerr
	case sum.Error != "":
		return errors.New(sum.Error)
	}
	return nil
}

// runAgent runs the agent headless with the task's timeout, streaming its
// stdout to output.jsonl and stderr to stderr.log on the host.
func (m *Manager) runAgent(ctx context.Context, c *ssh.Client, t *task.Task, cd string) (int, error) {
	st := m.Tasks()
	out, err := os.OpenFile(st.Path(t.ID, "output.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return -1, err
	}
	defer out.Close()
	secs := int(t.Spec.Timeout.Seconds())
	cmd := cd + "timeout -k 30 " + strconv.Itoa(secs) + " curral-env bash -c " + shQuote(task.Command(t.Spec))
	actx, cancel := context.WithTimeout(ctx, t.Spec.Timeout+2*time.Minute)
	defer cancel()
	m.logf("task %s: running %s", t.ID, t.Spec.Agent)
	stderr, err := sshx.Stream(actx, c, cmd, out, maxTaskOutput)
	_ = os.WriteFile(st.Path(t.ID, "stderr.log"), stderr, 0o600)
	var ee *sshx.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee) && ee.Status == 124:
		return ee.Status, fmt.Errorf("%s timed out after %s", t.Spec.Agent, t.Spec.Timeout)
	case errors.As(err, &ee):
		return ee.Status, fmt.Errorf("%s exited with status %d: %s", t.Spec.Agent, ee.Status, firstLine(sshx.Printable(string(stderr))))
	}
	return -1, err
}

func (m *Manager) exportTask(ctx context.Context, c *ssh.Client, t *task.Task, cd, base string) (string, error) {
	st := m.Tasks()
	po := PullOpts{Branch: t.Branch, Into: t.Spec.Into}
	switch t.Spec.Out {
	case task.OutBundle:
		p, err := m.Pull(ctx, t.VM, po)
		if err != nil {
			return "", err
		}
		return p.Ref + " in " + p.Into, nil
	case task.OutPatch:
		path := st.Path(t.ID, t.ID+".mbox")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return "", err
		}
		stderr, err := sshx.Stream(ctx, c, cd+"git format-patch --stdout "+shQuote(base)+"..HEAD", f, maxBundle)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", fmt.Errorf("format-patch: %w: %s", err, sshx.Printable(string(stderr)))
		}
		if err := m.scanPatch(t.VM, path, "in the task's patches"); err != nil {
			return "", err
		}
		return path, nil
	}
	title := t.Spec.Title
	if title == "" {
		title = "curral task " + t.ID + ": " + firstLine(readFile(st.Path(t.ID, "prompt.md")))
		if r := []rune(title); len(r) > 72 {
			title = string(r[:71]) + "…"
		}
	}
	// Forges cap PR bodies (GitHub: 65,536 characters).
	body := fmt.Sprintf("Opened by curral task `%s` (%s, VM `%s`).\n\n## Prompt\n\n%s\n\n## Result\n\n%s\n",
		t.ID, t.Spec.Agent, t.VM, cut(readFile(st.Path(t.ID, "prompt.md")), 20000), cut(readFile(st.Path(t.ID, "result.md")), 35000))
	bodyFile := st.Path(t.ID, "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o600); err != nil {
		return "", err
	}
	return m.PR(ctx, t.VM, PROpts{PullOpts: po, Title: title, BodyFile: bodyFile})
}

// cut shortens s to at most n runes, saying so.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n\n… (cut; the full text is in the task directory)"
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// CancelTask stops a task's supervisor, which then destroys its VM. A task
// whose supervisor died is marked cancelled and its VM destroyed here.
func (m *Manager) CancelTask(id string) error {
	t, err := m.Tasks().Load(id)
	if err != nil {
		return err
	}
	if task.Final(t.State) {
		return fmt.Errorf("task %s is already %s", id, t.State)
	}
	if t.Alive() {
		return t.Signal(syscall.SIGTERM)
	}
	if err := m.reapTask(t); err != nil {
		return err
	}
	_, err = m.Tasks().Update(id, func(t *task.Task) error {
		t.State, t.Error, t.Ended = task.Cancelled, "cancelled after its supervisor died", time.Now().UTC()
		return nil
	})
	return err
}

// reapTask destroys the VM of a task whose supervisor is gone.
func (m *Manager) reapTask(t *task.Task) error {
	if _, err := os.Stat(m.dir(t.VM)); err != nil {
		return nil
	}
	return m.Destroy(t.VM)
}

// RemoveTask deletes a finished task's record.
func (m *Manager) RemoveTask(id string) error {
	t, err := m.Tasks().Load(id)
	if err != nil {
		return err
	}
	if !task.Final(t.State) {
		if t.Alive() {
			return fmt.Errorf("task %s is %s; cancel it first", id, t.State)
		}
		if err := m.reapTask(t); err != nil {
			return err
		}
	}
	return os.RemoveAll(m.Tasks().Path(id, ""))
}
