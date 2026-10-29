package hosttmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	bin := filepath.Join(t.TempDir(), "fake-catchpen")
	os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755)
	tm := Tmux{Socket: fmt.Sprintf("catchpen-test-%d", os.Getpid()), Bin: bin}
	t.Cleanup(func() { _ = tm.cmd("kill-server").Run() })
	t.Setenv("TMUX", "")

	if tm.Exists() {
		t.Fatal("session exists before Ensure")
	}
	if err := tm.Add("a"); err != nil || tm.Exists() {
		t.Fatalf("Add without a session must do nothing: %v", err)
	}
	if err := tm.Ensure([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := tm.Add("c"); err != nil {
		t.Fatal(err)
	}
	if err := tm.Ensure([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	ws, _ := tm.Windows()
	if !slices.Equal(ws, []string{"a", "b", "c"}) {
		t.Errorf("windows = %v", ws)
	}
	out, _ := tm.run("list-panes", "-t", "="+Session+":=b", "-F", "#{pane_start_command}")
	if want := fmt.Sprintf("\"'%s' attach b\"\n", bin); out != want { // tmux shows the command quoted
		t.Errorf("pane command %q, want %q", out, want)
	}
	if err := tm.Close("b"); err != nil {
		t.Fatal(err)
	}
	if err := tm.Close("zz"); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.run("new-window", "-d", "-t", "="+Session+":", "-n", "mine", "sleep 300"); err != nil {
		t.Fatal(err)
	}
	if err := tm.Sync([]string{"c", "d"}); err != nil {
		t.Fatal(err)
	}
	ws, _ = tm.Windows()
	slices.Sort(ws)
	if !slices.Equal(ws, []string{"c", "d"}) {
		t.Errorf("after sync: %v", ws)
	}
	if all, _ := tm.list(); len(all) != 3 || !slices.ContainsFunc(all, func(w win) bool { return w.name == "mine" && w.vm == "" }) {
		t.Errorf("the user's own window was touched: %+v", all)
	}
	if err := tm.Sync(nil); err != nil {
		t.Fatal(err)
	}
	if ws, _ := tm.Windows(); len(ws) != 0 {
		t.Errorf("catchpen windows left after syncing with no running VMs: %v", ws)
	}
	if !tm.Exists() {
		t.Error("session with the user's window was killed")
	}
	tm.run("kill-window", "-t", "="+Session+":=mine")
	if err := tm.Ensure([]string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := tm.Close("x"); err != nil || tm.Exists() {
		t.Errorf("closing the last window should end the session: %v", err)
	}
}

func TestEnsureDedupe(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	bin := filepath.Join(t.TempDir(), "fake-catchpen")
	os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755)
	tm := Tmux{Socket: fmt.Sprintf("catchpen-dup-%d", os.Getpid()), Bin: bin}
	t.Cleanup(func() { _ = tm.cmd("kill-server").Run() })
	t.Setenv("TMUX", "")
	if err := tm.Ensure([]string{"a", "b", "b"}); err != nil { // a new session
		t.Fatal(err)
	}
	if err := tm.Ensure([]string{"c", "c"}); err != nil { // an existing one
		t.Fatal(err)
	}
	ws, _ := tm.Windows()
	slices.Sort(ws)
	if !slices.Equal(ws, []string{"a", "b", "c"}) {
		t.Errorf("windows = %v, want [a b c]", ws)
	}
}
