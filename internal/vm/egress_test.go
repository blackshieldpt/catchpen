package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/notify"
)

// egressVM writes a stopped VM named name with egress mode mode.
func egressVM(t *testing.T, m *Manager, name, mode string) {
	t.Helper()
	writeFile(t, filepath.Join(m.Dir(name), stateFile), `{"name":"`+name+`","egress":"`+mode+`"}`)
}

// auditEvents returns the events in m's audit log.
func auditEvents(t *testing.T, m *Manager) []audit.Event {
	t.Helper()
	b, err := os.ReadFile(m.P.Audit())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []audit.Event
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var e audit.Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("audit line %q: %v", l, err)
		}
		out = append(out, e)
	}
	return out
}

func readList(t *testing.T, m *Manager, name, list string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(m.Dir(name), list))
	return string(b)
}

// Each list edit changes the list and is audited, one record per entry.
func TestEditEgressList(t *testing.T) {
	m := testManager(t)
	egressVM(t, m, "e1", egress.ModeAllowlist)
	if err := m.EditEgressList("e1", egress.AllowlistFile, true, []string{"pypi.org", "example.com:8443"}); err != nil {
		t.Fatal(err)
	}
	if err := m.EditEgressList("e1", egress.AllowlistFile, false, []string{"pypi.org"}); err != nil {
		t.Fatal(err)
	}
	if err := m.EditEgressList("e1", egress.DenylistFile, true, []string{"evil.example.com", "evil2.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := m.EditEgressList("e1", egress.DenylistFile, false, []string{"evil2.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := readList(t, m, "e1", egress.AllowlistFile); got != "example.com:8443\n" {
		t.Errorf("allowlist %q", got)
	}
	if got := readList(t, m, "e1", egress.DenylistFile); got != "evil.example.com\n" {
		t.Errorf("denylist %q", got)
	}
	want := []string{"egress.allow e1 pypi.org true", "egress.allow e1 example.com:8443 true", "egress.allow e1 pypi.org false",
		"egress.deny e1 evil.example.com true", "egress.deny e1 evil2.example.com true", "egress.deny e1 evil2.example.com false"}
	ev := auditEvents(t, m)
	if len(ev) != len(want) {
		t.Fatalf("got %d audit events, want %d: %v", len(ev), len(want), ev)
	}
	for i, e := range ev {
		if got := strings.Join([]string{e.Event, e.VM, toString(e.Detail["entry"]), toString(e.Detail["add"])}, " "); got != want[i] {
			t.Errorf("audit %d: %q, want %q", i, got, want[i])
		}
	}
}

// An invalid entry stops the edit; the entries before it stay changed and
// audited, and nothing after it is applied.
func TestEditEgressListStopsAtBadEntry(t *testing.T) {
	m := testManager(t)
	egressVM(t, m, "e1", egress.ModeAllowlist)
	err := m.EditEgressList("e1", egress.AllowlistFile, true, []string{"a.example.com", "bad host", "b.example.com"})
	if err == nil {
		t.Fatal("bad entry accepted")
	}
	if got := readList(t, m, "e1", egress.AllowlistFile); got != "a.example.com\n" {
		t.Errorf("allowlist %q", got)
	}
	if ev := auditEvents(t, m); len(ev) != 1 || ev[0].Detail["entry"] != "a.example.com" {
		t.Errorf("audit %v", ev)
	}
}

// Lists exist only in allowlist mode, and only the two lists can be edited.
func TestEditEgressListRefuses(t *testing.T) {
	m := testManager(t)
	egressVM(t, m, "open1", egress.ModeOpen)
	egressVM(t, m, "e1", egress.ModeAllowlist)
	if err := m.EditEgressList("open1", egress.AllowlistFile, true, []string{"pypi.org"}); err == nil || !strings.Contains(err.Error(), `uses egress mode "open"; lists only apply to allowlist mode`) {
		t.Errorf("open VM: %v", err)
	}
	state, _ := os.ReadFile(filepath.Join(m.Dir("e1"), stateFile))
	for _, list := range []string{"../../escape.txt", stateFile, egress.ConfigFile} {
		if err := m.EditEgressList("e1", list, true, []string{"pypi.org"}); err == nil || !strings.Contains(err.Error(), "unknown egress list") {
			t.Errorf("other file %s: %v", list, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(m.Dir("e1"), stateFile)); string(b) != string(state) {
		t.Errorf("vm.json changed: %q", b)
	}
	for _, f := range []string{filepath.Join(m.Dir("e1"), egress.ConfigFile), filepath.Join(m.P.Data, "escape.txt"), filepath.Join(m.Dir("open1"), egress.AllowlistFile)} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s written: %v", f, err)
		}
	}
	if ev := auditEvents(t, m); len(ev) != 0 {
		t.Errorf("refused edits audited: %v", ev)
	}
}

func ptr[T any](v T) *T { return &v }

// A config change writes egress.json and audits the old and new value of
// each setting that changed; a call that changes nothing is not audited.
func TestSetEgressConfig(t *testing.T) {
	m := testManager(t)
	egressVM(t, m, "e1", egress.ModeAllowlist)
	c, err := m.SetEgressConfig("e1", EgressSettings{OnDeny: ptr(egress.OnDenyPrompt), PromptTimeout: ptr(45 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if c.OnDeny != egress.OnDenyPrompt || c.PromptTimeout != 45*time.Second || c.AllowPrivate {
		t.Errorf("returned %+v", c)
	}
	if got, _ := egress.ReadConfig(m.Dir("e1")); got != c {
		t.Errorf("egress.json has %+v, want %+v", got, c)
	}
	if _, err := m.SetEgressConfig("e1", EgressSettings{OnDeny: ptr(egress.OnDenyPrompt), AllowPrivate: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetEgressConfig("e1", EgressSettings{AllowPrivate: ptr(true)}); err != nil { // no change
		t.Fatal(err)
	}
	ev := auditEvents(t, m)
	want := []map[string]any{
		{"on_deny": "prompt", "on_deny_was": "log", "prompt_timeout": "45s", "prompt_timeout_was": "30s"},
		{"allow_private": true, "allow_private_was": false},
	}
	if len(ev) != len(want) {
		t.Fatalf("got %d audit events, want %d: %v", len(ev), len(want), ev)
	}
	for i, e := range ev {
		if e.Event != "egress.config" || e.VM != "e1" || !sameDetail(e.Detail, want[i]) {
			t.Errorf("audit %d: %s %s %v, want egress.config e1 %v", i, e.Event, e.VM, e.Detail, want[i])
		}
	}
}

// Settings egress config doesn't know, such as a base build's allow_all, are kept.
func TestSetEgressConfigKeepsOtherSettings(t *testing.T) {
	m := testManager(t)
	egressVM(t, m, "e1", egress.ModeAllowlist)
	if err := egress.WriteConfig(m.Dir("e1"), egress.Config{AllowAll: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetEgressConfig("e1", EgressSettings{AllowPrivate: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if c, _ := egress.ReadConfig(m.Dir("e1")); !c.AllowAll || !c.AllowPrivate {
		t.Errorf("egress.json has %+v", c)
	}
}

// Concurrent changes to different settings all land, and the audit log's
// old values chain: each record's old value is the previous record's new one.
func TestSetEgressConfigConcurrent(t *testing.T) {
	m := testManager(t)
	egressVM(t, m, "e1", egress.ModeAllowlist)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if _, err := m.SetEgressConfig("e1", EgressSettings{PromptTimeout: ptr(time.Duration(i+1) * time.Second)}); err != nil {
				t.Error(err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := m.SetEgressConfig("e1", EgressSettings{AllowPrivate: ptr(i%2 == 0)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	c, _ := egress.ReadConfig(m.Dir("e1"))
	timeout, private := "30s", "false"
	for _, e := range auditEvents(t, m) {
		if v, ok := e.Detail["prompt_timeout"]; ok {
			if was := fmt.Sprint(e.Detail["prompt_timeout_was"]); was != timeout {
				t.Fatalf("a record says prompt_timeout was %s, but it was %s", was, timeout)
			}
			timeout = fmt.Sprint(v)
		}
		if v, ok := e.Detail["allow_private"]; ok {
			if was := fmt.Sprint(e.Detail["allow_private_was"]); was != private {
				t.Fatalf("a record says allow_private was %s, but it was %s", was, private)
			}
			private = fmt.Sprint(v)
		}
	}
	if timeout != c.PromptTimeout.String() || private != fmt.Sprint(c.AllowPrivate) {
		t.Errorf("egress.json has %s/%v, the audit log ends at %s/%s", c.PromptTimeout, c.AllowPrivate, timeout, private)
	}
}

// The lock is egress.json.lock, a file lock that other processes see, and it
// is held until the audit record is written.
func TestSetEgressConfigHoldsFileLock(t *testing.T) {
	m := testManager(t)
	egressVM(t, m, "e1", egress.ModeAllowlist)
	lock := filepath.Join(m.Dir("e1"), egress.ConfigFile+".lock")
	unlock, err := fsutil.Lock(lock) // another holder: flock conflicts between open files, even in one process
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.SetEgressConfig("e1", EgressSettings{OnDeny: ptr(egress.OnDenyPrompt)})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("SetEgressConfig didn't wait for egress.json.lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// Hold the audit log's lock, let the call through, and wait for its write:
	// it then waits to record, and must still hold egress.json.lock.
	unlockAudit, err := fsutil.Lock(m.P.Audit() + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	for i := 0; ; i++ {
		if c, _ := egress.ReadConfig(m.Dir("e1")); c.OnDeny == egress.OnDenyPrompt {
			break
		}
		if i == 500 {
			t.Fatal("egress.json never changed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if u, ok, err := fsutil.TryLock(lock); err != nil || ok {
		if ok {
			u()
		}
		t.Errorf("egress.json.lock free before the audit record (ok %v, err %v)", ok, err)
	}
	unlockAudit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ev := auditEvents(t, m); len(ev) != 1 || ev[0].Event != "egress.config" {
		t.Errorf("audit %v", ev)
	}
}

// Invalid settings and VMs without allowlist egress are refused, and change nothing.
func TestSetEgressConfigRefuses(t *testing.T) {
	m := testManager(t)
	egressVM(t, m, "e1", egress.ModeAllowlist)
	egressVM(t, m, "none1", egress.ModeNone)
	for _, c := range []struct {
		name string
		set  EgressSettings
		want string
	}{
		{"e1", EgressSettings{OnDeny: ptr("")}, `on_deny must be "log" or "prompt", got ""`},
		{"e1", EgressSettings{OnDeny: ptr("ask")}, `on_deny must be "log" or "prompt", got "ask"`},
		{"e1", EgressSettings{PromptTimeout: ptr(time.Duration(0))}, "prompt timeout must be positive, got 0s"},
		{"e1", EgressSettings{PromptTimeout: ptr(-time.Second)}, "prompt timeout must be positive, got -1s"},
		{"none1", EgressSettings{AllowPrivate: ptr(true)}, `VM none1 uses egress mode "none"`},
	} {
		_, err := m.SetEgressConfig(c.name, c.set)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s %+v: %v, want %q", c.name, c.set, err, c.want)
		}
		// The CLI turns these two back into its flag messages.
		if (c.set.OnDeny != nil) != errors.Is(err, ErrOnDeny) || (c.set.PromptTimeout != nil) != errors.Is(err, ErrPromptTimeout) {
			t.Errorf("%s %+v: %v doesn't wrap the right error", c.name, c.set, err)
		}
	}
	if ents, _ := os.ReadDir(m.Dir("e1")); len(ents) != 1 || ents[0].Name() != stateFile {
		t.Errorf("refused changes left files: %v", ents)
	}
	if ev := auditEvents(t, m); len(ev) != 0 {
		t.Errorf("refused changes audited: %v", ev)
	}
}

// A prompt is announced through the notifier while the person is asked, and
// every answer a person took time over is audited, allowing or not; not a
// decider that answers at once, and not one whose prompt ended with ctx.
func TestPromptDecider(t *testing.T) {
	m := testManager(t)
	posted := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		posted <- string(b)
	}))
	defer srv.Close()
	m.Cfg.Notify = notify.Config{Sinks: []notify.Sink{{Webhook: &notify.Webhook{URL: srv.URL}}}}
	req := egress.Request{VM: "e1", Host: "pypi.org", Method: "CONNECT", Port: 443}

	// A person who reads the notification, then answers.
	person := func(d egress.Decision) egress.Decider {
		return func(ctx context.Context, r egress.Request) egress.Decision {
			select {
			case b := <-posted:
				if !strings.Contains(b, "egress.prompt") || !strings.Contains(b, "CONNECT pypi.org:443 is waiting for an answer") {
					t.Errorf("notification %s", b)
				}
			case <-time.After(10 * time.Second):
				t.Error("no notification while asking")
			}
			time.Sleep(400 * time.Millisecond)
			return d
		}
	}
	for _, d := range []egress.Decision{egress.DecisionAllowOnce, egress.DecisionDeny} {
		if got := m.PromptDecider(person(d))(context.Background(), req); got != d {
			t.Errorf("decision %q, want %q", got, d)
		}
	}
	fast := m.PromptDecider(func(ctx context.Context, r egress.Request) egress.Decision { return egress.DecisionAllowOnce })
	fast(context.Background(), req)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	timedOut := m.PromptDecider(func(ctx context.Context, r egress.Request) egress.Decision {
		<-ctx.Done()
		return egress.DecisionAllowOnce
	})
	timedOut(ctx, req)

	ev := auditEvents(t, m)
	if len(ev) != 2 {
		t.Fatalf("got %d audit events, want 2: %v", len(ev), ev)
	}
	for i, d := range []string{"allow-once", "deny"} {
		want := map[string]any{"host": "pypi.org", "port": float64(443), "method": "CONNECT", "decision": d}
		if e := ev[i]; e.Event != "egress.prompt" || e.VM != "e1" || !sameDetail(e.Detail, want) {
			t.Errorf("audit %d: %s %s %v, want egress.prompt e1 %v", i, e.Event, e.VM, e.Detail, want)
		}
	}
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func sameDetail(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
