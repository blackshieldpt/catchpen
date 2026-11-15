package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/canary"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/task"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// outputOf reads a task's output with c, and returns its lines.
func outputOf(t *testing.T, c *apiclient.Client, id string, after, limit int) ([]api.TaskOutputLine, error) {
	t.Helper()
	var got []api.TaskOutputLine
	err := c.TaskOutput(context.Background(), id, after, limit, false, func(l api.TaskOutputLine) error {
		got = append(got, l)
		return nil
	})
	return got, err
}

// appendTo appends s to a file, which must exist.
func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// canaryHits counts vm's canary.hit audit events.
func canaryHits(t *testing.T, h *vmHost, name string) int {
	t.Helper()
	n := 0
	if err := (audit.Log{Path: h.p.Audit()}).TailEvents(context.Background(), name, false, func(e audit.Event) error {
		if e.Event == audit.CanaryHit {
			n++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTaskOutputScope(t *testing.T) {
	h := home(t, nil)
	h.task(t, task.Task{ID: "aaaaaaa1", State: task.Done, VM: "task-aaaaaaa1", Created: created,
		Spec: task.Spec{Agent: "claude", Out: task.OutNone}})
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	const msg = "this call needs the config:write scope (catchpen daemon client add --scope config:write)"
	laptop := a.client(t, apiclient.Options{Cert: &a.laptop}) // no scope
	_, err := outputOf(t, laptop, "aaaaaaa1", 0, 0)
	refused(t, "laptop's output of a task", err, api.CodeForbidden, msg)
	_, err = outputOf(t, laptop, "nope", 0, 0)
	refused(t, "laptop's output of a missing task", err, api.CodeForbidden, msg) // the scope first: no existence oracle

	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}}) // config:write
	_, err = outputOf(t, cockpit, "nope", 0, 0)
	refused(t, "cockpit's output of a missing task", err, api.CodeNotFound, "no such task")
}

func TestTaskOutputLines(t *testing.T) {
	h := home(t, nil)
	h.task(t, task.Task{ID: "aaaaaaa1", State: task.Done, VM: "task-aaaaaaa1", Created: created,
		Spec: task.Spec{Agent: "claude", Out: task.OutNone}})
	out := filepath.Join(h.p.Tasks(), "aaaaaaa1", "output.jsonl")
	write(t, out, "one\ntwo\nthree\n")
	c := h.client(t)
	const id = "aaaaaaa1"

	all, err := outputOf(t, c, id, 0, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("all: %+v, %v; want 3 lines", all, err)
	}
	for i, w := range []string{"one", "two", "three"} {
		if all[i].N != i || all[i].Line != w {
			t.Errorf("all: line %d = %+v, want N %d and %q", i, all[i], i, w)
		}
	}
	got, err := outputOf(t, c, id, 1, 0)
	if err != nil || len(got) != 2 || got[0].N != 1 || got[0].Line != "two" || got[1].N != 2 {
		t.Errorf("after=1: %+v, %v; want N 1 and 2", got, err)
	}
	got, err = outputOf(t, c, id, 0, 2)
	if err != nil || len(got) != 2 || got[0].N != 0 || got[1].N != 1 {
		t.Errorf("limit=2: %+v, %v; want N 0 and 1", got, err)
	}
	got, err = outputOf(t, c, id, 1, 1)
	if err != nil || len(got) != 1 || got[0].N != 1 || got[0].Line != "two" {
		t.Errorf("after=1 limit=1: %+v, %v; want N 1", got, err)
	}
	for _, q := range []string{"?after=-1", "?limit=-1", "?limit=10001", "?follow=maybe"} {
		if st, body := h.raw(t, api.PathTaskOutput(id)+q); st != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", q, st, body)
		}
	}

	var big strings.Builder
	for i := 0; i < 1001; i++ {
		fmt.Fprintf(&big, "line %d\n", i)
	}
	write(t, out, big.String())
	got, err = outputOf(t, c, id, 0, 0)
	if err != nil || len(got) != 1001 || got[1000].N != 1000 {
		t.Errorf("limit=0 over 1001 lines: %d lines, %v; want all 1001", len(got), err)
	}
	st, body := h.raw(t, api.PathTaskOutput(id))
	if st != http.StatusOK || strings.Count(body, "\n") != api.DefaultTaskOutputLines {
		t.Errorf("no limit: %d, %d lines; want the default %d", st, strings.Count(body, "\n"), api.DefaultTaskOutputLines)
	}
}

