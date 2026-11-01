package task

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// storeWith makes a store holding one task in state, with output.jsonl and
// (when not empty) result.md holding the given guest text.
func storeWith(t *testing.T, state, agent, output, result string) (Store, *Task) {
	t.Helper()
	st := Store{Dir: t.TempDir()}
	tk := &Task{ID: NewID(), State: state, Created: time.Now(), Spec: Spec{Agent: agent}}
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
	return st, tk
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
func TestWriteOutputLinesStandAlone(t *testing.T) {
	lines := "\x1b]0;owned\n" + // unterminated OSC
		"\xc2\x9d0;owned\n" + // the same, as a UTF-8 C1 OSC
		"second\rSECOND\n" +
		"\x1b[1A\x1b[2Kthird\n" +
		"fourth\n"
	for _, raw := range []bool{true, false} {
		st, tk := storeWith(t, Done, "cmd", lines, "")
		var out bytes.Buffer
		if err := st.WriteOutput(context.Background(), tk.ID, false, raw, &out); err != nil {
			t.Fatal(err)
		}
		clean(t, "logs", out.String(), "SECOND", "third", "fourth")
	}
	// Rendered agent text: Render decodes \u001b in the JSON into a real ESC.
	st, tk := storeWith(t, Done, "claude",
		`{"type":"assistant","message":{"content":[{"type":"text","text":"\u001b]0;owned"}]}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"text","text":"after"}]}}`+"\n", "")
	var out bytes.Buffer
	if err := st.WriteOutput(context.Background(), tk.ID, false, false, &out); err != nil {
		t.Fatal(err)
	}
	clean(t, "rendered", out.String(), "after")
}

// task show: the guest-quoting error line and result.md follow guest text
// that starts a string without ending it.
func TestShowGuestText(t *testing.T) {
	st, tk := storeWith(t, Failed, "claude", "", "\x1b]0;owned\nresult-after\n")
	tk.Output = "out\x1b]0;owned"
	tk.Error = "error-after"
	var out bytes.Buffer
	if err := st.Show(&out, tk); err != nil {
		t.Fatal(err)
	}
	clean(t, "show", out.String(), "error-after", "result-after")
}

// task logs -f / run -f: the last line shows even without a final newline,
// lines after an unterminated string show, and the status line is clean.
func TestFollow(t *testing.T) {
	st, tk := storeWith(t, Done, "cmd", "\x1b]0;owned\nlast", "")
	tk.Output, tk.Error = "\x1b]0;owned", "error-after"
	if err := st.Save(tk); err != nil {
		t.Fatal(err)
	}
	var out, status bytes.Buffer
	got, err := st.Follow(context.Background(), tk.ID, false, &out, &status)
	if err != nil || got == nil || got.ID != tk.ID || got.State != Done {
		t.Fatalf("%+v, %v", got, err)
	}
	clean(t, "follow", out.String(), "last")
	clean(t, "status", status.String(), "task "+tk.ID+" done", "error-after")
}

// A batch's specs share one new batch id, and the batch-wide limit.
func TestAsBatch(t *testing.T) {
	a := []Spec{{Prompt: "a"}, {Prompt: "b"}}
	AsBatch(a, 3)
	if a[0].Batch == "" || a[0].Batch != a[1].Batch || a[0].Parallel != 3 || a[1].Parallel != 3 || a[1].Prompt != "b" {
		t.Errorf("batch %q %q, parallel %d %d", a[0].Batch, a[1].Batch, a[0].Parallel, a[1].Parallel)
	}
	b := []Spec{{Prompt: "c"}}
	AsBatch(b, 0)
	if b[0].Batch == "" || b[0].Batch == a[0].Batch || b[0].Parallel != 0 {
		t.Errorf("second batch %+v", b)
	}
}

// Follow returns a task that ended badly or was lost, for the caller to
// judge, and fails for one that doesn't exist.
func TestFollowEndedBadly(t *testing.T) {
	st, tk := storeWith(t, Failed, "cmd", "out\n", "")
	tk.Error = "agent exited 3"
	if err := st.Save(tk); err != nil {
		t.Fatal(err)
	}
	var out, status bytes.Buffer
	got, err := st.Follow(context.Background(), tk.ID, false, &out, &status)
	if err != nil || got == nil || got.State != Failed || out.String() != "out\n" || status.String() != "task "+tk.ID+" failed: agent exited 3\n" {
		t.Errorf("%+v, %v; out %q, status %q", got, err, out.String(), status.String())
	}
	// A running task whose supervisor is gone is lost: this process's
	// cmdline doesn't say task-supervise.
	st2, lost := storeWith(t, Running, "cmd", "", "")
	lost.PID = os.Getpid()
	if err := st2.Save(lost); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	status.Reset()
	if got, err := st2.Follow(context.Background(), lost.ID, false, &out, &status); err != nil || got == nil || got.Status() != "lost" || status.String() != "task "+lost.ID+" lost\n" {
		t.Errorf("lost: %+v, %v; status %q", got, err, status.String())
	}
	out.Reset()
	status.Reset()
	if got, err := st.Follow(context.Background(), "0badc0de", false, &out, &status); got != nil || err == nil || out.Len() != 0 || status.Len() != 0 {
		t.Errorf("missing task: %+v, %v; out %q, status %q", got, err, out.String(), status.String())
	}
}
