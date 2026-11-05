package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// hashOf is a configuration file's hash, as the API tells it.
func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// confFile is a configuration file as it is now.
type confFile struct {
	text   []byte
	hash   string // "" when there is no file
	exists bool
	mode   os.FileMode // what a write keeps
}

// readConf reads the configuration file at path; a missing one is no file.
// Anything but a regular file is an error: a FIFO would block the read.
func readConf(path string) (confFile, error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return confFile{mode: 0o600}, nil // it can hold secrets
	}
	if err != nil {
		return confFile{}, err
	}
	if !st.Mode().IsRegular() {
		return confFile{}, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return confFile{}, err
	}
	return confFile{text: b, hash: hashOf(b), exists: true, mode: st.Mode().Perm()}, nil
}

// replaceable checks, under its lock, that a write may replace the file at
// path, shown as name: it is a regular file (or none), not a symbolic link or
// a file with other hard links (renaming over one would cut it from the
// others), not read-only, and the file base was read from. It answers a
// request it refuses.
func (s *Server) replaceable(w http.ResponseWriter, r *http.Request, path, name, base string) (confFile, bool) {
	if st, err := os.Lstat(path); err == nil {
		why := ""
		switch sys, _ := st.Sys().(*syscall.Stat_t); {
		case st.Mode()&fs.ModeSymlink != 0:
			why = " is a symbolic link, which a write would replace"
		case !st.Mode().IsRegular():
			why = " is not a regular file"
		case sys != nil && sys.Nlink > 1:
			why = " has other hard links, which a write would leave as they are"
		case sys != nil && int(sys.Uid) != os.Getuid():
			why = " belongs to another user, whom a write would take it from"
		case st.Mode().Perm()&0o200 == 0:
			why = " is read-only"
		}
		if why != "" {
			writeError(w, api.CodeConflict, name+why+": edit it by hand")
			return confFile{}, false
		}
	}
	cur, err := readConf(path)
	if err != nil {
		s.fail(w, r, name+" can't be read", err)
		return cur, false
	}
	if base != cur.hash {
		writeError(w, api.CodeConflict, name+" changed since it was read: read it again")
		return cur, false
	}
	return cur, true
}

// written audits a configuration file's write by r's client.
func written(r *http.Request, m *vm.Manager, name, old, new string) {
	m.Client = clientOf(r)
	m.AuditEvent(audit.ConfigWrite, "", map[string]any{"file": name, "old_hash": old, "new_hash": new})
}

// configFile is config.yaml, as cfg, the settings it gives.
func configFile(f confFile, cfg config.Config) (api.ConfigFile, error) {
	eff, err := yaml.Marshal(cfg)
	return api.ConfigFile{Text: string(f.text), Hash: f.hash, Exists: f.exists, Effective: string(eff)}, err
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	path := m.P.ConfigFile()
	f, err := readConf(path)
	cfg := config.Default()
	if err == nil && f.exists {
		cfg, err = config.Parse(path, f.text) // the settings of the text told, not of the file m loaded
	}
	if err == nil {
		var c api.ConfigFile
		if c, err = configFile(f, cfg); err == nil {
			writeJSON(w, http.StatusOK, c)
			return
		}
	}
	s.fail(w, r, "config.yaml can't be read", err)
}

