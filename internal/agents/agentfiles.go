package agents

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/blackshieldpt/catchpen/internal/gitsrc"
	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/internal/template"
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
	"claude":   ".local/share/catchpen/plugins/claude",
	"codex":    ".codex/plugins/cache/catchpen",
	"opencode": ".local/share/catchpen/plugins/opencode",
	"pi":       ".local/share/catchpen/plugins/pi",
	"omp":      ".omp/agent/extensions",
}

// piPackage is pi's installed npm package in bases (Node lives in /usr/local).
var piPackage = func() string {
	d, _ := template.AgentDefByName("pi")
	return "/usr/local/lib/node_modules/" + d.Package
}()

// catchpenMark, holding the folder's own name, marks a folder catchpen copied in;
// only folders whose mark matches are replaced or removed.
const catchpenMark = ".catchpen"

const piSettings = ".pi/agent/settings.json"

// piBundled lists the paths catchpen added to pi's extensions, so it removes
// only those, never the user's own entries under pi's package.
const piBundled = ".local/share/catchpen/pi-bundled.json"

var homeRe = regexp.MustCompile(`^/home/[a-z_][a-z0-9_-]{0,31}$`)

// checkAgentFiles runs the checks agentFiles relies on, before Push uses any
// of it: the guest home and folders are catchpen's, and bundles whose name or
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
		if !template.DirNameRe.MatchString(b.Name) || strings.HasPrefix(b.Name, "catchpen") {
			p.warn("%s %q: invalid name; skipped", kind, b.Name)
			continue
		}
		if _, ok := template.AgentDefByName(b.Agent); !ok && (b.Agent != "" || !anyAgent) {
			p.warn("%s %s: unknown agent %q; skipped", kind, b.Name, b.Agent)
			continue
		}
		if b.Bundled != "" && gitsrc.CheckPath(b.Bundled) != nil { // vm.json isn't re-validated
			p.warn("%s %s: invalid bundled path %q; skipped", kind, b.Name, b.Bundled)
			continue
		}
		if slices.ContainsFunc(b.Files, func(f gitsrc.File) bool { return f.Name == catchpenMark || strings.HasPrefix(f.Name, catchpenMark+"/") }) {
			p.warn("%s %s: it holds a %s, which is catchpen's; skipped", kind, b.Name, catchpenMark)
			continue
		}
		out = append(out, b)
	}
	return out
}

