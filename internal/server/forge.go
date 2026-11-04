package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// forgeTimeout bounds a call that reaches a forge: its SSH server (a scan
// takes up to 30 s, a host-bound probe 20 s) or its API.
var forgeTimeout = 60 * time.Second

// forgeContext is r's context, bounded by forgeTimeout, with a TCP client's
// write deadline moved past it.
func forgeContext(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(forgeTimeout + requestTimeout))
	return context.WithTimeout(r.Context(), forgeTimeout)
}

// allowed reports whether the request's client has scope (over the unix
// socket, every scope); it answers one that hasn't.
func allowed(w http.ResponseWriter, r *http.Request, scope string) bool {
	if c, ok := CallerOf(r.Context()); ok && !slices.Contains(c.Scopes, scope) {
		writeError(w, api.CodeForbidden, "this call needs the "+scope+" scope (catchpen daemon client add --scope "+scope+")")
		return false
	}
	return true
}

// forgeInfo copies what the API may tell of a forge: its secrets' names,
// never their values.
func forgeInfo(f forge.Forge) api.ForgeInfo {
	i := api.ForgeInfo{Name: f.Name, Type: f.Type, Host: f.Host, WebURL: f.WebURL(), SSHPort: f.Port(), Mode: f.Auth.Mode,
		User: f.Auth.User, Scopes: api.List(f.Auth.Scopes), HostKeys: []string{}}
	i.TokenSecret, _ = strings.CutPrefix(f.Auth.Token, "secret:")
	i.PasswordSecret, _ = strings.CutPrefix(f.Auth.Password, "secret:")
	for _, k := range f.HostKeys {
		if pk, err := forge.ParseHostKey(k); err == nil {
			i.HostKeys = append(i.HostKeys, forge.Fingerprint(pk))
		}
	}
	return i
}