func (s *Server) writeConfig(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	var req api.WriteFileRequest
	if !decode(w, r, &req) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	unlock, ok := s.lockConfig(w, r, m)
	if !ok {
		return
	}
	defer unlock()
	path := m.P.ConfigFile()
	cur, ok := s.replaceable(w, r, path, "config.yaml", req.BaseHash)
	if !ok {
		return
	}
	old := config.Default()
	if cur.exists {
		var err error
		if old, err = config.Parse(path, cur.text); err != nil {
			s.fail(w, r, "config.yaml can't be loaded", err)
			return
		}
	}
	text := []byte(req.Text)
	c, err := config.Parse(path, text)
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if !reflect.DeepEqual(c.Server, old.Server) {
		writeError(w, api.CodeConflict, "server: decides whether and where the API is served, so a write over the API can't change it: edit config.yaml by hand")
		return
	}
	if err := checkSpecs(m.P, settings{old, m.Tpl}, settings{c, m.Tpl}); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if err := fsutil.WriteFileAtomic(path, text, cur.mode); err != nil {
		s.fail(w, r, "config.yaml can't be written", err)
		return
	}
	f := confFile{text: text, hash: hashOf(text), exists: true}
	written(r, m, "config.yaml", cur.hash, f.hash)
	out, err := configFile(f, c)
	if err != nil {
		s.fail(w, r, "config.yaml can't be read", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// templateCategory is the category the request names. It answers one that
// names none: the name becomes a file name.
func templateCategory(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := template.CategoryByName(r.PathValue("category"))
	if !ok {
		names := make([]string, 0, len(template.Categories))
		for _, c := range template.Categories {
			names = append(names, c.Name)
		}
		writeError(w, api.CodeNotFound, "no such template category ("+strings.Join(names, ", ")+")")
	}
	return c.Name, ok
}

// templateFile is category's user file f.
func templateFile(category string, f confFile) api.TemplateFile {
	return api.TemplateFile{Category: category, Text: string(f.text), Hash: f.hash, Exists: f.exists, Builtin: template.BuiltinFile(category)}
}

// settings is what checkSpecs compares: config.yaml's settings and the
// template files a loader reads.
type settings struct {
	cfg config.Config
	tpl template.Loader
}

// specs resolves, as a VM create resolves them (distro and cloud-init
// included), the templates catchpen builds without the user naming
// sections: the default, each profile and each pool entry's. It returns each
// one's error (nil: it resolves), labelled as the user would know it; err is
// set when the category files don't load.
func (st settings) specs(p paths.Paths) (map[string]error, error) {
	lib, err := st.tpl.Library()
	if err != nil {
		return nil, err
	}
	labels := map[string]string{"default": "-t default"}
	profiles, _ := lib.Sections("profiles")
	for _, sec := range profiles {
		labels["profiles:"+sec.Name] = "-t profiles:" + sec.Name
	}
	for _, e := range st.cfg.Pool {
		labels[e.Template] = "pool template " + e.Template
	}
	m := &vm.Manager{P: p, Cfg: st.cfg, Tpl: st.tpl}
	out := map[string]error{}
	for spec, label := range labels {
		t, err := lib.Load(spec)
		if err == nil {
			_, err = m.ResolveTemplate(t)
		}
		if err == nil {
			err = checkEgress(t)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "-t ") {
			err = fmt.Errorf("%s: %w", label, err)
		}
		out[spec] = err
	}
	return out, nil
}

// checkEgress is what a VM create checks of t's egress beyond resolving it
// (internal/vm/create.go): an allowlist's presets and entries.
func checkEgress(t *template.Template) error {
	if t.Network.Egress != "" && t.Network.Egress != egress.ModeAllowlist {
		return nil
	}
	if _, err := egress.ExpandPresets(t.Network.Presets); err != nil {
		return err
	}
	for _, e := range t.Network.Allow {
		if _, err := egress.ParseEntry(e); err != nil {
			return fmt.Errorf("network.allow: %w", err)
		}
	}
	return nil
}

// checkSpecs refuses a change from before to after that breaks what worked:
// the category files stop loading, or a template specs resolves stops
// resolving; a pool entry after adds must resolve. What was broken may stay
// so, and other sections are checked when picked, as after a hand edit: one
// may be meant only with another.
func checkSpecs(p paths.Paths, before, after settings) error {
	was, wasErr := before.specs(p)
	now, err := after.specs(p)
	if err != nil {
		if wasErr != nil {
			return nil // still broken: a VM create says so, as after a hand edit
		}
		return err
	}
	pool := map[string]bool{}
	for _, e := range before.cfg.Pool {
		pool[e.Template] = true
	}
	added := map[string]bool{}
	for _, e := range after.cfg.Pool {
		added[e.Template] = !pool[e.Template]
	}
	for _, spec := range slices.Sorted(maps.Keys(now)) {
		prev, existed := was[spec]
		if err := now[spec]; err != nil && ((existed && prev == nil) || added[spec]) {
			return err
		}
	}
	return nil
}

// withFile is m's template files, category's being text (nil: none).
func withFile(m *vm.Manager, category string, text []byte) template.Loader {
	return template.Loader{UserDir: m.P.Templates(), Files: map[string][]byte{category: text}}
}

func (s *Server) templateFile(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	cat, ok := templateCategory(w, r)
	if !ok {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	f, err := readConf(filepath.Join(m.P.Templates(), cat+".yaml"))
	if err != nil {
		s.fail(w, r, "templates/"+cat+".yaml can't be read", err)
		return
	}
	writeJSON(w, http.StatusOK, templateFile(cat, f))
}

// lockConfig takes the lock of m's configuration files, one for them all: a
// write to one checks them all. It answers a request it fails.
func (s *Server) lockConfig(w http.ResponseWriter, r *http.Request, m *vm.Manager) (func(), bool) {
	unlock, err := fsutil.Lock(filepath.Join(m.P.Config, "config.lock"))
	if err != nil {
		s.fail(w, r, "the configuration can't be locked", err)
		return nil, false
	}
	return unlock, true
}

// lockedConfig is config.yaml's settings under lockConfig, which a change to
// a template file is checked with: m's were loaded before it. It answers a
// request it fails.
func (s *Server) lockedConfig(w http.ResponseWriter, r *http.Request, m *vm.Manager) (config.Config, bool) {
	cfg, err := config.Load(m.P.ConfigFile())
	if err != nil {
		s.fail(w, r, "config.yaml can't be loaded", err)
	}
	return cfg, err == nil
}

func (s *Server) writeTemplateFile(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	cat, ok := templateCategory(w, r)
	if !ok {
		return
	}
	var req api.WriteFileRequest
	if !decode(w, r, &req) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	unlock, ok := s.lockConfig(w, r, m)
	if !ok {
		return
	}
	defer unlock()
	name, path := "templates/"+cat+".yaml", filepath.Join(m.P.Templates(), cat+".yaml")
	cur, ok := s.replaceable(w, r, path, name, req.BaseHash)
	if !ok {
		return
	}
	cfg, ok := s.lockedConfig(w, r, m)
	if !ok {
		return
	}
	text := []byte(req.Text)
	if err := checkSpecs(m.P, settings{cfg, m.Tpl}, settings{cfg, withFile(m, cat, text)}); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if err := fsutil.WriteFileAtomic(path, text, cur.mode); err != nil {
		s.fail(w, r, name+" can't be written", err)
		return
	}
	f := confFile{text: text, hash: hashOf(text), exists: true}
	written(r, m, name, cur.hash, f.hash)
	writeJSON(w, http.StatusOK, templateFile(cat, f))
}

func (s *Server) deleteTemplateFile(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	cat, ok := templateCategory(w, r)
	if !ok {
		return
	}
	var req api.DeleteFileRequest
	if !decode(w, r, &req) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	unlock, ok := s.lockConfig(w, r, m)
	if !ok {
		return
	}
	defer unlock()
	name, path := "templates/"+cat+".yaml", filepath.Join(m.P.Templates(), cat+".yaml")
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		writeError(w, api.CodeNotFound, name+" doesn't exist")
		return
	}
	cur, ok := s.replaceable(w, r, path, name, req.BaseHash)
	if !ok {
		return
	}
	if !req.Confirm {
		writeError(w, api.CodeConfirmRequired, "removing "+name+` can't be undone (the built-in sections apply again): send "confirm": true`)
		return
	}
	cfg, ok := s.lockedConfig(w, r, m)
	if !ok {
		return
	}
	if err := checkSpecs(m.P, settings{cfg, m.Tpl}, settings{cfg, withFile(m, cat, nil)}); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if err := os.Remove(path); err != nil {
		s.fail(w, r, name+" can't be removed", err)
		return
	}
	written(r, m, name, cur.hash, "")
	writeJSON(w, http.StatusOK, templateFile(cat, confFile{}))
}

func (s *Server) showTemplate(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	res, err := m.Resolve(r.URL.Query().Get("t"))
	if err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	b, err := res.T.YAML()
	if err != nil {
		s.fail(w, r, "the template can't be shown", err)
		return
	}
	parts := make([]api.TemplatePart, 0, len(res.T.Parts))
	for _, p := range res.T.Parts {
		parts = append(parts, api.TemplatePart{Category: p.Category, Sections: api.List(p.Sections), Default: p.Default})
	}
	writeJSON(w, http.StatusOK, api.TemplateShow{Spec: res.T.Name, Parts: parts, Base: res.BasePath(m.P), YAML: string(b)})
}
