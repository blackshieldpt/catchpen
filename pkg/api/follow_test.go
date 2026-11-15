package api

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestFollowPaths(t *testing.T) {
	if PathAuditFollow != "/v1/audit/follow" {
		t.Errorf("PathAuditFollow = %q", PathAuditFollow)
	}
	if got := PathEgressLog("a"); got != "/v1/vms/a/egress/log" {
		t.Errorf("PathEgressLog = %q", got)
	}
	if got := PathEgressLogFollow("a"); got != "/v1/vms/a/egress/log/follow" {
		t.Errorf("PathEgressLogFollow = %q", got)
	}
	if DefaultEgressLogLimit <= 0 || DefaultEgressLogLimit > MaxEgressLogLimit {
		t.Errorf("limits: default %d, max %d", DefaultEgressLogLimit, MaxEgressLogLimit)
	}
}

func TestEgressLogPageJSON(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	one := EgressLogPage{Entries: []EgressLogEntry{{Time: at, VM: "a", Method: "CONNECT", Host: "h.test", Port: 443, Action: "deny"}}, Next: "x~1"}
	b, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}
	var back EgressLogPage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one, back) {
		t.Errorf("round trip: %+v, want %+v", back, one)
	}

	empty := EgressLogPage{Entries: make([]EgressLogEntry, 0)}
	b, err = json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"entries":[]}` {
		t.Errorf("empty page: %s", b)
	}
}