// writeForges answers with the forges in m's registry.
func (s *Server) writeForges(w http.ResponseWriter, r *http.Request, m *vm.Manager) {
	reg, err := forge.Load(m.P.Forges())
	if err != nil {
		s.fail(w, r, "the forges can't be read", err)
		return
	}
	out := make([]api.ForgeInfo, 0, len(reg.Forges))
	for _, f := range reg.Forges {
		out = append(out, forgeInfo(f))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) forges(w http.ResponseWriter, r *http.Request) {
	if m, ok := s.manager(w, r); ok {
		s.writeForges(w, r, m)
	}
}

func (s *Server) scanForge(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	var req api.ForgeScanRequest
	if !decode(w, r, &req) {
		return
	}
	f := forge.Forge{Name: "scan", Type: req.Type, Host: req.Host, HTTPURL: req.HTTPURL, SSHPort: req.SSHPort, Auth: forge.Auth{Mode: forge.ModeNone}}
	if err := f.Validate(); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	ctx, cancel := forgeContext(w, r)
	defer cancel()
	scan, err := (&forge.Client{HTTP: m.HTTP, F: f}).ScanHostKeys(ctx)
	if err != nil {
		writeError(w, api.CodeBadRequest, "the forge's host keys can't be read: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.ForgeScan{Fingerprints: api.List(scan.Fingerprints), Published: scan.Published})
}

// addForge pins the keys of a scan made now, if they are those the client
// confirmed.
func (s *Server) addForge(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	var req api.AddForgeRequest
	if !decode(w, r, &req) {
		return
	}
	f := forge.Forge{Name: req.Name, Type: req.Type, Host: req.Host, HTTPURL: req.HTTPURL, SSHPort: req.SSHPort,
		Auth: forge.Auth{Mode: req.Mode, User: req.User, Scopes: slices.Clone(req.Scopes)}}
	if req.TokenSecret != "" {
		f.Auth.Token = "secret:" + req.TokenSecret
	}
	if req.PasswordSecret != "" {
		f.Auth.Password = "secret:" + req.PasswordSecret
	}
	if len(f.Auth.Scopes) == 0 {
		f.Auth.Scopes = []string{"write:repository", "read:user"} // catchpen forge add's default
	}
	where := f // the name, type, host and URLs, checked before the host is reached
	where.Auth = forge.Auth{Mode: forge.ModeNone}
	if err := where.Validate(); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if f.UsesSSH() && len(req.Fingerprints) == 0 {
		writeError(w, api.CodeBadRequest, "auth mode "+f.Auth.Mode+" pins the forge's SSH host keys: scan them, and send the fingerprints you confirmed")
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	reg, err := forge.Load(m.P.Forges())
	if err != nil {
		s.fail(w, r, "the forges can't be read", err)
		return
	}
	if err := reg.CheckNew(f.Name); err != nil {
		writeError(w, api.CodeConflict, err.Error())
		return
	}
	ctx, cancel := forgeContext(w, r)
	defer cancel()
	if len(req.Fingerprints) > 0 {
		scan, err := (&forge.Client{HTTP: m.HTTP, F: f}).ScanHostKeys(ctx)
		if err != nil {
			writeError(w, api.CodeBadRequest, "the forge's host keys can't be read: "+err.Error())
			return
		}
		if !slices.Equal(slices.Sorted(slices.Values(req.Fingerprints)), slices.Sorted(slices.Values(scan.Fingerprints))) {
			writeError(w, api.CodeConflict, "the forge's host keys are not those confirmed: scan them again")
			return
		}
		f.HostKeys = scan.Keys
	}
	if err := f.Validate(); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if err := forge.Add(ctx, m.P.Forges(), f); err != nil { // the name taken since, or no host-bound auth
		writeError(w, api.CodeConflict, err.Error())
		return
	}
	i := forgeInfo(f)
	m.Client = clientOf(r)
	m.AuditEvent(audit.ForgeAdd, "", map[string]any{"forge": f.Name, "type": f.Type, "web_url": i.WebURL, "host": f.Host,
		"ssh_port": i.SSHPort, "mode": f.Auth.Mode, "host_keys": i.HostKeys, "token_secret": i.TokenSecret, "password_secret": i.PasswordSecret})
	writeJSON(w, http.StatusOK, i)
}

func (s *Server) testForge(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	reg, err := forge.Load(m.P.Forges())
	if err != nil {
		s.fail(w, r, "the forges can't be read", err)
		return
	}
	f, ok := reg.Get(r.PathValue("name"))
	if !ok {
		writeError(w, api.CodeNotFound, "no such forge")
		return
	}
	ctx, cancel := forgeContext(w, r)
	defer cancel()
	res, err := m.TestForge(ctx, f) // f from the registry: its secrets go only where it says
	c := api.ForgeCheck{API: res.API, Login: res.Login, HostKeys: res.HostKeys, HostBound: res.HostBound}
	if err != nil {
		c.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, c)
}

var errNoForge = errors.New("no such forge")

func (s *Server) deleteForge(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	var req api.DeleteForgeRequest
	if !decode(w, r, &req) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	reg, err := forge.Load(m.P.Forges())
	if err != nil {
		s.fail(w, r, "the forges can't be read", err)
		return
	}
	if _, ok := reg.Get(name); !ok {
		writeError(w, api.CodeNotFound, "no such forge")
		return
	}
	if !req.Confirm {
		writeError(w, api.CodeConfirmRequired, "removing forge "+name+` can't be undone (VMs keep their copy of it): send "confirm": true`)
		return
	}
	err = forge.Update(m.P.Forges(), func(reg *forge.Registry) error {
		i := slices.IndexFunc(reg.Forges, func(f forge.Forge) bool { return f.Name == name })
		if i < 0 {
			return errNoForge
		}
		reg.Forges = slices.Delete(reg.Forges, i, i+1)
		return nil
	})
	switch {
	case errors.Is(err, errNoForge): // removed since
		writeError(w, api.CodeNotFound, "no such forge")
	case err != nil:
		s.fail(w, r, "the forge can't be removed", err)
	default:
		m.Client = clientOf(r)
		m.AuditEvent(audit.ForgeRemove, "", map[string]any{"forge": name})
		s.writeForges(w, r, m)
	}
}

// pullOpts checks a pull's host repository: an absolute path, as clean as
// the server would make it (a relative one would be the daemon's working
// directory's). It answers a request it refuses.
func pullOpts(w http.ResponseWriter, req api.PullVMRequest) (vm.PullOpts, bool) {
	if !filepath.IsAbs(req.Into) || filepath.Clean(req.Into) != req.Into { // "" isn't absolute
		writeError(w, api.CodeBadRequest, "into is the absolute path of a git repository on the daemon's host")
		return vm.PullOpts{}, false
	}
	return vm.PullOpts{Repo: req.Repo, Branch: req.Branch, Into: req.Into}, true
}

// prOpts is the pull request req asks for, o being its pull's.
func prOpts(o vm.PullOpts, req api.PRVMRequest) vm.PROpts {
	return vm.PROpts{PullOpts: o, Title: req.Title, Body: req.Body, Draft: req.Draft, Base: req.Base, Head: req.Head}
}

func (s *Server) pullVM(w http.ResponseWriter, r *http.Request) {
	var req api.PullVMRequest
	if !decode(w, r, &req) {
		return
	}
	o, ok := pullOpts(w, req)
	if !ok {
		return
	}
	v, m, ok := s.findVM(w, r)
	if !ok || held(w, v, false) {
		return
	}
	s.startJob(w, r, m, api.JobExportPull, v.Name, func(ctx context.Context, m *vm.Manager) error {
		_, err := m.Pull(ctx, v.Name, o)
		return err
	})
}

func (s *Server) prVM(w http.ResponseWriter, r *http.Request) {
	var req api.PRVMRequest
	if !decode(w, r, &req) {
		return
	}
	o, ok := pullOpts(w, req.PullVMRequest)
	if !ok {
		return
	}
	// The host's git credentials push it: never onto a branch of the repo's
	// own, its default branch included.
	if req.Head != "" && !strings.HasPrefix(req.Head, "catchpen/") {
		writeError(w, api.CodeBadRequest, "head is a branch under catchpen/ (default catchpen/<vm>/<branch>)")
		return
	}
	v, m, ok := s.findVM(w, r)
	if !ok || held(w, v, false) {
		return
	}
	po := prOpts(o, req)
	s.startJob(w, r, m, api.JobExportPR, v.Name, func(ctx context.Context, m *vm.Manager) error {
		_, err := m.PR(ctx, v.Name, po)
		return err
	})
}
