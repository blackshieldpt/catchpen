package template

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// profile writes settings (one YAML mapping) as sections <name>-<category> in
// the category files of dir, and a profile <name> picking them on top of
// minimal, or of the defaults when base is "default".
func profile(t *testing.T, dir, name, base, settings string) {
	t.Helper()
	var m map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(settings), &m); err != nil {
		t.Fatal(err)
	}
	picks := []string{}
	if base != "default" {
		picks = append(picks, "minimal")
	}
	for _, c := range Categories {
		body := map[string]yaml.Node{}
		for k, v := range m {
			if slices.Contains(c.Fields, k) && (k != "runtimes" || c.Name == "runtimes") {
				body[k] = v
			}
		}
		if len(body) == 0 {
			continue
		}
		b, err := yaml.Marshal(map[string]any{name + "-" + c.Name: body})
		if err != nil {
			t.Fatal(err)
		}
		write(t, dir, c.Name+".yaml", string(b))
		picks = append(picks, c.Name+":"+name+"-"+c.Name)
	}
	b, _ := yaml.Marshal(map[string][]string{name: picks})
	write(t, dir, "profiles.yaml", string(b))
}

func load(t *testing.T, dir, spec string) (*Template, error) {
	t.Helper()
	return Loader{UserDir: dir}.Load(spec)
}

