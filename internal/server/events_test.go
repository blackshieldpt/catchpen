package server

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// shortPoll makes the feed compare the VMs every 25 ms. Call it before home:
// its cleanup then runs after the server's own.
func shortPoll(t *testing.T) {
	t.Helper()
	old := eventsPoll
	eventsPoll = 25 * time.Millisecond
	t.Cleanup(func() { eventsPoll = old })
}

// eventsStream reads c's feed into a channel until ctx is done.
func eventsStream(ctx context.Context, c *apiclient.Client, after time.Time) <-chan api.Event {
	out := make(chan api.Event, 256)
	go func() {
		defer close(out)
		_ = c.Events(ctx, after, func(e api.Event) error {
			select {
			case out <- e:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
	}()
	return out
}

// nextEvent returns the next event on evs that match, or fails the test
// after d.
func nextEvent(t *testing.T, evs <-chan api.Event, d time.Duration, match func(api.Event) bool) api.Event {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case e, ok := <-evs:
			if !ok {
				t.Fatal("the feed ended")
			}
			if match(e) {
				return e
			}
		case <-timeout:
			t.Fatalf("no matching event within %v", d)
		}
	}
}

// noEvent fails the test if an event matching match arrives within d.
func noEvent(t *testing.T, evs <-chan api.Event, d time.Duration, match func(api.Event) bool) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case e, ok := <-evs:
			if !ok {
				return
			}
			if match(e) {
				t.Errorf("unexpected event %+v", e)
			}
		case <-timeout:
			return
		}
	}
}

func vmEvent(name string) func(api.Event) bool {
	return func(e api.Event) bool { return e.Kind == api.EventVM && e.VM == name }
}

func TestEventsSnapshot(t *testing.T) {
	shortPoll(t)
	h := serveVMs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs := eventsStream(ctx, h.client(t), time.Time{})
	want := map[string]string{"alpha": api.VMStopped, "beta": api.VMPaused, "gamma": api.VMRunning, "pool-1": api.VMStopped}
	got := map[string]string{}
	for range want {
		e := nextEvent(t, evs, 5*time.Second, func(e api.Event) bool { return e.Kind == api.EventVM })
		if _, dup := got[e.VM]; dup {
			t.Errorf("%s sent twice in the snapshot", e.VM)
		}
		got[e.VM] = e.State
	}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("%s: state %q, want %q (all: %v)", name, got[name], state, got)
		}
	}
	noEvent(t, evs, 4*eventsPoll, func(api.Event) bool { return true }) // an unchanged VM is not re-sent
}

func TestEventsStateChange(t *testing.T) {
	shortPoll(t)
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "seed", Template: "base", Created: created})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs := eventsStream(ctx, h.client(t), time.Time{})
	// The seed's snapshot event proves the snapshot was taken, so what follows
	// is a change, not part of the snapshot.
	nextEvent(t, evs, 5*time.Second, func(e api.Event) bool {
		return vmEvent("seed")(e) && e.State == api.VMStopped
	})
	fakeRunning(t, h, "new")
	nextEvent(t, evs, 5*time.Second, func(e api.Event) bool {
		return vmEvent("new")(e) && e.State == api.VMRunning
	})
	if err := os.RemoveAll(h.p.VM("new")); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, evs, 5*time.Second, func(e api.Event) bool {
		return vmEvent("new")(e) && e.State == api.EventStateGone
	})
}

func TestEventsAudit(t *testing.T) {
	h := home(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs := eventsStream(ctx, h.client(t), time.Now().Add(-time.Minute))
	log := audit.Log{Path: h.p.Audit()}
	for _, rec := range []struct {
		event, vm string
		detail    map[string]any
	}{
		{audit.CanaryHit, "beta", map[string]any{"path": "/canary"}},
		{audit.TaskEnd, "alpha", map[string]any{"task": "0123abcd"}},
		{audit.VMStart, "gamma", nil},
	} {
		if err := log.Record(rec.event, rec.vm, rec.detail); err != nil {
			t.Fatal(err)
		}
	}
	e := nextEvent(t, evs, 5*time.Second, func(e api.Event) bool { return e.Kind == audit.CanaryHit })
	if e.VM != "beta" || e.Detail["path"] != "/canary" {
		t.Errorf("canary.hit: %+v", e)
	}
	e = nextEvent(t, evs, 5*time.Second, func(e api.Event) bool { return e.Kind == audit.TaskEnd })
	if e.VM != "alpha" || e.Detail["task"] != "0123abcd" {
		t.Errorf("task.end: %+v", e)
	}
	noEvent(t, evs, time.Second, func(e api.Event) bool { return e.Kind == audit.VMStart })
}

func TestEventsAfter(t *testing.T) {
	h := home(t, nil)
	log := audit.Log{Path: h.p.Audit()}
	if err := log.Record(audit.TaskEnd, "alpha", map[string]any{"task": "old"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	after := time.Now()
	evs := eventsStream(ctx, h.client(t), after)
	noEvent(t, evs, time.Second, func(e api.Event) bool { return e.Kind == audit.TaskEnd })
	if err := log.Record(audit.TaskEnd, "alpha", map[string]any{"task": "new"}); err != nil {
		t.Fatal(err)
	}
	e := nextEvent(t, evs, 5*time.Second, func(e api.Event) bool { return e.Kind == audit.TaskEnd })
	if e.Detail["task"] != "new" {
		t.Errorf("task.end after the connection: %+v", e)
	}
	if st, body := h.raw(t, api.PathEvents+"?after=yesterday"); st != http.StatusBadRequest {
		t.Errorf("bad after: %d %s, want 400", st, body)
	}
}
