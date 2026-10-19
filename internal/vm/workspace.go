package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/agents"
	"github.com/blackshieldpt/curral/internal/cloudinit"
	"github.com/blackshieldpt/curral/internal/creds"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/sshx"
)

const (
	forgeDir   = "forge"       // per-VM forge secrets: deploy keys, minted tokens
	tokensFile = "tokens.json" // in forgeDir
)

// RepoState is a repo a VM clones, with the forge resources created for it.
type RepoState struct {
	forge.Repo
	ReadOnly    bool   `json:"read_only,omitempty"`
	Alias       string `json:"alias,omitempty"`         // SSH alias (deploy-key)
	DeployKeyID int64  `json:"deploy_key_id,omitempty"` // to remove on destroy
}

// mintedToken is a Gitea token created for one VM.
type mintedToken struct {
	ID    int64  `json:"id"`
	User  string `json:"user"`
	Token string `json:"token"`
}

func (m *Manager) resolver() *creds.Resolver {
	return &creds.Resolver{Sources: m.Cfg.Secrets, File: m.P.SecretsFile()}
}

func (m *Manager) forges() (forge.Registry, error) { return forge.Load(m.P.Forges()) }

// secretRef resolves a forge auth field (secret:NAME); missing is an error.
func secretRef(ctx context.Context, r *creds.Resolver, ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "secret:")
	if !ok {
		return "", fmt.Errorf("expected secret:NAME, got %q", ref)
	}
	v, found, err := r.Lookup(ctx, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("secret %s not found on the host", name)
	}
	return v, nil
}

// planRepos resolves repo references to forges, before anything is created.
func (m *Manager) planRepos(refs []string, readOnly bool) ([]RepoState, map[string]forge.Forge, error) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	reg, err := m.forges()
	if err != nil {
		return nil, nil, err
	}
	var repos []RepoState
	used := map[string]forge.Forge{}
	dirs := map[string]string{}
	for _, ref := range refs {
		f, repo, err := reg.Resolve(ref)
		if err != nil {
			return nil, nil, err
		}
		if f.Loopback() {
			return nil, nil, fmt.Errorf("forge %s is on the host's loopback, which VMs can't reach; use an address or name the host serves on another interface", f.Name)
		}
		if prev, ok := dirs[repo.Name]; ok {
			if prev == repo.String() {
				continue
			}
			return nil, nil, fmt.Errorf("repos %s and %s would both clone into /workspace/%s", prev, repo, repo.Name)
		}
		dirs[repo.Name] = repo.String()
		rs := RepoState{Repo: repo, ReadOnly: readOnly}
		if f.Auth.Mode == forge.ModeDeployKey {
			rs.Alias = forge.Alias(repo)
		}
		repos = append(repos, rs)
		used[f.Name] = f
	}
	return repos, used, nil
}

func forgeHosts(fs map[string]forge.Forge) []string {
	var hosts []string
	for _, f := range fs {
		hosts = append(hosts, f.Hosts()...)
	}
	return hosts
}

// provisionForges creates deploy keys and minted tokens and checks user tokens.
// It records what it created in s as it goes, so a failure can be undone.
func (m *Manager) provisionForges(ctx context.Context, s *State) error {
	if len(s.Repos) == 0 {
		return nil
	}
	sec := m.resolver()
	dir := m.file(s.Name, forgeDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tokens := map[string]mintedToken{}
	for name, f := range s.Forges {
		switch f.Auth.Mode {
		case forge.ModeMintedToken:
			pw, err := secretRef(ctx, sec, f.Auth.Password)
			if err != nil {
				return fmt.Errorf("forge %s: %w", name, err)
			}
			c := &forge.Client{HTTP: m.HTTP, F: f, User: f.Auth.User, Password: pw}
			rnd := make([]byte, 4)
			_, _ = rand.Read(rnd)
			id, tok, err := c.MintToken(ctx, "curral-"+s.Name+"-"+hex.EncodeToString(rnd), f.Auth.Scopes)
			if err != nil {
				return fmt.Errorf("forge %s: mint token: %w", name, err)
			}
			tokens[name] = mintedToken{ID: id, User: f.Auth.User, Token: tok}
			if err := writeJSON(filepath.Join(dir, tokensFile), tokens); err != nil {
				return err
			}
			m.logf("minted a %s token for %s", name, s.Name)
		case forge.ModeToken:
			tok, err := secretRef(ctx, sec, f.Auth.Token)
			if err != nil {
				return fmt.Errorf("forge %s: %w", name, err)
			}
			login, err := (&forge.Client{HTTP: m.HTTP, F: f, Token: tok}).Login(ctx)
			if err != nil {
				return fmt.Errorf("forge %s: checking token: %w", name, err)
			}
			if s.ForgeUsers == nil {
				s.ForgeUsers = map[string]string{}
			}
			s.ForgeUsers[name] = login
		}
	}
	for i := range s.Repos {
		r := &s.Repos[i]
		f := s.Forges[r.Forge]
		if f.Auth.Mode != forge.ModeDeployKey {
			continue
		}
		tok, err := secretRef(ctx, sec, f.Auth.Token)
		if err != nil {
			return fmt.Errorf("forge %s: %w", f.Name, err)
		}
		priv, pub, err := sshx.GenerateKey("curral@" + s.Name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, r.Alias), priv, 0o600); err != nil {
			return err
		}
		id, err := (&forge.Client{HTTP: m.HTTP, F: f, Token: tok}).AddDeployKey(ctx, r.Repo, "curral "+s.Name, strings.TrimSpace(string(pub)), r.ReadOnly)
		if err != nil {
			return fmt.Errorf("%s: add deploy key: %w", r.Repo, err)
		}
		r.DeployKeyID = id
		mode := "read-write"
		if r.ReadOnly {
			mode = "read-only"
		}
		m.logf("added a %s deploy key to %s", mode, r.Repo)
	}
	return nil
}