// The category defaults are what the default template used to be.
func TestDefaults(t *testing.T) {
	tp, err := load(t, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if tp.Name != "default" || tp.Distro != "ubuntu-24.04" || tp.Resources != (Resources{CPUs: 4, Memory: "8G", Disk: "40G"}) {
		t.Errorf("name %s distro %s resources %+v", tp.Name, tp.Distro, tp.Resources)
	}
	for _, p := range []string{"git", "curl", "build-essential", "python3-venv"} {
		if !slices.Contains(tp.Packages, p) {
			t.Errorf("missing package %s in %v", p, tp.Packages)
		}
	}
	if !tp.HasDocker() || tp.Runtimes != (Runtimes{Node: "24", Python: "uv"}) || tp.Network.Egress != "allowlist" || !slices.Equal(tp.Tools, []string{"gh"}) {
		t.Errorf("docker %v runtimes %+v egress %s tools %v", tp.HasDocker(), tp.Runtimes, tp.Network.Egress, tp.Tools)
	}
	if want := []string{"anthropic", "github", "npm", "pypi", "go", "apt-ubuntu", "apt-debian", "docker-apt", "docker-hub", "ghcr"}; !slices.Equal(tp.Network.Presets, want) {
		t.Errorf("presets %v", tp.Network.Presets)
	}
	a := tp.Agents
	if a.Claude == nil || a.Claude.Auth != ClaudeOAuth || a.OpenCode == nil || a.Pi == nil || a.Codex == nil || a.Codex.Auth != CodexNone || a.OMP != nil {
		t.Errorf("agents %+v", a)
	}
	if !slices.Equal(tp.Cache, []string{"apt", "go", "npm", "pypi", "docker"}) || !slices.Equal(tp.Canaries, []string{"aws", "env", "git"}) {
		t.Errorf("cache %v canaries %v", tp.Cache, tp.Canaries)
	}
	if tp.SSH.Mode() != AgentOff || tp.Env != nil || tp.MCP != nil || tp.Repos != nil || tp.IdleStop != "" {
		t.Errorf("ssh %+v env %v mcp %v repos %v idle %q", tp.SSH, tp.Env, tp.MCP, tp.Repos, tp.IdleStop)
	}
	for _, spec := range []string{"default", " , default ,"} {
		if tp2, err := load(t, t.TempDir(), spec); err != nil || !reflect.DeepEqual(tp2.Packages, tp.Packages) || tp2.Name != "default" {
			t.Errorf("%q: %v", spec, err)
		}
	}
	claudeSkills := []string{"claude/skill-creator", "claude/mcp-builder", "claude/frontend-design"}
	for spec, want := range map[string][]string{"": claudeSkills, "minimal": nil, "minimal,claude": claudeSkills} {
		tp2, err := load(t, t.TempDir(), spec)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, s := range tp2.Skills {
			got = append(got, s.Agent+"/"+s.DirName())
		}
		if !slices.Equal(got, want) || len(tp2.Plugins) != 0 {
			t.Errorf("%q: skills %v, want %v; plugins %+v", spec, got, want, tp2.Plugins)
		}
	}
}

// Every built-in section can be picked by its bare name.
func TestBuiltinNamesAreUnique(t *testing.T) {
	lib, err := Loader{}.Library()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range Categories {
		secs, _ := lib.Sections(c.Name)
		for _, s := range secs {
			if _, err := lib.Load(s.Name); err != nil && strings.Contains(err.Error(), "several categories") {
				t.Errorf("%s: %v", s.Name, err)
			}
		}
	}
}

func TestPicksAddReplaceAndRemove(t *testing.T) {
	dir := t.TempDir()
	tp, err := load(t, dir, "ubuntu-26.04,large,go-1.27,python-3.14,-docker,-npm-cache,omp")
	if err != nil {
		t.Fatal(err)
	}
	if tp.Name != "ubuntu-26.04,large,go-1.27,python-3.14,-docker,-npm-cache,omp" || tp.Distro != "ubuntu-26.04" || tp.Resources.CPUs != 8 {
		t.Errorf("name %s distro %s resources %+v", tp.Name, tp.Distro, tp.Resources)
	}
	if tp.Runtimes != (Runtimes{Node: "24", Go: "1.27", Python: "3.14"}) || tp.HasDocker() {
		t.Errorf("runtimes %+v docker %v", tp.Runtimes, tp.HasDocker())
	}
	if !slices.Equal(tp.Cache, []string{"apt", "go", "pypi", "docker"}) || tp.Agents.OMP == nil || tp.Agents.Claude == nil {
		t.Errorf("cache %v omp %v", tp.Cache, tp.Agents.OMP)
	}
	want := []Part{{"distros", []string{"ubuntu-26.04"}, false}, {"resources", []string{"large"}, false}}
	if !slices.EqualFunc(tp.Parts[:2], want, func(a, b Part) bool { return reflect.DeepEqual(a, b) }) {
		t.Errorf("parts %+v", tp.Parts)
	}
	// A runtime pick wins over the Node version agents ask for.
	if tp, err := load(t, dir, "node-22"); err != nil || tp.Runtimes.Node != "22" {
		t.Errorf("node-22: %+v %v", tp, err)
	}
	m, err := load(t, dir, "minimal")
	if err != nil || m.Cache != nil || m.Canaries != nil || m.Agents.Any() || m.HasDocker() || !slices.Equal(m.Network.Presets, []string{"anthropic"}) || m.Resources.CPUs != 2 {
		t.Errorf("minimal: %+v %v", m, err)
	}
	if tp, err := load(t, dir, "minimal,go-cache,git-canary"); err != nil || !slices.Equal(tp.Cache, []string{"go"}) || !slices.Equal(tp.Canaries, []string{"git"}) {
		t.Errorf("adding to minimal: %+v %v", tp, err)
	}
	if tp, err := load(t, dir, "runtimes:default,caches:go-cache"); err != nil || !tp.HasDocker() {
		t.Errorf("qualified: %v", err)
	}
}

func TestUserFilesReplaceBuiltinsAndAddSections(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "resources.yaml", "default: large\nlarge: { resources: { cpus: 12, memory: 24G, disk: 90G } }\n")
	write(t, dir, "scripts/hello.sh", "echo hi\n")
	write(t, dir, "files/rc", "set -o vi\n")
	write(t, dir, "tools.yaml", `
mine:
  description: Mine
  packages: [git, golang-go]
  scripts: [scripts/hello.sh]
  files: [{ src: files/rc, dst: ~/.inputrc, mode: "0600" }]
`)
	write(t, dir, "ports.yaml", "web: { ports: [3000, \"8080:80\"] }\n")
	write(t, dir, "profiles.yaml", "work: [mine, web, -build]\n")
	tp, err := load(t, dir, "work")
	if err != nil {
		t.Fatal(err)
	}
	if tp.Resources.CPUs != 12 {
		t.Errorf("user default and section: %+v", tp.Resources)
	}
	if n := strings.Count(" "+strings.Join(tp.Packages, " ")+" ", " git "); n != 1 || slices.Contains(tp.Packages, "build-essential") || !slices.Contains(tp.Packages, "golang-go") {
		t.Errorf("packages %v", tp.Packages)
	}
	if string(tp.Asset("scripts/hello.sh")) != "echo hi\n" || string(tp.Asset("files/rc")) != "set -o vi\n" {
		t.Error("assets not loaded")
	}
	if want := []Port{{Host: 0, Guest: 3000}, {Host: 8080, Guest: 80}}; !slices.Equal(tp.Ports, want) {
		t.Errorf("ports = %v, want %v", tp.Ports, want)
	}
	lib, err := Loader{UserDir: dir}.Library()
	if err != nil {
		t.Fatal(err)
	}
	secs, def := lib.Sections("tools")
	if def == nil || !slices.Equal(def.Group, []string{"core", "build", "gh"}) || len(secs) != 5 {
		t.Errorf("tools sections %d, default %+v", len(secs), def)
	}
}

func TestCompositionErrors(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "network.yaml", "docker: { network: { presets: [docker-hub] } }\n")
	write(t, dir, "profiles.yaml", "loop1: [loop2]\nloop2: [loop1]\nbadrm: [-minimal]\n")
	for spec, want := range map[string]string{
		"nosuch":                     "unknown section \"nosuch\"",
		"docker":                     "say which: runtimes:docker, network:docker",
		"ubuntu-26.04,debian-13":     "distros takes one section",
		"-large":                     "resources takes one section",
		"-omp":                       "not in agents's default",
		"loop1":                      "nested too deep",
		"badrm":                      "is a profile",
		"nope:large":                 "no category \"nope\"",
		"resources:nope":             "no section \"nope\" in resources.yaml",
		strings.Repeat("large,", 40): "longer than",
	} {
		if _, err := load(t, dir, spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", spec, err, want)
		}
	}
	for file, body := range map[string]string{
		"runtimes.yaml": "x: { packages: [git] }\n",
		"distros.yaml":  "Bad_Name: { distro: debian-13 }\n",
		"agents.yaml":   "x: 3\n",
		"canaries.yaml": "x: { canaries: [ssh] }\n",
	} {
		d := t.TempDir()
		write(t, d, file, body)
		lib, err := Loader{UserDir: d}.Library()
		if err == nil {
			_, err = lib.Load("x")
		}
		if err == nil {
			t.Errorf("%s %q: no error", file, body)
		}
	}
	d := t.TempDir()
	write(t, d, "profiles.yaml", "p: { distro: debian-13 }\n")
	if _, err := (Loader{UserDir: d}).Library(); err == nil || !strings.Contains(err.Error(), "profiles are lists") {
		t.Errorf("profile with settings: %v", err)
	}
	d = t.TempDir()
	write(t, d, "tools.yaml", "x: { description: x, distro: debian-13 }\n")
	if _, err := (Loader{UserDir: d}).Library(); err == nil || !strings.Contains(err.Error(), "distro doesn't belong in tools.yaml") {
		t.Errorf("field in the wrong file: %v", err)
	}
}

