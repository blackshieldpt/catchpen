package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/task"
)

// runTask runs `catchpen task <args>` (under a root command when one is
// given), and returns what it printed to stdout and stderr.
func runTask(t *testing.T, root *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }() // drained as it runs, so a full pipe can't block it
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()
	func() {
		defer func() { // restored even if the command panics, so the panic is seen
			os.Stdout, os.Stderr = oldOut, oldErr
			outW.Close()
			errW.Close()
		}()
		cmd := taskCmd()
		if root != nil {
			root.AddCommand(cmd)
			cmd = root
			args = append([]string{"task"}, args...)
		}
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	}()
	return <-outC, <-errC, err
}

// taskHome makes a CATCHPEN_HOME with three tasks: aaaaaaa1 (claude, done,
// with every field, rendered output and a result), bbbbbbb2 (cmd, failed,
// output without a final newline) and ccccccc3 (no output yet). Their guest
// text holds control characters. It also writes two batch files.
func taskHome(t *testing.T) (home string, st task.Store) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CATCHPEN_HOME", home)
	p := paths.Default()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	st = task.Store{Dir: p.Tasks()}
	exit0, exit3 := 0, 3
	write := func(path, s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(tk *task.Task, output, result string) {
		if err := st.Create(tk, "p"); err != nil {
			t.Fatal(err)
		}
		if output != "-" {
			write(st.Path(tk.ID, "output.jsonl"), output)
		}
		if result != "" {
			write(st.Path(tk.ID, "result.md"), result)
		}
	}
	mk(&task.Task{ID: "aaaaaaa1", State: task.Done, VM: "task-aaaaaaa1", Created: time.Now(), Spec: task.Spec{Agent: "claude", Repo: "g:o/r"},
		Branch: "catchpen/aaaaaaa1", Commits: 2, Exit: &exit0, Output: "https://g.test/o/r/pulls/1",
		Usage: task.Usage{Turns: 4, InputTokens: 1200, OutputTokens: 340, CostUSD: 0.0123}},
		`{"type":"system","subtype":"init"}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Looking at it.\nTwo lines."}]}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`+"\n"+
			`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok\x1b[31m"}]}}`+"\n"+
			`{"type":"result","subtype":"success","num_turns":4,"total_cost_usd":0.0123,"result":"Done."}`+"\n",
		"All done.\n\x1b[31mred\n")
	mk(&task.Task{ID: "bbbbbbb2", State: task.Failed, VM: "task-bbbbbbb2", Created: time.Now(), Spec: task.Spec{Agent: "cmd", Cmd: "x"},
		Exit: &exit3, Error: "agent exited 3: \x1b]0;t"}, "hello\n\x1b[2Jworld", "")
	mk(&task.Task{ID: "ccccccc3", State: task.Done, VM: "task-ccccccc3", Created: time.Now(), Spec: task.Spec{Agent: "claude"}}, "-", "")
	write(filepath.Join(home, "two.yaml"), "defaults: { out: none, agent: cmd, cmd: 'true' }\ntasks:\n  - { prompt: a }\n  - { prompt: b }\n")
	write(filepath.Join(home, "one.yaml"), "tasks:\n  - { prompt: a, agent: cmd, cmd: 'true', out: none }\n")
	write(filepath.Join(home, "half.yaml"), "tasks:\n  - { prompt: a, agent: cmd, cmd: 'true', out: none }\n  - { prompt: b, template: nosuch, agent: cmd, cmd: 'true', out: none }\n")
	write(filepath.Join(home, "bad.yaml"), "tasks:\n  - { prompt: a, template: nosuch, agent: cmd, cmd: 'true', out: none }\n  - { prompt: b }\n")
	return home, st
}

