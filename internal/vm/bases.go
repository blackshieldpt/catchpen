package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"

	"github.com/blackshieldpt/curral/internal/image"
)

// CurrentBase is the base a new VM of this template and distro would use.
func (m *Manager) CurrentBase(tpl, distro string) (string, error) {
	r, err := m.Resolve(tpl, distro)
	if err != nil {
		return "", err
	}
	return r.BasePath(m.P), nil
}

var baseNameRe = regexp.MustCompile(`^(.+)-([0-9a-f]{16})(?:\.([0-9]+))?\.qcow2$`)

// RebuildTargets are the template/distro pairs `template rebuild --all`
// covers: those of existing VMs and those that have a base built.
func (m *Manager) RebuildTargets() ([][2]string, error) {
	var out [][2]string
	add := func(t, d string) {
		if !slices.Contains(out, [2]string{t, d}) {
			out = append(out, [2]string{t, d})
		}
	}
	vms, err := m.List()
	if err != nil {
		return nil, err
	}
	for _, v := range vms {
		add(v.Template, v.Distro)
	}
	ents, err := os.ReadDir(m.P.Bases())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range ents {
		// A base of the template's current version, with the distro it is for;
		// bases of older versions have nothing current to rebuild.
		if mt := baseNameRe.FindStringSubmatch(e.Name()); mt != nil {
			if d, ok := m.baseDistro(mt[1], mt[2]); ok {
				add(mt[1], d)
			}
		}
	}
	return out, nil
}

// distros are the distro names a template may be resolved with ("" is its default).
func (m *Manager) distros() []string {
	ds := append([]string{""}, image.BuiltinNames()...)
	for n := range m.Cfg.Images {
		if !slices.Contains(ds, n) {
			ds = append(ds, n)
		}
	}
	return ds
}

// baseDistro finds the distro for which template tpl currently resolves to
// base hash; ok is false when no current version of tpl has that hash.
func (m *Manager) baseDistro(tpl, hash string) (string, bool) {
	for _, d := range m.distros() {
		if r, err := m.Resolve(tpl, d); err == nil && r.T.Name == tpl && r.Hash == hash {
			return r.Distro.Name, true
		}
	}
	return "", false
}

// Rebuild builds new base generations for the given template/distro pairs
// (same template content shares one base). Existing VMs keep theirs.
func (m *Manager) Rebuild(ctx context.Context, targets [][2]string) error {
	done := map[string]bool{}
	var errs []error
	for _, t := range targets {
		r, err := m.Resolve(t[0], t[1])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		key := r.T.Name + "-" + r.Hash
		if done[key] {
			continue
		}
		done[key] = true
		if _, err := m.EnsureBase(ctx, r, true); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t[0], err)) // the others still get rebuilt
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}
