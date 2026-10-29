package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/jsonl"
	"github.com/blackshieldpt/catchpen/internal/runner"
	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/internal/task"
	"github.com/blackshieldpt/catchpen/internal/vm"
)

func taskCmd() *cobra.Command {
	c := &cobra.Command{Use: "task", Short: "Run agents headless in throwaway VMs"}
	c.AddCommand(taskRunCmd(), taskLsCmd(), taskShowCmd(), taskLogsCmd(), taskCancelCmd(), taskRmCmd())
	return c
}

func taskRunCmd() *cobra.Command {
	var s task.Spec
	var file string
	var parallel int
	var follow bool
	c := &cobra.Command{
		Use:   "run [--file tasks.yaml] | run --prompt TEXT|--prompt-file F [--repo R] [--agent A]",
		Short: "Queue headless tasks; each runs in its own VM, which is destroyed afterwards",
		Long: `Each task gets a new VM (read-only deploy keys unless --rw), runs the agent on branch
catchpen/<id> without permission prompts, commits what it leaves, exports the work from the
host (--out pr: push with your credentials and open a PR; bundle: fetch into the host
repo; patch: an mbox in the task directory) and destroys the VM unless --keep.
Tasks wait for the budget in config.yaml tasks: (max_vms, max_memory).
--agent cmd --cmd '...' runs any shell command with the prompt on stdin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			var specs []task.Spec
			if file != "" {
				if cmd.LocalFlags().NFlag() > 1+boolToInt(cmd.Flags().Changed("parallel"))+boolToInt(follow) {
					return errors.New("--file can't be combined with other task flags (put them under defaults:)")
				}
				if specs, err = task.ParseFile(file, wd); err != nil {
					return err
				}
				batch := task.NewID()
				for i := range specs {
					specs[i].Batch, specs[i].Parallel = batch, parallel
				}
				if follow && len(specs) > 1 {
					return errors.New("-f follows one task; use catchpen task logs -f <id> per task")
				}
			} else {
				if cmd.Flags().Changed("parallel") {
					return errors.New("--parallel goes with --file")
				}
				if err := s.Normalize(wd, wd); err != nil {
					return err
				}
				specs = []task.Spec{s}
			}
			var ids []string
			for _, sp := range specs {
				t, err := (&runner.Runner{M: m}).Submit(sp)
				if err != nil {
					return fmt.Errorf("%w (submitted so far: %s)", err, strings.Join(ids, " "))
				}
				ids = append(ids, t.ID)
				fmt.Printf("%s queued (%s, %s)\n", t.ID, sp.Agent, orDash(sp.Repo))
			}
			if follow && len(ids) == 1 {
				return followTask(cmd.Context(), m, ids[0], false, os.Stdout, os.Stderr)
			}
			fmt.Fprintf(os.Stderr, "follow with: catchpen task logs -f %s\n", ids[0])
			return nil
		},
	}
	f := c.Flags()
	f.StringVarP(&s.Template, "template", "t", "", templateFlagHelp)
	f.StringVar(&s.Repo, "repo", "", "repo to clone and work on (forge:owner/repo or URL)")
	f.StringVar(&s.Agent, "agent", "claude", strings.Join(task.Agents, " | "))
	f.StringVar(&s.Cmd, "cmd", "", "shell command for --agent cmd (prompt on stdin, path in $CATCHPEN_PROMPT_FILE)")
	f.StringVar(&s.Prompt, "prompt", "", "the task")
	f.StringVar(&s.PromptFile, "prompt-file", "", "read the task from a file")
	f.DurationVar(&s.Timeout, "timeout", 2*time.Hour, "stop the agent after this long")
	f.Float64Var(&s.MaxBudgetUSD, "max-budget-usd", 0, "claude: stop after spending this much")
	f.StringVar(&s.Out, "out", "", "pr | bundle | patch | none (default pr with a repo)")
	f.StringVar(&s.Into, "into", "", "host repository for pr/bundle (default: current directory)")
	f.StringVar(&s.Title, "title", "", "PR title")
	f.BoolVar(&s.RW, "rw", false, "give the VM a read-write deploy key")
	f.BoolVar(&s.Keep, "keep", false, "keep the VM afterwards for inspection")
	f.StringVar(&file, "file", "", "a batch of tasks (YAML: defaults, tasks, matrix)")
	f.IntVar(&parallel, "parallel", 0, "with --file: at most this many of the batch at once")
	f.BoolVarP(&follow, "follow", "f", false, "follow the task's output until it ends")
	return c
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func taskLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			ts, err := m.Tasks().List()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATE\tAGENT\tREPO\tAGE\tTOOK\tCOST\tOUTPUT")
			for _, t := range ts {
				took, cost := "-", "-"
				if !t.Started.IsZero() {
					end := t.Ended
					if end.IsZero() {
						end = time.Now()
					}
					took = end.Sub(t.Started).Round(time.Second).String()
				}
				if t.Usage.CostUSD > 0 {
					cost = fmt.Sprintf("$%.2f", t.Usage.CostUSD)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Status(), t.Spec.Agent, orDash(t.Spec.Repo),
					age(t.Created), took, cost, orDash(t.Output))
			}
			return w.Flush()
		},
	}
}

func taskShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a task and its result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			t, err := m.Tasks().Load(args[0])
			if err != nil {
				return err
			}
			return showTask(os.Stdout, m.Tasks(), t)
		},
	}
}

func showTask(w io.Writer, st task.Store, t *task.Task) error {
	fmt.Fprintf(w, "task     %s (%s)\nagent    %s\nrepo     %s\nVM       %s\n", t.ID, t.Status(), t.Spec.Agent, orDash(t.Spec.Repo), t.VM)
	if t.Branch != "" {
		fmt.Fprintf(w, "branch   %s (%d commits)\n", t.Branch, t.Commits)
	}
	if t.Exit != nil {
		fmt.Fprintf(w, "exit     %d\n", *t.Exit)
	}
	if t.Usage != (task.Usage{}) {
		fmt.Fprintf(w, "usage    %d turns, %d in / %d out tokens, $%.4f\n", t.Usage.Turns, t.Usage.InputTokens, t.Usage.OutputTokens, t.Usage.CostUSD)
	}
	// Error text can quote the agent (guest) output.
	if t.Output != "" {
		fmt.Fprintf(w, "output   %s\n", sshx.Printable(t.Output))
	}
	if t.Error != "" {
		fmt.Fprintf(w, "error    %s\n", sshx.Printable(t.Error))
	}
	fmt.Fprintf(w, "files    %s\n", st.Path(t.ID, ""))
	if b, err := os.ReadFile(st.Path(t.ID, "result.md")); err == nil && len(b) > 0 {
		fmt.Fprintln(w, "\n--- result")
		// The result is the agent's text from the guest.
		_, err := fmt.Fprintln(w, sshx.Printable(string(b)))
		return err
	}
	return nil
}

func taskLogsCmd() *cobra.Command {
	var follow, raw bool
	c := &cobra.Command{
		Use:   "logs <id>",
		Short: "Show a task's agent output (rendered; --raw for the event stream)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if _, err := m.Tasks().Load(args[0]); err != nil {
				return err
			}
			if !follow {
				return showTaskOutput(cmd.Context(), m, args[0], false, raw, os.Stdout)
			}
			return followTask(cmd.Context(), m, args[0], raw, os.Stdout, os.Stderr)
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing until the task ends")
	c.Flags().BoolVar(&raw, "raw", false, "print the agent's event stream as is (control characters removed)")
	return c
}

func showTaskOutput(ctx context.Context, m *vm.Manager, id string, follow, raw bool, w io.Writer) error {
	t, err := m.Tasks().Load(id)
	if err != nil {
		return err
	}
	return jsonl.Tail(ctx, m.Tasks().Path(id, "output.jsonl"), follow, func(line string) error {
		s := line
		if !raw {
			s = task.Render(t.Spec.Agent, []byte(line))
		}
		if s == "" {
			return nil
		}
		// Guest text without control characters: nothing in one line can hide
		// the lines after it, move the cursor or act on the terminal.
		_, err := io.WriteString(w, sshx.Printable(s)+"\n")
		return err
	})
}

// followTask prints supervisor progress and agent output until the task ends.
func followTask(ctx context.Context, m *vm.Manager, id string, raw bool, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			if t, err := m.Tasks().Load(id); err == nil && (task.Final(t.State) || t.Status() == "lost") {
				time.Sleep(time.Second) // let the tail read the last lines
				cancel(jsonl.ErrWriterDone)
				return
			}
		}
	}()
	err := showTaskOutput(ctx, m, id, true, raw, stdout)
	t, lerr := m.Tasks().Load(id)
	if lerr != nil {
		return lerr
	}
	line := fmt.Sprintf("task %s %s", id, t.Status())
	if t.Output != "" {
		line += ": " + t.Output
	}
	if t.Error != "" {
		line += ": " + t.Error
	}
	fmt.Fprintln(stderr, sshx.Printable(line))
	if err == nil && t.State != task.Done {
		return exitError(1)
	}
	return err
}

func taskCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id>...",
		Short: "Cancel tasks (their VMs are destroyed)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			var errs []error
			for _, id := range args {
				errs = append(errs, (&runner.Runner{M: m}).Cancel(id))
			}
			return errors.Join(errs...)
		},
	}
}

func taskRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>...",
		Short: "Delete finished tasks' records",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			var errs []error
			for _, id := range args {
				errs = append(errs, (&runner.Runner{M: m}).Remove(id))
			}
			return errors.Join(errs...)
		},
	}
}

func taskSuperviseCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "task-supervise <id>",
		Short:  "Run one task (started by task run)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			m.In = nil // detached: no terminal for passphrases
			return (&runner.Runner{M: m}).Run(cmd.Context(), args[0])
		},
	}
}

func transcriptsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "transcripts <name>",
		Short: "Copy a running VM's agent sessions to the host now, and list the copies",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if _, ok := m.Running(args[0]); ok {
				if _, err := m.CopyTranscripts(cmd.Context(), args[0]); err != nil {
					return err
				}
			} else if _, err := m.Load(args[0]); err == nil {
				fmt.Fprintf(os.Stderr, "%s is not running; showing earlier copies\n", args[0])
			} else if err := vm.ValidName(args[0]); err != nil {
				return err
			}
			dirs, err := m.TranscriptDirs(args[0])
			if err != nil {
				return err
			}
			for _, d := range dirs {
				fmt.Println(d)
			}
			return nil
		},
	}
}
