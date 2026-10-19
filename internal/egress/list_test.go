package egress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseEntry(t *testing.T) {
	valid := map[string]string{
		"Example.COM.":          "example.com",
		"example.com:8443":      "example.com:8443",
		"*.example.com":         "*.example.com",
		"*.Example.com:22":      "*.example.com:22",
		"1.2.3.4":               "1.2.3.4",
		"1.2.3.4:80":            "1.2.3.4:80",
		"2001:db8::1":           "2001:db8::1",
		"[2001:DB8::1]":         "2001:db8::1",
		"[2001:db8::1]:443":     "[2001:db8::1]:443",
		"  under_score.io  ":    "under_score.io",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
	}
	for in, want := range valid {
		r, err := ParseEntry(in)
		if err != nil {
			t.Errorf("ParseEntry(%q): %v", in, err)
			continue
		}
		if r.String() != want {
			t.Errorf("ParseEntry(%q) = %q, want %q", in, r.String(), want)
		}
	}
	for _, in := range []string{"", "example.com:0", "example.com:70000", "example.com:x", "[zz::1]:80",
		"exa mple.com", "-bad.com", "a..b", "*.", "*", "http://example.com", "[2001:db8::1"} {
		if _, err := ParseEntry(in); err == nil {
			t.Errorf("ParseEntry(%q) succeeded, want error", in)
		}
	}
}

func TestMatch(t *testing.T) {
	rules, err := ParseList(strings.NewReader(`
# comment
example.com
*.wild.org:8080   # inline comment
github.com:22
2001:db8::1
10.0.0.1:5432
`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"example.com", 443, true},
		{"EXAMPLE.com.", 80, true},
		{"example.com", 8443, false},
		{"sub.example.com", 443, false},
		{"a.wild.org", 8080, true},
		{"a.b.wild.org", 8080, true},
		{"wild.org", 8080, false},
		{"a.wild.org", 443, false},
		{"evilwild.org", 8080, false},
		{"github.com", 22, true},
		{"github.com", 443, false},
		{"[2001:DB8::1]", 443, true},
		{"2001:db8:0::1", 80, true},
		{"10.0.0.1", 5432, true},
		{"10.0.0.1", 443, false},
	}
	for _, c := range cases {
		if got := Match(rules, c.host, c.port); got != c.want {
			t.Errorf("Match(%s, %d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
}

func TestParseListError(t *testing.T) {
	_, err := ParseList(strings.NewReader("ok.com\nbad host\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("got %v, want line 2 error", err)
	}
}

func TestListFiles(t *testing.T) {
	dir := t.TempDir()
	if rules, err := LoadList(filepath.Join(dir, AllowlistFile)); err != nil || rules != nil {
		t.Fatalf("missing list: %v %v", rules, err)
	}
	if err := WriteList(dir, AllowlistFile, []string{"A.com", "b.com:22"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, AllowlistFile), []byte("# keep me\na.com\nb.com:22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddEntry(dir, AllowlistFile, "A.COM."); err != nil { // duplicate
		t.Fatal(err)
	}
	if err := AddEntry(dir, AllowlistFile, "c.com:8080"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveEntry(dir, AllowlistFile, "b.com:22"); err != nil {
		t.Fatal(err)
	}
	if err := AddEntry(dir, AllowlistFile, "bad host"); err == nil {
		t.Fatal("AddEntry accepted invalid entry")
	}
	b, _ := os.ReadFile(filepath.Join(dir, AllowlistFile))
	if got, want := string(b), "# keep me\na.com\nc.com:8080\n"; got != want {
		t.Fatalf("list = %q, want %q", got, want)
	}
	fi, _ := os.Stat(filepath.Join(dir, AllowlistFile))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := ReadConfig(dir)
	if err != nil || c.OnDeny != OnDenyLog || c.PromptTimeout != 30*time.Second || c.AllowPrivate {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	if err := WriteConfig(dir, Config{OnDeny: OnDenyPrompt, PromptTimeout: 5 * time.Second, AllowPrivate: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ConfigFile))
	if !strings.Contains(string(b), `"prompt_timeout": "5s"`) || !strings.Contains(string(b), `"on_deny": "prompt"`) {
		t.Fatalf("config json = %s", b)
	}
	c, err = ReadConfig(dir)
	if err != nil || c != (Config{OnDeny: OnDenyPrompt, PromptTimeout: 5 * time.Second, AllowPrivate: true}) {
		t.Fatalf("roundtrip = %+v, %v", c, err)
	}
	if err := WriteConfig(dir, Config{OnDeny: "block"}); err == nil {
		t.Fatal("WriteConfig accepted invalid on_deny")
	}
}

func TestExpandPresets(t *testing.T) {
	entries, err := ExpandPresets([]string{"github", "ghcr", "github"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e] {
			t.Fatalf("duplicate %q", e)
		}
		seen[e] = true
	}
	if !seen["github.com:22"] || !seen["ghcr.io"] {
		t.Fatalf("entries = %v", entries)
	}
	if _, err := ExpandPresets([]string{"nope"}); err == nil {
		t.Fatal("unknown preset accepted")
	}
	for name, list := range Presets {
		if err := WriteList(t.TempDir(), AllowlistFile, list); err != nil {
			t.Errorf("preset %s: %v", name, err)
		}
	}
}