func TestTaskOutputFollow(t *testing.T) {
	h := home(t, nil)
	h.task(t, task.Task{ID: "aaaaaaa1", State: task.Running, VM: "task-aaaaaaa1", Created: created,
		Started: created, Spec: task.Spec{Agent: "claude", Out: task.OutNone}})
	out := filepath.Join(h.p.Tasks(), "aaaaaaa1", "output.jsonl")
	write(t, out, "one\n")
	c := h.client(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan api.TaskOutputLine, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.TaskOutput(ctx, "aaaaaaa1", 0, 0, true, func(l api.TaskOutputLine) error {
			lines <- l
			return nil
		})
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case l := <-lines:
			if l.Line != want {
				t.Fatalf("got %+v, want %q", l, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no line %q within 5s", want)
		}
	}
	// A line is held until the next is written (a decoy can straddle two), so
	// "one" arrives once "two" does, and "two" once "three" does.
	appendTo(t, out, "two\n")
	expect("one")
	appendTo(t, out, "three\n")
	expect("two")

	st := task.Store{Dir: h.p.Tasks()}
	tk, err := st.Load("aaaaaaa1")
	if err != nil {
		t.Fatal(err)
	}
	tk.State = task.Done
	if err := st.Save(tk); err != nil {
		t.Fatal(err)
	}
	expect("three")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follow did not end within 5s of the task's end")
	}
}

func TestTaskOutputDecoys(t *testing.T) {
	h := home(t, nil)
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}}) // config:write

	for _, tc := range []struct {
		id, name string
		split    bool // the decoy straddles two lines
	}{
		{id: "aaaaaaa1", name: "task-aaaaaaa1"},
		{id: "bbbbbbb2", name: "task-bbbbbbb2", split: true},
	} {
		h.task(t, task.Task{ID: tc.id, State: task.Done, VM: tc.name, Created: created,
			Spec: task.Spec{Agent: "claude", Out: task.OutNone}})
		h.writeVM(t, vm.State{Name: tc.name, Template: "base", Created: created})
		cs := canary.Generate([]string{"aws"})
		if err := canary.Save(h.p.VM(tc.name), cs); err != nil {
			t.Fatal(err)
		}
		v := cs[0].Values[0]
		// The whole-line case ends with "three" after a clean "two", so a line whose
		// predecessor no longer holds the decoy is withheld too.
		body := "one\n" + v + "\ntwo\nthree\n"
		if tc.split {
			body = "one\n" + v[:len(v)/2] + "\n" + v[len(v)/2:] + "\ntwo\n"
		}
		write(t, filepath.Join(h.p.Tasks(), tc.id, "output.jsonl"), body)

		// The hit line and every line after it are withheld; the split case's
		// first half is held back, so it is withheld too.
		want := []string{"one", withheldDecoy, withheldDecoy, withheldDecoy}
		for read := 0; read < 2; read++ { // the second read must not act again
			got, err := outputOf(t, cockpit, tc.id, 0, 0)
			if err != nil {
				t.Fatalf("%s read %d: %v", tc.id, read, err)
			}
			if len(got) != len(want) {
				t.Fatalf("%s read %d: %+v, want %d lines", tc.id, read, got, len(want))
			}
			for i, w := range want {
				if got[i].Line != w {
					t.Errorf("%s read %d: line %d = %q, want %q", tc.id, read, i, got[i].Line, w)
				}
				if strings.Contains(got[i].Line, v) {
					t.Errorf("%s read %d: line %d carries the decoy", tc.id, read, i)
				}
			}
		}

		e := hitEvent(t, h.p, tc.name)
		if e.Detail["where"] != "in a task's output over the API" || e.Detail["client"] != "cockpit" || e.Detail["action"] != "pause" {
			t.Errorf("%s: canary.hit detail %+v", tc.id, e.Detail)
		}
		if n := canaryHits(t, h, tc.name); n != 1 {
			t.Errorf("%s: %d canary.hit events after two reads, want 1", tc.id, n)
		}
	}
}
