package vm

import (
	"context"

	"github.com/blackshieldpt/catchpen/internal/forge"
)

// TestForge checks a forge with its credentials from the host's secrets (see
// forge.Client.Check). f's auth references and URL decide which secret is
// sent where, so f must come from the registry (forge.Load), never from a
// client.
func (m *Manager) TestForge(ctx context.Context, f forge.Forge) (forge.CheckResult, error) {
	sec := m.resolver()
	c := &forge.Client{HTTP: m.HTTP, F: f}
	switch {
	case f.Auth.Token != "":
		tok, err := secretRef(ctx, sec, f.Auth.Token)
		if err != nil {
			return forge.CheckResult{}, err
		}
		c.Token = tok
	case f.Auth.Password != "":
		pw, err := secretRef(ctx, sec, f.Auth.Password)
		if err != nil {
			return forge.CheckResult{}, err
		}
		c.User, c.Password, c.Basic = f.Auth.User, pw, true
	}
	return c.Check(ctx)
}
