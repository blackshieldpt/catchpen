package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// A job's output is split into lines, each cut to MaxJobLine; the line left
// unended is written out when the job ends.
func TestJobLines(t *testing.T) {
	j := &job{info: api.JobInfo{State: api.JobRunning}}
	long := strings.Repeat("x", api.MaxJobLine)
	for _, s := range []string{"one\ntw", "o\n", long + "yz\n", long[:10], long + "\nlast, unended"} {
		io.WriteString(j, s)
	}
	if g := j.from(0, true); g.Written != 4 || g.State != api.JobRunning || len(g.Lines) != 4 {
		t.Fatalf("running: %+v", g)
	}
	j.end(nil)
	want := []string{"one", "two", long, long, "last, unended"}
	if g := j.from(0, true); g.From != 0 || g.Written != 5 || g.State != api.JobDone || g.Ended.IsZero() || !reflect.DeepEqual(g.Lines, want) {
		t.Errorf("all: %+v", g)
	}
	if g := j.from(4, true); g.From != 4 || !reflect.DeepEqual(g.Lines, want[4:]) {
		t.Errorf("after 4: %+v", g)
	}
	if g := j.from(9, true); g.From != 5 || g.Lines == nil || len(g.Lines) != 0 {
		t.Errorf("after the last: %+v", g)
	}
	if g := (&job{}).from(0, true); g.Lines == nil {
		t.Errorf("no lines: %+v", g)
	}
}

// A line not ended yet keeps no more than MaxJobLine bytes either.
func TestJobLinePartBounded(t *testing.T) {
	j := &job{}
	io.WriteString(j, strings.Repeat("y", 3*api.MaxJobLine))
	io.WriteString(j, "z")
	if len(j.part) != api.MaxJobLine {
		t.Errorf("%d bytes kept", len(j.part))
	}
}

// Only the last MaxJobLines are kept; From says where they start.
func TestJobLinesKept(t *testing.T) {
	j := &job{}
	for i := range api.MaxJobLines + 5 {
		fmt.Fprintf(j, "%d\n", i)
	}
	j.end(errors.New("boom"))
	g := j.from(2, true)
	if g.From != 5 || g.Written != api.MaxJobLines+5 || len(g.Lines) != api.MaxJobLines || g.Lines[0] != "5" ||
		g.State != api.JobFailed || g.Error != "boom" {
		t.Errorf("%+v", g)
	}
}

