// Package hostherdr keeps a herdr workspace per running VM, each running an
// agent through curral run so herdr can track it and resume it after a
// restart. herdr runs on the host only: nothing in a guest reaches its socket.
package hostherdr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// labelPrefix marks the workspaces curral created; others are the user's and
// are left alone.
const labelPrefix = "curral:"

// source is how curral identifies itself to herdr when it reports a pane.
const source = "custom:curral"

// Herdr drives the host herdr server.
type Herdr struct {
	Session string        // named herdr session; "": the default one
	Bin     string        // curral binary run in each workspace
	Wait    time.Duration // how long herdr gets to recognise an agent (0: 15s)
}

func (h Herdr) cmd(args ...string) *exec.Cmd {
	if h.Session != "" {
		args = append([]string{"--session", h.Session}, args...)
	}
	return exec.Command("herdr", args...)
}

func (h Herdr) run(args ...string) ([]byte, error) {
	var out, errb bytes.Buffer
	c := h.cmd(args...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		if json.Unmarshal(errb.Bytes(), &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return nil, fmt.Errorf("herdr %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), err, msg)
	}
	return out.Bytes(), nil
}

// Installed reports whether herdr is on PATH.
func Installed() bool {
	_, err := exec.LookPath("herdr")
	return err == nil
}

// Running reports whether the herdr server is running.
func (h Herdr) Running() bool {
	if !Installed() {
		return false
	}
	out, err := h.cmd("status", "server").Output()
	return err == nil && bytes.HasPrefix(out, []byte("status: running"))
}

// StartServer starts a headless herdr server unless one is running.
func (h Herdr) StartServer() error {
	if !Installed() {
		return errors.New("herdr is not installed (https://herdr.dev)")
	}
	if h.Running() {
		return nil
	}
	c := h.cmd("server")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives curral
	if err := c.Start(); err != nil {
		return err
	}
	_ = c.Process.Release()
	for range 50 {
		if h.Running() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("the herdr server didn't start")
}

// workspaces maps each VM with a curral workspace to its workspace ID.
func (h Herdr) workspaces() (map[string]string, error) {
	out, err := h.run("workspace", "list")
	if err != nil {
		return nil, err
	}
	var r struct {
		Result struct {
			Workspaces []struct {
				ID    string `json:"workspace_id"`
				Label string `json:"label"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("herdr workspace list: %w", err)
	}
	ws := map[string]string{}
	for _, w := range r.Result.Workspaces {
		if vm, ok := strings.CutPrefix(w.Label, labelPrefix); ok && vm != "" {
			ws[vm] = w.ID
		}
	}
	return ws, nil
}

// Workspaces lists the VMs that have a curral workspace, sorted.
func (h Herdr) Workspaces() ([]string, error) {
	ws, err := h.workspaces()
	if err != nil {
		return nil, err
	}
	var out []string
	for vm := range ws {
		out = append(out, vm)
	}
	slices.Sort(out)
	return out, nil
}

// argv runs agent in vm (a shell in its main tmux session when agent is "").
// HERDR_AGENT tells herdr which agent runs behind curral; env keeps it a
// plain command, as herdr requires of a resume command.
func (h Herdr) argv(vm, agent string) []string {
	if agent == "" {
		return []string{h.Bin, "attach", vm}
	}
	return []string{"env", "HERDR_AGENT=" + agent, h.Bin, "run", vm, agent}
}

// Ensure adds a workspace for each VM in vms that lacks one, running the
// agent agentOf returns for it.
func (h Herdr) Ensure(vms []string, agentOf func(vm string) string) error {
	have, err := h.workspaces()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	var errs []error
	for _, vm := range vms {
		if _, ok := have[vm]; ok {
			continue
		}
		if err := h.add(vm, agentOf(vm), home); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", vm, err))
		}
	}
	return errors.Join(errs...)
}

func (h Herdr) add(vm, agent, cwd string) error {
	out, err := h.run("workspace", "create", "--cwd", cwd, "--label", labelPrefix+vm, "--no-focus")
	if err != nil {
		return err
	}
	var r struct {
		Result struct {
			Pane struct {
				ID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Result.Pane.ID == "" {
		return fmt.Errorf("herdr workspace create: no pane in %q", out)
	}
	pane, argv := r.Result.Pane.ID, h.argv(vm, agent)
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shQuote(a)
	}
	if _, err := h.run("pane", "run", pane, strings.Join(q, " ")); err != nil {
		return err
	}
	if agent == "" {
		return nil
	}
	// herdr takes a resume command only for a pane it sees an agent in.
	wait := h.Wait
	if wait == 0 {
		wait = 15 * time.Second
	}
	for deadline := time.Now().Add(wait); h.paneAgent(pane) != agent; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			return fmt.Errorf("herdr didn't recognise %s in its pane; it won't come back after a herdr restart", agent)
		}
	}
	_, err = h.run(append([]string{"pane", "report-agent-session", pane, "--source", source, "--agent", agent, "--"}, argv...)...)
	return err
}

func (h Herdr) paneAgent(pane string) string {
	out, err := h.run("pane", "get", pane)
	if err != nil {
		return ""
	}
	var r struct {
		Result struct {
			Pane struct {
				Agent string `json:"agent"`
			} `json:"pane"`
		} `json:"result"`
	}
	_ = json.Unmarshal(out, &r)
	return r.Result.Pane.Agent
}

// Add adds a workspace for vm if herdr is running and already has curral
// workspaces (used by curral start).
func (h Herdr) Add(vm, agent string) error {
	if !h.Running() {
		return nil
	}
	ws, err := h.workspaces()
	if err != nil || len(ws) == 0 {
		return err
	}
	return h.Ensure([]string{vm}, func(string) string { return agent })
}

// Close closes vm's curral workspace if there is one.
func (h Herdr) Close(vm string) error {
	if !h.Running() {
		return nil
	}
	ws, err := h.workspaces()
	if err != nil {
		return err
	}
	id, ok := ws[vm]
	if !ok {
		return nil
	}
	_, err = h.run("workspace", "close", id)
	return err
}

// Sync adds workspaces for running VMs and closes curral workspaces of the
// others.
func (h Herdr) Sync(running []string, agentOf func(vm string) string) error {
	if err := h.Ensure(running, agentOf); err != nil {
		return err
	}
	ws, err := h.Workspaces()
	if err != nil {
		return err
	}
	for _, vm := range ws {
		if !slices.Contains(running, vm) {
			if err := h.Close(vm); err != nil {
				return err
			}
		}
	}
	return nil
}

// Attach focuses vm's workspace (if vm is set) and, outside herdr, opens
// the herdr client on the terminal.
func (h Herdr) Attach(vm string) error {
	if vm != "" {
		ws, err := h.workspaces()
		if err != nil {
			return err
		}
		if id, ok := ws[vm]; ok {
			if _, err := h.run("workspace", "focus", id); err != nil {
				return err
			}
		}
	}
	if os.Getenv("HERDR_ENV") == "1" {
		return nil
	}
	c := h.cmd()
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
