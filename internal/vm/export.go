package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/sshx"
)

const maxBundle = 2 << 30

type PullOpts struct {
	Repo    string // repo name (or forge:owner/repo); optional when the VM has one repo
	Branch  string // default: the clone's current branch
	Into    string // host repository (default ".")
	Patches string // also write format-patch output into this directory
}

// Pulled describes an imported branch.
type Pulled struct {
	Repo   RepoState
	Branch string
	Ref    string // refs/curral/<vm>/<branch> in the host repo
	Into   string
}

var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

func validBranch(b string) error {
	if !branchRe.MatchString(b) || strings.Contains(b, "..") || strings.Contains(b, "//") || strings.HasSuffix(b, "/") ||
		strings.HasSuffix(b, ".lock") || strings.HasSuffix(b, ".") || strings.Contains(b, "/.") {
		return fmt.Errorf("unsupported branch name %q", b)
	}
	return nil
}

func (s *State) repo(ref string) (RepoState, error) {
	if ref == "" {
		if len(s.Repos) == 1 {
			return s.Repos[0], nil
		}
		return RepoState{}, fmt.Errorf("%s has %d repos; pick one with --repo", s.Name, len(s.Repos))
	}
	for _, r := range s.Repos {
		if ref == r.Name || ref == r.Path() || ref == r.String() {
			return r, nil
		}
	}
	return RepoState{}, fmt.Errorf("%s has no repo %q", s.Name, ref)
}

func hostGitCmd(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// No hooks, no fsmonitor; reject malformed objects from the guest.
	base := []string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "transfer.fsckObjects=true"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, sshx.Printable(strings.TrimSpace(string(out))))
	}
	return out, nil
}

// Pull copies a branch out of the VM as a git bundle and fetches it into
// refs/curral/<vm>/<branch> of a host repository. Nothing from the guest runs
// on the host, and the host working tree is untouched.
func (m *Manager) Pull(ctx context.Context, name string, o PullOpts) (*Pulled, error) {
	s, err := m.Load(name)
	if err != nil {
		return nil, err
	}
	rs, err := s.repo(o.Repo)
	if err != nil {
		return nil, err
	}
	into := o.Into
	if into == "" {
		into = "."
	}
	if into, err = filepath.Abs(into); err != nil {
		return nil, err
	}
	if _, err := hostGitCmd(ctx, into, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository (use --into): %w", into, err)
	}
	port, err := m.SSHPort(name)
	if err != nil {
		return nil, err
	}
	c, err := m.keys(name).Dial(ctx, port, s.User)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dir := shQuote(forge.GuestRepo{Repo: rs.Repo}.Dir())
	branch := o.Branch
	if branch == "" {
		out, err := sshx.Run(ctx, c, "git -C "+dir+" symbolic-ref --short HEAD", nil)
		if err != nil {
			return nil, fmt.Errorf("finding the current branch: %w", err)
		}
		branch = strings.TrimSpace(string(out))
	}
	if err := validBranch(branch); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "curral-bundle-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	stderr, err := sshx.Stream(ctx, c, "git -C "+dir+" bundle create - "+shQuote("refs/heads/"+branch), tmp, maxBundle)
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w\n%s", branch, err, sshx.Printable(strings.TrimSpace(string(stderr))))
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if _, err := hostGitCmd(ctx, into, "bundle", "verify", "-q", tmp.Name()); err != nil {
		return nil, fmt.Errorf("bundle from %s failed verification: %w", name, err)
	}
	ref := "refs/curral/" + name + "/" + branch
	if _, err := hostGitCmd(ctx, into, "fetch", "--no-tags", "--no-write-fetch-head", tmp.Name(), "+refs/heads/"+branch+":"+ref); err != nil {
		return nil, err
	}
	if o.Patches != "" {
		if err := os.MkdirAll(o.Patches, 0o755); err != nil {
			return nil, err
		}
		f, err := os.Create(filepath.Join(o.Patches, name+"-"+strings.ReplaceAll(branch, "/", "-")+".mbox"))
		if err != nil {
			return nil, err
		}
		stderr, err := sshx.Stream(ctx, c, "git -C "+dir+" format-patch --stdout origin/HEAD.."+shQuote(branch), f, maxBundle)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("format-patch: %w\n%s", err, sshx.Printable(strings.TrimSpace(string(stderr))))
		}
	}
	m.logf("fetched %s:%s into %s (%s)", name, branch, into, ref)
	m.audit(audit.ExportPull, name, map[string]any{"repo": rs.Repo.String(), "branch": branch, "into": into, "ref": ref})
	return &Pulled{Repo: rs, Branch: branch, Ref: ref, Into: into}, nil
}

