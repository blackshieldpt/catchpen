package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventsPaths(t *testing.T) {
	if PathEvents != "/v1/events" {
		t.Errorf("PathEvents = %q", PathEvents)
	}
	if EventVM == "" || EventStateGone == "" || EventVM == EventStateGone {
		t.Errorf("EventVM %q and EventStateGone %q must be non-empty and distinct", EventVM, EventStateGone)
	}
	for _, s := range []string{VMRunning, VMPaused, VMStopped} {
		if EventStateGone == s || EventVM == s {
			t.Errorf("EventStateGone or EventVM collides with the VM state %q", s)
		}
	}

	b, err := json.Marshal(Event{Kind: EventVM, Time: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), VM: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"state"`) || strings.Contains(string(b), `"detail"`) {
		t.Errorf("empty State and Detail must be omitted: %s", b)
	}
	var back Event
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Kind != EventVM || back.VM != "alpha" || back.State != "" || back.Detail != nil {
		t.Errorf("round trip: %+v", back)
	}
}
