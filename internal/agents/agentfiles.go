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
	if err := p.copyIn(root, put); err != nil {
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
				p.warn("skills: %v", err)
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
	if err := p.copyIn(root, put); err != nil {
		p.warn("skills: %v", err)
	}
	if err := p.cleanup(root, keep); err != nil {
		p.warn("skills: %v", err)
	}
	if err := p.enablePlugins(s, agent, wanted, failed, bundled); err != nil {
		p.warn("skills: %v", err)
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
		setList(cfg, "plugin", prefix, failed, paths(wanted))
		return p.putJSON(opencodeConfig, cfg)
	case "pi":
		doc, err := p.getJSON(piSettings)
		if err != nil {
			return err
		}
		var add []any
		for _, b := range bundled {
			add = append(add, b)
		}
		c1 := setList(doc, "packages", prefix, failed, paths(wanted))
		c2 := setList(doc, "extensions", piPackage+"/", nil, add)
		if !c1 && !c2 {
			return nil
		}
		return p.putJSON(piSettings, doc)
	}
	return nil
}

// setList sets doc[key] to its items, except strings under prefix that aren't
// prefix+<one of failed>, followed by add. Other items (objects, arrays) are
// kept as they are. It reports whether doc changed.
func setList(doc map[string]any, key, prefix string, failed []string, add []any) bool {
	old, had := doc[key]
	list, _ := old.([]any)
	var out []any
	for _, x := range list {
		if v, ok := x.(string); ok && strings.HasPrefix(v, prefix) && !slices.Contains(failed, strings.TrimPrefix(v, prefix)) {
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
		return false
	}
	if out == nil {
		out = []any{}
	}
	doc[key] = out
	return fmt.Sprint(old) != fmt.Sprint(out)
}

func checkRoot(root string) error {
	if root == "" || strings.ContainsAny(root, "\"'$`\\\n ") {
		return fmt.Errorf("invalid guest folder %q", root)
	}
	return nil
}

// copyIn copies entries into ~/root in one tar, replacing only folders that
// carry curral's mark; folders without it are the user's and are left alone.
func (p *pusher) copyIn(root string, entries []entry) error {
	if err := checkRoot(root); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.name)
	}
	out, err := p.r.Run(p.ctx, fmt.Sprintf(`r="$HOME/%s"; for n in %s; do
  [ -e "$r/$n" ] && [ "$(cat "$r/$n/%s" 2>/dev/null)" != "$n" ] && echo "$n"
done; true`, root, strings.Join(names, " "), curralMark), nil)
	if err != nil {
		return fmt.Errorf("~/%s: %w", root, err)
	}
	// The output is guest-controlled: only exact names we sent count.
	for _, l := range strings.Split(string(out.Raw()), "\n") {
		if i := slices.Index(names, l); i >= 0 {
			p.warn("%s: ~/%s/%s is yours (no curral mark); not replaced", l, root, l)
			names = slices.Delete(names, i, i+1)
			entries = slices.DeleteFunc(entries, func(e entry) bool { return e.name == l })
		}
	}
	if len(entries) == 0 {
		return nil
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
			return fmt.Errorf("%s: %w", e.name, err)
		}
		tr := tar.NewReader(bytes.NewReader(b))
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	list := strings.Join(names, " ")
	cmd := fmt.Sprintf(`set -e; r="$HOME/%s"; (umask 077; mkdir -p "$r")
for n in %s; do rm -rf -- "$r/$n"; done
tar -xf - --no-same-owner -C "$r"
for n in %s; do printf %%s "$n" > "$r/$n/%s"; done`, root, list, list, curralMark)
	if _, err := p.r.Run(p.ctx, cmd, buf.Bytes()); err != nil {
		return fmt.Errorf("~/%s: %w", root, err)
	}
	return nil
}

// cleanup removes the folders in ~/root that carry curral's mark and aren't
// in keep.
func (p *pusher) cleanup(root string, keep []string) error {
	if err := checkRoot(root); err != nil {
		return err
	}
	cmd := fmt.Sprintf(`cd "$HOME/%s" 2>/dev/null || exit 0
for d in */; do d=${d%%/}; [ "$(cat "$d/%s" 2>/dev/null)" = "$d" ] || continue
  case " %s " in *" $d "*) ;; *) rm -rf -- "$d";; esac
done`, root, curralMark, strings.Join(keep, " "))
	if _, err := p.r.Run(p.ctx, cmd, nil); err != nil {
		return fmt.Errorf("~/%s: %w", root, err)
	}
	return nil
}
