package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/record"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
)

// Session kinds, used in recording names and the audit log.
const (
	KindSSH     = "ssh"
	KindRun     = "run"
	KindAttach  = "attach"
	KindCommand = "command" // ssh <vm> -- cmd: no tty, not recorded
)

// RunCommand is the guest command for `curral run`: the agent in a tmux
// session of its own name (reattached if it exists), in the VM's repo.
func RunCommand(s *State, agent string, args []string) (string, error) {
	d, ok := template.AgentDefByName(agent)
	if !ok {
		return "", fmt.Errorf("unknown agent %q (%s)", agent, strings.Join(template.AgentNames(), ", "))
	}
	a, yolo := s.Agents.Get(agent), d.Yolo // yolo: the agent's skip-permission flag
	if a == nil {
		return "", fmt.Errorf("%s is not installed in %s (add agents.%s to its template)", agent, s.Name, agent)
	}
	// curral-env loads the environment pushed at the last start, which a
	// long-lived guest tmux server would otherwise not see.
	cmd := []string{"curral-env", agent}
	if a.SkipPermissions() {
		cmd = append(cmd, yolo)
	}
	cmd = append(cmd, args...)
	q := make([]string, len(cmd))
	for i, c := range cmd {
		q[i] = shQuote(c)
	}
	// Bases built before curral run lack curral-env; say so instead of a pane that exits.
	check := "command -v curral-env >/dev/null || { echo 'curral: this VM was built before curral run existed; recreate it (curral destroy, then curral up builds a current base)' >&2; exit 1; }; "
	notice := "tmux has-session -t =" + agent + " 2>/dev/null && echo 'curral: reattaching to the running " + agent +
		" session; arguments and yolo only apply when it starts' >&2; "
	return check + notice + "tmux new-session -A -s " + agent + dirFlag(s) + " -- " + strings.Join(q, " "), nil
}

// AttachCommand opens (or reattaches) the guest's main tmux session.
func AttachCommand(s *State) string {
	return "tmux new-session -A -s main" + dirFlag(s)
}

func dirFlag(s *State) string {
	if d := WorkDir(s); d != "" {
		return " -c " + shQuote(d)
	}
	return ""
}

// WorkDir is the VM's repo when it has exactly one, /workspace with several,
// and "" (the home directory) with none.
func WorkDir(s *State) string {
	switch len(s.Repos) {
	case 0:
		return ""
	case 1:
		return forge.GuestRepo{Repo: s.Repos[0].Repo}.Dir()
	}
	return "/workspace"
}

// Session runs an interactive ssh session to a VM. remote is the guest
// command ("" for a login shell). With a terminal and recording on, the
// session runs on a host pty and its output is recorded.
func (m *Manager) Session(ctx context.Context, name, kind, remote string) (int, error) {
	if _, err := m.SSHPort(name); err != nil {
		return -1, err
	}
	args := []string{"-F", m.P.SSHConfig()}
	tty := kind != KindCommand && record.IsTerminal()
	if tty {
		args = append(args, "-t")
	}
	args = append(args, sshx.AliasPrefix+name)
	if remote != "" {
		args = append(args, remote)
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if !tty || !m.Cfg.Recording() {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		start := time.Now()
		m.audit(audit.SessionStart, name, map[string]any{"kind": kind})
		code, err := record.ExitStatus(cmd.Run())
		m.audit(audit.SessionEnd, name, map[string]any{"kind": kind, "exit": code, "seconds": int(time.Since(start).Seconds())})
		return code, err
	}
	rnd := make([]byte, 2)
	_, _ = rand.Read(rnd)
	id := time.Now().UTC().Format("20060102-150405") + "-" + kind + "-" + hex.EncodeToString(rnd)
	w, h := record.Size()
	rec, err := record.Create(m.RecordingPath(name, id), w, h, fmt.Sprintf("curral %s %s", kind, name))
	if err != nil {
		return -1, fmt.Errorf("recording: %w (set record: false in config.yaml to skip)", err)
	}
	cmd.Stderr = os.Stderr // ssh's own messages go to the terminal unrecorded
	start := time.Now()
	m.audit(audit.SessionStart, name, map[string]any{"kind": kind, "recording": id})
	code, err := record.Run(cmd, rec)
	if cerr := rec.Close(); err == nil && cerr != nil {
		m.logf("warning: recording %s: %v", id, cerr)
	}
	m.audit(audit.SessionEnd, name, map[string]any{"kind": kind, "recording": id, "exit": code, "seconds": int(time.Since(start).Seconds())})
	return code, err
}

// RecordingPath is where a VM's recording with this id lives.
func (m *Manager) RecordingPath(name, id string) string {
	return filepath.Join(m.P.Recordings(), name, id+".cast")
}

// Recordings lists a VM's recording ids, oldest first. They outlive the VM.
func (m *Manager) Recordings(name string) ([]string, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(m.P.Recordings(), name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if id, ok := strings.CutSuffix(e.Name(), ".cast"); ok && e.Type().IsRegular() {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}
