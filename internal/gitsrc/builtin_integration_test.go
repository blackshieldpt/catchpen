//go:build integration

package gitsrc

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/curral/internal/template"
)

// Every pinned built-in skill and plugin is still fetchable, with its
// manifest and a licence.
func TestBuiltinSourcesFetch(t *testing.T) {
	f := Fetcher{Dir: t.TempDir()}
	manifest := map[string]string{"claude": ".claude-plugin/plugin.json", "codex": ".codex-plugin/plugin.json"}
	for _, cat := range []string{"skills", "plugins"} {
		var secs map[string]yaml.Node
		if err := yaml.Unmarshal([]byte(template.BuiltinFile(cat)), &secs); err != nil {
			t.Fatal(err)
		}
		delete(secs, "default")
		if len(secs) == 0 {
			t.Fatalf("no built-in %s", cat)
		}
		for _, name := range slices.Sorted(maps.Keys(secs)) {
			node := secs[name]
			var sec struct {
				Skills  []template.Skill
				Plugins []template.Plugin
			}
			if err := node.Decode(&sec); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			type src struct{ git, ref, path, want string }
			var srcs []src
			for _, s := range sec.Skills {
				srcs = append(srcs, src{s.Git, s.Ref, s.Path, "SKILL.md"})
			}
			for _, p := range sec.Plugins {
				if p.Git == "" {
					continue
				}
				if manifest[p.Agent] == "" {
					t.Fatalf("%s: no manifest known for %s plugins", name, p.Agent)
				}
				srcs = append(srcs, src{p.Git, p.Ref, p.Path, manifest[p.Agent]})
			}
			if len(srcs) == 0 {
				continue
			}
			t.Run(name, func(t *testing.T) {
				for _, s := range srcs {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					files, err := f.Files(ctx, s.git, s.ref, s.path)
					cancel()
					if err != nil {
						t.Fatalf("%s %s %s: %v", s.git, s.ref, s.path, err)
					}
					var names []string
					for _, fl := range files {
						names = append(names, fl.Name)
					}
					if !slices.Contains(names, s.want) {
						t.Errorf("%s: no %s in %v", s.path, s.want, names)
					}
					if !slices.ContainsFunc(names, func(n string) bool { return n == "LICENSE" || n == "LICENSE.txt" || n == "LICENSE.md" }) {
						t.Errorf("%s: no licence in %v", s.path, names)
					}
				}
			})
		}
	}
}
