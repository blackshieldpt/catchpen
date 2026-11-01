package runner

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/task"
)

// SubmitAll submits in order and stops at the first error, which names the
// tasks submitted before it; those are returned.
func TestSubmitAll(t *testing.T) {
	r := testRunner(t)
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	r.M.Bin = bin // the supervisor it starts exits at once
	ok := task.Spec{Template: "default", Agent: "cmd", Cmd: "true", Prompt: "x", Out: task.OutNone, Timeout: time.Hour}
	bad := ok
	bad.Template = "nosuch"
	ts, err := r.SubmitAll([]task.Spec{ok, ok})
	if err != nil || len(ts) != 2 || ts[0].ID == ts[1].ID {
		t.Fatalf("%v, %v", ts, err)
	}
	for _, tk := range ts {
		if got, err := r.M.Tasks().Load(tk.ID); err != nil || got.State != task.Queued || got.Spec.Cmd != "true" {
			t.Errorf("stored %+v, %v", got, err)
		}
	}
	ts, err = r.SubmitAll([]task.Spec{ok, bad, ok})
	if len(ts) != 1 || err == nil || !strings.HasSuffix(err.Error(), " (submitted so far: "+ts[0].ID+")") {
		t.Errorf("second fails: %v, %v", ts, err)
	}
	ts, err = r.SubmitAll([]task.Spec{bad, ok})
	if len(ts) != 0 || err == nil || !strings.HasSuffix(err.Error(), " (submitted so far: )") {
		t.Errorf("first fails: %v, %v", ts, err)
	}
	if all, err := r.M.Tasks().List(); err != nil || len(all) != 3 {
		t.Errorf("%d tasks stored, %v; want 3", len(all), err)
	}
}
