package server

import (
	"context"
	"net/http"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// eventsPoll is how often the feed compares the VMs' states (a var: tests
// shorten it).
var eventsPoll = 2 * time.Second

// eventsQueue bounds the events a slow client can have waiting: past it, the
// newest is dropped, not the poller or the audit tail stalled.
const eventsQueue = 64

// eventKinds are the audit events the feed carries: the rest have their own
// follow (GET /v1/audit/follow).
var eventKinds = map[string]bool{audit.CanaryHit: true, audit.TaskEnd: true}

// events is GET /v1/events: a snapshot of the VMs' states, then their changes
// and the audit events above, as NDJSON. A slow client drops events; the
// audit read erroring leaves the VM part running.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	after, err := followAfter(r)
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	write, end := ndjson(w)
	ch := make(chan api.Event, eventsQueue)
	send := func(e api.Event) { // never blocks: a full queue drops the event
		select {
		case ch <- e:
		default:
		}
	}

	prev := map[string]string{}
	baseline := false // the snapshot is the baseline; a tick never invents one by itself
	list := func() (map[string]string, error) {
		vms, err := m.List()
		if err != nil {
			return nil, err
		}
		st := m.Statuses(vms)
		out := make(map[string]string, len(vms))
		for _, v := range vms {
			out[v.Name] = st[v.Name].State
		}
		return out, nil
	}
	// The snapshot, and the baseline the changes compare against. It is
	// written before the goroutines start, so a slow client cannot stall them;
	// a list error ends the stream with an error line rather than leaving the
	// client with no baseline.
	cur, err := list()
	if err != nil {
		end(&api.Error{Code: api.CodeInternal, Message: "the VMs can't be read"})
		return
	}
	for name, state := range cur {
		if !write(api.Event{Kind: api.EventVM, Time: time.Now(), VM: name, State: state}) {
			return
		}
		prev[name] = state
	}
	baseline = true
	// The audit part: the two kinds at or after after, shaped per the request.
	go func() {
		err := m.Audit().TailEvents(ctx, "", true, func(e audit.Event) error {
			if !eventKinds[e.Event] || e.Time.Before(after) {
				return nil
			}
			ev := shownEvent(r, e)
			send(api.Event{Kind: ev.Event, Time: ev.Time, VM: ev.VM, Detail: ev.Detail})
			return nil
		})
		if err != nil && ctx.Err() == nil && s.Logf != nil {
			s.Logf("events: the audit log: %v", err)
		}
	}()
	// The VM part: its own goroutine, so a slow client's write never stalls the
	// poller; it drops into ch when the client is behind.
	// The interval is read here, before the goroutine: a test that changes
	// eventsPoll after this request must not race with a goroutine of a
	// finished one.
	poll := eventsPoll
	go func() {
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cur, err := list()
			if err != nil {
				continue // the next poll may read it
			}
			if !baseline { // only without the snapshot (which errors out above)
				prev, baseline = cur, true
				continue
			}
			now := time.Now()
			for name, state := range cur {
				if prev[name] == state {
					continue
				}
				send(api.Event{Kind: api.EventVM, Time: now, VM: name, State: state})
			}
			for name := range prev {
				if _, ok := cur[name]; ok {
					continue
				}
				send(api.Event{Kind: api.EventVM, Time: now, VM: name, State: api.EventStateGone})
			}
			prev = cur
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			if !write(ev) {
				cancel()
				return
			}
		}
	}
}
