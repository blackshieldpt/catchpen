package vm

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Guest connections come from DialGuest or waitGuest, which close them when
// a canary hit pauses the VM; a raw Dial would hang on a paused guest. The
// runner, outside this package, can't dial with a VM's keys at all.
func TestGuestConnectionsGuarded(t *testing.T) {
	raw := regexp.MustCompile(`\.keys\([^)]*\)\.(Dial|WaitReady)\(`)
	outside := regexp.MustCompile(`sshx\.Keys\{|\.(Dial|WaitReady)\(`)
	files, _ := filepath.Glob("*.go")
	runner, _ := filepath.Glob("../runner/*.go")
	for _, f := range append(files, runner...) {
		if strings.HasSuffix(f, "_test.go") || f == "guest.go" {
			continue
		}
		if strings.HasPrefix(f, "../") {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, l := range strings.Split(string(b), "\n") {
				if outside.MatchString(l) {
					t.Errorf("%s:%d dials a guest directly; use vm's DialGuest", f, i+1)
				}
			}
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
