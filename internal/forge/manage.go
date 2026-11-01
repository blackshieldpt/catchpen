package forge

import (
	"context"
	"fmt"
)

// HostKeyScan is the first step of adding a forge: the SSH host keys to pin,
// and their fingerprints for a person to compare with the forge's published
// ones. The second step is Add, with the confirmed keys in Forge.HostKeys. A
// scan doesn't record which host it came from: the caller must pin it only on
// a forge with the same Host and Port.
type HostKeyScan struct {
	Keys         []string // authorized_keys format, as Forge.HostKeys holds them
	Fingerprints []string // Fingerprints[i] is Keys[i]'s
	// Published is true for github.com, whose keys come from GitHub's API over
	// HTTPS rather than from the SSH server, so need no comparison.
	Published bool
}

// ScanHostKeys fetches the host keys of c.F: github.com's from GitHub's API,
// any other forge's from its SSH server.
func (c *Client) ScanHostKeys(ctx context.Context) (HostKeyScan, error) {
	if c.F.Type == GitHub && c.F.Host == "github.com" {
		keys, err := c.GitHubKeys(ctx)
		if err != nil {
			return HostKeyScan{}, fmt.Errorf("fetching GitHub host keys: %w", err)
		}
		s := HostKeyScan{Keys: keys, Published: true}
		for _, k := range keys {
			pk, err := ParseHostKey(k)
			if err != nil {
				return HostKeyScan{}, err
			}
			s.Fingerprints = append(s.Fingerprints, Fingerprint(pk))
		}
		return s, nil
	}
	keys, err := Scan(ctx, c.F.Host, c.F.Port())
	if err != nil {
		return HostKeyScan{}, err
	}
	var s HostKeyScan
	for _, k := range keys {
		s.Keys = append(s.Keys, FormatHostKey(k))
		s.Fingerprints = append(s.Fingerprints, Fingerprint(k))
	}
	return s, nil
}

// CheckNew refuses a name the registry already has.
func (r Registry) CheckNew(name string) error {
	if _, ok := r.Get(name); ok {
		return fmt.Errorf("forge %s exists (catchpen forge rm %s first)", name, name)
	}
	return nil
}

// Add saves a new forge to the registry at path. f.HostKeys are the keys a
// person confirmed (see ScanHostKeys). An agent-mode forge's SSH server must
// support host-bound auth; that check runs the system ssh against f.Host
// before Update validates f, so a caller that takes f from outside validates
// it first. The name is checked again under the registry's lock, so two
// concurrent Adds can't both save it.
func Add(ctx context.Context, path string, f Forge) error {
	if f.Auth.Mode == ModeAgent {
		if err := CheckHostBound(ctx, f); err != nil {
			return err
		}
	}
	return Update(path, func(r *Registry) error {
		if err := r.CheckNew(f.Name); err != nil {
			return err
		}
		r.Forges = append(r.Forges, f)
		return nil
	})
}

// CheckHostBound refuses a forge whose SSH server can't take signatures from
// a restricted (destination-constrained) agent.
func CheckHostBound(ctx context.Context, f Forge) error {
	ok, err := HostBound(ctx, f)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s's SSH server does not support host-bound auth (publickey-hostbound@openssh.com), "+
			"so a restricted agent can't sign for it; use deploy-key or minted-token mode instead", f.Name)
	}
	return nil
}

// CheckResult is what Check confirmed. On an error, it holds the checks that
// passed before it.
type CheckResult struct {
	API       bool   // the API credentials work (checked when c has them)
	Login     string // the account they authenticate as
	HostKeys  bool   // every host key the SSH server offers is pinned (checked when keys are pinned, except on github.com)
	HostBound bool   // the SSH server supports host-bound auth (checked in agent mode)
}

// Check tests a forge's API credentials, its pinned host keys and, in agent
// mode, host-bound auth, in that order, stopping at the first failure. The
// caller sets c's credentials (Token, or User, Password and Basic).
func (c *Client) Check(ctx context.Context) (CheckResult, error) {
	var r CheckResult
	f := c.F
	if c.Token != "" || c.Basic {
		login, err := c.Login(ctx)
		if err != nil {
			return r, fmt.Errorf("API: %w", err)
		}
		r.API, r.Login = true, login
	}
	if len(f.HostKeys) > 0 && !(f.Type == GitHub && f.Host == "github.com") {
		keys, err := Scan(ctx, f.Host, f.Port())
		if err != nil {
			return r, fmt.Errorf("SSH: %w", err)
		}
		pinned := map[string]bool{}
		for _, k := range f.HostKeys {
			pinned[k] = true
		}
		for _, k := range keys {
			if !pinned[FormatHostKey(k)] {
				return r, fmt.Errorf("SSH host key changed: %s is not pinned (if expected: catchpen forge rm %s && catchpen forge add ...)", Fingerprint(k), f.Name)
			}
		}
		r.HostKeys = true
	}
	if f.Auth.Mode == ModeAgent {
		if err := CheckHostBound(ctx, f); err != nil {
			return r, err
		}
		r.HostBound = true
	}
	return r, nil
}
