package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidDecision(t *testing.T) {
	for _, d := range []string{DecisionAllowOnce, DecisionAllowAlways, DecisionDeny, DecisionDenyAlways} {
		if !ValidDecision(d) {
			t.Errorf("ValidDecision(%q) = false, want true", d)
		}
	}
	for _, d := range []string{"", "allow", "ALLOW-ONCE", "allow-always ", "deny-maybe"} {
		if ValidDecision(d) {
			t.Errorf("ValidDecision(%q) = true, want false", d)
		}
	}
}

func TestPromptJSON(t *testing.T) {
	ask := Prompt{Type: PromptAsk, ID: "0123456789abcdef", VM: "v", Host: "example.com", Port: 443, Method: "CONNECT"}
	b, err := json.Marshal(ask)
	if err != nil {
		t.Fatal(err)
	}
	var got Prompt
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != ask {
		t.Errorf("ask round trip = %+v, want %+v", got, ask)
	}

	done := Prompt{Type: PromptDone, ID: "0123456789abcdef", Error: &Error{Code: CodeConflict, Message: "the prompt expired"}}
	b, err = json.Marshal(done)
	if err != nil {
		t.Fatal(err)
	}
	var gotDone Prompt
	if err := json.Unmarshal(b, &gotDone); err != nil {
		t.Fatal(err)
	}
	if gotDone.Type != PromptDone || gotDone.ID != done.ID || gotDone.Error == nil ||
		gotDone.Error.Code != CodeConflict || gotDone.Error.Message != "the prompt expired" {
		t.Errorf("done round trip = %+v, want %+v", gotDone, done)
	}

	b, err = json.Marshal(Prompt{})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, `"error"`) || strings.Contains(s, `"decision"`) {
		t.Errorf("empty Prompt marshals as %s, want no error or decision", s)
	}
}
