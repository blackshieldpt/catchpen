//go:build integration

package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// auditEvents are the audit events named event, whatever their VM.
func auditEvents(t *testing.T, p paths.Paths, event string) []audit.Event {
	t.Helper()
	var out []audit.Event
	err := audit.Log{Path: p.Audit()}.TailEvents(context.Background(), "", false, func(e audit.Event) error {
		if e.Event == event {
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestStreams drives a real VM through the API: a terminal runs a command,
// resizes and is detached, its session is recorded and audited as web-ssh,
// and the serial console is one per VM and ends when a canary hit pauses it.
func TestStreams(t *testing.T) {
	p, bin := vmtest.Home(t)
	vmtest.Template(t, p.Templates(), "st", "", "")
	mgr := func() (*vm.Manager, error) {
		return &vm.Manager{P: p, Cfg: config.Default(), Tpl: template.Loader{UserDir: p.Templates()}, Bin: bin}, nil
	}
	t.Cleanup(func() {
		if m, _ := mgr(); m.Destroy("st") == nil {
			t.Log("destroyed st after the test")
		}
	})
	h := home(t, mgr)
	c := h.client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	wait := func(j *api.JobInfo, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		end, err := c.WaitJob(ctx, j.ID, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		if end.State != api.JobDone {
			t.Fatalf("%s failed: %s", end.Kind, end.Error)
		}
	}
	wait(c.CreateVM(ctx, api.CreateVMRequest{Name: "st", Template: "st"}))
	wait(c.StartVM(ctx, "st"))
	if v, err := c.VM(ctx, "st"); err != nil || v.State != api.VMRunning {
		t.Fatalf("st: %+v %v, want running", v, err)
	}
	m, err := mgr()
	if err != nil {
		t.Fatal(err)
	}

	term, err := c.Terminal(ctx, "st", apiclient.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The quotes keep the typed command from containing the output.
	if _, err := term.Write([]byte(`echo STREAM-0""66` + "\r")); err != nil {
		t.Fatal(err)
	}
	if got, err := drain(t, term, 5*time.Minute, "STREAM-066"); err != nil {
		t.Fatalf("shell: read %q, %v", got, err)
	}
	if err := term.Resize(100, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	if got, err := drain(t, term, time.Minute, "40 100"); err != nil {
		t.Fatalf("resize: read %q, %v", got, err)
	}
	// Leave the guest silent: a shell that writes after Close ends the
	// session through its output, which hides the detach path.
	if _, err := term.Write([]byte("sleep 30\r")); err != nil {
		t.Fatal(err)
	}
	if got, err := drain(t, term, time.Minute, "sleep 30"); err != nil {
		t.Fatalf("sleep: read %q, %v", got, err)
	}
	time.Sleep(200 * time.Millisecond)
	closed := time.Now()
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	var end *audit.Event
	for end == nil && time.Since(closed) < 10*time.Second {
		for _, e := range auditEvents(t, p, audit.SessionEnd) {
			if e.VM == "st" && e.Detail["kind"] == "web-ssh" {
				end = &e
			}
		}
		if end == nil {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if end == nil {
		t.Fatal("no session.end for st's web-ssh session within 10s of Close")
	}
	if e := *end; e.Detail["client"] != api.ClientLocal {
		t.Errorf("session.end: %+v, want client %s", e, api.ClientLocal)
	}
	var started bool
	for _, e := range auditEvents(t, p, audit.SessionStart) {
		if e.VM == "st" && e.Detail["kind"] == "web-ssh" && e.Detail["client"] == api.ClientLocal &&
			strings.Contains(fmt.Sprint(e.Detail["recording"]), "-web-ssh-") {
			started = true
		}
	}
	if !started {
		t.Error("no session.start for st with kind web-ssh, client @local and a web-ssh recording")
	}
	ids, err := m.Recordings("st")
	if err != nil || !slices.ContainsFunc(ids, func(id string) bool { return strings.Contains(id, "-web-ssh-") }) {
		t.Errorf("recordings %v %v, want a web-ssh one", ids, err)
	}

	con, err := c.Console(ctx, "st")
	if err != nil {
		t.Fatal(err)
	}
	defer con.Close()
	_, err = c.Console(ctx, "st")
	refused(t, "second console", err, api.CodeConflict, "another console stream is open")
	pausedFile := filepath.Join(m.Dir("st"), "paused.json")
	write(t, pausedFile, "{}")
	defer os.Remove(pausedFile)
	if _, err := drain(t, con, 10*time.Second, ""); err != io.EOF {
		t.Errorf("console after a pause: read ended with %v, want io.EOF", err)
	}
	if e := con.End(); e == nil || e.Error == nil || e.Error.Code != api.CodeVMPaused {
		t.Errorf("console after a pause: end %+v, want %s", e, api.CodeVMPaused)
	}
}
