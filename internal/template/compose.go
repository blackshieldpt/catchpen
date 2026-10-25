package template

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Category is one file of sections (<name>.yaml). Its sections may set only
// Fields. With -t naming none of its sections, its default applies; picks add
// to an additive category's default (-name removes one of its sections) and
// replace an exclusive category's.
type Category struct {
	Name      string
	Fields    []string
	Exclusive bool
	About     string
}

// Categories in the order their sections are merged. agents comes before
// runtimes so a runtime pick overrides the Node version agents ask for.
var Categories = []Category{
	{"distros", []string{"distro"}, true, "the guest distro"},
	{"resources", []string{"resources"}, true, "CPUs, memory and disk"},
	{"tools", []string{"packages", "tools", "scripts", "files"}, false, "apt packages, gh/tea, build scripts and files"},
	{"agents", []string{"agents", "runtimes"}, false, "coding agents, with the runtimes they need"},
	{"runtimes", []string{"runtimes", "docker"}, false, "Node, Go, Python and Docker"},
	{"network", []string{"network"}, false, "egress mode and allowlist presets"},
	{"caches", []string{"cache"}, false, "host package caches"},
	{"canaries", []string{"canaries"}, false, "decoy credentials"},
	{"env", []string{"env"}, false, "environment for ssh sessions"},
	{"mcp", []string{"mcp"}, false, "MCP servers for all agents"},
	{"skills", []string{"skills"}, false, "agent skills, from pinned git commits, copied in at each start"},
	{"plugins", []string{"plugins"}, false, "agent plugins, from pinned git commits, copied in and enabled at each start"},
	{"ssh", []string{"ssh"}, true, "ssh-agent forwarding"},
	{"sync", []string{"sync"}, false, "host settings copied in"},
	{"repos", []string{"repos"}, false, "repos cloned into /workspace"},
	{"ports", []string{"ports"}, false, "guest ports forwarded to the host"},
	{"idle", []string{"idle_stop"}, true, "stopping the VM when idle"},
	{"profiles", nil, false, "named lists of picks from any category"},
}

// CategoryByName finds a category.
func CategoryByName(name string) (Category, bool) {
	for _, c := range Categories {
		if c.Name == name {
			return c, true
		}
	}
	return Category{}, false
}

// Part is the sections one category contributed to a template.
type Part struct {
	Category string
	Sections []string
	Default  bool // the category's default, unchanged
}

// Section is a named entry of a category file: settings, or a list of picks.
type Section struct {
	Category    string
	Name        string
	Description string
	Group       []string // picks, for a list section
	Source      string   // "builtin" or the file
	t           *Template
	fsys        fs.FS // where its scripts and files are read, when picked
}

// Loader reads the built-in category files, then the user's from UserDir,
// whose sections replace built-in ones of the same name.
type Loader struct {
	UserDir string
}

