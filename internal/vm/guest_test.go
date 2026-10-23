package vm

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Guest connections come from DialGuest or waitGuest, which close them when
// a canary hit pauses the VM; a raw Dial would hang on a paused guest.
func TestGuestConnectionsGuarded(t *testing.T) {
	raw := regexp.MustCompile(`\.keys\([^)]*\)\.(Dial|WaitReady)\(`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "guest.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(b), "\n") {
			if raw.MatchString(l) {
				t.Errorf("%s:%d dials a guest directly; use DialGuest or waitGuest", f, i+1)
			}
		}
	}
}
