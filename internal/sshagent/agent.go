// Package sshagent runs a per-VM ssh-agent holding only chosen keys, each
// destination-constrained so it signs only on the path host -> VM -> the VM's
// forges (and, optionally, only after confirmation via ssh-askpass).
package sshagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blackshieldpt/curral/internal/forge"
)

// Agent is the agent of one VM, kept in the VM's ssh directory.
type Agent struct{ Dir string }

func (a Agent) Socket() string     { return filepath.Join(a.Dir, "agent.sock") }
func (a Agent) pidFile() string    { return filepath.Join(a.Dir, "agent.pid") }
func (a Agent) knownHosts() string { return filepath.Join(a.Dir, "agent_known_hosts") }

// Spec says what to load.
type Spec struct {
	VMAlias      string // ssh alias of the VM (curral-<vm>), as in VMKnownHosts
	VMKnownHosts string // known_hosts file with the VM's pinned host key
	Forges       []forge.Forge
	Keys         []string // private key files on the host
	Confirm      bool
	// Passphrase prompts from ssh-add.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Running returns the agent's pid if it is alive.
func (a Agent) Running() (int, bool) {
	b, err := os.ReadFile(a.pidFile())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	cmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !bytes.Contains(cmd, []byte("ssh-agent")) || !bytes.Contains(cmd, []byte(a.Socket())) {
		return 0, false
	}
	return pid, true
}

var pidRe = regexp.MustCompile(`SSH_AGENT_PID=([0-9]+)`)

// Start replaces any running agent with a fresh one holding spec.Keys.
func (a Agent) Start(ctx context.Context, s Spec) error {
	if len(s.Forges) == 0 {
		return errors.New("a restricted agent needs at least one SSH forge to constrain its keys to")
	}
	if len(a.Socket()) > 100 {
		return fmt.Errorf("agent socket path too long for a unix socket: %s", a.Socket())
	}
	if err := a.Stop(); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "ssh-agent", "-a", a.Socket()).Output()
	if err != nil {
		return fmt.Errorf("ssh-agent: %w", err)
	}
	m := pidRe.FindSubmatch(out)
	if m == nil {
		return fmt.Errorf("ssh-agent: no pid in %q", out)
	}
	if err := os.WriteFile(a.pidFile(), append(m[1], '\n'), 0o600); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = a.Stop()
		}
	}()
	vmKeys, err := os.ReadFile(s.VMKnownHosts)
	if err != nil {
		return err
	}
	var kh bytes.Buffer
	kh.Write(vmKeys)
	args := []string{"-H", a.knownHosts(), "-h", s.VMAlias}
	if s.Confirm {
		args = append(args, "-c")
	}
	for _, f := range s.Forges {
		for _, k := range f.HostKeys {
			fmt.Fprintf(&kh, "%s %s\n", f.AgentAlias(), k)
		}
		args = append(args, "-h", s.VMAlias+">"+f.AgentAlias())
	}
	if err := os.WriteFile(a.knownHosts(), kh.Bytes(), 0o600); err != nil {
		return err
	}
	for _, k := range s.Keys {
		p, err := expand(k)
		if err != nil {
			return err
		}
		args = append(args, p)
	}
	add := exec.CommandContext(ctx, "ssh-add", args...)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+a.Socket())
	add.Stdin, add.Stdout, add.Stderr = s.Stdin, s.Stdout, s.Stderr
	if err := add.Run(); err != nil {
		return fmt.Errorf("ssh-add: %w", err)
	}
	ok = true
	return nil
}

// Stop kills the agent and removes its socket.
func (a Agent) Stop() error {
	if pid, ok := a.Running(); ok {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 50; i++ {
			if _, ok := a.Running(); !ok {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, f := range []string{a.Socket(), a.pidFile(), a.knownHosts()} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func expand(p string) (string, error) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, rest)
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("agent key: %w", err)
	}
	return p, nil
}
