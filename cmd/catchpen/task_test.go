package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/task"
	"github.com/blackshieldpt/catchpen/internal/vm"
)

// taskWith makes a manager whose task store holds one task in state, with
// output.jsonl and (when not empty) result.md holding the given guest text.
func taskWith(t *testing.T, state, agent, output, result string) (*vm.Manager, *task.Task) {
	t.Helper()
	root := t.TempDir()
	p := paths.Paths{Config: filepath.Join(root, "config"), Data: filepath.Join(root, "data"), Cache: filepath.Join(root, "cache")}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	m := &vm.Manager{P: p, Cfg: config.Default()}
	tk := &task.Task{ID: task.NewID(), State: state, Created: time.Now(), Spec: task.Spec{Agent: agent}}
	st := m.Tasks()
	if err := st.Create(tk, "p"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.Path(tk.ID, "output.jsonl"), []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	if result != "" {
		if err := os.WriteFile(st.Path(tk.ID, "result.md"), []byte(result), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return m, tk
}

// clean fails unless out holds want and no control character but newline.
func clean(t *testing.T, what, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("%s: %q is missing %q", what, out, w)
		}
	}
	for _, r := range out {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			t.Errorf("%s: control character %U in %q", what, r, out)
			return
		}
	}
}

// A guest line can't hide the lines after it or move the cursor: an
// unterminated OSC, a CR or a cursor-up/erase sequence acts on its own line
// at most.
func TestShowTaskOutputLinesStandAlone(t *testing.T) {
	lines := "\x1b]0;owned\n" + // unterminated OSC
		"\xc2\x9d0;owned\n" + // the same, as a UTF-8 C1 OSC
		"second\rSECOND\n" +
		"\x1b[1A\x1b[2Kthird\n" +
		"fourth\n"
	for _, raw := range []bool{true, false} {
		m, tk := taskWith(t, task.Done, "cmd", lines, "")
		var out bytes.Buffer
		if err := showTaskOutput(context.Background(), m, tk.ID, false, raw, &out); err != nil {
			t.Fatal(err)
		}
		clean(t, "logs", out.String(), "SECOND", "third", "fourth")
	}
	// Rendered agent text: Render decodes \u001b in the JSON into a real ESC.
	m, tk := taskWith(t, task.Done, "claude",
		`{"type":"assistant","message":{"content":[{"type":"text","text":"\u001b]0;owned"}]}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"text","text":"after"}]}}`+"\n", "")
	var out bytes.Buffer
	if err := showTaskOutput(context.Background(), m, tk.ID, false, false, &out); err != nil {
		t.Fatal(err)
	}
	clean(t, "rendered", out.String(), "after")
}

// task show: the guest-quoting error line and result.md follow guest text
// that starts a string without ending it.
func TestShowTaskGuestText(t *testing.T) {
	m, tk := taskWith(t, task.Failed, "claude", "", "\x1b]0;owned\nresult-after\n")
	tk.Output = "out\x1b]0;owned"
	tk.Error = "error-after"
	var out bytes.Buffer
	if err := showTask(&out, m.Tasks(), tk); err != nil {
		t.Fatal(err)
	}
	clean(t, "show", out.String(), "error-after", "result-after")
}

// task logs -f / run -f: the last line shows even without a final newline,
// lines after an unterminated string show, and the status line is clean.
func TestFollowTask(t *testing.T) {
	m, tk := taskWith(t, task.Done, "cmd", "\x1b]0;owned\nlast", "")
	tk.Output, tk.Error = "\x1b]0;owned", "error-after"
	if err := m.Tasks().Save(tk); err != nil {
		t.Fatal(err)
	}
	var out, status bytes.Buffer
	if err := followTask(context.Background(), m, tk.ID, false, &out, &status); err != nil {
		t.Fatal(err)
	}
	clean(t, "follow", out.String(), "last")
	clean(t, "status", status.String(), "task "+tk.ID+" done", "error-after")
}
