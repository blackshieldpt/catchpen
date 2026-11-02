package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/record"
	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/internal/template"
)

// Session kinds, used in recording names and the audit log.
const (
	KindSSH     = "ssh"
	KindRun     = "run"
	KindAttach  = "attach"
	KindCommand = "command" // ssh <vm> -- cmd: no tty, not recorded
)

// RunCommand is the guest command for `catchpen run`: the agent in a tmux
// session of its own name (reattached if it exists), in the VM's repo.
func RunCommand(s *State, agent string, args []string) (string, error) {
	d, ok := template.AgentDefByName(agent)
	if !ok {
		return "", fmt.Errorf("unknown agent %q (%s)", agent, strings.Join(template.AgentNames(), ", "))
	}
	a, yolo := s.Agents.Get(agent), d.Yolo // yolo: the agent's skip-permission flag
	if a == nil {
		return "", fmt.Errorf("%s is not installed in %s (create a VM whose -t has it, e.g. -t %s)", agent, s.Name, agent)
	}
	// catchpen-env loads the environment pushed at the last start, which a
	// long-lived guest tmux server would otherwise not see.
	cmd := []string{"catchpen-env", agent}
	if a.SkipPermissions() {
		cmd = append(cmd, yolo)
	}
	cmd = append(cmd, args...)
	q := make([]string, len(cmd))
	for i, c := range cmd {
		q[i] = shQuote(c)
	}
	// Bases built before catchpen run lack catchpen-env; say so instead of a pane that exits.
	check := "command -v catchpen-env >/dev/null || { echo 'catchpen: this VM was built before catchpen run existed; recreate it (catchpen destroy, then catchpen up builds a current base)' >&2; exit 1; }; "
	notice := "tmux has-session -t =" + agent + " 2>/dev/null && echo 'catchpen: reattaching to the running " + agent +
		" session; arguments and yolo only apply when it starts' >&2; "
	return check + notice + "tmux new-session -A -s " + agent + dirFlag(s) + " -- " + strings.Join(q, " "), nil
}

// DefaultAgent is the agent catchpen herdr runs in s: Claude Code if it is
// installed, else the first installed agent, else "" (a shell).
func (s *State) DefaultAgent() string {
	if s.Agents.Get("claude") != nil {
		return "claude"
	}
	for _, n := range template.AgentNames() {
		if s.Agents.Get(n) != nil {
			return n
		}
	}
	return ""
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

// validTerm is what a TERM may be: the names terminfo uses.
var validTerm = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// Session runs an interactive ssh session to a VM on s. remote is the
// guest command ("" for a login shell). With a terminal (s.Size set, and a
// kind other than KindCommand) the session gets a tty, and with recording
// on it runs on a host pty and its output is recorded. On this process's
// own terminal with recording off, ssh uses the terminal directly; on any
// other IO it always runs on a host pty.
func (m *Manager) Session(ctx context.Context, name, kind, remote string, s record.IO) (int, error) {
	if s.In == nil || s.Out == nil {
		return -1, errors.New("a session needs an input and an output")
	}
	if s.Term != "" && !validTerm.MatchString(s.Term) {
		return -1, fmt.Errorf("invalid TERM %q", s.Term)
	}
	if _, err := m.SSHPort(name); err != nil {
		return -1, err
	}
	args := []string{"-F", m.P.SSHConfig()}
	tty := kind != KindCommand && s.Size != nil
	if tty {
		args = append(args, "-t")
	}
	args = append(args, sshx.AliasPrefix+name)
	if remote != "" {
		args = append(args, remote)
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if s.Term != "" {
		cmd.Env = append(os.Environ(), "TERM="+s.Term)
	}
	// exec copies an Out or Err that isn't a file in goroutines, which a
	// process still holding ssh's stderr (its ProxyCommand) could keep open.
	// When the delay cuts a copy short, Session returns exec.ErrWaitDelay.
	cmd.WaitDelay = time.Second
	if !tty || (!m.Cfg.Recording() && s.OnTerminal()) {
		cmd.Stdout, cmd.Stderr = s.Out, s.Err
		if _, ok := s.In.(*os.File); ok {
			cmd.Stdin = s.In
		} else {
			// Not cmd.Stdin: Wait would wait for a Read of s.In to return,
			// whatever WaitDelay says. Wait closes the pipe when ssh exits,
			// which ends this copy at its next write.
			w, err := cmd.StdinPipe()
			if err != nil {
				return -1, err
			}
			go func() {
				_, _ = io.Copy(w, s.In)
				_ = w.Close()
			}()
		}
		start := time.Now()
		m.audit(audit.SessionStart, name, map[string]any{"kind": kind})
		code, err := record.ExitStatus(cmd.Run())
		m.audit(audit.SessionEnd, name, map[string]any{"kind": kind, "exit": code, "seconds": int(time.Since(start).Seconds())})
		return code, err
	}
	cmd.Stderr = s.Err // ssh's own messages go to the terminal unrecorded
	if s.Err == nil {
		cmd.Stderr = io.Discard // not the pty, which pty.Start would pick
	}
	if !m.Cfg.Recording() {
		start := time.Now()
		m.audit(audit.SessionStart, name, map[string]any{"kind": kind})
		code, err := record.Run(cmd, nil, s)
		m.audit(audit.SessionEnd, name, map[string]any{"kind": kind, "exit": code, "seconds": int(time.Since(start).Seconds())})
		return code, err
	}
	rnd := make([]byte, 2)
	_, _ = rand.Read(rnd)
	id := time.Now().UTC().Format("20060102-150405") + "-" + kind + "-" + hex.EncodeToString(rnd)
	w, h := s.Size()
	rec, err := record.Create(m.RecordingPath(name, id), w, h, fmt.Sprintf("catchpen %s %s", kind, name))
	if err != nil {
		return -1, fmt.Errorf("recording: %w (set record: false in config.yaml to skip)", err)
	}
	start := time.Now()
	m.audit(audit.SessionStart, name, map[string]any{"kind": kind, "recording": id})
	code, err := record.Run(cmd, rec, s)
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
