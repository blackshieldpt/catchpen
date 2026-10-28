//go:build integration

package gitsrc_test

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/catchpen/internal/gitsrc"
	"github.com/blackshieldpt/catchpen/internal/template"
)

// Every pinned skill and plugin, built in or in catchpen init's examples, is
// still fetchable, with its manifest and a licence.
func TestBuiltinSourcesFetch(t *testing.T) {
	f := gitsrc.Fetcher{Dir: t.TempDir()}
	manifest := map[string]string{"claude": ".claude-plugin/plugin.json", "codex": ".codex-plugin/plugin.json"}
	examples, err := os.ReadFile("../../cmd/catchpen/initfiles/examples/agents.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fetched := 0
	for _, file := range []string{template.BuiltinFile("agents"), string(examples)} {
		var secs map[string]yaml.Node
		if err := yaml.Unmarshal([]byte(file), &secs); err != nil {
			t.Fatal(err)
		}
		delete(secs, "default")
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
			placeholder := func(git string) bool { return strings.Contains(git, "/acme/") }
			for _, s := range sec.Skills {
				if !placeholder(s.Git) {
					srcs = append(srcs, src{s.Git, s.Ref, s.Path, "SKILL.md"})
				}
			}
			for _, p := range sec.Plugins {
				if p.Git == "" || placeholder(p.Git) {
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
			fetched += len(srcs)
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
					// A licence file, or a license: field in SKILL.md.
					if !slices.ContainsFunc(names, func(n string) bool { return n == "LICENSE" || n == "LICENSE.txt" || n == "LICENSE.md" }) &&
						!slices.ContainsFunc(files, func(fl gitsrc.File) bool {
							return fl.Name == "SKILL.md" && strings.Contains(string(fl.Data), "\nlicense: ")
						}) {
						t.Errorf("%s: no licence in %v", s.path, names)
					}
				}
			})
		}
	}
	if fetched < 6 { // claude's three skills and the examples' pins
		t.Errorf("fetched %d pins", fetched)
	}
}
