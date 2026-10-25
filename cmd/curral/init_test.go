package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/creds"
	"github.com/blackshieldpt/curral/internal/paths"
	"github.com/blackshieldpt/curral/internal/template"
)

func testPaths(t *testing.T) paths.Paths {
	root := t.TempDir()
	return paths.Paths{Config: filepath.Join(root, "config"), Data: filepath.Join(root, "data"), Cache: filepath.Join(root, "cache")}
}

// uncomment turns "# setting" lines into settings, leaving "## " prose as comments.
var settingLine = regexp.MustCompile(`(?m)^# ?`)

func uncomment(b []byte) []byte {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "##") {
			l = settingLine.ReplaceAllString(l, "")
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

func TestInitFilesLoadAsWritten(t *testing.T) {
	p := testPaths(t)
	if err := writeInitFiles(p, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(p.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	sec, err := creds.ParseFile(p.SecretsFile())
	if err != nil || len(sec) != 0 {
		t.Fatalf("secrets.env: %v %v", sec, err)
	}
	if st, _ := os.Stat(p.SecretsFile()); st.Mode().Perm() != 0o600 {
		t.Fatalf("secrets.env mode %o", st.Mode().Perm())
	}
	// The category files are all comments: the built-in defaults apply.
	lib, err := template.Loader{UserDir: p.Templates()}.Library()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range template.Categories {
		if _, err := os.Stat(filepath.Join(p.Templates(), c.Name+".yaml")); err != nil {
			t.Errorf("no %s.yaml: %v", c.Name, err)
		}
	}
	got, err := lib.Load("")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := template.Loader{}.Load("")
	gy, _ := got.YAML()
	wy, _ := want.YAML()
	if string(gy) != string(wy) {
		t.Fatalf("defaults with the init files:\n%s\nwant:\n%s", gy, wy)
	}
}

// Every commented setting is valid, so the samples stay in step with the schema.
func TestInitFilesLoadUncommented(t *testing.T) {
	p := testPaths(t)
	if err := writeInitFiles(p, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p.ConfigFile())
	if err := os.WriteFile(p.ConfigFile(), uncomment(b), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(p.ConfigFile()); err != nil {
		t.Fatal(err)
	}

	b, _ = os.ReadFile(p.SecretsFile())
	if err := os.WriteFile(p.SecretsFile(), uncomment(b), 0o600); err != nil {
		t.Fatal(err)
	}
	sec, err := creds.ParseFile(p.SecretsFile())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if _, ok := sec[k]; !ok {
			t.Errorf("secrets.env lacks %s", k)
		}
	}
	for k := range template.Providers {
		if _, ok := sec[k]; !ok {
			t.Errorf("secrets.env lacks provider key %s", k)
		}
	}

	dir := p.Templates()
	for _, c := range template.Categories {
		f := filepath.Join(dir, c.Name+".yaml")
		b, _ = os.ReadFile(f)
		if err := os.WriteFile(f, uncomment(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []string{"scripts/setup.sh", "files/gitconfig"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(a)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, a), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := template.Loader{UserDir: dir}.Library()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Load(""); err != nil {
		t.Fatal(err)
	}
	// Every section, built-in copy or example, loads on its own.
	n := 0
	for _, c := range template.Categories {
		secs, _ := lib.Sections(c.Name)
		for _, sec := range secs {
			if _, err := lib.Load(c.Name + ":" + sec.Name); err != nil {
				t.Errorf("%s:%s: %v", c.Name, sec.Name, err)
			}
			n++
		}
	}
	if n < 60 {
		t.Errorf("only %d sections", n)
	}
}

func TestInitKeepsExistingFiles(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.SecretsFile(), []byte("X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := writeInitFiles(p, &out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.SecretsFile()); string(b) != "X=1\n" {
		t.Fatalf("secrets.env overwritten: %q", b)
	}
	if !strings.Contains(out.String(), "kept     "+p.SecretsFile()) {
		t.Fatalf("output: %s", out.String())
	}
	if _, err := os.Stat(p.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := writeInitFiles(p, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "created") {
		t.Fatalf("second run created files: %s", out.String())
	}
}