// Order, list picks yielding to -t, partial sizes, lazy assets.
func TestPickOrderAndOverrides(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "resources.yaml", "fat: { resources: { cpus: 12 } }\n")
	write(t, dir, "agents.yaml", "default: [claude, omp]\n")
	write(t, dir, "runtimes.yaml", "go: { runtimes: { go: \"1.27\" } }\n")
	write(t, dir, "tools.yaml", "broken: { scripts: [missing.sh] }\n")
	write(t, dir, "ports.yaml", "web: { ports: [3000] }\nweb2: { ports: [3000] }\n")
	write(t, dir, "profiles.yaml", "big: [minimal, xlarge]\n")
	for spec, check := range map[string]func(*Template) bool{
		"minimal,large":                   func(tp *Template) bool { return tp.Resources.CPUs == 8 },
		"big":                             func(tp *Template) bool { return tp.Resources.CPUs == 16 },
		"big,small":                       func(tp *Template) bool { return tp.Resources.CPUs == 2 },
		"fat":                             func(tp *Template) bool { return tp.Resources == (Resources{CPUs: 12, Memory: "8G", Disk: "40G"}) },
		"minimal":                         func(tp *Template) bool { return tp.Agents.Claude == nil && tp.Agents.OMP != nil },
		"docker,-docker":                  func(tp *Template) bool { return !tp.HasDocker() },
		"-docker,docker":                  func(tp *Template) bool { return tp.HasDocker() },
		"runtimes:go,network:go,web,web2": func(tp *Template) bool { return tp.Runtimes.Go == "1.27" && len(tp.Ports) == 1 },
		"omp":                             func(tp *Template) bool { return tp.Agents.OMP != nil && tp.Agents.Pi == nil },
	} {
		tp, err := load(t, dir, spec)
		if err != nil || !check(tp) {
			t.Errorf("%q: %+v %v", spec, tp, err)
		}
	}
	if _, err := load(t, dir, "broken"); err == nil || !strings.Contains(err.Error(), "missing.sh") {
		t.Errorf("picked section with a missing script: %v", err)
	}
	for file, c := range map[string]struct{ body, want string }{
		"profiles.yaml":  {"default: [small]\n", "profiles.yaml has no default"},
		"tools.yaml":     {"a: { tools: [gh] }\na: { tools: [tea] }\n", "a is defined twice"},
		"runtimes.yaml":  {"web: [node-24, network:open]\n", "names only its own sections"},
		"resources.yaml": {"default: [small, large]\n", "resources takes one section, got small, large"},
		"gh.yaml":        {"extends: default\n", "gh.yaml: old-style templates"},
	} {
		d := t.TempDir()
		write(t, d, file, c.body)
		lib, err := Loader{UserDir: d}.Library()
		if err == nil {
			_, err = lib.Load("web")
			if file == "resources.yaml" {
				_, err = lib.Load("")
			}
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", file, err, c.want)
		}
	}
	d := t.TempDir()
	write(t, d, "resources.yaml", "default: { resources: { cpus: 2 } }\n")
	if _, err := load(t, d, ""); err == nil || !strings.Contains(err.Error(), "resources need cpus, memory and disk") {
		t.Errorf("incomplete default size: %v", err)
	}
}

