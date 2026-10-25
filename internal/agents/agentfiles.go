package agents

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/blackshieldpt/curral/internal/gitsrc"
	"github.com/blackshieldpt/curral/internal/template"
)

// Where each agent reads skills and plugins, relative to $HOME. Researched
// against claude 2.1.310, codex 0.164.0, opencode 1.19.4, pi 1.4.0, omp 18.12.6.
var skillDirs = map[string]string{
	"claude":   ".claude/skills",
	"codex":    ".codex/skills",
	"opencode": ".config/opencode/skills",
	"pi":       ".pi/agent/skills",
	"omp":      ".omp/agent/skills",
}

var pluginDirs = map[string]string{
	"claude":   ".local/share/curral/plugins/claude",
	"codex":    ".codex/plugins/cache/curral",
	"opencode": ".local/share/curral/plugins/opencode",
	"pi":       ".local/share/curral/plugins/pi",
	"omp":      ".omp/agent/extensions",
}

// piPackage is pi's installed npm package in bases (Node lives in /usr/local).
var piPackage = "/usr/local/lib/node_modules/@earendil-works/pi-coding-agent"

// curralMark, holding the folder's own name, marks a folder curral copied in;
// only folders whose mark matches are replaced or removed.
const curralMark = ".curral"

const piSettings = ".pi/agent/settings.json"

// piBundled lists the paths curral added to pi's extensions, so it removes
// only those, never the user's own entries under pi's package.
const piBundled = ".local/share/curral/pi-bundled.json"

// bundledRe is a path inside pi's package (vm.json isn't re-validated).
var bundledRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

var homeRe = regexp.MustCompile(`^/home/[a-z_][a-z0-9_-]{0,31}$`)

// checkAgentFiles runs the checks agentFiles relies on, before Push uses any
// of it: the guest home and folders are curral's, and bundles whose name or
// agent is invalid are dropped with a warning (they come from vm.json, which
// isn't re-validated).
func (p *pusher) checkAgentFiles(s *Spec) error {
	if !s.AgentFiles {
		return nil
	}
	if !homeRe.MatchString(s.Home) {
		return fmt.Errorf("guest home %q isn't /home/<user>", s.Home)
	}
	for _, d := range template.AgentDefs {
		if *d.Get(&s.Agents) != nil && (skillDirs[d.Name] == "" || pluginDirs[d.Name] == "") {
			return fmt.Errorf("no skill or plugin folder for agent %s", d.Name)
		}
	}
	s.Skills = p.validBundles("skill", s.Skills, true)
	s.Plugins = p.validBundles("plugin", s.Plugins, false)
	return nil
}

func (p *pusher) validBundles(kind string, bs []Bundle, anyAgent bool) []Bundle {
	var out []Bundle
	for _, b := range bs {
		if !template.DirNameRe.MatchString(b.Name) || strings.HasPrefix(b.Name, "curral") {
			p.warn("%s %q: invalid name; skipped", kind, b.Name)
			continue
		}
		if _, ok := template.AgentDefByName(b.Agent); !ok && (b.Agent != "" || !anyAgent) {
			p.warn("%s %s: unknown agent %q; skipped", kind, b.Name, b.Agent)
			continue
		}
		if b.Bundled != "" && (!bundledRe.MatchString(b.Bundled) || slices.Contains(strings.Split(b.Bundled, "/"), "..")) {
			p.warn("%s %s: invalid bundled path %q; skipped", kind, b.Name, b.Bundled)
			continue
		}
		if slices.ContainsFunc(b.Files, func(f gitsrc.File) bool { return f.Name == curralMark || strings.HasPrefix(f.Name, curralMark+"/") }) {
			p.warn("%s %s: it holds a %s, which is curral's; skipped", kind, b.Name, curralMark)
			continue
		}
		out = append(out, b)
	}
	return out
}

// claudePluginDirs is CLAUDE_CODE_PLUGIN_DIRS for s's claude plugins.
func claudePluginDirs(s Spec) string {
	var dirs []string
	for _, b := range s.Plugins {
		if b.Agent == "claude" {
			dirs = append(dirs, s.Home+"/"+pluginDirs["claude"]+"/"+b.Name)
		}
	}
	return strings.Join(dirs, ":")
}

// entry is one folder copied into a root: its files go under name/sub.
type entry struct {
	name, sub string
	files     []gitsrc.File
}

// agentFiles copies skills and plugins into each installed agent, removes
// curral's folders that are no longer wanted, and turns the plugins on.
// Failures are warnings. s has passed checkAgentFiles.
func (p *pusher) agentFiles(s Spec) error {
	if !s.AgentFiles {
		return nil
	}
	for _, b := range s.Plugins {
		if d, _ := template.AgentDefByName(b.Agent); *d.Get(&s.Agents) == nil {
			p.warn("plugin %s: %s isn't installed in this VM; skipped", b.Name, b.Agent)
		}
	}
	for _, d := range template.AgentDefs {
		if *d.Get(&s.Agents) == nil {
			continue
		}
		p.agentSkills(s, d.Name)
		p.agentPlugins(s, d.Name)
	}
	return nil
}

