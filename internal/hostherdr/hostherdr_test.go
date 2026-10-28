package hostherdr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// testHerdr runs a herdr server in a throwaway named session.
func testHerdr(t *testing.T) Herdr {
	t.Helper()
	if !Installed() {
		t.Skip("herdr not installed")
	}
	bin := filepath.Join(t.TempDir(), "fake-catchpen")
	os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755)
	h := Herdr{Session: fmt.Sprintf("catchpen-test-%d", os.Getpid()), Bin: bin}
	t.Cleanup(func() {
		_ = h.cmd("session", "stop", h.Session).Run()
		_ = h.cmd("session", "delete", h.Session).Run()
	})
	return h
}

func TestWorkspaces(t *testing.T) {
	h := testHerdr(t)
	if h.Running() {
		t.Fatal("test session running before StartServer")
	}
	if err := h.Add("a", "claude"); err != nil || h.Running() {
		t.Fatalf("Add without a server must do nothing: %v", err)
	}
	if err := h.StartServer(); err != nil {
		t.Fatal(err)
	}
	if err := h.Add("a", "claude"); err != nil {
		t.Fatal(err)
	}
	if ws, _ := h.Workspaces(); len(ws) != 0 {
		t.Fatalf("Add without catchpen workspaces must do nothing: %v", ws)
	}
	// The user's own workspaces are left alone.
	if _, err := h.run("workspace", "create", "--label", "mine", "--no-focus"); err != nil {
		t.Fatal(err)
	}
	agents := map[string]string{"a": "claude", "b": ""}
	of := func(vm string) string { return agents[vm] }
	if err := h.Ensure([]string{"a", "b"}, of); err != nil {
		t.Fatal(err)
	}
	if err := h.Add("c", "pi"); err != nil {
		t.Fatal(err)
	}
	if err := h.Ensure([]string{"a"}, of); err != nil {
		t.Fatal(err)
	}
	if ws, _ := h.Workspaces(); !slices.Equal(ws, []string{"a", "b", "c"}) {
		t.Errorf("workspaces = %v", ws)
	}
	if err := h.Close("b"); err != nil {
		t.Fatal(err)
	}
	if err := h.Sync([]string{"a", "d"}, func(string) string { return "claude" }); err != nil {
		t.Fatal(err)
	}
	if ws, _ := h.Workspaces(); !slices.Equal(ws, []string{"a", "d"}) {
		t.Errorf("after Sync: %v", ws)
	}
	out, _ := h.run("workspace", "list")
	if !slices.Contains(labels(t, out), "mine") {
		t.Errorf("the user's workspace was closed: %s", out)
	}

	// After each herdr restart, the workspace comes back and reruns catchpen
	// run, which herdr again recognises as the agent.
	for restart := 1; restart <= 2; restart++ {
		if _, err := h.run("session", "stop", h.Session); err != nil {
			t.Fatal(err)
		}
		if err := h.StartServer(); err != nil {
			t.Fatal(err)
		}
		ws, err := h.workspaces()
		if err != nil || ws["a"] == "" {
			t.Fatalf("after restart %d: %v %v", restart, ws, err)
		}
		var got string
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
			if got = h.agentIn(ws["a"]); got == "claude" {
				break
			}
		}
		if got != "claude" {
			t.Fatalf("after restart %d, a's workspace runs %q, not claude", restart, got)
		}
	}
}

func TestArgv(t *testing.T) {
	h := Herdr{Bin: "/opt/catchpen"}
	if got := h.argv("dev", "pi"); !slices.Equal(got, []string{"env", "HERDR_AGENT=pi", "/opt/catchpen", "run", "dev", "pi"}) {
		t.Errorf("agent argv %v", got)
	}
	if got := h.argv("dev", ""); !slices.Equal(got, []string{"/opt/catchpen", "attach", "dev"}) {
		t.Errorf("shell argv %v", got)
	}
}

// agentIn is the agent herdr sees in workspace id's first pane.
func (h Herdr) agentIn(id string) string {
	out, err := h.run("pane", "list", "--workspace", id)
	if err != nil {
		return ""
	}
	var r struct {
		Result struct {
			Panes []struct {
				Agent string `json:"agent"`
			} `json:"panes"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &r) != nil || len(r.Result.Panes) == 0 {
		return ""
	}
	return r.Result.Panes[0].Agent
}

func labels(t *testing.T, out []byte) []string {
	t.Helper()
	var r struct {
		Result struct {
			Workspaces []struct {
				Label string `json:"label"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	var ls []string
	for _, w := range r.Result.Workspaces {
		ls = append(ls, w.Label)
	}
	return ls
}
