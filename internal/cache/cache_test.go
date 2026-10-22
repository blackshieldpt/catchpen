package cache

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
)

func req(t *testing.T, method, target string) *http.Request {
	t.Helper()
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(method + " " + target + " HTTP/1.1\r\nHost: 10.0.2.102\r\n\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolve(t *testing.T) {
	all := map[string]bool{"apt": true, "go": true, "npm": true, "pypi": true, "docker": true}
	d := strings.Repeat("ab", 32)
	for _, c := range []struct {
		target, url string
		store       bool
		code        int
	}{
		{"http://deb.debian.org/debian/pool/main/j/jq/jq_1.7_amd64.deb", "https://deb.debian.org/debian/pool/main/j/jq/jq_1.7_amd64.deb", true, 0},
		{"http://Archive.Ubuntu.com/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + d, "https://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + d, true, 0},
		{"http://archive.ubuntu.com/ubuntu/dists/noble/InRelease", "https://archive.ubuntu.com/ubuntu/dists/noble/InRelease", false, 0},
		{"http://evil.example/pool/x.deb", "", false, http.StatusForbidden},
		{"http://deb.debian.org:8080/pool/x.deb", "", false, http.StatusForbidden},
		{"https://deb.debian.org/pool/x.deb", "", false, http.StatusForbidden},
		{"http://deb.debian.org/debian/pool/../dists/stable/InRelease", "https://deb.debian.org/debian/pool/../dists/stable/InRelease", false, 0},
		{"http://deb.debian.org/debian/pool/%2e%2e/dists/stable/InRelease", "https://deb.debian.org/debian/pool/%2e%2e/dists/stable/InRelease", false, 0},
		{"http://deb.debian.org/debian/dists/stable/main/by-hash/MD5Sum/x", "https://deb.debian.org/debian/dists/stable/main/by-hash/MD5Sum/x", false, 0},
		{"http://deb.debian.org/debian/dists/stable/main/by-hash/SHA256/" + d + "/x", "https://deb.debian.org/debian/dists/stable/main/by-hash/SHA256/" + d + "/x", false, 0},

		{"/go/github.com/!burnt!sushi/toml/@v/v1.4.0.zip", "https://proxy.golang.org/github.com/!burnt!sushi/toml/@v/v1.4.0.zip", true, 0},
		{"/go/golang.org/x/mod/@v/v0.0.0-20240101000000-abcdef123456.info", "https://proxy.golang.org/golang.org/x/mod/@v/v0.0.0-20240101000000-abcdef123456.info", true, 0},
		{"/go/github.com/x/y/@v/v2.0.0+incompatible.mod", "https://proxy.golang.org/github.com/x/y/@v/v2.0.0+incompatible.mod", true, 0},
		{"/go/github.com/x/y/@v/master.info", "https://proxy.golang.org/github.com/x/y/@v/master.info", false, 0},
		{"/go/github.com/x/y/@v/list", "https://proxy.golang.org/github.com/x/y/@v/list", false, 0},
		{"/go/github.com/x/y/@latest", "https://proxy.golang.org/github.com/x/y/@latest", false, 0},
		{"/go/sumdb/sum.golang.org/tile/8/0/x002/123", "https://sum.golang.org/tile/8/0/x002/123", true, 0},
		{"/go/sumdb/sum.golang.org/latest", "https://sum.golang.org/latest", false, 0},
		{"/go/sumdb/sum.golang.org/lookup/golang.org/x/text@v0.20.0", "https://sum.golang.org/lookup/golang.org/x/text@v0.20.0", false, 0},
		{"/go/sumdb/sum.example.org/latest", "", false, http.StatusNotFound},
		{"/go/sumdb/sum.golang.org/tile/../latest", "https://sum.golang.org/tile/../latest", false, 0},
		{"/go/sumdb/sum.golang.org/tile/%2E%2E/latest", "https://sum.golang.org/tile/%2E%2E/latest", false, 0},
		{"/go/x/@v/list/../@v/v1.0.0.zip", "https://proxy.golang.org/x/@v/list/../@v/v1.0.0.zip", false, 0},
		{"/go/x/./@v/v1.0.0.zip", "https://proxy.golang.org/x/./@v/v1.0.0.zip", false, 0},
		{"/go/x//@v/v1.0.0.zip", "https://proxy.golang.org/x//@v/v1.0.0.zip", false, 0},

		{"/npm/@types%2fnode", "https://registry.npmjs.org/@types%2fnode", false, 0},
		{"/npm/@types/node/-/node-22.0.0.tgz", "https://registry.npmjs.org/@types/node/-/node-22.0.0.tgz", true, 0},
		{"/npm/left-pad/-/left-pad-1.3.0.tgz?x=1", "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz?x=1", false, 0},
		{"/npm/a/-/..%2f..%2fb.tgz", "https://registry.npmjs.org/a/-/..%2f..%2fb.tgz", false, 0},

		{"/pypi/simple/requests/", "https://pypi.org/simple/requests/", false, 0},
		{"/pypi/files/packages/ab/cd/ef/requests-2.32.3-py3-none-any.whl", "https://files.pythonhosted.org/packages/ab/cd/ef/requests-2.32.3-py3-none-any.whl", true, 0},
		{"/pypi/files/packages/../x.whl", "https://files.pythonhosted.org/packages/../x.whl", false, 0},
		{"/pypi/files/packages/a%5c..%5cx.whl", "https://files.pythonhosted.org/packages/a%5c..%5cx.whl", false, 0},

		{"/v2/library/alpine/manifests/sha256:" + d + "?ns=docker.io", "https://registry-1.docker.io/v2/library/alpine/manifests/sha256:" + d, true, 0},
		{"/v2/library/alpine/blobs/sha256:" + d, "https://registry-1.docker.io/v2/library/alpine/blobs/sha256:" + d, true, 0},
		{"/v2/library/alpine/manifests/3.20", "https://registry-1.docker.io/v2/library/alpine/manifests/3.20", false, 0},
		{"/v2/library/../x/blobs/sha256:" + d, "https://registry-1.docker.io/v2/library/../x/blobs/sha256:" + d, false, 0},
		{"/v2/library/alpine/tags/list", "", false, http.StatusNotFound},
		{"/other", "", false, http.StatusNotFound},
	} {
		rt, code := resolve(req(t, "GET", c.target), all)
		if c.code != 0 {
			if rt != nil || code != c.code {
				t.Errorf("%s: %+v %d, want %d", c.target, rt, code, c.code)
			}
			continue
		}
		if rt == nil || rt.url != c.url || rt.store != c.store {
			t.Errorf("%s: %+v %d, want %s store=%v", c.target, rt, code, c.url, c.store)
		}
	}
	for _, p := range []string{"/v2/library/alpine/blobs/sha256:" + d, "http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + d} {
		if rt, _ := resolve(req(t, "GET", p), all); rt.digest != d {
			t.Errorf("%s: digest %q", p, rt.digest)
		}
	}
	a, _ := resolve(req(t, "GET", "/v2/library/alpine/blobs/sha256:"+d), all)
	b, _ := resolve(req(t, "GET", "/v2/me/app/blobs/sha256:"+d), all)
	if a.key != b.key || a.key == a.url {
		t.Errorf("a blob is stored once for every repository: %q %q", a.key, b.key)
	}
	for _, p := range []string{"/v2/", "/go/sumdb/sum.golang.org/supported"} {
		if rt, _ := resolve(req(t, "GET", p), all); rt == nil || !rt.local {
			t.Errorf("%s is answered locally", p)
		}
	}
	if rt, code := resolve(req(t, "GET", "/npm/left-pad"), map[string]bool{"go": true}); rt != nil || code != http.StatusNotFound {
		t.Errorf("a kind the VM doesn't use: %+v %d", rt, code)
	}
	if rt, code := resolve(req(t, "POST", "/npm/-/npm/v1/security/advisories/bulk"), all); rt != nil || code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %+v %d", rt, code)
	}
}

// oneByte reads one byte at a time: every split of a link across reads.
type oneByte struct{ r io.Reader }

func (o oneByte) Read(p []byte) (int, error) { return o.r.Read(p[:1]) }

func TestReplace(t *testing.T) {
	for _, c := range []struct{ in, from, to, want string }{
		{`{"tarball":"https://registry.npmjs.org/a/-/a-1.0.0.tgz","x":"https://registry.npmjs.org.evil/x"}`, `"https://registry.npmjs.org/`, `"http://10.0.2.102/npm/`,
			`{"tarball":"http://10.0.2.102/npm/a/-/a-1.0.0.tgz","x":"https://registry.npmjs.org.evil/x"}`},
		{`<a href="https://files.pythonhosted.org/packages/ab/x.whl#sha256=00">x</a>https://files.pythonhosted.org/`, "https://files.pythonhosted.org/", "http://10.0.2.102/pypi/files/",
			`<a href="http://10.0.2.102/pypi/files/packages/ab/x.whl#sha256=00">x</a>http://10.0.2.102/pypi/files/`},
		{"https://files.pythonhosted.or", "https://files.pythonhosted.org/", "X", "https://files.pythonhosted.or"},
	} {
		for _, r := range []io.Reader{strings.NewReader(c.in), oneByte{strings.NewReader(c.in)}} {
			var b strings.Builder
			if err := replace(&b, r, []byte(c.from), []byte(c.to)); err != nil || b.String() != c.want {
				t.Errorf("%q: %q %v", c.in, b.String(), err)
			}
		}
	}
}
