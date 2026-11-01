package main

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// task run --file takes no other task flag but --parallel and --follow, as
// docs/tasks.md says, and refuses before submitting anything. The root's
// --debug isn't a task flag.
func TestTaskRunFileTakesNoOtherFlags(t *testing.T) {
	home, st := taskHome(t)
	one := filepath.Join(home, "one.yaml")
	for _, extra := range [][]string{
		{"--prompt", "x"}, {"--repo", "g:o/r"}, {"--agent", "cmd"}, {"-t", "default"}, {"--rw"}, {"--keep"},
		{"--timeout", "1h"}, {"--out", "none"}, {"--parallel", "2", "--title", "T"},
	} {
		_, _, err := runTask(t, nil, append([]string{"run", "--file", one}, extra...)...)
		if err == nil || err.Error() != "--file can't be combined with other task flags (put them under defaults:)" {
			t.Errorf("--file with %v: %v", extra, err)
		}
	}
	if ts, err := st.List(); err != nil || len(ts) != 3 {
		t.Fatalf("%d tasks after the refused runs, %v; want the 3 taskHome made", len(ts), err)
	}
	var dbg bool
	root := &cobra.Command{Use: "catchpen", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&dbg, "debug", false, "")
	if _, _, err := runTask(t, root, "--debug", "run", "--file", one, "--parallel", "2"); err != nil {
		t.Errorf("--debug with --file: %v", err)
	}
}
