package vm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/notify"
)

// EditEgressList adds entries to, or removes them from, the allowlist
// (list egress.AllowlistFile) or the denylist (egress.DenylistFile) of a VM
// with allowlist egress, and records each change in the audit log. Entries
// are handled in order; an invalid one stops it, and those before it stay
// changed.
func (m *Manager) EditEgressList(name, list string, add bool, entries []string) error {
	ev := audit.EgressAllow
	switch list {
	case egress.AllowlistFile:
	case egress.DenylistFile:
		ev = audit.EgressDeny
	default:
		return fmt.Errorf("unknown egress list %q", list)
	}
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	if s.Egress != egress.ModeAllowlist {
		return fmt.Errorf("VM %s uses egress mode %q; lists only apply to allowlist mode", s.Name, s.Egress)
	}
	for _, e := range entries {
		if add {
			err = egress.AddEntry(m.dir(s.Name), list, e)
		} else {
			err = egress.RemoveEntry(m.dir(s.Name), list, e)
		}
		if err != nil {
			return err
		}
		m.audit(ev, s.Name, map[string]any{"entry": e, "add": add})
	}
	return nil
}

// SetEgressConfig's errors for a value it refuses wrap one of these.
var (
	ErrOnDeny        = errors.New(`on_deny must be "log" or "prompt"`)
	ErrPromptTimeout = errors.New("prompt timeout must be positive")
)

// EgressSettings are the egress settings to change; a nil field stays as it is.
type EgressSettings struct {
	OnDeny        *string // egress.OnDenyLog or egress.OnDenyPrompt
	PromptTimeout *time.Duration
	AllowPrivate  *bool
}

// SetEgressConfig changes the settings of a VM with allowlist egress, records
// what changed in the audit log (old and new values), and returns the
// settings now in force. A refused call changes and creates nothing. From the
// read to the audit record it holds egress.json's lock, so concurrent
// SetEgressConfig calls neither lose a change nor record a wrong old value.
// Other writers of egress.json (creating a VM, claiming a pool VM) don't take
// it.
func (m *Manager) SetEgressConfig(name string, set EgressSettings) (egress.Config, error) {
	s, err := m.Load(name)
	if err != nil {
		return egress.Config{}, err
	}
	if s.Egress != egress.ModeAllowlist {
		return egress.Config{}, fmt.Errorf("VM %s uses egress mode %q", s.Name, s.Egress)
	}
	// A broken egress.json is reported before a refused setting, as the CLI
	// always has; the read under the lock below is the one that counts.
	if _, err := egress.ReadConfig(m.dir(s.Name)); err != nil {
		return egress.Config{}, err
	}
	if set.OnDeny != nil && *set.OnDeny != egress.OnDenyLog && *set.OnDeny != egress.OnDenyPrompt {
		return egress.Config{}, fmt.Errorf("%w, got %q", ErrOnDeny, *set.OnDeny)
	}
	if set.PromptTimeout != nil && *set.PromptTimeout <= 0 {
		return egress.Config{}, fmt.Errorf("%w, got %s", ErrPromptTimeout, *set.PromptTimeout)
	}
	unlock, err := fsutil.Lock(filepath.Join(m.dir(s.Name), egress.ConfigFile+".lock"))
	if err != nil {
		return egress.Config{}, err
	}
	defer unlock()
	old, err := egress.ReadConfig(m.dir(s.Name))
	if err != nil {
		return egress.Config{}, err
	}
	c := old
	if set.OnDeny != nil {
		c.OnDeny = *set.OnDeny
	}
	if set.PromptTimeout != nil {
		c.PromptTimeout = *set.PromptTimeout
	}
	if set.AllowPrivate != nil {
		c.AllowPrivate = *set.AllowPrivate
	}
	if err := egress.WriteConfig(m.dir(s.Name), c); err != nil {
		return egress.Config{}, err
	}
	changed := map[string]any{}
	if c.OnDeny != old.OnDeny {
		changed["on_deny"], changed["on_deny_was"] = c.OnDeny, old.OnDeny
	}
	if c.PromptTimeout != old.PromptTimeout {
		changed["prompt_timeout"], changed["prompt_timeout_was"] = c.PromptTimeout.String(), old.PromptTimeout.String()
	}
	if c.AllowPrivate != old.AllowPrivate {
		changed["allow_private"], changed["allow_private_was"] = c.AllowPrivate, old.AllowPrivate
	}
	if len(changed) > 0 {
		m.audit(audit.EgressConfig, s.Name, changed)
	}
	return c, nil
}

// PromptDecider wraps inner, a decider that asks a person about a blocked
// request, with the egress.prompt notification and the audit record of the
// answer.
func (m *Manager) PromptDecider(inner egress.Decider) egress.Decider {
	nt := m.Notifier()
	return func(ctx context.Context, r egress.Request) egress.Decision {
		if nt.Wants(notify.EgressPrompt) {
			go func() {
				if err := nt.Send(context.Background(), notify.Event{Kind: notify.EgressPrompt, VM: r.VM,
					Title: "catchpen " + r.VM + ": egress prompt", Message: fmt.Sprintf("%s %s:%d is waiting for an answer", r.Method, r.Host, r.Port)}); err != nil {
					m.logf("warning: notification: %v", err)
				}
			}()
		}
		start := time.Now()
		d := inner(ctx, r)
		// Only answers given by a person: timeouts are in the VM's egress log, and
		// a guest could otherwise flood the audit log until it rotates. So could
		// a decider failing at once (notify-send without a notification server).
		if ctx.Err() == nil && time.Since(start) > 300*time.Millisecond {
			m.audit(audit.EgressPrompt, r.VM, map[string]any{"host": r.Host, "port": r.Port, "method": r.Method, "decision": string(d)})
		}
		return d
	}
}
