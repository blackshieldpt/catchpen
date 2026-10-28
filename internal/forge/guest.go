package forge

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// GuestRepo is a repo to clone in the VM.
type GuestRepo struct {
	Repo  Repo
	Forge Forge
	Alias string // SSH host alias (deploy-key mode)
}

// Dir is the clone directory in the guest.
func (g GuestRepo) Dir() string { return "/workspace/" + g.Repo.Name }

// CloneURL is the remote URL the guest uses.
func (g GuestRepo) CloneURL() string {
	switch g.Forge.Auth.Mode {
	case ModeDeployKey:
		return g.Alias + ":" + g.Repo.Path() + ".git"
	case ModeAgent:
		return g.Forge.AgentAlias() + ":" + g.Repo.Path() + ".git"
	}
	return g.Forge.WebURL() + "/" + g.Repo.Path() + ".git"
}

// GuestCred is an HTTPS token credential for one forge.
type GuestCred struct {
	Forge Forge
	User  string
	Token string
}

var aliasBad = regexp.MustCompile(`[^a-z0-9-]+`)

// Alias names the SSH host alias for a deploy-keyed repo.
func Alias(r Repo) string {
	s := strings.ToLower(r.Forge + "-" + r.Owner + "-" + r.Name)
	return "catchpen-" + strings.Trim(aliasBad.ReplaceAllString(s, "-"), "-")
}

// SSHConfig renders ~/.ssh/catchpen_config: one block per deploy-keyed repo,
// tunnelled through the egress proxy with a pinned host key.
func SSHConfig(repos []GuestRepo, proxy string) string {
	var b strings.Builder
	b.WriteString("# written by catchpen at each start; do not edit\n")
	agentSeen := map[string]bool{}
	for _, g := range repos {
		if g.Forge.Auth.Mode == ModeAgent && !agentSeen[g.Forge.Name] {
			agentSeen[g.Forge.Name] = true
			// Keys come from the agent forwarded by `catchpen ssh` / ssh catchpen-<vm>.
			fmt.Fprintf(&b, `
Host %s
  HostName %s
  Port %d
  User git
  UserKnownHostsFile ~/.ssh/catchpen_known_hosts
  StrictHostKeyChecking yes
  ProxyCommand nc -X connect -x %s %%h %%p
`, g.Forge.AgentAlias(), g.Forge.Host, g.Forge.Port(), proxy)
		}
		if g.Forge.Auth.Mode != ModeDeployKey {
			continue
		}
		fmt.Fprintf(&b, `
Host %s
  HostName %s
  Port %d
  User git
  IdentityFile ~/.ssh/catchpen/%s
  IdentitiesOnly yes
  IdentityAgent none
  UserKnownHostsFile ~/.ssh/catchpen_known_hosts
  StrictHostKeyChecking yes
  ProxyCommand nc -X connect -x %s %%h %%p
`, g.Alias, g.Forge.Host, g.Forge.Port(), g.Alias, proxy)
	}
	return b.String()
}

// KnownHosts renders pinned host keys for the forges deploy keys use.
func KnownHosts(repos []GuestRepo) string {
	seen := map[string]bool{}
	var b strings.Builder
	for _, g := range repos {
		if g.Forge.UsesSSH() && !seen[g.Forge.Name] {
			seen[g.Forge.Name] = true
			b.WriteString(g.Forge.KnownHosts())
		}
	}
	return b.String()
}

// GitCredentials renders a git-credential-store file.
func GitCredentials(creds []GuestCred) string {
	var lines []string
	for _, c := range creds {
		u, _ := url.Parse(c.Forge.WebURL())
		u.User = url.UserPassword(c.User, c.Token)
		u.Path = ""
		lines = append(lines, u.String())
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// TeaConfig renders ~/.config/tea/config.yml for Gitea credentials.
func TeaConfig(creds []GuestCred) string {
	var b strings.Builder
	b.WriteString("logins:\n")
	first := true
	n := 0
	for _, c := range creds {
		if c.Forge.Type != Gitea {
			continue
		}
		n++
		fmt.Fprintf(&b, "  - name: %q\n    url: %q\n    token: %q\n    user: %q\n    default: %t\n", c.Forge.Name, c.Forge.WebURL(), c.Token, c.User, first)
		first = false
	}
	if n == 0 {
		return ""
	}
	return b.String()
}

// GitHubEnv returns the environment gh uses for a GitHub credential.
func GitHubEnv(creds []GuestCred) map[string]string {
	env := map[string]string{}
	for _, c := range creds {
		if c.Forge.Type != GitHub {
			continue
		}
		if c.Forge.Host == "github.com" {
			env["GH_TOKEN"] = c.Token
		} else {
			env["GH_HOST"], env["GH_ENTERPRISE_TOKEN"] = c.Forge.Host, c.Token
		}
	}
	return env
}
