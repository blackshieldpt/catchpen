package audit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pageLog is a log whose events, named e1 to e7, rotate in the middle, share
// a time (e3 to e5) and are out of time order (e6). Newest first they are
// e7, e5, e4, e3, e2, e1, e6.
func pageLog(t *testing.T) Log {
	t.Helper()
	l := Log{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	at := func(s int) time.Time { return time.Date(2026, 10, 1, 12, 0, s, 0, time.UTC) }
	line := func(name, vm string, s int) string {
		b, err := json.Marshal(Event{Time: at(s), Event: name, VM: vm})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	for path, lines := range map[string][]string{
		l.Path + ".1": {line("e1", "a", 1), line("e2", "b", 2)},
		l.Path:        {"not json\n", line("e3", "a", 3), line("e4", "b", 3), line("e5", "a", 3), line("e6", "a", 0), line("e7", "a", 4)},
	} {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// pages walks every page of limit events, for their names, page by page.
func pages(t *testing.T, l Log, vm string, limit int) [][]string {
	t.Helper()
	var out [][]string
	var before *Cursor
	for range 20 {
		es, next, err := l.Page(context.Background(), vm, before, limit)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range es {
			names = append(names, e.Event)
		}
		out = append(out, names)
		if next == nil {
			return out
		}
		before = next
	}
	t.Fatalf("limit %d: no last page: %v", limit, out)
	return nil
}

func TestPage(t *testing.T) {
	l := pageLog(t)
	for _, c := range []struct {
		vm    string
		limit int
		want  string // the pages, | between them
	}{
		{"", 1, "e7|e5|e4|e3|e2|e1|e6"},
		{"", 2, "e7 e5|e4 e3|e2 e1|e6"},
		{"", 3, "e7 e5 e4|e3 e2 e1|e6"},
		{"", 7, "e7 e5 e4 e3 e2 e1 e6"},
		{"", 100, "e7 e5 e4 e3 e2 e1 e6"},
		{"a", 2, "e7 e5|e3 e1|e6"},
		{"b", 1, "e4|e2"},
	} {
		var got []string
		for _, p := range pages(t, l, c.vm, c.limit) {
			got = append(got, strings.Join(p, " "))
		}
		if g := strings.Join(got, "|"); g != c.want {
			t.Errorf("vm %q, limit %d: %s, want %s", c.vm, c.limit, g, c.want)
		}
	}
}

// A page's end at a time other events share: the next page starts with the
// rest of them.
func TestPageCursor(t *testing.T) {
	l := pageLog(t)
	es, next, err := l.Page(context.Background(), "", nil, 2)
	if err != nil || len(es) != 2 || next == nil || !next.Time.Equal(es[1].Time) || next.Skip != 1 {
		t.Fatalf("first page: %v, %+v, %v", es, next, err)
	}
	es, next, err = l.Page(context.Background(), "", next, 2)
	if err != nil || len(es) != 2 || es[0].Event != "e4" || next == nil || next.Skip != 3 {
		t.Fatalf("second page: %v, %+v, %v", es, next, err)
	}
}

// A ctx done while the log is read is an error, not the events read so far
// (TailEvents stops after the rotated-out file without one).
func TestPageCancelled(t *testing.T) {
	l := pageLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if es, next, err := l.Page(ctx, "", nil, 2); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, %+v, %v, want context.Canceled", es, next, err)
	}
}

// A limit below 1 is 1, and a negative skip none: Page doesn't panic.
func TestPageBadArgs(t *testing.T) {
	l := pageLog(t)
	if es, next, err := l.Page(context.Background(), "", nil, 0); err != nil || len(es) != 1 || es[0].Event != "e7" || next == nil {
		t.Errorf("limit 0: %v, %+v, %v", es, next, err)
	}
	at3 := &Cursor{Time: time.Date(2026, 10, 1, 12, 0, 3, 0, time.UTC), Skip: -1}
	es, next, err := l.Page(context.Background(), "", at3, 10)
	var names []string
	for _, e := range es {
		names = append(names, e.Event)
	}
	if err != nil || strings.Join(names, " ") != "e5 e4 e3 e2 e1 e6" || next != nil {
		t.Errorf("skip -1: %v, %+v, %v", names, next, err)
	}
}

func TestPageEmpty(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	es, next, err := l.Page(context.Background(), "", nil, 10)
	if err != nil || es == nil || len(es) != 0 || next != nil {
		t.Errorf("no log: %v, %+v, %v", es, next, err)
	}
}
