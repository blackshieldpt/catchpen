package egress

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFormatEntryPrintable(t *testing.T) {
	b, _ := json.Marshal(LogEntry{Time: time.Unix(0, 0), VM: "vm1", Method: "GET", Host: "cdn\x1b]0;x\x07.\u202eexample", Port: 443,
		Action: ActionCache, Reason: "redirect /\x1b[2Jx\u009b\n2026-01-01T00:00:00Z ALLOW vm1"})
	got := formatEntry(string(b))
	if strings.ContainsAny(got, "\x1b\x07\u009b\u202e\n") || !strings.Contains(got, "cdn]0;x.example:443") {
		t.Errorf("%q", got)
	}
	if got := formatEntry("not json \x1b[2J"); got != "not json [2J" {
		t.Errorf("%q", got)
	}
}