// Picks in -t win over lists, re-picks move last, and a one-choice file's
// own lists count as named in -t.
func TestPickPrecedence(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "resources.yaml", "big: large\ncombo: [small, large]\n")
	write(t, dir, "profiles.yaml", "dk: [runtimes:docker]\nwide: [open]\nnolarge: [-resources:large]\nnomedium: [-resources:medium]\nagents: [omp]\nteam: [agents]\n")
	write(t, dir, "network.yaml", "tools: [docker-apt, docker-hub]\n")
	write(t, dir, "runtimes.yaml", "wr: [node-24, -docker]\n")
	for spec, check := range map[string]func(*Template) bool{
		"docker,minimal":          func(tp *Template) bool { return tp.HasDocker() },
		"claude,minimal":          func(tp *Template) bool { return tp.Agents.Claude != nil },
		"gh,minimal":              func(tp *Template) bool { return slices.Contains(tp.Tools, "gh") },
		"-docker,dk":              func(tp *Template) bool { return !tp.HasDocker() },
		"open,allowlist":          func(tp *Template) bool { return tp.Network.Egress == "allowlist" },
		"wide,allowlist":          func(tp *Template) bool { return tp.Network.Egress == "allowlist" },
		"node-22,node-24,node-22": func(tp *Template) bool { return tp.Runtimes.Node == "22" },
		"nolarge":                 func(tp *Template) bool { return tp.Resources.CPUs == 4 },
		"-tools:default":          func(tp *Template) bool { return len(tp.Packages) == 0 && len(tp.Tools) == 0 },
		"-env:default":            func(tp *Template) bool { return true },
		"big":                     func(tp *Template) bool { return tp.Resources.CPUs == 8 },
		"build": func(tp *Template) bool { // moved last: no longer the default's order
			return slices.ContainsFunc(tp.Parts, func(p Part) bool {
				return p.Category == "tools" && !p.Default && slices.Equal(p.Sections, []string{"core", "gh", "build"})
			})
		},
		"network:tools": func(tp *Template) bool { return slices.Contains(tp.Network.Presets, "docker-hub") },
		"team":          func(tp *Template) bool { return tp.Agents.OMP != nil },
	} {
		tp, err := load(t, dir, spec)
		if err != nil || !check(tp) {
			t.Errorf("%q: %+v %v", spec, tp, err)
		}
	}
	for spec, want := range map[string]string{
		"big,small":               "resources takes one section, got large and small",
		"small,big":               "resources takes one section, got small and large",
		"combo":                   "resources takes one section, got small, large",
		"resources:default,large": "resources takes one section, got medium and large",
		":go-1.27":                "name the category",
		"base":                    "base was a built-in template",
		"-base":                   "the tools section base is now core",
		"nomedium":                "a list removed medium; pick another",
		"-wr":                     "wr holds a removal",
	} {
		if _, err := load(t, dir, spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", spec, err, want)
		}
	}
}

// Category files that would silently mean something else are refused.
func TestFileErrors(t *testing.T) {
	for file, c := range map[string]struct{ body, want string }{
		"agents.yaml":   {"claude:\n", "claude: empty (uncomment the lines under it too"},
		"distros.yaml":  {"extends: default\ndistro: debian-13\n", "looks like an old-style template"},
		"tools.yaml":    {"a: { tools: [gh] }\n---\nb: { tools: [tea] }\n", "one YAML document only"},
		"caches.yaml":   {"x: ~\n", "x: empty"},
		"sync.yaml":     {"sync: [claude-config]\n", "looks like an old-style template"},
		"runtimes.yaml": {strings.Repeat("x", 49) + ": { docker: true }\n", "at most 48 characters"},
	} {
		d := t.TempDir()
		write(t, d, file, c.body)
		if _, err := (Loader{UserDir: d}).Library(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %q: %v, want %q", file, c.body, err, c.want)
		}
	}
}

// Scripts resolve inside the templates folder, once each.
func TestScriptPaths(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "s.sh", "echo s")
	write(t, dir, "..setup.sh", "echo setup")
	outside := filepath.Join(t.TempDir(), "secret")
	write(t, filepath.Dir(outside), "secret", "OUTSIDE")
	if err := os.Symlink(outside, filepath.Join(dir, "link.sh")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "tools.yaml", "twice: { scripts: [s.sh, ./s.sh] }\ndots: { scripts: [..setup.sh] }\nlink: { scripts: [link.sh] }\n")
	if tp, err := load(t, dir, "twice"); err != nil || !slices.Equal(tp.Scripts, []string{"s.sh"}) {
		t.Errorf("twice: %v %v", tp, err)
	}
	if _, err := load(t, dir, "dots"); err != nil {
		t.Errorf("..setup.sh: %v", err)
	}
	if tp, err := load(t, dir, "link"); err != nil || string(tp.assets["link.sh"]) != "OUTSIDE" {
		t.Errorf("a symlinked script (dotfile managers use them): %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"escape":   {"scripts: [../../etc/passwd]\n", "must be a relative path"},
		"abs":      {"scripts: [/etc/passwd]\n", "must be a relative path"},
		"badpkg":   {"packages: [\"git; rm -rf /\"]\n", "invalid package name"},
		"badnode":  {"runtimes: { node: \"lts; x\" }\n", "runtimes.node"},
		"badpy":    {"runtimes: { python: \"3\" }\n", "runtimes.python"},
		"baddst":   {"files: [{ src: f, dst: \"relative/path\" }]\n", "invalid dst"},
		"egress":   {"network: { egress: sometimes }\n", "network.egress"},
		"ondeny":   {"network: { on_deny: ask }\n", "network.on_deny"},
		"timeout0": {"network: { prompt_timeout: 0s }\n", "prompt_timeout"},
		"timeoutx": {"network: { prompt_timeout: soon }\n", "prompt_timeout"},
		"missing":  {"scripts: [nope.sh]\n", "\"nope.sh\""},
		"cachex":   {"cache: [maven]\n", "unknown cache \"maven\""},
		"canaryx":  {"canaries: [ssh]\n", "unknown canary \"ssh\""},
		"ompyolo":  {"agents: { omp: { yolo: true } }\n", "agents.omp.yolo is not supported"},
		"ompver":   {"agents: { omp: { version: \"18.12\" } }\n", "agents.omp.version must be latest or a full version"},
		"ompkey":   {"agents: { omp: { keys: [FOO_KEY] } }\n", "agents.omp.keys: unknown provider key"},
		"pinode":   {"agents: { pi: {} }\n", "agents need runtimes.node"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		write(t, dir, "f", "x")
		profile(t, dir, name, "", c.body)
		_, err := load(t, dir, name)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestOMPNeedsNoNode(t *testing.T) {
	dir := t.TempDir()
	profile(t, dir, "o", "", "agents: { omp: { version: 18.12.5, keys: [ANTHROPIC_API_KEY] } }\n")
	tp, err := load(t, dir, "o")
	if err != nil || tp.Agents.OMP == nil || tp.Agents.OMP.Version != "18.12.5" || !tp.Agents.Any() || tp.Runtimes.Node != "" {
		t.Fatalf("%+v %v", tp, err)
	}
}

func TestPromptTimeoutAccepted(t *testing.T) {
	dir := t.TempDir()
	profile(t, dir, "p", "", "network: { prompt_timeout: 1m30s }\n")
	if _, err := load(t, dir, "p"); err != nil {
		t.Fatal(err)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"512M": 512, "8G": 8192, "1T": 1 << 20} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "8", "8g", "0M", "-1G", "8GB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): expected error", in)
		}
	}
}

