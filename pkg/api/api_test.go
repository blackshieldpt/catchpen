package api

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestStatus(t *testing.T) {
	for code, want := range map[string]int{
		CodeBadRequest: 400, CodeUnauthorized: 401, CodeForbidden: 403, CodeNotFound: 404,
		CodeConflict: 409, CodeVMPaused: 409, CodeConfirmRequired: 428, CodeTooLarge: 413,
		CodeRateLimited: 429, CodeInternal: 500, "no_such_code": 500,
	} {
		if got := Status(code); got != want {
			t.Errorf("Status(%q) = %d, want %d", code, got, want)
		}
	}
}

func TestErrorJSON(t *testing.T) {
	b, err := json.Marshal(ErrorResponse{Error: &Error{Code: CodeNotFound, Message: "no VM dev"}})
	if err != nil || string(b) != `{"error":{"code":"not_found","message":"no VM dev"}}` {
		t.Errorf("%s, %v", b, err)
	}
	if e := (&Error{Code: CodeNotFound, Message: "no VM dev"}).Error(); e != "not_found: no VM dev" {
		t.Errorf("%q", e)
	}
}

func TestListEncodesEmpty(t *testing.T) {
	b, _ := json.Marshal(struct {
		Items []*Error `json:"items"`
	}{List[*Error](nil)})
	if string(b) != `{"items":[]}` {
		t.Errorf("%s", b)
	}
	s := []int{1}
	if got := List(s); len(got) != 1 || &got[0] != &s[0] {
		t.Errorf("List changed a non-nil slice: %v", got)
	}
}

// The cockpit imports this package: it must not pull in anything but the
// standard library.
func TestStandardLibraryOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); len(got) != 1 || got[0] != "github.com/blackshieldpt/catchpen/pkg/api" {
		t.Errorf("non-standard dependencies: %q", got)
	}
}