// releaseForges removes the deploy keys and tokens created for a VM. It is best
// effort: failures are reported so the user can clean up by hand.
func (m *Manager) releaseForges(s *State) {
	if s == nil || len(s.Repos) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sec := m.resolver()
	for _, r := range s.Repos {
		if r.DeployKeyID == 0 {
			continue
		}
		f := s.Forges[r.Forge]
		tok, err := secretRef(ctx, sec, f.Auth.Token)
		if err == nil {
			err = (&forge.Client{HTTP: m.HTTP, F: f, Token: tok}).DeleteDeployKey(ctx, r.Repo, r.DeployKeyID)
		}
		if err != nil {
			m.logf("warning: could not remove deploy key %d from %s: %v (remove it in the forge's repo settings)", r.DeployKeyID, r.Repo, err)
		}
	}
	tokens, _ := m.mintedTokens(s.Name)
	for name, t := range tokens {
		f := s.Forges[name]
		pw, err := secretRef(ctx, sec, f.Auth.Password)
		if err == nil {
			err = (&forge.Client{HTTP: m.HTTP, F: f, User: t.User, Password: pw}).DeleteToken(ctx, t.ID)
		}
		if err != nil {
			m.logf("warning: could not delete %s token %d for %s: %v (delete it in the user's settings)", name, t.ID, t.User, err)
		}
	}
}

func (m *Manager) mintedTokens(name string) (map[string]mintedToken, error) {
	b, err := os.ReadFile(filepath.Join(m.file(name, forgeDir), tokensFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t map[string]mintedToken
	return t, json.Unmarshal(b, &t)
}

// forgeCreds resolves the HTTPS credentials a VM gets, at push time.
func (m *Manager) forgeCreds(ctx context.Context, s *State) ([]forge.GuestCred, error) {
	tokens, err := m.mintedTokens(s.Name)
	if err != nil {
		return nil, err
	}
	var out []forge.GuestCred
	for name, f := range s.Forges {
		switch f.Auth.Mode {
		case forge.ModeMintedToken:
			if t, ok := tokens[name]; ok {
				out = append(out, forge.GuestCred{Forge: f, User: t.User, Token: t.Token})
			}
		case forge.ModeToken:
			tok, err := secretRef(ctx, m.resolver(), f.Auth.Token)
			if err != nil {
				return nil, fmt.Errorf("forge %s: %w", name, err)
			}
			out = append(out, forge.GuestCred{Forge: f, User: s.ForgeUsers[name], Token: tok})
		}
	}
	return out, nil
}

// pushForges writes the guest's forge SSH config, keys and git credentials,
// then clones repos that aren't there yet.
func (m *Manager) pushForges(ctx context.Context, r agents.Remote, s *State, gcreds []forge.GuestCred) error {
	if len(s.Repos) == 0 {
		return nil
	}
	var repos []forge.GuestRepo
	for _, rs := range s.Repos {
		repos = append(repos, forge.GuestRepo{Repo: rs.Repo, Forge: s.Forges[rs.Forge], Alias: rs.Alias})
	}
	proxy := fmt.Sprintf("%s:%d", cloudinit.ProxyAddr, cloudinit.ProxyPort)
	files := map[string]string{
		".ssh/curral_config":             forge.SSHConfig(repos, proxy),
		".ssh/curral_known_hosts":        forge.KnownHosts(repos),
		".config/curral/git-credentials": forge.GitCredentials(gcreds),
	}
	if tea := forge.TeaConfig(gcreds); tea != "" {
		files[".config/tea/config.yml"] = tea
	}
	for _, rs := range s.Repos {
		if rs.Alias != "" {
			key, err := os.ReadFile(filepath.Join(m.file(s.Name, forgeDir), rs.Alias))
			if err != nil {
				return err
			}
			files[".ssh/curral/"+rs.Alias] = string(key)
		}
	}
	for rel, content := range files {
		if err := agents.WriteFile(ctx, r, rel, []byte(content)); err != nil {
			return err
		}
	}
	script := `set -e
touch ~/.ssh/config && chmod 600 ~/.ssh/config
grep -qxF 'Include ~/.ssh/curral_config' ~/.ssh/config || { printf 'Include ~/.ssh/curral_config\n\n'; cat ~/.ssh/config; } > ~/.ssh/config.curral-tmp
[ ! -f ~/.ssh/config.curral-tmp ] || mv -f ~/.ssh/config.curral-tmp ~/.ssh/config
git config --global credential.helper "store --file=$HOME/.config/curral/git-credentials"
[ -d /workspace ] || sudo install -d -o "$(id -u)" -g "$(id -g)" /workspace
`
	if _, err := r.Run(ctx, script, nil); err != nil {
		return fmt.Errorf("forge setup: %w", err)
	}
	for _, g := range repos {
		cmd := fmt.Sprintf(`[ -d %[1]s/.git ] && exit 0; GIT_TERMINAL_PROMPT=0 git clone -q %[2]s %[1]s 2>&1`, shQuote(g.Dir()), shQuote(g.CloneURL()))
		if out, err := r.Run(ctx, cmd, nil); err != nil {
			return fmt.Errorf("clone %s: %w\n%s", g.Repo, err, sshx.Printable(strings.TrimSpace(string(out))))
		}
	}
	return nil
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
