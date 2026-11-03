package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/canary"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// A decoy in a job's lines (whole, split across two, or unended) or in its
// error (or split from the last line) withholds every line and the error,
// their count kept, and is acted on once; one in a line, at once, while the
// job's fn still runs. Decoys that can't be read withhold too, with no hit.
func TestJobDecoys(t *testing.T) {
	aws := &canary.Canary{Kind: "aws"}
	for _, tc := range []struct {
		name   string
		out    string
		err    error
		scan   error
		hit    bool // a hit
		early  bool // acted on before fn returns: fn waits for it
		lines  []string
		errMsg string
	}{
		{name: "clean", out: "one\ntwo\n", err: errors.New("boom"), lines: []string{"one", "two"}, errMsg: "boom"},
		{name: "line", out: "one\nsee DECOY here\ntwo\n", hit: true, early: true, lines: []string{withheldDecoy, withheldDecoy, withheldDecoy}},
		{name: "split", out: "one\nDE\nCOY\n", hit: true, early: true, lines: []string{withheldDecoy, withheldDecoy, withheldDecoy}},
		{name: "unended", out: "one\nDECOY", hit: true, lines: []string{withheldDecoy, withheldDecoy}},
		{name: "error", out: "one\n", err: errors.New("clone: DECOY"), hit: true, lines: []string{withheldDecoy}, errMsg: withheldDecoy},
		{name: "error split", out: "one\nDE\n", err: errors.New("COY"), hit: true, lines: []string{withheldDecoy, withheldDecoy}, errMsg: withheldDecoy},
		{name: "unread", out: "one\n", err: errors.New("boom"), scan: errors.New("bad canaries.json"), lines: []string{withheldUnread}, errMsg: withheldUnread},
	} {
		js := newJobs()
		hits := 0
		acted := make(chan struct{})
		w := &watch{
			scan: func(s string) (*canary.Canary, error) {
				if tc.scan != nil {
					return nil, tc.scan
				}
				if strings.Contains(s, "DECOY") {
					return aws, nil
				}
				return nil, nil
			},
			hit: func(c *canary.Canary) {
				if hits++; hits == 1 && c == aws {
					close(acted)
				}
			},
		}
		i, err := js.start("k", "a", "me", w, func(_ context.Context, out io.Writer) error {
			io.WriteString(out, tc.out)
			if tc.early {
				select {
				case <-acted:
				case <-time.After(5 * time.Second):
					return errors.New("not acted on while fn runs")
				}
			}
			return tc.err
		})
		if err != nil {
			t.Fatal(err)
		}
		js.stop() // waits for the job, and the hit, to end
		j, _ := js.find(i.ID)
		g := j.from(0)
		if !reflect.DeepEqual(g.Lines, tc.lines) || g.Written != len(tc.lines) || g.Error != tc.errMsg {
			t.Errorf("%s: %+v", tc.name, g)
		}
		if l := js.list(); l[0].Error != tc.errMsg {
			t.Errorf("%s: listed %+v", tc.name, l[0])
		}
		if want := map[bool]int{true: 1}[tc.hit]; hits != want {
			t.Errorf("%s: %d hits, want %d", tc.name, hits, want)
		}
	}
}

// A job on a VM with decoys gives clients none of them: those it has when
// the job starts (kept once its directory is gone, as after a destroy), or
// planted while it runs (as Create does). The hit is recorded, and audited
// with the job's client.
func TestJobDecoysWithheld(t *testing.T) {
	h := home(t, nil)
	c := h.client(t)
	wait := waiter(t, c)
	for _, tc := range []struct {
		name   string
		before bool // decoys planted before the job
		gone   bool // the VM's directory removed before any output, as by a destroy
		clean  bool // the job's text has another VM's decoy
	}{
		{name: "error", before: true},
		{name: "gone", before: true, gone: true},
		{name: "planted"},
		{name: "clean", before: true, clean: true},
	} {
		h.writeVM(t, vm.State{Name: tc.name, Template: "base", Created: created})
		dir := h.p.VM(tc.name)
		cs := canary.Generate([]string{"aws"})
		plant := func() {
			if err := canary.Save(dir, cs); err != nil {
				t.Fatal(err)
			}
		}
		if tc.before {
			plant()
		}
		v := cs[0].Values[0]
		if tc.clean {
			v = canary.Generate([]string{"aws"})[0].Values[0] // not the VM's
		}
		m, err := h.srv.Manager()
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.srv.startJob(rec, httptest.NewRequest("POST", "/", nil), m, api.JobVMStart, tc.name, func(_ context.Context, m *vm.Manager) error {
			if !tc.before {
				plant()
			}
			if tc.gone {
				os.RemoveAll(dir)
			}
			fmt.Fprintln(m.Out, "cloning", v)
			if tc.clean {
				return errors.New("boom")
			}
			return fmt.Errorf("clone o/r: exit status 128\nfatal: %s", v)
		})
		var info api.JobInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
			t.Fatalf("%s: %v %s", tc.name, err, rec.Body)
		}
		end, lines := wait(&info, nil)
		if tc.clean {
			if end.Error != "boom" || !reflect.DeepEqual(lines, []string{"cloning " + v}) {
				t.Errorf("%s: %+v %q", tc.name, end, lines)
			}
			continue
		}
		if end.State != api.JobFailed || end.Error != withheldDecoy || !reflect.DeepEqual(lines, []string{withheldDecoy}) {
			t.Errorf("%s: %+v %q", tc.name, end, lines)
		}
		for _, path := range []string{api.PathJobs, api.PathJob(info.ID)} {
			if st, body := h.raw(t, path); st != 200 || strings.Contains(body, v) {
				t.Errorf("%s: %s: %d %s", tc.name, path, st, body)
			}
		}
		action := map[bool]string{true: "none", false: "pause"}[tc.gone] // a destroyed VM: nothing to act on
		if e := hitEvent(t, h.p, tc.name); e.Detail["where"] != "in an API job's output" || e.Detail["client"] != api.ClientLocal || e.Detail["action"] != action {
			t.Errorf("%s: audit %+v", tc.name, e)
		}
	}
}

// hitEvent waits for vm's canary.hit audit event: the hit is acted on in a
// goroutine of its own.
func hitEvent(t *testing.T, p paths.Paths, vm string) audit.Event {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var hit *audit.Event
		_ = audit.Log{Path: p.Audit()}.TailEvents(context.Background(), vm, false, func(e audit.Event) error {
			if e.Event == audit.CanaryHit {
				hit = &e
			}
			return nil
		})
		if hit != nil {
			return *hit
		}
		if time.Now().After(end) {
			t.Fatalf("no canary.hit event for %s", vm)
		}
	}
}