func (p *pusher) agentSkills(s Spec, agent string) {
	root := skillDirs[agent]
	var put []entry
	var keep []string
	for _, b := range s.Skills {
		if b.Agent != agent && b.Agent != "" {
			continue
		}
		if agent == "claude" && s.Sync.Claude() && s.ClaudeDir != "" {
			if _, err := os.Stat(filepath.Join(s.ClaudeDir, "skills", b.Name)); err == nil {
				p.warn("skill %s: your synced ~/.claude/skills/%s is used instead", b.Name, b.Name)
				continue
			}
		}
		keep = append(keep, b.Name)
		if !b.Failed {
			put = append(put, entry{name: b.Name, files: b.Files})
		}
	}
	if _, err := p.copyIn(root, put); err != nil {
		p.warn("skills: %v", err)
	}
	if err := p.cleanup(root, keep); err != nil {
		p.warn("skills: %v", err)
	}
}

func hasFile(fs []gitsrc.File, names ...string) bool {
	return slices.ContainsFunc(fs, func(f gitsrc.File) bool { return slices.Contains(names, f.Name) })
}

func (p *pusher) agentPlugins(s Spec, agent string) {
	root := pluginDirs[agent]
	var put []entry
	var keep, wanted, failed, bundled []string
	for _, b := range s.Plugins {
		if b.Agent != agent {
			continue
		}
		if b.Failed {
			keep, failed = append(keep, b.Name), append(failed, b.Name)
			continue
		}
		e := entry{name: b.Name, files: b.Files}
		switch {
		case agent == "codex":
			e.sub = "local"
		case agent == "opencode" && !hasFile(b.Files, "package.json"):
			p.warn("plugin %s: opencode plugins need a package.json; skipped", b.Name)
			continue
		case agent == "omp" && !hasFile(b.Files, "index.ts", "index.js", "package.json"):
			p.warn("plugin %s: omp plugins need an index.ts, index.js or package.json; skipped", b.Name)
			continue
		case agent == "pi" && b.Bundled != "":
			path := piPackage + "/" + b.Bundled
			out, err := p.r.Run(p.ctx, "test -e "+shQuote(path)+" && echo yes || true", nil)
			if err != nil {
				p.warn("plugin %s: %v", b.Name, err)
				continue
			}
			if strings.TrimSpace(string(out.Raw())) != "yes" {
				p.warn("plugin %s: %s isn't in this VM's pi package; skipped", b.Name, b.Bundled)
				continue
			}
			bundled = append(bundled, path)
			continue
		}
		keep, wanted = append(keep, b.Name), append(wanted, b.Name)
		put = append(put, e)
	}
	yours, err := p.copyIn(root, put)
	if err != nil {
		p.warn("plugins: %v", err)
	}
	// A folder of the user's with a plugin's name isn't curral's to turn on.
	wanted = slices.DeleteFunc(wanted, func(n string) bool { return slices.Contains(yours, n) })
	if err := p.cleanup(root, keep); err != nil {
		p.warn("plugins: %v", err)
	}
	if err := p.enablePlugins(s, agent, wanted, failed, bundled); err != nil {
		p.warn("plugins: %v", err)
	}
}

// enablePlugins edits agent's config so it loads wanted (and pi's bundled
// paths), and drops curral's other entries except failed ones.
func (p *pusher) enablePlugins(s Spec, agent string, wanted, failed, bundled []string) error {
	prefix := s.Home + "/" + pluginDirs[agent] + "/"
	paths := func(names []string) []any {
		var out []any
		for _, n := range names {
			out = append(out, prefix+n)
		}
		return out
	}
	switch agent {
	case "codex":
		cfg, err := p.getDoc(codexConfig, "TOML", tomlUnmarshal)
		if err != nil {
			return err
		}
		plugins := obj(cfg, "plugins")
		for k := range plugins {
			if n, ok := strings.CutSuffix(k, "@curral"); ok && !slices.Contains(wanted, n) && !slices.Contains(failed, n) {
				delete(plugins, k)
			}
		}
		for _, n := range wanted {
			plugins[n+"@curral"] = map[string]any{"enabled": true}
		}
		if len(plugins) == 0 {
			delete(cfg, "plugins")
		}
		b, err := toml.Marshal(cfg)
		if err != nil {
			return err
		}
		return p.put(codexConfig, b)
	case "opencode":
		cfg, err := p.getJSON(opencodeConfig)
		if err != nil {
			return err
		}
		if _, err := setList(cfg, "plugin", isCurrals(prefix, failed), paths(wanted)); err != nil {
			return err
		}
		return p.putJSON(opencodeConfig, cfg)
	case "pi":
		doc, err := p.getJSON(piSettings)
		if err != nil {
			return err
		}
		rec, err := p.getJSON(piBundled)
		if err != nil {
			return err
		}
		before := stringList(rec["paths"]) // what curral added last time
		var add []any
		for _, b := range bundled {
			add = append(add, b)
		}
		c1, err := setList(doc, "packages", isCurrals(prefix, failed), paths(wanted))
		if err != nil {
			return err
		}
		c2, err := setList(doc, "extensions", func(v string) bool { return slices.Contains(before, v) }, add)
		if err != nil {
			return err
		}
		if c1 || c2 {
			if err := p.putJSON(piSettings, doc); err != nil {
				return err
			}
		}
		if !slices.Equal(before, bundled) {
			return p.putJSON(piBundled, map[string]any{"paths": add})
		}
	}
	return nil
}