func TestAgentsMerge(t *testing.T) {
	dir := t.TempDir()
	profile(t, dir, "a", "default", `
agents:
  claude: { version: "2.1.303" }
  opencode: { keys: [OPENROUTER_API_KEY] }
env: { FOO: bar, TOKEN: "secret:TOKEN" }
mcp:
  - { name: fs, type: stdio, command: [npx, server-fs] }
sync: { claude_config: true }
`)
	profile(t, dir, "b", "default", `
agents:
  opencode: { keys: [ANTHROPIC_API_KEY, OPENROUTER_API_KEY] }
env: { FOO: baz }
mcp:
  - { name: fs, type: http, url: "https://fs.example.com/mcp" }
  - { name: docs, type: sse, url: "https://docs.example.com/sse", headers: { Authorization: "secret:DOCS" } }
`)
	tp, err := load(t, dir, "a,b")
	if err != nil {
		t.Fatal(err)
	}
	a := tp.Agents
	if a.Claude == nil || a.Claude.Auth != ClaudeOAuth || a.Claude.Version != "2.1.303" || a.Pi == nil {
		t.Errorf("agents not merged: claude=%+v pi=%+v", a.Claude, a.Pi)
	}
	if !slices.Equal(a.OpenCode.Keys, []string{"OPENROUTER_API_KEY", "ANTHROPIC_API_KEY"}) {
		t.Errorf("opencode keys = %v", a.OpenCode.Keys)
	}
	if tp.Env["FOO"] != "baz" || tp.Env["TOKEN"] != "secret:TOKEN" {
		t.Errorf("env = %v", tp.Env)
	}
	if len(tp.MCP) != 2 || tp.MCP[0].Type != "http" || tp.MCP[1].Name != "docs" {
		t.Errorf("mcp = %+v", tp.MCP)
	}
	if !tp.Sync.Claude() || !tp.Sync.Git() {
		t.Errorf("sync = %+v", tp.Sync)
	}
	// Loading again isn't affected by the merge above.
	pa, _ := load(t, dir, "a")
	if len(pa.Agents.OpenCode.Keys) != 1 {
		t.Errorf("section keys mutated: %v", pa.Agents.OpenCode.Keys)
	}
}