// claudePluginDirs is CLAUDE_CODE_PLUGIN_DIRS for s's claude plugins in on.
func claudePluginDirs(s Spec, on []string) string {
	var dirs []string
	for _, b := range s.Plugins {
		if b.Agent == "claude" && slices.Contains(on, b.Name) {
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
// catchpen's folders that are no longer wanted, and turns the plugins on.
// Failures are warnings. s has passed checkAgentFiles.
func (p *pusher) agentFiles(s Spec) {
	if !s.AgentFiles {
		return
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
		if d.Name != "claude" { // Push did claude's, before its env file names them
			p.agentPlugins(s, d.Name)
		}
	}
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
	if _, err := p.cleanup(root, keep); err != nil {
		p.warn("skills: %v", err)
	}
}

func hasFile(fs []gitsrc.File, names ...string) bool {
	return slices.ContainsFunc(fs, func(f gitsrc.File) bool { return slices.Contains(names, f.Name) })
}

// agentPlugins copies agent's plugins in and returns the ones it should load:
// folders with catchpen's mark, including failed ones that keep their old copy.
func (p *pusher) agentPlugins(s Spec, agent string) []string {
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
	if err := p.copyIn(root, put); err != nil {
		p.warn("plugins: %v", err)
	}
	ours, err := p.cleanup(root, keep)
	if err != nil {
		p.warn("plugins: %v", err)
	}
	// Only folders with catchpen's mark are turned on, never a folder of the
	// user's with a plugin's name.
	notOurs := func(n string) bool { return !slices.Contains(ours, n) }
	wanted, failed = slices.DeleteFunc(wanted, notOurs), slices.DeleteFunc(failed, notOurs)
	if err := p.enablePlugins(s, agent, wanted, failed, bundled); err != nil {
		p.warn("plugins: %v", err)
	}
	return append(wanted, failed...)
}

// enablePlugins edits agent's config so it loads wanted (and pi's bundled
// paths), and drops catchpen's other entries except failed ones.
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
		was, err := toml.Marshal(cfg)
		if err != nil {
			return err
		}
		plugins := obj(cfg, "plugins")
		for k := range plugins {
			if n, ok := strings.CutSuffix(k, "@catchpen"); ok && !slices.Contains(wanted, n) && !slices.Contains(failed, n) {
				delete(plugins, k)
			}
		}
		for _, n := range wanted {
			plugins[n+"@catchpen"] = map[string]any{"enabled": true}
		}
		if len(plugins) == 0 {
			delete(cfg, "plugins")
		}
		b, err := toml.Marshal(cfg)
		if err != nil || bytes.Equal(b, was) {
			return err
		}
		return p.put(codexConfig, b)
	case "opencode":
		cfg, err := p.getJSON(opencodeConfig)
		if err != nil {
			return err
		}
		if changed, err := setList(cfg, "plugin", isCatchpens(prefix, failed), paths(wanted)); err != nil || !changed {
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
		before := stringList(rec["paths"]) // what catchpen added last time
		// A path the user listed themselves stays theirs.
		users := slices.DeleteFunc(stringList(doc["extensions"]), func(v string) bool { return slices.Contains(before, v) })
		var add []any
		var ours []string
		for _, b := range bundled {
			add = append(add, b)
			if !slices.Contains(users, b) {
				ours = append(ours, b)
			}
		}
		c1, err := setList(doc, "packages", isCatchpens(prefix, failed), paths(wanted))
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
		if !slices.Equal(before, ours) {
			return p.putJSON(piBundled, map[string]any{"paths": ours})
		}
	}
	return nil
}

// isCatchpens matches the entries catchpen added under prefix, except failed
// ones, which stay until their fetch works again.
func isCatchpens(prefix string, failed []string) func(string) bool {
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
	return !reflect.DeepEqual(old, out), nil
}

func checkRoot(root string) error {
	if root == "" || strings.ContainsAny(root, "\"'$`\\\n ") {
		return fmt.Errorf("invalid guest folder %q", root)
	}
	return nil
}

// markOf prints the mark of folder "$r/$n": a regular file's first bytes,
// never a FIFO or device a guest could plant to block the push.
const markOf = `"$( [ -f "$r/$n/` + catchpenMark + `" ] && head -c 64 "$r/$n/` + catchpenMark + `" 2>/dev/null)"`

// copyIn copies entries into ~/root, replacing only folders that carry
// catchpen's mark; folders without it are the user's, left alone with a warning.
// It is one guest command: the files are unpacked next to the root and
// marked, then each old folder is moved aside and the new one moved into
// place. Only renames touch the root, so a failed or interrupted copy never
// leaves an unmarked part of a folder of catchpen's behind.
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
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		dir := e.name
		if e.sub != "" {
			dir += "/" + e.sub
		}
		if err := gitsrc.WriteTar(tw, dir, e.files); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// .catchpen-new and .old can't clash: names start with a letter or digit.
	cmd := fmt.Sprintf(lockFiles+`
r="$HOME/%s"; t="$r/.catchpen-new"
(umask 077; mkdir -p "$r") || exit 1
`+tidy+`
mkdir "$t" "$t/.old" || exit 1
tar -xf - --no-same-owner -C "$t" || { rm -rf -- "$t"; exit 1; }
for n in %s; do
  if { [ -e "$r/$n" ] || [ -L "$r/$n" ]; } && [ %s != "$n" ]; then echo "yours $n"; continue; fi
  printf %%s "$n" > "$t/$n/%s" || { echo "failed $n"; continue; }
  if [ -e "$r/$n" ] || [ -L "$r/$n" ]; then
    [ -L "$r/$n" ] || chmod u+w -- "$r/$n" 2>/dev/null
    mv -T -- "$r/$n" "$t/.old/$n" || { echo "failed $n"; continue; }
  fi
  mv -T -- "$t/$n" "$r/$n" || { mv -T -- "$t/.old/$n" "$r/$n"; echo "failed $n"; }
done
`+tidy+`
exit 0`, root, strings.Join(names, " "), markOf, catchpenMark)
	out, err := p.r.Run(p.ctx, cmd, buf.Bytes())
	if err != nil {
		return fmt.Errorf("copying into ~/%s failed (%s)", root, runErr(err, cmd))
	}
	// The output is guest-controlled: only exact names we sent count, once.
	var seen []string
	for _, l := range strings.Split(string(out.Raw()), "\n") {
		if n, ok := strings.CutPrefix(l, "yours "); ok && slices.Contains(names, n) && !slices.Contains(seen, l) {
			p.warn("%s: ~/%s/%s is yours (no catchpen mark); not replaced", n, root, n)
			seen = append(seen, l)
		}
		if n, ok := strings.CutPrefix(l, "failed "); ok && slices.Contains(names, n) && !slices.Contains(seen, l) {
			p.warn("%s: copying it into ~/%s failed", n, root)
			seen = append(seen, l)
		}
	}
	return nil
}

// runErr is a guest command's error without the command, a whole script here.
func runErr(err error, cmd string) string {
	return strings.TrimPrefix(err.Error(), cmd+": ")
}

// lockFiles starts copyIn's and cleanup's scripts: two pushes at once (a
// start and a creds push) would share a root's staging folder.
const lockFiles = `(umask 077; mkdir -p "$HOME/.local/share/catchpen") && exec 9>>"$HOME/.local/share/catchpen/files.lock" &&
  { flock -w 10 9 || { echo "another push holds ~/.local/share/catchpen/files.lock" >&2; exit 1; }; } || exit 1`

// tidy is the guest script copyIn and cleanup start and end with, for root
// "$r" and its staging folder "$t". It moves back a folder an interrupted
// copy left aside, then removes "$t" (an agent may have made its folders
// read-only). What it can't remove, e.g. files made with sudo, is parked in
// a "$t.*" folder so it never blocks a copy, and reported.
const tidy = `[ -L "$t" ] || [ -L "$t/.old" ] || for o in "$t"/.old/*; do n=${o##*/}; [ -e "$r/$n" ] || [ -L "$r/$n" ] || mv -T -- "$o" "$r/$n"; done 2>/dev/null
for o in "$t" "$t".*; do { [ -e "$o" ] || [ -L "$o" ]; } || continue
  [ -L "$o" ] || chmod -R u+w -- "$o" 2>/dev/null; rm -rf -- "$o" 2>/dev/null
done
{ [ -e "$t" ] || [ -L "$t" ]; } && { o=$(mktemp -d "$t.XXXXXXXX") && mv -T -- "$t" "$o/d"; } 2>/dev/null
for o in "$t" "$t".*; do if [ -e "$o" ] || [ -L "$o" ]; then echo leftover; break; fi; done`

func (p *pusher) warnLeftover(root string, out sshx.GuestOutput) {
	if slices.Contains(strings.Split(string(out.Raw()), "\n"), "leftover") {
		p.warn("~/%s/.catchpen-new*: catchpen can't remove its old copies there (files made with sudo?); remove them with sudo rm -rf", root)
	}
}

// cleanup removes the folders in ~/root that carry catchpen's mark and aren't
// in keep, and returns those in keep that carry it. Each is first moved
// aside whole, so a failed removal leaves nothing in the root.
func (p *pusher) cleanup(root string, keep []string) (ours []string, err error) {
	if err := checkRoot(root); err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf(lockFiles+`
r="$HOME/%s"; t="$r/.catchpen-new"; cd "$r" 2>/dev/null || exit 0
`+tidy+`
for n in */; do n=${n%%/}; [ %s = "$n" ] || continue
  case " %s " in *" $n "*) echo "ours $n";; *)
    [ -L "$n" ] || chmod u+w -- "$n" 2>/dev/null
    { mkdir -p "$t" && mv -T -- "$n" "$t/$n"; } || echo "failed $n";; esac
done
`+tidy+`
exit 0`, root, markOf, strings.Join(keep, " "))
	out, err := p.r.Run(p.ctx, cmd, nil)
	if err != nil {
		return nil, fmt.Errorf("cleaning up ~/%s failed (%s)", root, runErr(err, cmd))
	}
	p.warnLeftover(root, out)
	// The output is guest-controlled: only names catchpen could have marked
	// count, and only the first few are named.
	named := 0
	for _, l := range strings.Split(string(out.Raw()), "\n") {
		if n, ok := strings.CutPrefix(l, "ours "); ok && slices.Contains(keep, n) && !slices.Contains(ours, n) {
			ours = append(ours, n)
		}
		if n, ok := strings.CutPrefix(l, "failed "); ok && template.DirNameRe.MatchString(n) {
			if named++; named <= 10 {
				p.warn("%s: removing catchpen's ~/%s/%s failed", n, root, n)
			} else if named == 11 {
				p.warn("removing more of catchpen's folders in ~/%s failed", root)
			}
		}
	}
	return ours, nil
}
