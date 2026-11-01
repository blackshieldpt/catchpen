package task

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/blackshieldpt/catchpen/internal/jsonl"
	"github.com/blackshieldpt/catchpen/internal/sshx"
)

// AsBatch makes specs one batch: they share a new batch id, and parallel is
// their batch-wide limit (0: only the budget limits them).
func AsBatch(specs []Spec, parallel int) {
	batch := NewID()
	for i := range specs {
		specs[i].Batch, specs[i].Parallel = batch, parallel
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Show writes what catchpen task show prints about t: its fields, then its
// result. The text that can quote the agent is written without control
// characters.
func (s Store) Show(w io.Writer, t *Task) error {
	fmt.Fprintf(w, "task     %s (%s)\nagent    %s\nrepo     %s\nVM       %s\n", t.ID, t.Status(), t.Spec.Agent, orDash(t.Spec.Repo), t.VM)
	if t.Branch != "" {
		fmt.Fprintf(w, "branch   %s (%d commits)\n", t.Branch, t.Commits)
	}
	if t.Exit != nil {
		fmt.Fprintf(w, "exit     %d\n", *t.Exit)
	}
	if t.Usage != (Usage{}) {
		fmt.Fprintf(w, "usage    %d turns, %d in / %d out tokens, $%.4f\n", t.Usage.Turns, t.Usage.InputTokens, t.Usage.OutputTokens, t.Usage.CostUSD)
	}
	// Error text can quote the agent (guest) output.
	if t.Output != "" {
		fmt.Fprintf(w, "output   %s\n", sshx.Printable(t.Output))
	}
	if t.Error != "" {
		fmt.Fprintf(w, "error    %s\n", sshx.Printable(t.Error))
	}
	fmt.Fprintf(w, "files    %s\n", s.Path(t.ID, ""))
	if b, err := os.ReadFile(s.Path(t.ID, "result.md")); err == nil && len(b) > 0 {
		fmt.Fprintln(w, "\n--- result")
		// The result is the agent's text from the guest.
		_, err := fmt.Fprintln(w, sshx.Printable(string(b)))
		return err
	}
	return nil
}

// WriteOutput writes a task's agent output to w, one line per event: rendered
// for its agent, or the event stream as is when raw. With follow it waits for
// more until ctx ends; a cancel whose cause is jsonl.ErrWriterDone reads the
// last lines first.
func (s Store) WriteOutput(ctx context.Context, id string, follow, raw bool, w io.Writer) error {
	t, err := s.Load(id)
	if err != nil {
		return err
	}
	return jsonl.Tail(ctx, s.Path(id, "output.jsonl"), follow, func(line string) error {
		out := line
		if !raw {
			out = Render(t.Spec.Agent, []byte(line))
		}
		if out == "" {
			return nil
		}
		// Guest text without control characters: nothing in one line can hide
		// the lines after it, move the cursor or act on the terminal.
		_, err := io.WriteString(w, sshx.Printable(out)+"\n")
		return err
	})
}

// Follow writes a task's output to out until the task ends or ctx does, then
// a status line to status, and returns the task as it then was. The error is
// the output's: nil both when the task ended and when ctx ended first, so a
// caller that cancels checks ctx.Err() before taking the task as ended (a
// task that ended is Final(t.State), or lost: t.Status() == "lost"). What a task that ended other than done means is the
// caller's to decide. A task that can't be loaded afterwards gives (nil, err).
func (s Store) Follow(ctx context.Context, id string, raw bool, out, status io.Writer) (*Task, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			if t, err := s.Load(id); err == nil && (Final(t.State) || t.Status() == "lost") {
				time.Sleep(time.Second) // let the tail read the last lines
				cancel(jsonl.ErrWriterDone)
				return
			}
		}
	}()
	err := s.WriteOutput(ctx, id, true, raw, out)
	t, lerr := s.Load(id)
	if lerr != nil {
		return nil, lerr
	}
	line := fmt.Sprintf("task %s %s", id, t.Status())
	if t.Output != "" {
		line += ": " + t.Output
	}
	if t.Error != "" {
		line += ": " + t.Error
	}
	fmt.Fprintln(status, sshx.Printable(line))
	return t, err
}
