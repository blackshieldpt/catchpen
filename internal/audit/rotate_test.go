package audit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// `catchpen audit` shows the events rotated out to audit.jsonl.1 first.
func TestTailReadsRotatedFile(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	if err := l.Record(VMCreate, "oldvm", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(l.Path, l.Path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(VMStart, "newvm", nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := l.Tail(context.Background(), "", false, &out); err != nil {
		t.Fatal(err)
	}
	o, n := strings.Index(out.String(), "oldvm"), strings.Index(out.String(), "newvm")
	if o < 0 || n < 0 || o > n {
		t.Errorf("want oldvm then newvm:\n%s", out.String())
	}
	var one bytes.Buffer
	if err := l.Tail(context.Background(), "oldvm", false, &one); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(one.String(), "oldvm") || strings.Contains(one.String(), "newvm") {
		t.Errorf("filtered to oldvm:\n%s", one.String())
	}
}

// The API's audit follow: decoded events, audit.jsonl.1 first, then across
// the next rotation. Lines that aren't events are skipped, and so are other
// VMs' events when a VM is named.
func TestTailEventsAcrossRotation(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	record := func(event, vm string) {
		t.Helper()
		if err := l.Record(event, vm, map[string]any{"n": event}); err != nil {
			t.Fatal(err)
		}
	}
	rotate := func() {
		t.Helper()
		if err := os.Rename(l.Path, l.Path+".1"); err != nil {
			t.Fatal(err)
		}
	}
	record(VMCreate, "a")
	rotate()
	record(VMStart, "b")
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not an event\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	record(VMStop, "a")

	var all []string
	if err := l.TailEvents(context.Background(), "", false, func(e Event) error {
		all = append(all, e.Event+" "+e.VM)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"vm.create a", "vm.start b", "vm.stop a"}; !slices.Equal(all, want) {
		t.Errorf("all events: %q, want %q", all, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Event, 10)
	done := make(chan error, 1)
	go func() {
		done <- l.TailEvents(ctx, "a", true, func(e Event) error { got <- e; return nil })
	}()
	next := func(want string) {
		t.Helper()
		select {
		case e := <-got:
			if e.Event != want || e.VM != "a" || e.Detail["n"] != want {
				t.Fatalf("got %+v, want %s of a", e, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %s", want)
		}
	}
	next(VMCreate)
	next(VMStop)
	rotate()
	record(VMStart, "b")
	record(VMDestroy, "a")
	next(VMDestroy)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		t.Errorf("extra event %+v", e)
	default:
	}
}
