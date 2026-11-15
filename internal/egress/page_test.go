package egress

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pageLog is a log whose entries, named by host (a1 to a2, then x to z, v
// and w), rotate in the middle, share a time (x to z) and are out of time
// order (w). Newest first they are v, z, y, x, a2, a1, w.
func pageLog(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, LogFile)
	at := func(s int) time.Time { return time.Date(2026, 10, 1, 12, 0, s, 0, time.UTC) }
	line := func(host string, s int) string {
		b, err := json.Marshal(LogEntry{Time: at(s), VM: "a", Method: "GET", Host: host, Port: 80, Action: ActionAllow})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	if err := os.WriteFile(path+".1", []byte(line("a1", 1)+line("a2", 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, e := range []LogEntry{
		{Time: at(3), VM: "a", Host: "x", Action: ActionAllow},
		{Time: at(3), VM: "a", Host: "y", Action: ActionAllow},
		{Time: at(3), VM: "a", Host: "z", Action: ActionAllow},
		{Time: at(0), VM: "a", Host: "w", Action: ActionAllow},
		{Time: at(4), VM: "a", Host: "v", Action: ActionAllow},
	} {
		if err := Append(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	return dir, path
}

// pages walks every page of limit entries, for their hosts, page by page.
func pages(t *testing.T, path string, limit int) []string {
	t.Helper()
	var out []string
	var before *Cursor
	for range 20 {
		es, next, err := Page(context.Background(), path, before, limit)
		if err != nil {
			t.Fatal(err)
		}
		var hosts []string
		for _, e := range es {
			hosts = append(hosts, e.Host)
		}
		out = append(out, strings.Join(hosts, " "))
		if next == nil {
			return out
		}
		before = next
	}
	t.Fatalf("limit %d: no last page: %v", limit, out)
	return nil
}

func TestEgressPage(t *testing.T) {
	_, path := pageLog(t)
	for _, c := range []struct {
		limit int
		want  string // the pages, | between them
	}{
		{1, "v|z|y|x|a2|a1|w"},
		{2, "v z|y x|a2 a1|w"},
		{3, "v z y|x a2 a1|w"},
		{7, "v z y x a2 a1 w"},
		{100, "v z y x a2 a1 w"},
		{0, "v|z|y|x|a2|a1|w"}, // below 1 is 1
	} {
		if g := strings.Join(pages(t, path, c.limit), "|"); g != c.want {
			t.Errorf("limit %d: %s, want %s", c.limit, g, c.want)
		}
	}

	// The first page ends at a tie: its next splits the tie by Skip, and the
	// second page starts with the rest of it.
	es, next, err := Page(context.Background(), path, nil, 2)
	if err != nil || len(es) != 2 || next == nil || !next.Time.Equal(es[1].Time) || next.Skip != 1 {
		t.Fatalf("first page: %+v, %+v, %v", es, next, err)
	}
	es, next, err = Page(context.Background(), path, next, 2)
	if err != nil || len(es) != 2 || es[0].Host != "y" || next == nil || next.Skip != 3 {
		t.Fatalf("second page: %+v, %+v, %v", es, next, err)
	}
	es, next, err = Page(context.Background(), path, nil, 1)
	if err != nil || len(es) != 1 || es[0].Host != "v" || next == nil {
		t.Fatalf("limit 1: %+v, %+v, %v", es, next, err)
	}
}

func TestEgressPageEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), LogFile)
	es, next, err := Page(context.Background(), path, nil, 10)
	if err != nil || es == nil || len(es) != 0 || next != nil {
		t.Errorf("no log: %v, %+v, %v", es, next, err)
	}
}
