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

// The API's egress log follow: decoded entries, egress.log.1 first, then
// across the next rotation. A line that isn't an entry is skipped, while
// TailLog prints it as it is, made printable.
func TestTailEntriesAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LogFile)
	add := func(host string) {
		t.Helper()
		if err := Append(dir, LogEntry{Time: time.Unix(0, 0), VM: "vm1", Method: "CONNECT", Host: host, Port: 443, Action: ActionAllow}); err != nil {
			t.Fatal(err)
		}
	}
	rotate := func() {
		t.Helper()
		if err := os.Rename(path, path+".1"); err != nil {
			t.Fatal(err)
		}
	}
	add("a.example")
	rotate()
	add("b.example")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not json \x1b[2J\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	add("c.example")

	var text bytes.Buffer
	if err := TailLog(context.Background(), path, false, &text); err != nil {
		t.Fatal(err)
	}
	if want := "1970-01-01T00:00:00Z ALLOW vm1  CONNECT b.example:443\nnot json [2J\n"; !strings.Contains(text.String(), want) {
		t.Errorf("TailLog:\n%s\nwant it to hold:\n%s", text.String(), want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan LogEntry, 10)
	done := make(chan error, 1)
	go func() {
		done <- TailEntries(ctx, path, true, func(e LogEntry) error { got <- e; return nil })
	}()
	next := func(host string) {
		t.Helper()
		select {
		case e := <-got:
			if e.Host != host || e.Port != 443 || e.VM != "vm1" || e.Action != ActionAllow || !e.Time.Equal(time.Unix(0, 0)) {
				t.Fatalf("got %+v, want %s", e, host)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %s", host)
		}
	}
	next("a.example")
	next("b.example")
	next("c.example")
	rotate()
	add("d.example")
	next("d.example")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		t.Errorf("extra entry %+v", e)
	default:
	}
}
