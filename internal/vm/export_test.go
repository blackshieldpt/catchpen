package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// A pull request's description is BodyFile's, else Body, else a line
// naming the VM and branch.
func TestPRBody(t *testing.T) {
	file := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(file, []byte("From the file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		o    PROpts
		want string
	}{
		{PROpts{BodyFile: file}, "From the file"},
		{PROpts{Body: "From the API"}, "From the API"},
		{PROpts{BodyFile: file, Body: "From the API"}, "From the file"},
		{PROpts{}, "Opened by catchpen from VM `dev` (branch `main`)."},
	} {
		if got, err := c.o.body("dev", "main"); err != nil || got != c.want {
			t.Errorf("%+v: %q, %v", c.o, got, err)
		}
	}
	if _, err := (PROpts{BodyFile: file + ".gone"}).body("dev", "main"); !os.IsNotExist(err) {
		t.Errorf("a missing file: %v", err)
	}
}