// isCurrals matches the entries curral added under prefix, except failed
// ones, which stay until their fetch works again.
func isCurrals(prefix string, failed []string) func(string) bool {
	return func(v string) bool {
		n, ok := strings.CutPrefix(v, prefix)
		return ok && !slices.Contains(failed, n)
	}
}

// setList sets doc[key] to its items, except the strings drop matches,
// followed by add. Other items (objects, arrays) are kept as they are. It
// reports whether doc changed, and leaves a value that isn't a list alone.
func setList(doc map[string]any, key string, drop func(string) bool, add []any) (bool, error) {
	old, had := doc[key]
	list, isList := old.([]any)
	if had && !isList {
		return false, fmt.Errorf("%s isn't a list; left alone", key)
	}
	var out []any
	for _, x := range list {
		if v, ok := x.(string); ok && drop(v) {
			continue
		}
		out = append(out, x)
	}
	for _, x := range add {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	if !had && len(out) == 0 {
		return false, nil
	}
	if out == nil {
		out = []any{}
	}
	doc[key] = out
	return fmt.Sprint(old) != fmt.Sprint(out), nil
}

func checkRoot(root string) error {
	if root == "" || strings.ContainsAny(root, "\"'$`\\\n ") {
		return fmt.Errorf("invalid guest folder %q", root)
	}
	return nil
}

// markOf prints the mark of folder "$r/$n": a regular file's first bytes,
// never a FIFO or device a guest could plant to block the push.
const markOf = `"$( [ -f "$r/$n/` + curralMark + `" ] && head -c 64 "$r/$n/` + curralMark + `" 2>/dev/null)"`

// copyIn copies entries into ~/root, replacing only folders that carry
// curral's mark; folders without it are the user's, left alone and returned.
// It is one guest command: the files are unpacked next to the root, marked,
// then each folder is moved into place, so an interrupted copy never leaves
// an unmarked folder of curral's behind.
func (p *pusher) copyIn(root string, entries []entry) (yours []string, err error) {
	if err := checkRoot(root); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.name)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		dir := e.name
		if e.sub != "" {
			dir += "/" + e.sub
		}
		b, err := gitsrc.Tar(dir, e.files)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.name, err)
		}
		tr := tar.NewReader(bytes.NewReader(b))
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if err := tw.WriteHeader(h); err != nil {
				return nil, err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	// .curral-new can't clash: names start with a letter or digit.
	cmd := fmt.Sprintf(`r="$HOME/%s"; t="$r/.curral-new"
(umask 077; mkdir -p "$r") && rm -rf -- "$t" && mkdir "$t" || exit 1
tar -xf - --no-same-owner -C "$t" || { rm -rf -- "$t"; exit 1; }
for n in %s; do
  if { [ -e "$r/$n" ] || [ -L "$r/$n" ]; } && [ %s != "$n" ]; then echo "yours $n"; continue; fi
  printf %%s "$n" > "$t/$n/%s" && rm -rf -- "$r/$n" && mv -- "$t/$n" "$r/$n" || echo "failed $n"
done
rm -rf -- "$t"`, root, strings.Join(names, " "), markOf, curralMark)
	out, err := p.r.Run(p.ctx, cmd, buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("copying into ~/%s failed (%s)", root, runErr(err, cmd))
	}
	// The output is guest-controlled: only exact names we sent count.
	for _, l := range strings.Split(string(out.Raw()), "\n") {
		if n, ok := strings.CutPrefix(l, "yours "); ok && slices.Contains(names, n) && !slices.Contains(yours, n) {
			p.warn("%s: ~/%s/%s is yours (no curral mark); not replaced", n, root, n)
			yours = append(yours, n)
		}
		if n, ok := strings.CutPrefix(l, "failed "); ok && slices.Contains(names, n) {
			p.warn("%s: copying it into ~/%s failed", n, root)
		}
	}
	return yours, nil
}

// runErr is a guest command's error without the command, a whole script here.
func runErr(err error, cmd string) string {
	return strings.TrimPrefix(err.Error(), cmd+": ")
}

// cleanup removes the folders in ~/root that carry curral's mark and aren't
// in keep.
func (p *pusher) cleanup(root string, keep []string) error {
	if err := checkRoot(root); err != nil {
		return err
	}
	cmd := fmt.Sprintf(`r="$HOME/%s"; cd "$r" 2>/dev/null || exit 0
for n in */; do n=${n%%/}; [ %s = "$n" ] || continue
  case " %s " in *" $n "*) ;; *) rm -rf -- "$n";; esac
done`, root, markOf, strings.Join(keep, " "))
	if _, err := p.r.Run(p.ctx, cmd, nil); err != nil {
		return fmt.Errorf("cleaning up ~/%s failed (%s)", root, runErr(err, cmd))
	}
	return nil
}
