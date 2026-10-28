package vm

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/notify"
	"github.com/blackshieldpt/catchpen/internal/template"
)

const (
	idleCPU    = 0.05 // share of one core below which QEMU counts as idle
	clockTicks = 100  // USER_HZ, the unit of /proc/<pid>/stat times
)

// IdleTimeout is how long a VM may be idle before it is stopped (0: never).
func (m *Manager) IdleTimeout(s *State) time.Duration {
	d, _ := template.ParseInterval(cmp.Or(s.IdleStop, m.Cfg.IdleStop))
	return d
}

// IdleTracker finds running VMs idle past their idle_stop: no connection to
// their ssh or forwarded ports, no running task, and QEMU's CPU use below
// idleCPU since the previous check. VMs a canary hit paused are left for
// you to look at.
type IdleTracker struct{ vms map[string]*idleState }

// Idle is a VM due to be stopped.
type Idle struct {
	Name string
	For  time.Duration
}

type idleState struct {
	pid    int
	ticks  uint64
	at     time.Time // last sample
	active time.Time // last activity
}

// observe records a sample and returns how long the VM has been idle.
func (st *idleState) observe(pid int, ticks uint64, busy bool, now time.Time) time.Duration {
	if pid != st.pid || ticks < st.ticks { // first sample, or a new QEMU
		*st = idleState{pid: pid, ticks: ticks, at: now, active: now}
		return 0
	}
	if el := now.Sub(st.at).Seconds(); el > 0 && float64(ticks-st.ticks)/clockTicks/el >= idleCPU {
		busy = true
	}
	st.ticks, st.at = ticks, now
	if busy {
		st.active = now
	}
	return now.Sub(st.active)
}

// Check samples the running VMs and returns those idle past their timeout.
// Pool and ephemeral VMs are left alone.
func (t *IdleTracker) Check(m *Manager, now time.Time) ([]Idle, error) {
	vms, err := m.List()
	if err != nil {
		return nil, err
	}
	conns, err := establishedPorts()
	if err != nil {
		return nil, err
	}
	if t.vms == nil {
		t.vms = map[string]*idleState{}
	}
	seen := map[string]bool{}
	var out []Idle
	for _, v := range vms {
		limit := m.IdleTimeout(&v.State)
		if !v.Running || v.Paused || v.Pool || v.Ephemeral || limit == 0 {
			continue
		}
		ticks, err := cpuTicks(v.Runtime.PID)
		if err != nil {
			continue
		}
		seen[v.Name] = true
		busy := m.busy(&v.State, v.Runtime, conns)
		st := t.vms[v.Name]
		if st == nil {
			st = &idleState{}
			t.vms[v.Name] = st
		}
		if d := st.observe(v.Runtime.PID, ticks, busy, now); d >= limit {
			out = append(out, Idle{v.Name, d})
		}
	}
	for n := range t.vms {
		if !seen[n] {
			delete(t.vms, n)
		}
	}
	return out, nil
}

// StopIdle stops a VM found idle, unless it got busy since: stopping the
// ones before it may have taken a while.
func (m *Manager) StopIdle(name string, idle time.Duration) error {
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	rt, ok := m.Running(name)
	if !ok {
		return nil
	}
	conns, err := establishedPorts()
	if err != nil {
		return err
	}
	if m.busy(s, rt, conns) {
		return nil
	}
	if err := m.Stop(name, false); err != nil {
		return err
	}
	d := idle.Round(time.Minute).String()
	m.audit(audit.VMIdleStop, name, map[string]any{"idle": d})
	m.notify(notify.Event{Kind: notify.VMIdleStop, VM: name, Title: "catchpen " + name + " stopped",
		Message: fmt.Sprintf("idle for %s (catchpen start %s)", d, name)})
	return nil
}

// busy reports a connection to the VM's ports or a running task.
func (m *Manager) busy(s *State, rt *Runtime, conns map[int]bool) bool {
	if conns[rt.SSHPort] || m.busyTask(s) != nil {
		return true
	}
	for _, f := range rt.Forwards {
		if conns[f.Host] {
			return true
		}
	}
	return false
}

// cpuTicks is the user+system CPU time of a process, in clock ticks.
func cpuTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseStatTicks(string(b))
}

func parseStatTicks(stat string) (uint64, error) {
	// The command name is in parentheses and may contain spaces.
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat")
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 13 { // utime and stime are fields 14 and 15, f[11] and f[12]
		return 0, fmt.Errorf("malformed stat")
	}
	u, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return 0, err
	}
	s, err := strconv.ParseUint(f[12], 10, 64)
	return u + s, err
}

// establishedPorts is the set of local loopback TCP ports with an established
// connection: QEMU's side of a host connection to a forwarded port.
func establishedPorts() (map[int]bool, error) {
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseTCP(f)
}

func parseTCP(r io.Reader) (map[int]bool, error) {
	out := map[int]bool{}
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[3] != "01" { // ESTABLISHED
			continue
		}
		addr, port, ok := strings.Cut(f[1], ":")
		if !ok || addr != "0100007F" { // 127.0.0.1, little endian
			continue
		}
		p, err := strconv.ParseUint(port, 16, 16)
		if err != nil {
			continue
		}
		out[int(p)] = true
	}
	return out, sc.Err()
}