// task show, logs, logs -f and run, as a person sees them: every byte on
// stdout and stderr, and each error. ($HOME stands for CATCHPEN_HOME.)
func TestTaskCommands(t *testing.T) {
	home, st := taskHome(t)
	for _, c := range []struct {
		args                []string
		stdout, stderr, err string
	}{
		{[]string{"show", "aaaaaaa1"},
			"task     aaaaaaa1 (done)\nagent    claude\nrepo     g:o/r\nVM       task-aaaaaaa1\nbranch   catchpen/aaaaaaa1 (2 commits)\nexit     0\nusage    4 turns, 1200 in / 340 out tokens, $0.0123\noutput   https://g.test/o/r/pulls/1\nfiles    $HOME/data/tasks/aaaaaaa1\n\n--- result\nAll done.\n[31mred\n\n",
			"", ""},
		{[]string{"show", "bbbbbbb2"},
			"task     bbbbbbb2 (failed)\nagent    cmd\nrepo     -\nVM       task-bbbbbbb2\nexit     3\nerror    agent exited 3: ]0;t\nfiles    $HOME/data/tasks/bbbbbbb2\n",
			"", ""},
		{[]string{"show", "ccccccc3"},
			"task     ccccccc3 (done)\nagent    claude\nrepo     -\nVM       task-ccccccc3\nfiles    $HOME/data/tasks/ccccccc3\n",
			"", ""},
		{[]string{"show", "0badc0de"},
			"",
			"", "no task 0badc0de (see: catchpen task ls)"},
		{[]string{"show", "nope"},
			"",
			"", "invalid task id \"nope\""},
		{[]string{"logs", "aaaaaaa1"},
			"Looking at it.\nTwo lines.\n→ Bash {\"command\":\"go test ./...\"}\n{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"content\":\"ok\\x1b[31m\"}]}}\n== success: 4 turns, $0.0123\n",
			"", ""},
		{[]string{"logs", "--raw", "aaaaaaa1"},
			"{\"type\":\"system\",\"subtype\":\"init\"}\n{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"Looking at it.\\nTwo lines.\"}]}}\n{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"name\":\"Bash\",\"input\":{\"command\":\"go test ./...\"}}]}}\n{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"content\":\"ok\\x1b[31m\"}]}}\n{\"type\":\"result\",\"subtype\":\"success\",\"num_turns\":4,\"total_cost_usd\":0.0123,\"result\":\"Done.\"}\n",
			"", ""},
		{[]string{"logs", "bbbbbbb2"},
			"hello\n[2Jworld\n",
			"", ""},
		{[]string{"logs", "ccccccc3"},
			"",
			"", ""},
		{[]string{"logs", "0badc0de"},
			"",
			"", "no task 0badc0de (see: catchpen task ls)"},
		{[]string{"logs", "-f", "aaaaaaa1"},
			"Looking at it.\nTwo lines.\n→ Bash {\"command\":\"go test ./...\"}\n{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"content\":\"ok\\x1b[31m\"}]}}\n== success: 4 turns, $0.0123\n",
			"task aaaaaaa1 done: https://g.test/o/r/pulls/1\n", ""},
		{[]string{"logs", "-f", "bbbbbbb2"},
			"hello\n[2Jworld\n",
			"task bbbbbbb2 failed: agent exited 3: ]0;t\n", "exit status 1"},
		{[]string{"logs", "-f", "--raw", "bbbbbbb2"},
			"hello\n[2Jworld\n",
			"task bbbbbbb2 failed: agent exited 3: ]0;t\n", "exit status 1"},
		{[]string{"run", "--prompt", "x", "--parallel", "2"},
			"",
			"", "--parallel goes with --file"},
		{[]string{"run", "--file", "$HOME/two.yaml", "-f"},
			"",
			"", "-f follows one task; use catchpen task logs -f <id> per task"},
		{[]string{"run", "--file", "$HOME/none.yaml"},
			"",
			"", "open $HOME/none.yaml: no such file or directory"},
		{[]string{"run", "--prompt", "x", "--agent", "nosuch"},
			"",
			"", "unknown agent \"nosuch\" (claude, opencode, pi, omp, codex, cmd)"},
		{[]string{"run", "--prompt", "x", "--agent", "cmd"},
			"",
			"", "--cmd goes with --agent cmd (and only with it)"},
		{[]string{"run", "--prompt", "x", "-t", "nosuch", "--agent", "cmd", "--cmd", "true", "--out", "none"},
			"",
			"", "unknown section \"nosuch\" (catchpen template ls lists them) (submitted so far: )"},
		{[]string{"run", "--file", "$HOME/bad.yaml"},
			"",
			"", "unknown section \"nosuch\" (catchpen template ls lists them) (submitted so far: )"},
	} {
		for i, a := range c.args {
			c.args[i] = strings.ReplaceAll(a, "$HOME", home)
		}
		out, errOut, err := runTask(t, nil, c.args...)
		got := ""
		if err != nil {
			got = err.Error()
		}
		r := strings.NewReplacer(home, "$HOME")
		if r.Replace(out) != c.stdout || r.Replace(errOut) != c.stderr || r.Replace(got) != c.err {
			t.Errorf("task %s:\nstdout %q\n  want %q\nstderr %q\n  want %q\nerror  %q\n  want %q", strings.Join(c.args, " "),
				r.Replace(out), c.stdout, r.Replace(errOut), c.stderr, r.Replace(got), c.err)
		}
	}
	if ts, err := st.List(); err != nil || len(ts) != 3 {
		t.Errorf("%d tasks after the refused runs, %v; want the 3 made here", len(ts), err)
	}

	// A batch of one is submitted (the supervisor this starts is this test
	// binary, which TestMain makes exit at once), with --parallel.
	out, errOut, err := runTask(t, nil, "run", "--file", filepath.Join(home, "one.yaml"), "--parallel", "2")
	m := regexp.MustCompile(`^([0-9a-f]{8}) queued \(cmd, -\)\n$`).FindStringSubmatch(out)
	if err != nil || m == nil || errOut != "follow with: catchpen task logs -f "+m[1]+"\n" {
		t.Fatalf("run --file one.yaml: %q, %q, %v", out, errOut, err)
	}
	if tk, err := st.Load(m[1]); err != nil || tk.Spec.Parallel != 2 || tk.Spec.Batch == "" || tk.Spec.Agent != "cmd" || tk.State != task.Queued {
		t.Errorf("submitted %+v, %v", tk, err)
	}
	// Its supervisor is gone, so it is lost: logs -f ends, and fails.
	out, errOut, err = runTask(t, nil, "logs", "-f", m[1])
	if out != "" || errOut != "task "+m[1]+" lost\n" || err == nil || err.Error() != "exit status 1" {
		t.Errorf("logs -f of a lost task: %q, %q, %v", out, errOut, err)
	}
	// A batch of two: a queued line each, in order, and the hint names the first.
	out, errOut, err = runTask(t, nil, "run", "--file", filepath.Join(home, "two.yaml"))
	m2 := regexp.MustCompile(`^([0-9a-f]{8}) queued \(cmd, -\)\n([0-9a-f]{8}) queued \(cmd, -\)\n$`).FindStringSubmatch(out)
	if err != nil || m2 == nil || m2[1] == m2[2] || errOut != "follow with: catchpen task logs -f "+m2[1]+"\n" {
		t.Errorf("run --file two.yaml: %q, %q, %v", out, errOut, err)
	}
	// A batch whose second task fails: the first is queued and printed, and
	// the error names it.
	out, errOut, err = runTask(t, nil, "run", "--file", filepath.Join(home, "half.yaml"))
	m = regexp.MustCompile(`^([0-9a-f]{8}) queued \(cmd, -\)\n$`).FindStringSubmatch(out)
	if m == nil || errOut != "" || err == nil || err.Error() != `unknown section "nosuch" (catchpen template ls lists them) (submitted so far: `+m[1]+")" {
		t.Errorf("run --file half.yaml: %q, %q, %v", out, errOut, err)
	}
	// run -f follows a batch of one the same way.
	out, errOut, err = runTask(t, nil, "run", "--file", filepath.Join(home, "one.yaml"), "-f")
	m = regexp.MustCompile(`^([0-9a-f]{8}) queued \(cmd, -\)\n$`).FindStringSubmatch(out)
	if m == nil || errOut != "task "+m[1]+" lost\n" || err == nil || err.Error() != "exit status 1" {
		t.Errorf("run -f: %q, %q, %v", out, errOut, err)
	}
}
