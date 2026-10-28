package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/blackshieldpt/catchpen/internal/template"
)

// CurrentBase is the base a new VM of this template (a -t spec) would use.
func (m *Manager) CurrentBase(tpl string) (string, error) {
	r, err := m.Resolve(tpl)
	if err != nil {
		return "", err
	}
	return r.BasePath(m.P), nil
}

var baseNameRe = regexp.MustCompile(`^(.+)-([0-9a-f]{16})(?:\.([0-9]+))?\.qcow2$`)

// RebuildTargets are the templates `template rebuild --all` covers: those of
// existing VMs and those that have a base of their current version built.
func (m *Manager) RebuildTargets() ([]string, error) {
	var out []string
	add := func(t string) {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	vms, err := m.List()
	if err != nil {
		return nil, err
	}
	for _, v := range vms {
		if v.Picks {
			add(v.Template)
		}
	}
	ents, err := os.ReadDir(m.P.Bases())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range ents {
		// Bases of older versions have nothing current to rebuild.
		if mt := baseNameRe.FindStringSubmatch(e.Name()); mt != nil {
			for _, spec := range m.currentSpecs(mt[1], mt[2]) {
				add(spec)
			}
		}
	}
	return out, nil
}

// currentSpecs are the specs recorded for base stem-hash that still give it.
func (m *Manager) currentSpecs(stem, hash string) []string {
	var out []string
	specs, _ := readSpecs(filepath.Join(m.P.Bases(), stem+"-"+hash+".specs"))
	for _, spec := range specs {
		if r, err := m.Resolve(spec); err == nil && r.stem() == stem && r.Hash == hash {
			out = append(out, spec)
		}
	}
	return out
}

// baseWanted reports whether gc must keep base stem-hash's newest generation:
// a recorded spec still gives it, or doesn't resolve for a reason other than
// naming a section that's gone, such as a broken file being edited.
func (m *Manager) baseWanted(stem, hash string) bool {
	specs, err := readSpecs(filepath.Join(m.P.Bases(), stem+"-"+hash+".specs"))
	if err != nil {
		m.logf("gc: keeping %s-%s: %v", stem, hash, err)
		return true
	}
	for _, spec := range specs {
		r, err := m.Resolve(spec)
		switch {
		case err == nil:
			if r.stem() == stem && r.Hash == hash {
				return true
			}
		case !errors.Is(err, template.ErrNoSection):
			m.logf("gc: keeping %s-%s until -t %s loads: %v", stem, hash, spec, err)
			return true
		}
	}
	return false
}

// Rebuild builds new base generations for the given templates (same content
// shares one base). Existing VMs keep theirs.
func (m *Manager) Rebuild(ctx context.Context, targets []string) error {
	done := map[string]bool{}
	var errs []error
	for _, t := range targets {
		r, err := m.Resolve(t)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		key := r.stem() + "-" + r.Hash
		if done[key] {
			continue
		}
		done[key] = true
		if _, err := m.EnsureBase(ctx, r, true); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t, err)) // the others still get rebuilt
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}
