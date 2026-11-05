package server

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/creds"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// readSecrets is the text of m's secrets.env and what it sets. Anything but
// a regular file is an error: a FIFO would block the read.
func readSecrets(m *vm.Manager) (confFile, map[string]string, error) {
	path := m.P.SecretsFile()
	f, err := readConf(path)
	if err != nil {
		return f, nil, err
	}
	vals, err := creds.ParseFile(path)
	return f, vals, err
}

// secretList is the names vals sets, never their values: those CheckName
// takes, with a value not only of = (a token pasted on a line of its own
// reads as NAME=value, its base64 padding the value: its "name" is a
// secret).
func secretList(vals map[string]string, cfg config.Config) []api.Secret {
	out := []api.Secret{}
	for _, name := range slices.Sorted(maps.Keys(vals)) {
		if v := vals[name]; strings.Trim(v, "=") == "" || creds.CheckName(name) != nil {
			continue // NAME= sets nothing; a value only of = is a pasted token's padding
		}
		_, shadowed := cfg.Secrets[name]
		out = append(out, api.Secret{Name: name, Shadowed: shadowed})
	}
	return out
}

func (s *Server) secrets(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	_, vals, err := readSecrets(m)
	if err != nil {
		s.fail(w, r, "secrets.env can't be read", err)
		return
	}
	writeJSON(w, http.StatusOK, secretList(vals, m.Cfg))
}

// editSecrets changes secrets.env under lockConfig: edit gets its text and
// returns the new text, or false having answered the request. The file must
// be one ParseFile reads. The change is written, keeping the file's mode
// (0600 for a new one), audited as event with detail, and answered with the
// names left.
func (s *Server) editSecrets(w http.ResponseWriter, r *http.Request, event string, detail map[string]any, edit func(text []byte) ([]byte, bool)) {
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	unlock, ok := s.lockConfig(w, r, m)
	if !ok {
		return
	}
	defer unlock()
	path := m.P.SecretsFile()
	if why := unreplaceable(path); why != "" {
		writeError(w, api.CodeConflict, "secrets.env"+why+": edit it by hand")
		return
	}
	cur, _, err := readSecrets(m)
	if err != nil {
		s.fail(w, r, "secrets.env can't be read", err)
		return
	}
	text, ok := edit(cur.text)
	if !ok {
		return
	}
	if err := fsutil.WriteFileAtomic(path, text, cur.mode); err != nil {
		s.fail(w, r, "secrets.env can't be written", err)
		return
	}
	m.Client = clientOf(r)
	m.AuditEvent(event, "", detail)
	_, vals, err := readSecrets(m)
	if err != nil {
		s.fail(w, r, "secrets.env can't be read", err)
		return
	}
	writeJSON(w, http.StatusOK, secretList(vals, m.Cfg))
}

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	var req api.SetSecretRequest
	if !decode(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if err := creds.CheckName(name); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	if err := creds.CheckValue(req.Value); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	detail := map[string]any{"name": name}
	s.editSecrets(w, r, audit.SecretSet, detail, func(text []byte) ([]byte, bool) {
		out, replaced, err := creds.Set(text, name, req.Value)
		if err != nil {
			writeError(w, api.CodeBadRequest, err.Error())
			return nil, false
		}
		if replaced && !req.Confirm {
			writeError(w, api.CodeConfirmRequired, "secrets.env sets "+name+` already, and its value would be lost: send "confirm": true`)
			return nil, false
		}
		detail["replaced"] = replaced
		return out, true
	})
}

func (s *Server) unsetSecret(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) {
		return
	}
	var req api.UnsetSecretRequest
	if !decode(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if err := creds.CheckName(name); err != nil {
		writeError(w, api.CodeBadRequest, err.Error())
		return
	}
	s.editSecrets(w, r, audit.SecretUnset, map[string]any{"name": name}, func(text []byte) ([]byte, bool) {
		out, found := creds.Unset(text, name)
		if !found {
			writeError(w, api.CodeNotFound, "secrets.env doesn't set "+name)
			return nil, false
		}
		if !req.Confirm {
			writeError(w, api.CodeConfirmRequired, "removing "+name+` from secrets.env can't be undone: send "confirm": true`)
			return nil, false
		}
		return out, true
	})
}
