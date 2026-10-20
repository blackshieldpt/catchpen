// Package hosttmux keeps a host tmux session "curral" with one window per
// running VM, each attached to that VM's guest tmux.
package hosttmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// Session is the host tmux session name.
const Session = "curral"

// Tmux drives the host tmux server (Socket: a -L socket name; "" = default).
type Tmux struct {
	Socket string
	Bin    string // curral binary run in each window
}

func (t Tmux) cmd(args ...string) *exec.Cmd {
	if t.Socket != "" {
		args = append([]string{"-L", t.Socket}, args...)
	}
	return exec.Command("tmux", args...)
}

func (t Tmux) run(args ...string) (string, error) {
	var out, errb bytes.Buffer
	c := t.cmd(args...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Exists reports whether the curral session exists.
func (t Tmux) Exists() bool {
	return t.cmd("has-session", "-t", "="+Session).Run() == nil
}

// mark is a window option naming the VM of a window curral created; windows
// without it are the user's and are left alone.
const mark = "@curral-vm"

type win struct{ name, vm string }

func (t Tmux) list() ([]win, error) {
	out, err := t.run("list-windows", "-t", "="+Session, "-F", "#{window_name}\t#{"+mark+"}")
	if err != nil {
		return nil, err
	}
	var ws []win
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		name, vm, _ := strings.Cut(l, "\t")
		ws = append(ws, win{name, vm})
	}
	return ws, nil
}

// Windows lists the VMs that have a curral window.
func (t Tmux) Windows() ([]string, error) {
	ws, err := t.list()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, w := range ws {
		if w.vm != "" {
			out = append(out, w.vm)
		}
	}
	return out, nil
}

func (t Tmux) window(vm string) string {
	return shQuote(t.Bin) + " attach " + vm
}

// Ensure creates the session if needed and adds a window for each VM that
// lacks one.
func (t Tmux) Ensure(vms []string) error {
	if len(vms) == 0 {
		return fmt.Errorf("no running VMs")
	}
	have := map[string]bool{}
	if t.Exists() {
		ws, err := t.Windows()
		if err != nil {
			return err
		}
		for _, w := range ws {
			have[w] = true
		}
	} else {
		id, err := t.run("new-session", "-d", "-P", "-F", "#{window_id}", "-s", Session, "-n", vms[0], t.window(vms[0]))
		if err != nil {
			return err
		}
		if err := t.markWindow(id, vms[0]); err != nil {
			return err
		}
		have[vms[0]] = true
	}
	for _, vm := range vms {
		if !have[vm] {
			id, err := t.run("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "="+Session+":", "-n", vm, t.window(vm))
			if err != nil {
				return err
			}
			if err := t.markWindow(id, vm); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t Tmux) markWindow(id, vm string) error {
	_, err := t.run("set-option", "-w", "-t", strings.TrimSpace(id), mark, vm)
	return err
}

// Add adds a window for vm if the session exists (used by curral start).
func (t Tmux) Add(vm string) error {
	if !t.Exists() {
		return nil
	}
	return t.Ensure([]string{vm})
}

// Close closes vm's curral window if there is one.
func (t Tmux) Close(vm string) error {
	if !t.Exists() {
		return nil
	}
	ws, err := t.list()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ws, func(w win) bool { return w.vm == vm })
	if i < 0 {
		return nil
	}
	if len(ws) == 1 {
		_, err = t.run("kill-session", "-t", "="+Session)
		return err
	}
	_, err = t.run("kill-window", "-t", "="+Session+":="+ws[i].name)
	return err
}

// Sync adds windows for running VMs and closes curral windows of the others.
func (t Tmux) Sync(running []string) error {
	if !t.Exists() {
		if len(running) == 0 {
			return nil
		}
		return t.Ensure(running)
	}
	ws, err := t.Windows()
	if err != nil {
		return err
	}
	if len(running) > 0 {
		if err := t.Ensure(running); err != nil {
			return err
		}
	}
	for _, w := range ws {
		if !slices.Contains(running, w) {
			if err := t.Close(w); err != nil {
				return err
			}
		}
	}
	return nil
}

// Attach attaches the terminal to the session (switching if already in tmux).
func (t Tmux) Attach(vm string) error {
	target := "=" + Session
	if vm != "" {
		target += ":=" + vm
	}
	verb := "attach-session"
	if os.Getenv("TMUX") != "" {
		verb = "switch-client"
	}
	c := t.cmd(verb, "-t", target)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