type PROpts struct {
	PullOpts
	Title    string
	BodyFile string
	Draft    bool
	Base     string // default: the repo's default branch
	Head     string // branch to push on the forge (default curral/<vm>/<branch>)
}

// PR pulls a branch, pushes it to the forge with the host's git credentials
// (through a remote of the host repo that points at the same forge repo) and
// opens a pull request with the forge API token.
func (m *Manager) PR(ctx context.Context, name string, o PROpts) (string, error) {
	p, err := m.Pull(ctx, name, o.PullOpts)
	if err != nil {
		return "", err
	}
	s, err := m.Load(name)
	if err != nil {
		return "", err
	}
	f := s.Forges[p.Repo.Forge]
	remote, err := findRemote(ctx, p.Into, f, p.Repo.Repo)
	if err != nil {
		return "", err
	}
	head := o.Head
	if head == "" {
		head = "curral/" + name + "/" + p.Branch
	}
	if err := validBranch(head); err != nil {
		return "", err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := hostGitCmd(pctx, p.Into, "push", remote, p.Ref+":refs/heads/"+head); err != nil {
		return "", err
	}
	c, err := m.apiClient(ctx, f)
	if err != nil {
		return "", err
	}
	base := o.Base
	if base == "" {
		if base, err = c.DefaultBranch(ctx, p.Repo.Repo); err != nil {
			return "", err
		}
	}
	title := o.Title
	if title == "" {
		out, err := hostGitCmd(ctx, p.Into, "log", "-1", "--format=%s", p.Ref)
		if err != nil {
			return "", err
		}
		title = strings.TrimSpace(string(out))
	}
	body := fmt.Sprintf("Opened by curral from VM `%s` (branch `%s`).", name, p.Branch)
	if o.BodyFile != "" {
		b, err := os.ReadFile(o.BodyFile)
		if err != nil {
			return "", err
		}
		body = string(b)
	}
	url, err := c.CreatePR(ctx, p.Repo.Repo, head, base, title, body, o.Draft)
	if err != nil {
		return "", err
	}
	m.logf("opened %s", url)
	m.audit(audit.ExportPR, name, map[string]any{"repo": p.Repo.Repo.String(), "branch": head, "base": base, "url": url})
	return url, nil
}

// apiClient builds a host-side API client for PRs: the forge's token, or for
// minted-token forges the account's basic auth.
func (m *Manager) apiClient(ctx context.Context, f forge.Forge) (*forge.Client, error) {
	sec := m.resolver()
	if f.Auth.Token != "" {
		tok, err := secretRef(ctx, sec, f.Auth.Token)
		if err != nil {
			return nil, fmt.Errorf("forge %s: %w", f.Name, err)
		}
		return &forge.Client{HTTP: m.HTTP, F: f, Token: tok}, nil
	}
	if f.Auth.Password != "" {
		pw, err := secretRef(ctx, sec, f.Auth.Password)
		if err != nil {
			return nil, fmt.Errorf("forge %s: %w", f.Name, err)
		}
		return &forge.Client{HTTP: m.HTTP, F: f, User: f.Auth.User, Password: pw, Basic: true}, nil
	}
	return nil, fmt.Errorf("forge %s has no API credentials (auth.token) to open pull requests", f.Name)
}

// findRemote returns the host repo's remote for repo on forge f.
func findRemote(ctx context.Context, dir string, f forge.Forge, repo forge.Repo) (string, error) {
	out, err := hostGitCmd(ctx, dir, "remote")
	if err != nil {
		return "", err
	}
	reg := forge.Registry{Forges: []forge.Forge{f}}
	for _, name := range strings.Fields(string(out)) {
		u, err := hostGitCmd(ctx, dir, "remote", "get-url", "--push", name)
		if err != nil {
			continue
		}
		if _, r, err := reg.Resolve(strings.TrimSpace(string(u))); err == nil && strings.EqualFold(r.Path(), repo.Path()) {
			return name, nil
		}
	}
	return "", errors.New("no remote in " + dir + " points at " + f.WebURL() + "/" + repo.Path() + " (git remote add it)")
}
