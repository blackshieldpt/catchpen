package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/paths"
)

// runAsking runs c with args, in as its stdin, and returns what it printed to
// stdout and stderr.
func runAsking(t *testing.T, c *cobra.Command, in string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	go func() { _, _ = io.WriteString(inW, in); inW.Close() }()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()
	func() {
		defer func() {
			os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
			inR.Close()
			outW.Close()
			errW.Close()
		}()
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		err = c.Execute()
	}()
	return <-outC, <-errC, err
}

// vmHome makes a CATCHPEN_HOME with stopped VMs: v1, whose disk has the
// snapshots s1 and s2, and v2, whose disk has none.
func vmHome(t *testing.T) paths.Paths {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("needs qemu-img")
	}
	t.Setenv("CATCHPEN_HOME", t.TempDir())
	p := paths.Default()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		name  string
		snaps []string
	}{{"v1", []string{"s1", "s2"}}, {"v2", nil}} {
		dir := filepath.Join(p.VMs(), v.name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vm.json"), []byte(`{"name":"`+v.name+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		disk := filepath.Join(dir, "disk.qcow2")
		cmds := [][]string{{"create", "-q", "-f", "qcow2", disk, "1M"}}
		for _, s := range v.snaps {
			cmds = append(cmds, []string{"snapshot", "-c", s, disk})
		}
		for _, a := range cmds {
			if out, err := exec.Command("qemu-img", a...).CombinedOutput(); err != nil {
				t.Fatalf("qemu-img %v: %v %s", a, err, out)
			}
		}
	}
	return p
}

// The [y/N] prompts of reset, rebase and destroy, and when they are asked.
func TestConfirmPrompts(t *testing.T) {
	p := vmHome(t)
	for _, c := range []struct {
		cmd     func() *cobra.Command
		in      string
		args    []string
		errOut  string
		errText string
	}{
		{resetCmd, "n\n", []string{"v1"}, "Reset v1 and delete all data on its disk, snapshots included? [y/N] ", "aborted"},
		{resetCmd, "", []string{"nosuch"}, "Reset nosuch and delete all data on its disk, snapshots included? [y/N] ", "aborted"},
		{rebaseCmd, "n\n", []string{"v1"}, "Rebasing deletes the snapshots of v1 (s1, s2). Continue? [y/N] ", "aborted"},
		{rebaseCmd, "no\n", []string{"v1"}, "Rebasing deletes the snapshots of v1 (s1, s2). Continue? [y/N] ", "aborted"},
		{rebaseCmd, "", []string{"nosuch"}, "", `no VM named "nosuch" (see: catchpen ls)`},
		{rebaseCmd, "", []string{"bad name"}, "", `invalid VM name "bad name": use lowercase letters, digits and dashes (max 32)`},
		{destroyCmd, "n\n", []string{"v1", "v2"}, "Destroy v1, v2 and delete all data inside? [y/N] ", "aborted"},
		{destroyCmd, "\n", []string{"nosuch"}, "Destroy nosuch and delete all data inside? [y/N] ", "aborted"},
	} {
		out, errOut, err := runAsking(t, c.cmd(), c.in, c.args...)
		if out != "" || errOut != c.errOut || err == nil || err.Error() != c.errText {
			t.Errorf("%s %v with %q: %q, %q, %v", c.cmd().Name(), c.args, c.in, out, errOut, err)
		}
	}
	// Without snapshots, or with -y, rebase doesn't ask: it goes on, and
	// fails on the VM's template (vm.json names none).
	for _, args := range [][]string{{"v2"}, {"-y", "v1"}} {
		out, errOut, err := runAsking(t, rebaseCmd(), "n\n", args...)
		if out != "" || errOut != "" || err == nil || !strings.HasPrefix(err.Error(), args[len(args)-1]+" was created with the old-style template") {
			t.Errorf("rebase %v: %q, %q, %v", args, out, errOut, err)
		}
	}
	for _, n := range []string{"v1", "v2"} {
		if _, err := os.Stat(filepath.Join(p.VMs(), n, "disk.qcow2")); err != nil {
			t.Errorf("%s after the refusals: %v", n, err)
		}
	}
}