// waitEnded waits for the job with id to end.
func waitEnded(t *testing.T, js *jobs, id string) api.JobInfo {
	t.Helper()
	for range 500 {
		j, ok := js.find(id)
		if !ok {
			t.Fatalf("no job %s", id)
		}
		if i := j.get(); i.State != api.JobRunning {
			return i
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s still running", id)
	return api.JobInfo{}
}

// At most maxRunning jobs run; of those ended, the last maxEnded are kept.
func TestJobsBusyAndPruned(t *testing.T) {
	defer func(r, e int) { maxRunning, maxEnded = r, e }(maxRunning, maxEnded)
	maxRunning, maxEnded = 2, 2
	js := newJobs()
	defer js.stop()
	release := make(chan struct{})
	block := func(context.Context, io.Writer) error { <-release; return nil }
	var first []string
	for _, vm := range []string{"a", "b"} {
		i, err := js.start("k", vm, "me", nil, block)
		if err != nil {
			t.Fatal(err)
		}
		first = append(first, i.ID)
	}
	if _, err := js.start("k", "c", "me", nil, block); !errors.Is(err, errBusy) {
		t.Errorf("a third job: %v", err)
	}
	close(release)
	for _, id := range first {
		waitEnded(t, js, id)
	}
	for _, vm := range []string{"c", "d", "e"} {
		i, err := js.start("k", vm, "me", nil, func(context.Context, io.Writer) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		waitEnded(t, js, i.ID)
	}
	var vms []string
	for _, i := range js.list(true) {
		vms = append(vms, i.VM)
	}
	if !reflect.DeepEqual(vms, []string{"e", "d"}) {
		t.Errorf("kept %v, want [e d]", vms)
	}
	if _, ok := js.find(first[0]); ok {
		t.Errorf("a pruned job found")
	}
}

// stop cancels the running jobs, waits for them to clean up, and refuses
// new ones.
func TestJobsStop(t *testing.T) {
	js := newJobs()
	started := make(chan struct{})
	i, err := js.start("k", "a", "me", nil, func(ctx context.Context, out io.Writer) error {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintln(out, "cleaned up")
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	stopped := make(chan struct{})
	go func() { js.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop didn't end the job")
	}
	j, _ := js.find(i.ID)
	if g := j.from(0, true); g.State != api.JobFailed || g.Error != "context canceled" || !reflect.DeepEqual(g.Lines, []string{"cleaned up"}) {
		t.Errorf("after stop: %+v", g)
	}
	if _, err := js.start("k", "b", "me", nil, func(context.Context, io.Writer) error { return nil }); !errors.Is(err, errStopping) {
		t.Errorf("a job after stop: %v", err)
	}
}

// A job that panics fails with the panic; the runner goes on.
func TestJobPanic(t *testing.T) {
	js := newJobs()
	t.Cleanup(js.stop)
	i, err := js.start("k", "a", "me", nil, func(context.Context, io.Writer) error { panic("boom") })
	if err != nil {
		t.Fatal(err)
	}
	j, _ := js.find(i.ID)
	for g := j.get(); g.State == api.JobRunning; g = j.get() {
		time.Sleep(10 * time.Millisecond)
	}
	if g := j.get(); g.State != api.JobFailed || g.Error != "the job failed: panic: boom" {
		t.Errorf("got %+v", g)
	}
}

// A *vm.SecretError, wrapped, and a line starting vm.MissingSecrets are
// withheld from a client without config:write; a decoy's note wins over
// both; a line's mark goes with the line once it is dropped.
func TestJobSecretsWithheld(t *testing.T) {
	named := vm.MissingSecrets + "GONE_061 (set them …)"
	secretErr := fmt.Errorf("forge gone: %w", &vm.SecretError{Err: errors.New("secret GONE_061 not found on the host")})
	view := func(j *job, after int, secrets bool) string {
		g := j.from(after, secrets)
		return fmt.Sprintf("%q %s", g.Lines, g.Error)
	}
	j := &job{}
	fmt.Fprintf(j, "one\n%s\ntwo\n", named)
	j.end(secretErr)
	for _, tc := range []struct {
		after   int
		secrets bool
		want    string
	}{
		{0, true, fmt.Sprintf("[%q %q %q] %s", "one", named, "two", secretErr)},
		{0, false, fmt.Sprintf("[%q %q %q] %s", "one", withheldSecretLine, "two", withheldSecretError)},
		{1, false, fmt.Sprintf("[%q %q] %s", withheldSecretLine, "two", withheldSecretError)},
		{2, false, fmt.Sprintf("[%q] %s", "two", withheldSecretError)},
	} {
		if got := view(j, tc.after, tc.secrets); got != tc.want {
			t.Errorf("after %d, secrets %v: %s", tc.after, tc.secrets, got)
		}
	}
	j.withheld = withheldDecoy
	for _, secrets := range []bool{false, true} {
		if got := view(j, 0, secrets); got != fmt.Sprintf("[%q %q %q] %s", withheldDecoy, withheldDecoy, withheldDecoy, withheldDecoy) {
			t.Errorf("with a decoy, secrets %v: %s", secrets, got)
		}
	}
	plain := &job{}
	fmt.Fprintln(plain, named)
	for range api.MaxJobLines {
		fmt.Fprintln(plain, "x")
	}
	plain.end(errors.New("secret GONE_061 not found on the host")) // not a *vm.SecretError
	if g := plain.from(0, false); g.Error != "secret GONE_061 not found on the host" || slices.Contains(g.Lines, withheldSecretLine) {
		t.Errorf("the named line dropped: %q, %s", g.Lines[:min(2, len(g.Lines))], g.Error)
	}
}
