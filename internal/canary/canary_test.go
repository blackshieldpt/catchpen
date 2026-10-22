package canary

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestGenerateAndFiles(t *testing.T) {
	a, b := Generate(Kinds), Generate(Kinds)
	if len(a) != 3 || a[0].Kind != "aws" || a[1].Kind != "env" || a[2].Kind != "git" {
		t.Fatalf("generated %+v", a)
	}
	if a[0].Values[0] == b[0].Values[0] || a[0].Hosts[0] == b[0].Hosts[0] {
		t.Error("two VMs got the same canaries")
	}
	if !regexp.MustCompile(`^AKIA[A-Z2-7]{16}$`).MatchString(a[0].Values[0]) || len(a[0].Values[1]) != 40 ||
		!strings.HasPrefix(a[1].Values[1], "sk_live_") || !regexp.MustCompile(`^ghp_[A-Za-z0-9]{36}$`).MatchString(a[2].Values[0]) {
		t.Errorf("values don't look real: %+v", a)
	}
	files := map[string]string{}
	for _, f := range Files(a) {
		files[f.Path] = f.Content
	}
	for p, want := range map[string][]string{
		".aws/credentials": {a[0].Values[0], a[0].Values[1]},
		".aws/config":      {"endpoint_url = https://" + a[0].Hosts[0] + "\n"},
		".env.production":  {a[1].Values[0], a[1].Values[1], a[1].Values[2], "https://" + a[1].Hosts[0] + "\n", "@" + a[1].Hosts[1] + ":5432/"},
		".git-credentials": {"https://deploy-bot:" + a[2].Values[0] + "@" + a[2].Hosts[0] + "\n"},
	} {
		for _, w := range want {
			if !strings.Contains(files[p], w) {
				t.Errorf("%s lacks %q:\n%s", p, w, files[p])
			}
		}
	}
	if Generate(nil) != nil || Files(nil) != nil {
		t.Error("no kinds, no canaries")
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	if cs, err := Load(dir); err != nil || cs != nil {
		t.Fatalf("no file: %v %v", cs, err)
	}
	cs := Generate([]string{"git"})
	if err := Save(dir, cs); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(dir); err != nil || len(got) != 1 || got[0].Values[0] != cs[0].Values[0] {
		t.Fatalf("loaded %+v %v", got, err)
	}
}

type oneByte struct{ r io.Reader }

func (o oneByte) Read(p []byte) (int, error) { return o.r.Read(p[:1]) }

func TestFind(t *testing.T) {
	cs := Generate(Kinds)
	if c := Host(cs, strings.ToUpper(cs[1].Hosts[1])+"."); c == nil || c.Kind != "env" {
		t.Errorf("host match: %+v", c)
	}
	if Host(cs, "api.github.com") != nil || Host(cs, "x"+cs[0].Hosts[0]) != nil {
		t.Error("other hosts match")
	}
	key := cs[0].Values[1]
	text := strings.Repeat("diff --git a/x b/x\n+ok\n", 5000) + "+secret = " + key + "\n" + strings.Repeat("+more\n", 100)
	for _, r := range []io.Reader{strings.NewReader(text), oneByte{strings.NewReader(text)}} {
		if c, err := Scan(r, cs); err != nil || c == nil || c.Kind != "aws" {
			t.Errorf("scan: %+v %v", c, err)
		}
	}
	if c, err := Scan(strings.NewReader(strings.Replace(text, key, "x", 1)), cs); err != nil || c != nil {
		t.Errorf("clean text: %+v %v", c, err)
	}
	if c := In(cs, "GET /v1/x HTTP/1.1\r\nAuthorization: Bearer "+cs[2].Values[0]); c == nil || c.Kind != "git" {
		t.Errorf("header value: %+v", c)
	}
	if c := In(cs, "see https://"+strings.ToUpper(cs[2].Hosts[0])+"/x"); c == nil || c.Kind != "git" {
		t.Errorf("host in upper case: %+v", c)
	}
	if In(cs, strings.ToLower(cs[0].Values[0])) != nil {
		t.Error("values are matched in any case")
	}
	r := httptest.NewRequest("GET", "http://"+cs[1].Hosts[0]+"/x", nil)
	if c := InRequest(cs, r); c == nil || c.Kind != "env" {
		t.Errorf("Host header: %+v", c)
	}
	r = httptest.NewRequest("GET", "http://example.com/x", nil)
	r.Header.Set("X-Token", cs[2].Values[0])
	if c := InRequest(cs, r); c == nil || c.Kind != "git" {
		t.Errorf("request header: %+v", c)
	}
	if c := InRequest(cs, httptest.NewRequest("GET", "http://example.com/x", nil)); c != nil {
		t.Errorf("clean request: %+v", c)
	}
	if c, err := Scan(strings.NewReader("anything"), nil); c != nil || err != nil {
		t.Errorf("no canaries: %+v %v", c, err)
	}
}

func TestScanPatchBinary(t *testing.T) {
	cs := Generate([]string{"git"})
	dir := t.TempDir()
	git := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return out
	}
	scan := func(patch []byte) (*Canary, *Canary) {
		t.Helper()
		p := filepath.Join(dir, "p")
		os.WriteFile(p, patch, 0o600)
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		plain, _ := Scan(f, cs)
		f.Seek(0, io.SeekStart)
		c, err := ScanPatch(f, cs)
		if err != nil {
			t.Fatal(err)
		}
		return plain, c
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "base")
	old := make([]byte, 8000)
	rand.Read(old)
	os.WriteFile(filepath.Join(dir, "b"), old, 0o600)
	git("add", "b")
	git("commit", "-q", "-m", "binary")
	literal := git("format-patch", "--stdout", "HEAD~1..HEAD")
	if !bytes.Contains(literal, []byte("\nliteral ")) {
		t.Fatalf("no literal hunk:\n%s", literal)
	}
	if _, c := scan(literal); c != nil {
		t.Errorf("clean binary: %+v", c)
	}
	// Values split across delta ops (at most 127 bytes each) too.
	for _, n := range []int{0, 60, 100, 120, 125} {
		pad := make([]byte, n)
		rand.Read(pad)
		os.WriteFile(filepath.Join(dir, "b"), append(append(slices.Clone(old), pad...), cs[0].Values[0]...), 0o600)
		git("commit", "-q", "-am", "token")
		patch := git("format-patch", "--stdout", "HEAD~1..HEAD")
		if !bytes.Contains(patch, []byte("\ndelta ")) {
			t.Fatalf("%d: no delta hunk", n)
		}
		if plain, c := scan(patch); plain != nil || c == nil {
			t.Errorf("delta with %d bytes before the value: plain %+v, ScanPatch %+v", n, plain, c)
		}
		git("reset", "-q", "--hard", "HEAD~1")
	}
	// A new file holding the value, with headers git apply also takes.
	os.WriteFile(filepath.Join(dir, "n"), append([]byte{0}, cs[0].Values[0]...), 0o600)
	git("add", "n")
	git("commit", "-q", "-m", "new")
	patch := git("format-patch", "--stdout", "HEAD~1..HEAD")
	re := regexp.MustCompile(`(?m)^literal (\d+)$`)
	for _, hdr := range []string{"literal $1", "literal  $1", "literal $1 x", "literal \t$1"} {
		if plain, c := scan(re.ReplaceAll(patch, []byte(hdr))); plain != nil || c == nil {
			t.Errorf("header %q: plain %+v, ScanPatch %+v", hdr, plain, c)
		}
	}
	if _, ok := decode85("zzzzz"); ok {
		t.Error("a short line decoded")
	}
}
