package api

import (
	"encoding/json"
	"testing"
)

func TestTaskOutputPaths(t *testing.T) {
	if got := PathTaskOutput("aaaaaaa1"); got != "/v1/tasks/aaaaaaa1/output" {
		t.Errorf("PathTaskOutput = %q", got)
	}
	if got, want := PathTaskOutput("a.b/c"), PathTask("a.b/c")+"/output"; got != want {
		t.Errorf("PathTaskOutput(escaped) = %q, want %q", got, want)
	}
	if !(DefaultTaskOutputLines > 0 && DefaultTaskOutputLines <= MaxTaskOutputLines) {
		t.Errorf("limits: default %d, max %d", DefaultTaskOutputLines, MaxTaskOutputLines)
	}
	b, err := json.Marshal(TaskOutputLine{N: 3, Line: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"n":3,"line":"hi"}` {
		t.Errorf("TaskOutputLine = %s", b)
	}
	var l TaskOutputLine
	if err := json.Unmarshal(b, &l); err != nil || l != (TaskOutputLine{N: 3, Line: "hi"}) {
		t.Errorf("round trip = %+v, %v", l, err)
	}
}
