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
	"github.com/spf13/pflag"

	"github.com/blackshieldpt/catchpen/internal/runner"
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
				others := 0 // run's own flags set, other than these four (not --debug)
				cmd.Flags().Visit(func(fl *pflag.Flag) {
					if cmd.LocalFlags().Lookup(fl.Name) != nil && fl.Name != "file" && fl.Name != "parallel" && fl.Name != "follow" && fl.Name != "help" {
						others++
					}
				})
				if others > 0 {
					return errors.New("--file can't be combined with other task flags (put them under defaults:)")
				}
				if specs, err = task.ParseFile(file, wd); err != nil {
					return err
				}
				task.AsBatch(specs, parallel)
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
			ts, err := (&runner.Runner{M: m}).SubmitAll(specs)
			for _, t := range ts {
				fmt.Printf("%s queued (%s, %s)\n", t.ID, t.Spec.Agent, orDash(t.Spec.Repo))
			}
			if err != nil {
				return err
			}
			if follow && len(ts) == 1 {
				return followTask(cmd.Context(), m, ts[0].ID, false, os.Stdout, os.Stderr)
			}
			fmt.Fprintf(os.Stderr, "follow with: catchpen task logs -f %s\n", ts[0].ID)
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
			return m.Tasks().Show(os.Stdout, t)
		},
	}
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
				return m.Tasks().WriteOutput(cmd.Context(), args[0], false, raw, os.Stdout)
			}
			return followTask(cmd.Context(), m, args[0], raw, os.Stdout, os.Stderr)
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing until the task ends")
	c.Flags().BoolVar(&raw, "raw", false, "print the agent's event stream as is (control characters removed)")
	return c
}

// followTask prints a task's output and status line until it ends or ctx
// does, and fails (exit status 1) unless it ended done.
func followTask(ctx context.Context, m *vm.Manager, id string, raw bool, stdout, stderr io.Writer) error {
	t, err := m.Tasks().Follow(ctx, id, raw, stdout, stderr)
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
