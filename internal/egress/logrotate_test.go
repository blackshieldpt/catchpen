package egress

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `catchpen egress log` shows the entries rotated out to egress.log.1 first.
func TestTailLogReadsRotatedFile(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, LogEntry{Time: time.Unix(0, 0), VM: "vm1", Method: "CONNECT", Host: "old.example", Port: 443, Action: ActionDeny}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, LogFile), filepath.Join(dir, LogFile+".1")); err != nil {
		t.Fatal(err)
	}
	if err := Append(dir, LogEntry{Time: time.Unix(1, 0), VM: "vm1", Method: "CONNECT", Host: "new.example", Port: 443, Action: ActionAllow}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := TailLog(context.Background(), filepath.Join(dir, LogFile), false, &out); err != nil {
		t.Fatal(err)
	}
	o, n := strings.Index(out.String(), "old.example"), strings.Index(out.String(), "new.example")
	if o < 0 || n < 0 || o > n {
		t.Errorf("want old.example then new.example:\n%s", out.String())
	}
}
