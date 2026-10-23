package vm

import (
	"context"
	"time"

	"golang.org/x/crypto/ssh"
)

// Guest is an SSH connection to a VM's guest. It closes when its context ends
// or a canary hit pauses the VM: closing a session doesn't end a command a
// paused guest never answers; closing the connection does. Every connection
// to a guest is made here (a test checks).
type Guest struct {
	*ssh.Client
	stop func()
}

func (g *Guest) Close() error {
	if g.stop != nil { // one built outside DialGuest has none
		g.stop()
	}
	return g.Client.Close()
}

// guestDialTimeout bounds connecting to a running guest.
const guestDialTimeout = 30 * time.Second

// DialGuest connects to the running VM name as its user, for the life of ctx.
func (m *Manager) DialGuest(ctx context.Context, name string) (*Guest, *State, error) {
	s, err := m.Load(name)
	if err != nil {
		return nil, nil, err
	}
	port, err := m.SSHPort(name)
	if err != nil {
		return nil, nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, guestDialTimeout)
	defer cancel()
	c, err := m.keys(name).Dial(dctx, port, s.User)
	if err != nil {
		return nil, nil, err
	}
	return m.guard(ctx, name, c), s, nil
}

// waitGuest waits (within wctx) for name's guest to accept SSH on port, for
// the life of ctx; alive reports whether its QEMU still runs.
func (m *Manager) waitGuest(ctx, wctx context.Context, name string, port int, user string, alive func() bool) (*Guest, error) {
	c, err := m.keys(name).WaitReady(wctx, port, user, alive)
	if err != nil {
		return nil, err
	}
	return m.guard(ctx, name, c), nil
}

func (m *Manager) guard(ctx context.Context, name string, c *ssh.Client) *Guest {
	ctx, cancel := m.CancelOnPause(ctx, name)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	return &Guest{Client: c, stop: func() { stop(); cancel() }}
}

// CancelOnPause cancels ctx once a canary hit pauses name: nothing in the
// guest answers then.
func (m *Manager) CancelOnPause(ctx context.Context, name string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if m.Paused(name) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}
