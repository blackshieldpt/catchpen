package vmtest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/curral/internal/template"
)

// Template writes settings given as one YAML mapping into the category files
// in dir: a section <name>-<category> per category it touches, and a profile
// <name> picking them, on top of the built-in minimal profile unless base is
// "default". Tests use -t <name>.
func Template(t testing.TB, dir, name, base, settings string) {
	t.Helper()
	var m yaml.Node
	if err := yaml.Unmarshal([]byte(settings), &m); err != nil {
		t.Fatal(err)
	}
	picks := []string{}
	if base != "default" {
		picks = append(picks, "minimal")
	}
	byCat := map[string][]*yaml.Node{}
	var order []string
	if len(m.Content) > 0 {
		root := m.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			cat := categoryOf(t, root.Content[i].Value)
			if _, ok := byCat[cat]; !ok {
				order = append(order, cat)
			}
			byCat[cat] = append(byCat[cat], root.Content[i], root.Content[i+1])
		}
	}
	for _, cat := range order {
		sec := &yaml.Node{Kind: yaml.MappingNode, Content: byCat[cat]}
		doc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: name + "-" + cat}, sec}}
		setYAML(t, filepath.Join(dir, cat+".yaml"), doc)
		picks = append(picks, cat+":"+name+"-"+cat)
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, p := range picks {
		list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: p})
	}
	setYAML(t, filepath.Join(dir, "profiles.yaml"), &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: name}, list}})
}

func categoryOf(t testing.TB, field string) string {
	if field == "runtimes" {
		return "runtimes"
	}
	for _, c := range template.Categories {
		if slices.Contains(c.Fields, field) {
			return c.Name
		}
	}
	t.Fatalf("vmtest.Template: no category takes %q", field)
	return ""
}

// setYAML sets n's one key in the mapping file at path, replacing it if a
// test writes the same template again.
func setYAML(t testing.TB, path string, n *yaml.Node) {
	t.Helper()
	var doc yaml.Node
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
	}
	root := n
	if len(doc.Content) > 0 {
		root = doc.Content[0]
		replaced := false
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == n.Content[0].Value {
				root.Content[i+1], replaced = n.Content[1], true
			}
		}
		if !replaced {
			root.Content = append(root.Content, n.Content...)
		}
	}
	b, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
