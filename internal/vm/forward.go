package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blackshieldpt/curral/internal/egress"
	"github.com/blackshieldpt/curral/internal/fsutil"
	"github.com/blackshieldpt/curral/internal/sshx"
)

const forwardsFile = "forwards.json"

// ForwardSession is a background `ssh -N` carrying port forwards for a VM.
type ForwardSession struct {
	PID     int       `json:"pid"`
	Specs   []string  `json:"specs"` // e.g. "-L 127.0.0.1:3000:localhost:3000"
	Started time.Time `json:"started"`
}

func validPort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

func validFwdHost(h string) error {
	if h == "" || strings.ContainsAny(h, " /\\\n@") || strings.HasPrefix(h, "-") {
		return fmt.Errorf("invalid host %q", h)
	}
	return nil
}

// LocalSpec normalizes -L: PORT, HOSTPORT:GUESTPORT or HOSTPORT:HOST:PORT
// (HOST as seen from the guest). Host listeners always bind to 127.0.0.1.
func LocalSpec(s string) (string, error) {
	parts := strings.Split(s, ":")
	var hp, host, gp string
	switch len(parts) {
	case 1:
		hp, host, gp = parts[0], "localhost", parts[0]
	case 2:
		hp, host, gp = parts[0], "localhost", parts[1]
	case 3:
		hp, host, gp = parts[0], parts[1], parts[2]
	default:
		return "", fmt.Errorf("-L %q: use PORT, HOSTPORT:GUESTPORT or HOSTPORT:HOST:PORT", s)
	}
	if _, err := validPort(hp); err != nil {
		return "", err
	}
	if _, err := validPort(gp); err != nil {
		return "", err
	}
	if err := validFwdHost(host); err != nil {
		return "", err
	}
	return "127.0.0.1:" + hp + ":" + host + ":" + gp, nil
}

// RemoteSpec normalizes -R: PORT, GUESTPORT:HOSTPORT or GUESTPORT:HOST:PORT
// (HOST as seen from the host). Guest listeners bind to the guest's loopback.
func RemoteSpec(s string) (string, error) {
	parts := strings.Split(s, ":")
	var gp, host, hp string
	switch len(parts) {
	case 1:
		gp, host, hp = parts[0], "localhost", parts[0]
	case 2:
		gp, host, hp = parts[0], "localhost", parts[1]
	case 3:
		gp, host, hp = parts[0], parts[1], parts[2]
	default:
		return "", fmt.Errorf("-R %q: use PORT, GUESTPORT:HOSTPORT or GUESTPORT:HOST:PORT", s)
	}
	if _, err := validPort(gp); err != nil {
		return "", err
	}
	if _, err := validPort(hp); err != nil {
		return "", err
	}
	if err := validFwdHost(host); err != nil {
		return "", err
	}
	return "127.0.0.1:" + gp + ":" + host + ":" + hp, nil
}

// DynamicSpec normalizes -D PORT (a SOCKS proxy on the host into the guest).
func DynamicSpec(s string) (string, error) {
	if _, err := validPort(s); err != nil {
		return "", err
	}
	return "127.0.0.1:" + s, nil
}

// Forward starts a background ssh session with the given forwards.
func (m *Manager) Forward(ctx context.Context, name string, local, remote, dynamic []string) (*ForwardSession, error) {
	s, err := m.Load(name)
	if err != nil {
		return nil, err
	}
	if _, err := m.SSHPort(name); err != nil {
		return nil, err
	}
	var specs []string
	var remotes []string
	for _, l := range local {
		v, err := LocalSpec(l)
		if err != nil {
			return nil, err
		}
		specs = append(specs, "-L", v)
	}
	for _, r := range remote {
		v, err := RemoteSpec(r)
		if err != nil {
			return nil, err
		}
		specs = append(specs, "-R", v)
		remotes = append(remotes, v)
	}
	for _, d := range dynamic {
		v, err := DynamicSpec(d)
		if err != nil {
			return nil, err
		}
		specs = append(specs, "-D", v)
	}
	if len(specs) == 0 {
		return nil, errors.New("nothing to forward (use -L, -R or -D)")
	}
	logf, err := os.OpenFile(m.file(name, "forward.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	args := append([]string{"-F", m.P.SSHConfig(), "-N", "-o", "BatchMode=yes", "-o", "ClearAllForwardings=no",
		"-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30", "-o", "GatewayPorts=no"}, specs...)
	args = append(args, sshx.AliasPrefix+name)
	cmd := exec.Command("ssh", args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	fmt.Fprintf(logf, "--- %s ssh %s\n", time.Now().Format(time.RFC3339), strings.Join(specs, " "))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// ExitOnForwardFailure makes ssh exit quickly if a forward can't be set up.
	select {
	case err := <-exited:
		tail, _ := readTail(m.file(name, "forward.log"), 2048)
		return nil, fmt.Errorf("ssh forward exited: %v\n%s", err, strings.TrimSpace(tail))
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, ctx.Err()
	}
	fs := &ForwardSession{PID: cmd.Process.Pid, Specs: pairs(specs), Started: time.Now().UTC()}
	if err := m.updateForwards(name, func(l []ForwardSession) []ForwardSession { return append(l, *fs) }); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	// Host services exposed to the guest bypass the egress proxy: record them.
	for _, r := range remotes {
		parts := strings.Split(r, ":")
		port, _ := strconv.Atoi(parts[3])
		if s.Egress == egress.ModeAllowlist {
			_ = egress.Append(m.dir(name), egress.LogEntry{Time: time.Now().UTC(), VM: name, Method: "FORWARD", Host: parts[2], Port: port,
				Action: egress.ActionForward, Reason: "curral forward -R " + r})
		}
	}
	return fs, nil
}

func pairs(specs []string) []string {
	var out []string
	for i := 0; i+1 < len(specs); i += 2 {
		out = append(out, specs[i]+" "+specs[i+1])
	}
	return out
}

func (m *Manager) forwardAlive(name string, pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.HasPrefix(b, []byte("ssh\x00")) && bytes.Contains(b, []byte("\x00"+sshx.AliasPrefix+name+"\x00"))
}

func (m *Manager) updateForwards(name string, fn func([]ForwardSession) []ForwardSession) error {
	unlock, err := fsutil.Lock(m.file(name, forwardsFile+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	var list []ForwardSession
	if b, err := os.ReadFile(m.file(name, forwardsFile)); err == nil {
		_ = json.Unmarshal(b, &list)
	}
	var alive []ForwardSession
	for _, f := range list {
		if m.forwardAlive(name, f.PID) {
			alive = append(alive, f)
		}
	}
	return writeJSON(m.file(name, forwardsFile), fn(alive))
}

// Forwards lists a VM's live forward sessions.
func (m *Manager) Forwards(name string) ([]ForwardSession, error) {
	if _, err := m.Load(name); err != nil {
		return nil, err
	}
	var out []ForwardSession
	err := m.updateForwards(name, func(l []ForwardSession) []ForwardSession { out = l; return l })
	return out, err
}

// StopForwards ends a VM's forward sessions.
func (m *Manager) StopForwards(name string) error {
	if _, err := m.Load(name); err != nil {
		return err
	}
	m.stopForwards(name)
	return nil
}

func (m *Manager) stopForwards(name string) {
	if _, err := os.Stat(m.file(name, forwardsFile)); err != nil {
		return
	}
	_ = m.updateForwards(name, func(l []ForwardSession) []ForwardSession {
		for _, f := range l {
			_ = syscall.Kill(f.PID, syscall.SIGTERM)
		}
		return nil
	})
}