var (
	sectionRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,47}$`)
	maxSpec   = 160 // longer lists of picks belong in a profile
)

// ErrNoSection marks a -t that names a section or category that doesn't
// exist (gc takes the bases of such specs for unused; other errors mean a
// broken file).
var ErrNoSection = errors.New("no such section")

type missing string

func (e missing) Error() string        { return string(e) }
func (e missing) Is(target error) bool { return target == ErrNoSection }

// Library is every category's sections and default.
type Library struct {
	sections map[string]map[string]*Section // category, name
	defaults map[string]*Section
}

// Library loads all category files.
func (l Loader) Library() (*Library, error) {
	lib := &Library{sections: map[string]map[string]*Section{}, defaults: map[string]*Section{}}
	if l.UserDir != "" {
		if err := oldFiles(l.UserDir); err != nil {
			return nil, err
		}
	}
	for _, c := range Categories {
		lib.sections[c.Name] = map[string]*Section{}
		b, err := fs.ReadFile(builtinFS, "builtin/"+c.Name+".yaml")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err := lib.add(c, b, "builtin", nil); err != nil {
			return nil, err
		}
		if l.UserDir == "" {
			continue
		}
		p := filepath.Join(l.UserDir, c.Name+".yaml")
		b, err = os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := lib.add(c, b, p, os.DirFS(l.UserDir)); err != nil {
			return nil, err
		}
	}
	return lib, nil
}

// add parses one category file; assets (scripts, files) resolve in fsys.
func (lib *Library) add(c Category, b []byte, source string, fsys fs.FS) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
		return nil // empty
	} else if err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	for {
		var more yaml.Node
		err := dec.Decode(&more)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(more.Content) > 0 {
			return fmt.Errorf("%s: one YAML document only (remove the ---)", source)
		}
	}
	if len(doc.Content) == 0 {
		return nil // only comments
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected sections (name: settings)", source)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		name, v := m.Content[i].Value, m.Content[i+1]
		if name != "default" && !sectionRe.MatchString(name) {
			return fmt.Errorf("%s:%d: invalid section name %q (lower case letters, digits, . _ -, starting with a letter or digit, at most 48 characters)", source, m.Content[i].Line, name)
		}
		// An old-style template's settings, where sections belong.
		if (name == "extends" || name == "description" || slices.Contains(c.Fields, name)) && v.Kind != yaml.MappingNode {
			return fmt.Errorf("%s:%d: %s: this looks like an old-style template, which curral no longer reads. Move its settings into sections (see https://blackshieldpt.github.io/curral/templates/)", source, m.Content[i].Line, name)
		}
		if seen[name] {
			return fmt.Errorf("%s:%d: %s is defined twice", source, m.Content[i].Line, name)
		}
		seen[name] = true
		if name == "default" && c.Name == "profiles" {
			return fmt.Errorf("%s:%d: profiles.yaml has no default; set each category's default in its own file", source, m.Content[i].Line)
		}
		s, err := parseSection(c, name, v, source, fsys)
		if err != nil {
			return err
		}
		if name == "default" {
			lib.defaults[c.Name] = s
		} else {
			lib.sections[c.Name][name] = s
		}
	}
	return nil
}

func parseSection(c Category, name string, v *yaml.Node, source string, fsys fs.FS) (*Section, error) {
	s := &Section{Category: c.Name, Name: name, Source: source}
	where := fmt.Sprintf("%s:%d: %s", source, v.Line, name)
	switch v.Kind {
	case yaml.ScalarNode:
		if v.Value == "" || v.Tag == "!!null" {
			return nil, fmt.Errorf("%s: empty (uncomment the lines under it too, or write [] for an empty list)", where)
		}
		s.Group = []string{v.Value}
		return s, nil
	case yaml.SequenceNode:
		if err := v.Decode(&s.Group); err != nil {
			return nil, fmt.Errorf("%s: a list section holds names: %w", where, err)
		}
		return s, nil
	case yaml.MappingNode:
	default:
		return nil, fmt.Errorf("%s: expected settings, a name or a list of names", where)
	}
	if c.Name == "profiles" {
		return nil, fmt.Errorf("%s: profiles are lists of picks, e.g. [go-1.27, large]", where)
	}
	for i := 0; i < len(v.Content); i += 2 {
		k := v.Content[i].Value
		if k != "description" && !slices.Contains(c.Fields, k) {
			return nil, fmt.Errorf("%s: %s doesn't belong in %s.yaml (it takes %s)", where, k, c.Name, strings.Join(c.Fields, ", "))
		}
	}
	var buf bytes.Buffer
	if err := yaml.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	t := &Template{}
	err := dec.Decode(t)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	s.Description, t.Description = t.Description, ""
	check := func(r string) (string, error) {
		clean := path.Clean(r)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
			return "", fmt.Errorf("%s: %q must be a relative path inside the templates directory", where, r)
		}
		if fsys == nil {
			return "", fmt.Errorf("%s: built-in sections can't reference %q", where, r)
		}
		return clean, nil
	}
	for i, r := range t.Scripts {
		if t.Scripts[i], err = check(r); err != nil {
			return nil, err
		}
	}
	for i, f := range t.Files {
		if t.Files[i].Src, err = check(f.Src); err != nil {
			return nil, err
		}
	}
	s.t, s.fsys = t, fsys
	return s, nil
}

// assets reads a picked section's scripts and files into t.
func (s *Section) assets(t *Template) error {
	refs := append([]string{}, s.t.Scripts...)
	for _, f := range s.t.Files {
		refs = append(refs, f.Src)
	}
	for _, r := range refs {
		data, err := fs.ReadFile(s.fsys, path.Clean(r))
		if err != nil {
			return fmt.Errorf("%s: %s: %q: %w", s.Source, s.Name, r, err)
		}
		t.assets[r] = data
	}
	return nil
}

// Sections lists a category's sections by name, and its default.
func (lib *Library) Sections(category string) ([]*Section, *Section) {
	var out []*Section
	for _, n := range slices.Sorted(maps.Keys(lib.sections[category])) {
		out = append(out, lib.sections[category][n])
	}
	return out, lib.defaults[category]
}

// Load builds the template for spec, a comma-separated list of picks: section
// names, optionally category:name, or -name to drop a section from its
// category's default. "" and "default" mean every category's default.
func (l Loader) Load(spec string) (*Template, error) {
	lib, err := l.Library()
	if err != nil {
		return nil, err
	}
	return lib.Load(spec)
}

type pick struct {
	remove bool
	direct bool // named in -t itself, not through a list
	s      *Section
}

// Load builds the template for spec (see Loader.Load).
func (lib *Library) Load(spec string) (*Template, error) {
	name := NormalizeSpec(spec)
	if len(name) > maxSpec {
		return nil, fmt.Errorf("-t %s: longer than %d characters; put the picks in a profile (profiles.yaml)", name, maxSpec)
	}
	var picks []pick
	if name != "default" {
		for _, item := range strings.Split(name, ",") {
			if err := lib.expand(item, "", true, 0, &picks); err != nil {
				return nil, err
			}
		}
	}
	res := &Template{Name: name, assets: map[string][]byte{}}
	for _, c := range Categories {
		if c.Name == "profiles" {
			continue
		}
		def, err := lib.defaultSections(c)
		if err != nil {
			return nil, err
		}
		// Picks from lists apply first, then those named in -t, each in
		// order: a pick in -t wins over a profile's. Picking a section again
		// moves it last, so its settings win. In a one-choice category the
		// last pick wins; removals from lists of a section that isn't there
		// are no-ops.
		chosen := slices.Clone(def)
		var direct *Section
		ordered := slices.Concat(
			slices.DeleteFunc(slices.Clone(picks), func(p pick) bool { return p.direct }),
			slices.DeleteFunc(slices.Clone(picks), func(p pick) bool { return !p.direct }))
		for _, p := range ordered {
			switch {
			case p.s.Category != c.Name:
			case c.Exclusive && p.remove:
				if p.direct {
					return nil, fmt.Errorf("-%s: %s takes one section, so pick another instead of removing one", p.s.Name, c.Name)
				}
				chosen = slices.DeleteFunc(chosen, func(x *Section) bool { return x == p.s })
			case c.Exclusive:
				if p.direct {
					if direct != nil && direct != p.s {
						return nil, fmt.Errorf("%s takes one section, got %s and %s", c.Name, direct.Name, p.s.Name)
					}
					direct = p.s
				}
				chosen = []*Section{p.s}
			case p.remove:
				if i := slices.Index(chosen, p.s); i >= 0 {
					chosen = slices.Delete(chosen, i, i+1)
				} else if p.direct && !slices.Contains(def, p.s) {
					return nil, fmt.Errorf("-%s: not in %s's default (%s)", p.s.Name, c.Name, names(def))
				}
			default:
				chosen = append(slices.DeleteFunc(chosen, func(x *Section) bool { return x == p.s }), p.s)
			}
		}
		if c.Exclusive && len(def) > 0 && len(chosen) == 0 {
			return nil, fmt.Errorf("%s takes one section, and a list removed %s; pick another", c.Name, names(def))
		}
		merged := chosen
		if c.Name == "resources" && !sameSections(chosen, def) {
			merged = append(slices.Clone(def), chosen...) // a size may set only some fields
		}
		for _, s := range merged {
			res.merge(s.t)
			if err := s.assets(res); err != nil {
				return nil, err
			}
		}
		if len(chosen) > 0 {
			res.Parts = append(res.Parts, Part{Category: c.Name, Sections: sectionNames(chosen), Default: slices.Equal(chosen, def)})
		}
	}
	if err := res.validateSkillEntries(); err != nil {
		return nil, fmt.Errorf("-t %s: %w", name, err)
	}
	// Skills and plugins for agents the VM doesn't have are dropped, so defaults can list every agent's.
	res.Skills = slices.DeleteFunc(res.Skills, func(s Skill) bool {
		return (s.Agent == "" && !res.Agents.Any()) || (s.Agent != "" && res.Agents.Get(s.Agent) == nil)
	})
	res.Plugins = slices.DeleteFunc(res.Plugins, func(p Plugin) bool { return res.Agents.Get(p.Agent) == nil })
	if len(res.Skills) == 0 {
		res.Parts = slices.DeleteFunc(res.Parts, func(p Part) bool { return p.Category == "skills" })
	}
	if len(res.Plugins) == 0 {
		res.Parts = slices.DeleteFunc(res.Parts, func(p Part) bool { return p.Category == "plugins" })
	}
	if r := res.Resources; r.CPUs == 0 || r.Memory == "" || r.Disk == "" {
		return nil, fmt.Errorf("-t %s: resources need cpus, memory and disk (got %+v)", name, r)
	}
	if err := res.Validate(); err != nil {
		return nil, fmt.Errorf("-t %s: %w", name, err)
	}
	return res, nil
}

// NormalizeSpec trims a -t spec's picks; no picks is "default".
func NormalizeSpec(spec string) string {
	var items []string
	for _, it := range strings.Split(spec, ",") {
		if it = strings.TrimSpace(it); it != "" && it != "default" {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return "default"
	}
	return strings.Join(items, ",")
}

// expand resolves one pick into sections, following list sections. In a
// category's own lists, bare names are that category's sections, and they
// count as named in -t when the list is; a profile's picks don't.
func (lib *Library) expand(item, in string, direct bool, depth int, out *[]pick) error {
	if depth > 8 {
		return fmt.Errorf("%s: lists nested too deep (a cycle?)", item)
	}
	remove := strings.HasPrefix(item, "-")
	ref := strings.TrimPrefix(item, "-")
	s, err := lib.lookup(ref, in)
	switch {
	case err != nil && ref == "base" && remove:
		return missing(err.Error() + ": the tools section base is now core (-core)")
	case err != nil && ref == "base":
		return missing(err.Error() + ": base was a built-in template, and -t minimal is closest to it now")
	case err != nil:
		return err
	}
	if in != "" && s.Category != in {
		return fmt.Errorf("%s is in %s.yaml; a list in %s.yaml names only its own sections (profiles.yaml spans categories)", ref, s.Category, in)
	}
	if s.t != nil {
		*out = append(*out, pick{remove, direct, s})
		return nil
	}
	if remove && s.Category == "profiles" {
		return fmt.Errorf("-%s: %s is a profile; remove its sections one by one", ref, ref)
	}
	scope := s.Category
	if scope == "profiles" {
		scope = ""
	}
	start := len(*out)
	for _, g := range s.Group {
		if remove { // -list removes each of its sections
			if strings.HasPrefix(g, "-") {
				return fmt.Errorf("-%s: %s holds a removal (%s), so it can't be removed", ref, ref, g)
			}
			g = "-" + g
		}
		if err := lib.expand(g, scope, direct && s.Category != "profiles", depth+1, out); err != nil {
			return fmt.Errorf("%s.yaml %s: %v", s.Category, s.Name, err) // a broken list, not a missing pick
		}
	}
	if c, _ := CategoryByName(s.Category); c.Exclusive && !remove {
		var got []*Section
		for _, p := range (*out)[start:] {
			if !p.remove {
				got = append(got, p.s)
			}
		}
		if len(got) > 1 {
			return fmt.Errorf("%s.yaml %s: %s takes one section, got %s", s.Category, s.Name, s.Category, names(got))
		}
	}
	return nil
}

// lookup finds name (or category:name); in limits bare names to one category.
func (lib *Library) lookup(ref, in string) (*Section, error) {
	cat, name, qualified := strings.Cut(ref, ":")
	if qualified && cat == "" {
		return nil, fmt.Errorf("%s: name the category before the colon, as in category:%s", ref, name)
	}
	if !qualified {
		cat, name = in, ref
	}
	if cat != "" {
		if _, ok := CategoryByName(cat); !ok {
			return nil, missing(fmt.Sprintf("%s: no category %q (%s)", ref, cat, categoryNames()))
		}
		if name == "default" {
			if d := lib.defaults[cat]; d != nil {
				return d, nil
			}
			return &Section{Category: cat, Name: "default"}, nil
		}
		if s := lib.sections[cat][name]; s != nil {
			return s, nil
		}
		return nil, missing(fmt.Sprintf("%s: no section %q in %s.yaml", ref, name, cat))
	}
	var found []*Section
	for _, c := range Categories {
		if s := lib.sections[c.Name][name]; s != nil {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, missing(fmt.Sprintf("unknown section %q (curral template ls lists them)", name))
	}
	var q []string
	for _, s := range found {
		q = append(q, s.Category+":"+name)
	}
	return nil, fmt.Errorf("%q is in several categories; say which: %s", name, strings.Join(q, ", "))
}

// oldFiles refuses template files curral no longer reads: an old default.yaml
// may be stricter than the defaults that would silently replace it.
func oldFiles(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil // no templates dir: built-ins only
	}
	var old []string
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			if _, cat := CategoryByName(n); !cat {
				old = append(old, e.Name())
			}
		}
	}
	if len(old) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s: old-style templates, which curral no longer reads. Move their settings into sections of the category files (curral init writes them; see https://blackshieldpt.github.io/curral/templates/), then remove or rename them",
		dir, strings.Join(old, ", "))
}

// Default is the sections a category's default expands to.
func (lib *Library) Default(category string) ([]*Section, error) {
	c, ok := CategoryByName(category)
	if !ok {
		return nil, fmt.Errorf("no category %q", category)
	}
	return lib.defaultSections(c)
}

// defaultSections expands a category's default into its sections.
func (lib *Library) defaultSections(c Category) ([]*Section, error) {
	d := lib.defaults[c.Name]
	if d == nil {
		return nil, nil
	}
	if d.t != nil {
		return []*Section{d}, nil
	}
	var picks []pick
	for _, g := range d.Group {
		if err := lib.expand(g, c.Name, false, 1, &picks); err != nil {
			return nil, fmt.Errorf("%s.yaml default: %v", c.Name, err) // a broken file, not a missing pick
		}
	}
	var out []*Section
	for _, p := range picks {
		if p.remove {
			return nil, fmt.Errorf("%s.yaml default: list sections, not removals", c.Name)
		}
		if !slices.Contains(out, p.s) {
			out = append(out, p.s)
		}
	}
	if c.Exclusive && len(out) > 1 {
		return nil, fmt.Errorf("%s.yaml default: %s takes one section, got %s", c.Name, c.Name, names(out))
	}
	return out, nil
}

func names(ss []*Section) string { return strings.Join(sectionNames(ss), ", ") }

// sameSections reports whether a and b hold the same sections, in any order.
func sameSections(a, b []*Section) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(s *Section) bool { return !slices.Contains(b, s) })
}

func sectionNames(ss []*Section) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func categoryNames() string {
	var out []string
	for _, c := range Categories {
		out = append(out, c.Name)
	}
	return strings.Join(out, ", ")
}

// BuiltinFile is a category's built-in file ("" when it has none).
func BuiltinFile(category string) string {
	b, _ := fs.ReadFile(builtinFS, "builtin/"+category+".yaml")
	return string(b)
}