func TestAgentsValidation(t *testing.T) {
	cases := map[string]struct{ base, body, want string }{
		"nonode":     {"", "agents: { claude: {} }\n", "agents need runtimes.node"},
		"badauth":    {"default", "agents: { claude: { auth: password } }\n", "agents.claude.auth"},
		"subauth":    {"default", "agents: { pi: { auth: oauth-token } }\n", "only claude and codex"},
		"codexauth":  {"default", "agents: { codex: { auth: oauth-token } }\n", "agents.codex.auth must be api-key, chatgpt or none"},
		"codexkeys":  {"default", "agents: { codex: { keys: [OPENAI_API_KEY] } }\n", "agents.codex.keys"},
		"codexnode":  {"", "agents: { codex: {} }\n", "agents need runtimes.node"},
		"claudekeys": {"default", "agents: { claude: { keys: [OPENAI_API_KEY] } }\n", "agents.claude.keys"},
		"badkey":     {"default", "agents: { pi: { keys: [MY_KEY] } }\n", "unknown provider key"},
		"badver":     {"default", "agents: { pi: { version: \"1.0; rm\" } }\n", "agents.pi.version"},
		"reserved":   {"", "env: { PATH: /x }\n", "env: invalid name"},
		"lower":      {"", "env: { foo: x }\n", "env: invalid name"},
		"secretname": {"", "env: { A: \"secret:bad name\" }\n", "invalid secret name"},
		"override":   {"default", "env: { ANTHROPIC_API_KEY: \"secret:K\" }\n", "override Claude's subscription token"},
		"mcpname":    {"", "mcp: [{ name: \"a b\", type: stdio, command: [x] }]\n", "mcp: invalid name"},
		"mcptype":    {"", "mcp: [{ name: a, type: ws, url: \"https://x\" }]\n", "type must be"},
		"mcpstdio":   {"", "mcp: [{ name: a, type: stdio }]\n", "need command"},
		"mcpurl":     {"", "mcp: [{ name: a, type: http, url: \"file:///etc\" }]\n", "http(s) url"},
		"mcphdr":     {"", "mcp: [{ name: a, type: http, url: \"https://x\", headers: { \"Bad Header\": v } }]\n", "invalid header"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		profile(t, dir, name, c.base, c.body)
		_, err := load(t, dir, name)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	dir := t.TempDir()
	if tp, err := load(t, dir, "minimal,codex-chatgpt"); err != nil || tp.Agents.Codex.Auth != CodexChatGPT {
		t.Errorf("codex with chatgpt auth: %+v %v", tp, err)
	}
	profile(t, dir, "codexyolo", "", "runtimes: { node: \"24\" }\nagents: { codex: { auth: chatgpt, yolo: true } }\n")
	if tp, err := load(t, dir, "codexyolo"); err != nil || !tp.Agents.Codex.SkipPermissions() {
		t.Errorf("codex yolo: %v", err)
	}
	if tp, err := load(t, dir, "codex-api-key"); err != nil || tp.Agents.Codex.Auth != CodexAPIKey {
		t.Errorf("turning codex on over the defaults: %v", err)
	}
	profile(t, dir, "apikey", "default", "env: { ANTHROPIC_API_KEY: \"secret:K\" }\n")
	if _, err := load(t, dir, "claude-api-key,apikey"); err != nil {
		t.Errorf("api-key mode with ANTHROPIC_API_KEY env: %v", err)
	}
}

func TestSSHAgentConfig(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ssh.yaml", "mine: { ssh: { agent: restricted, agent_keys: [~/.ssh/id_ed25519, /keys/deploy], confirm: false } }\n")
	tp, err := load(t, dir, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if tp.SSH.Mode() != AgentRestricted || tp.SSH.ConfirmEach() || !slices.Equal(tp.SSH.AgentKeys, []string{"~/.ssh/id_ed25519", "/keys/deploy"}) {
		t.Errorf("ssh = %+v", tp.SSH)
	}
	if (SSH{}).Mode() != AgentOff || !(SSH{}).ConfirmEach() {
		t.Error("defaults")
	}
	for body, want := range map[string]string{
		"x: { ssh: { agent: always } }\n":                                 "ssh.agent must be",
		"x: { ssh: { agent: restricted } }\n":                             "needs ssh.agent_keys",
		"x: { ssh: { agent: restricted, agent_keys: [relative/key] } }\n": "absolute or ~/ path",
		"x: { ssh: { agent: host, agent_keys: [/k] } }\n":                 "only applies to ssh.agent: restricted",
	} {
		d := t.TempDir()
		write(t, d, "ssh.yaml", body)
		if _, err := load(t, d, "x"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

func TestAgentYolo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "agents.yaml", "a: { runtimes: { node: \"24\" }, agents: { claude: { yolo: true }, opencode: { yolo: true } } }\nb: { agents: { opencode: { yolo: false } } }\nc: { runtimes: { node: \"24\" }, agents: { pi: { yolo: true } } }\n")
	tp, err := load(t, dir, "a,b")
	if err != nil {
		t.Fatal(err)
	}
	if !tp.Agents.Claude.SkipPermissions() || tp.Agents.OpenCode.SkipPermissions() {
		t.Errorf("yolo merge: claude %v opencode %v", tp.Agents.Claude.SkipPermissions(), tp.Agents.OpenCode.SkipPermissions())
	}
	if (*Agent)(nil).SkipPermissions() {
		t.Error("nil agent")
	}
	if _, err := load(t, dir, "c"); err == nil || !strings.Contains(err.Error(), "agents.pi.yolo") {
		t.Errorf("pi yolo: %v", err)
	}
}

func TestParseInterval(t *testing.T) {
	for s, want := range map[string]time.Duration{"": 0, "off": 0, "daily": 24 * time.Hour, "weekly": 168 * time.Hour, "90m": 90 * time.Minute} {
		if d, err := ParseInterval(s); err != nil || d != want {
			t.Errorf("ParseInterval(%q) = %s, %v", s, d, err)
		}
	}
	for _, s := range []string{"30s", "-1h", "soon", "0"} {
		if _, err := ParseInterval(s); err == nil {
			t.Errorf("ParseInterval(%q) accepted", s)
		}
	}
}

// Every Agents field is in the registry under its YAML name, and the other
// way round.
func TestAgentRegistryMatchesAgents(t *testing.T) {
	typ := reflect.TypeOf(Agents{})
	if typ.NumField() != len(AgentDefs) {
		t.Fatalf("%d Agents fields, %d registered agents", typ.NumField(), len(AgentDefs))
	}
	for i, d := range AgentDefs {
		var a Agents
		*d.Get(&a) = &Agent{}
		v := reflect.ValueOf(a)
		for j := range typ.NumField() {
			name, _, _ := strings.Cut(typ.Field(j).Tag.Get("yaml"), ",")
			if set := !v.Field(j).IsNil(); set != (name == d.Name) {
				t.Errorf("agent %d (%s): field %s set %v", i, d.Name, typ.Field(j).Name, set)
			}
		}
	}
}

const (
	pinGit = "https://github.com/example/skills"
	pinRef = "0123456789abcdef0123456789abcdef01234567"
)

// pinned is a skill or plugin entry from the test pin, with more fields.
func pinned(more string) string { return "{ git: " + pinGit + ", ref: " + pinRef + ", " + more + " }" }

func skillNames(tp *Template) []string {
	var out []string
	for _, s := range tp.Skills {
		out = append(out, s.DirName())
	}
	return out
}

// Skills merge in pick order (default first), identical entries once; those
// for agents the template doesn't install are dropped. The same name with
// another ref is no duplicate for appendNew: it is a clash, but only among
// installed agents.
func TestSkillsMergeAndFilter(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "skills.yaml", "default: []\n"+
		"sk-a: { skills: ["+pinned("agent: claude, path: skills/x")+"] }\n"+
		"sk-a2: { skills: ["+pinned("agent: claude, path: skills/x")+"] }\n"+
		"sk-b: { skills: ["+pinned("agent: codex, path: skills/y")+"] }\n"+
		"sk-any: { skills: ["+pinned("path: skills/z")+"] }\n"+
		"sk-clash: { skills: ["+pinned("path: skills/w")+", { agent: codex, git: "+pinGit+", ref: 1123456789abcdef0123456789abcdef01234567, path: other/w }] }\n")
	write(t, dir, "plugins.yaml", "default: []\npl-cx: { plugins: ["+pinned("agent: codex, path: plugins/p")+"] }\n")
	for spec, want := range map[string][]string{
		"sk-a,sk-b":               {"x", "y"},
		"sk-a,sk-a2":              {"x"},
		"minimal,sk-a":            nil,
		"minimal,claude,sk-a":     {"x"},
		"minimal,claude,sk-any":   {"z"},
		"minimal,sk-any":          nil,
		"minimal,claude,sk-clash": {"w"},
	} {
		tp, err := load(t, dir, spec)
		if err != nil || !slices.Equal(skillNames(tp), want) {
			t.Errorf("%s: %v %v, want %v", spec, err, tp, want)
		}
	}
	if tp, err := load(t, dir, "minimal,claude,pl-cx"); err != nil || len(tp.Plugins) != 0 {
		t.Errorf("codex plugin without codex: %v %+v", err, tp)
	}
	if tp, err := load(t, dir, "minimal,codex,pl-cx"); err != nil || len(tp.Plugins) != 1 {
		t.Errorf("codex plugin with codex: %v %+v", err, tp)
	}
	hasPart := func(tp *Template) bool {
		return slices.ContainsFunc(tp.Parts, func(p Part) bool { return p.Category == "skills" })
	}
	if tp, err := load(t, dir, "minimal,sk-a"); err != nil || hasPart(tp) {
		t.Errorf("skills part with no skills left: %v %+v", err, tp)
	}
	if tp, err := load(t, dir, "minimal,claude,sk-a"); err != nil || !hasPart(tp) {
		t.Errorf("no skills part: %v %+v", err, tp)
	}
}

func TestSkillsValidation(t *testing.T) {
	entry := func(git, ref, more string) string {
		return "{ agent: claude, git: " + git + ", ref: " + ref + ", " + more + " }"
	}
	ref2 := "1123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct{ entries, spec, want string }{
		{entry("http://github.com/example/skills", pinRef, "path: skills/x"), "", `git must be an https URL, got "http://`},
		{entry("https:///skills", pinRef, "path: skills/x"), "", `git must be an https URL, got "https:///skills"`},
		{entry("https://u:p@github.com/example/skills", pinRef, "path: skills/x"), "", "git must not hold a user or password"},
		{entry(`"`+pinGit+`?a=b"`, pinRef, "path: skills/x"), "", "git must have no query or fragment"},
		{entry(pinGit, "abc123", "path: skills/x"), "", `ref must be a full 40-character commit id, got "abc123"`},
		{entry(pinGit, "0123456789ABCDEF0123456789abcdef01234567", "path: skills/x"), "", `got "0123456789ABCDEF`},
		{entry(pinGit, pinRef, "path: ../x"), "", `path must not contain .., got "../x"`},
		{entry(pinGit, pinRef, "path: /x"), "", `not starting with - or /, got "/x"`},
		{entry(pinGit, pinRef, "path: a//b"), "", `path must be a clean path, got "a//b"`},
		// minimal installs no agents, so the filter alone would drop it.
		{"{ agent: claud, git: " + pinGit + ", ref: " + pinRef + ", path: skills/x }", "minimal,", `skills x: unknown agent "claud" (known: claude, opencode, pi, omp, codex)`},
		{entry(pinGit, pinRef, "path: skills/x, name: Bad"), "", `skills skills/x: name "Bad" must be lower case`},
		{entry(pinGit, pinRef, `path: skills/x, name: "a b"`), "", `name "a b" must be`},
		{entry(pinGit, pinRef, "path: skills/x, name: a/b"), "", `name "a/b" must be`},
		{entry(pinGit, pinRef, `path: skills/x, name: "$(x)"`), "", `name "$(x)" must be`},
		{entry(pinGit, pinRef, "path: skills/x, name: "+strings.Repeat("a", 65)), "", `name "` + strings.Repeat("a", 65) + `" must be`},
		{entry(pinGit, pinRef, "path: skills/x, name: curral-x"), "", `name "curral-x" is reserved`},
		{entry(pinGit, pinRef, `path: ":(exclude)x"`), "", `got ":(exclude)x"`},
		{entry(pinGit, pinRef, "path: -x"), "", `got "-x"`},
		{entry(pinGit, pinRef, `path: "skills/*"`), "", `got "skills/*"`},
		{entry(pinGit, pinRef, "path: skills/x") + ", " + entry(pinGit, ref2, "path: other/x"), "minimal,claude,", `skills: two skills named "x" for claude`},
		{pinned("path: skills/z") + ", " + entry(pinGit, ref2, "path: other/z"), "minimal,claude,", `skills: two skills named "z" for claude`},
	} {
		dir := t.TempDir()
		write(t, dir, "skills.yaml", "default: []\nbad: { skills: ["+c.entries+"] }\n")
		if _, err := load(t, dir, c.spec+"bad"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.entries, err, c.want)
		}
	}
}

func TestSkillDirName(t *testing.T) {
	for s, want := range map[Skill]string{
		{Git: pinGit, Ref: pinRef, Path: "skills/x"}:                 "x",
		{Git: pinGit, Ref: pinRef, Path: "skills/x", Name: "y"}:      "y",
		{Git: "https://github.com/a/b.git", Ref: pinRef, Path: "."}:  "b",
		{Git: "https://github.com/a/Skills", Ref: pinRef, Path: "."}: "Skills",
	} {
		if got := s.DirName(); got != want {
			t.Errorf("%+v: %q, want %q", s, got, want)
		}
	}
	if err := validDirName("Skills"); err == nil {
		t.Error("upper case name accepted")
	}
	if got := (Plugin{Agent: "pi", Bundled: "examples/extensions/plan-mode.ts"}).DirName(); got != "plan-mode" {
		t.Errorf("bundled plugin: %q", got)
	}
}

func TestPluginsValidation(t *testing.T) {
	ref2 := "1123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct{ entries, spec, want string }{
		{pinned("path: p/x"), "minimal,pi,", "plugins x: agent is required"},
		{pinned("agent: claud, path: p/x"), "minimal,pi,", `plugins x: unknown agent "claud"`},
		{pinned("agent: pi, path: p/x, bundled: examples/a.ts"), "minimal,pi,", "plugins a: set git or bundled, not both"},
		{"{ agent: pi, name: nb }", "minimal,pi,", "plugins nb: set git or bundled"},
		{"{ agent: pi, git: " + pinGit + ", ref: abc, path: p/x }", "minimal,pi,", `plugins x: ref must be a full 40-character commit id, got "abc"`},
		{"{ agent: claude, bundled: examples/extensions/plan-mode.ts }", "minimal,claude,", "plugins plan-mode: bundled is only for pi"},
		{"{ agent: pi, bundled: ../x }", "minimal,pi,", `bundled must not contain .., got "../x"`},
		{`{ agent: pi, bundled: ".", name: dot }`, "minimal,pi,", `plugins dot: bundled must name a file in the pi package, got "."`},
		{pinned("agent: pi, path: p/x, name: Bad"), "minimal,pi,", `plugins p/x: name "Bad" must be`},
		{pinned("agent: pi, path: p/x") + ", { agent: pi, git: " + pinGit + ", ref: " + ref2 + ", path: q/x }", "minimal,pi,", `plugins: two pi plugins named "x"`},
	} {
		dir := t.TempDir()
		write(t, dir, "plugins.yaml", "default: []\nbad: { plugins: ["+c.entries+"] }\n")
		if _, err := load(t, dir, c.spec+"bad"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.entries, err, c.want)
		}
	}
	dir := t.TempDir()
	write(t, dir, "plugins.yaml", "default: []\nplan: { plugins: [{ agent: pi, bundled: examples/extensions/plan-mode.ts }] }\n")
	tp, err := load(t, dir, "minimal,pi,plan")
	if err != nil || len(tp.Plugins) != 1 || tp.Plugins[0].DirName() != "plan-mode" {
		t.Errorf("bundled pi plugin: %v %+v", err, tp)
	}
}

func TestSyncSkillSources(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "sync.yaml", "src-on: { sync: { claude_ai: true, opencode_claude_skills: true } }\nsrc-off-ai: { sync: { claude_ai: false } }\n")
	for spec, want := range map[string][2]bool{
		"":                  {false, false},
		"src-on":            {true, true},
		"src-on,src-off-ai": {false, true},
	} {
		tp, err := load(t, dir, spec)
		if err != nil || [2]bool{tp.Sync.ClaudeAISync(), tp.Sync.SharedClaudeSkills()} != want {
			t.Errorf("%q: %v %+v, want %v", spec, err, tp, want)
		}
	}
}
