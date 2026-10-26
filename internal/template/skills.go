package template

import (
	"cmp"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/blackshieldpt/curral/internal/gitsrc"
)

// Skill is a folder with a SKILL.md, copied into an agent's skills directory
// at each start. It comes from a pinned commit of a public git repo, fetched
// on the host.
type Skill struct {
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"` // "": every agent the VM has
	Git   string `yaml:"git" json:"git"`                         // https URL of the repo
	Ref   string `yaml:"ref" json:"ref"`                         // full 40-hex commit
	Path  string `yaml:"path" json:"path"`                       // folder in the repo; "." for its root
	Name  string `yaml:"name,omitempty" json:"name,omitempty"`   // folder name in the guest; default: the last element of path, or the repo's name for "."
}

// Plugin is an agent plugin: a folder from a pinned commit, or for pi a file
// that ships inside the installed pi package (Bundled).
type Plugin struct {
	Agent   string `yaml:"agent" json:"agent"`
	Git     string `yaml:"git,omitempty" json:"git,omitempty"`
	Ref     string `yaml:"ref,omitempty" json:"ref,omitempty"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
	Bundled string `yaml:"bundled,omitempty" json:"bundled,omitempty"` // pi: a path under the installed @earendil-works/pi-coding-agent package
	Name    string `yaml:"name,omitempty" json:"name,omitempty"`
}

// DirName is the skill's folder name in the guest.
func (s Skill) DirName() string {
	if s.Name != "" {
		return s.Name
	}
	return srcName(s.Git, s.Path)
}

// DirName is the plugin's name in the guest.
func (p Plugin) DirName() string {
	if p.Name != "" {
		return p.Name
	}
	if p.Bundled != "" {
		b := path.Base(p.Bundled)
		return strings.TrimSuffix(strings.TrimSuffix(b, ".ts"), ".js")
	}
	return srcName(p.Git, p.Path)
}

// srcName is the last element of p, or for the repo's root, of the git URL.
func srcName(git, p string) string {
	if p != "." {
		return path.Base(p)
	}
	u, err := url.Parse(git)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(path.Base(u.Path), ".git")
}

// DirNameRe is what a skill's or plugin's name in the guest must match.
var DirNameRe = regexp.MustCompile("^[a-z0-9][a-z0-9_.-]{0,63}$")

func validSource(git, ref, p string) error {
	u, err := url.Parse(git)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("git must be an https URL, got %q", git)
	}
	if u.User != nil {
		return fmt.Errorf("git must not hold a user or password")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("git must have no query or fragment, got %q", git)
	}
	if !gitsrc.IsCommit(ref) {
		return fmt.Errorf("ref must be a full 40-character commit id, got %q", ref)
	}
	if p == "." {
		return nil
	}
	return validPath("path", p)
}

// validPath checks a path inside a repo or package, which goes to git.
func validPath(field, p string) error {
	if err := gitsrc.CheckPath(p); err != nil {
		return fmt.Errorf("%s %v, got %q", field, err, p)
	}
	return nil
}

func validDirName(n string) error {
	if !DirNameRe.MatchString(n) {
		return fmt.Errorf("name %q must be lower case letters, digits, . _ -, starting with a letter or digit, at most 64 characters", n)
	}
	if strings.HasPrefix(n, "curral") {
		return fmt.Errorf("name %q is reserved: curral's own files start with curral", n)
	}
	return nil
}

func unknownAgent(a string) error {
	return fmt.Errorf("unknown agent %q (known: %s)", a, strings.Join(AgentNames(), ", "))
}

func (t *Template) validateSkillEntries() error {
	for _, s := range t.Skills {
		nameErr := validDirName(s.DirName())
		label := s.DirName()
		if nameErr != nil {
			label = s.Path
		}
		err := validSource(s.Git, s.Ref, s.Path)
		if s.Agent != "" && !slices.Contains(AgentNames(), s.Agent) {
			err = unknownAgent(s.Agent)
		}
		if err == nil {
			err = nameErr
		}
		if err != nil {
			return fmt.Errorf("skills %s: %w", label, err)
		}
	}
	for _, p := range t.Plugins {
		nameErr := validDirName(p.DirName())
		label := p.DirName()
		if nameErr != nil {
			label = cmp.Or(p.Path, p.Bundled)
		}
		var err error
		switch {
		case p.Agent == "":
			err = fmt.Errorf("agent is required")
		case !slices.Contains(AgentNames(), p.Agent):
			err = unknownAgent(p.Agent)
		case p.Git != "" && p.Bundled != "":
			err = fmt.Errorf("set git or bundled, not both")
		case p.Git == "" && p.Bundled == "":
			err = fmt.Errorf("set git or bundled")
		case p.Git != "":
			err = validSource(p.Git, p.Ref, p.Path)
		case p.Agent != "pi":
			err = fmt.Errorf("bundled is only for pi")
		case p.Bundled == ".":
			err = fmt.Errorf("bundled must name a file in the pi package, got %q", p.Bundled)
		default:
			err = validPath("bundled", p.Bundled)
		}
		if err == nil {
			err = nameErr
		}
		if err != nil {
			return fmt.Errorf("plugins %s: %w", label, err)
		}
	}
	return nil
}

// validateSkillNames rejects two skills or plugins with the same name in the
// guest, among the agents the template installs.
func (t *Template) validateSkillNames() error {
	for _, a := range AgentNames() {
		if t.Agents.Get(a) == nil {
			continue
		}
		seen := map[string]bool{}
		for _, s := range t.Skills {
			if s.Agent != "" && s.Agent != a {
				continue
			}
			n := s.DirName()
			if seen[n] {
				return fmt.Errorf("skills: two skills named %q for %s", n, a)
			}
			seen[n] = true
		}
		seen = map[string]bool{}
		for _, p := range t.Plugins {
			if p.Agent != a {
				continue
			}
			n := p.DirName()
			if seen[n] {
				return fmt.Errorf("plugins: two %s plugins named %q", a, n)
			}
			seen[n] = true
		}
	}
	return nil
}
