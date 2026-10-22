// Package cache serves the pull-through package caches guests reach at
// Addr: apt, Go modules, npm, PyPI and Docker Hub. The host fetches over
// HTTPS from fixed upstreams (and the CDNs they redirect to) and stores only
// objects that never change; indexes are fetched every time. Guests can only
// read: nothing they send ends up in the store.
package cache

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// The guest-side endpoint, served by QEMU guestfwd.
const (
	Addr = "10.0.2.102"
	Port = 80
	Base = "http://" + Addr
)

// Kinds are the caches a template can list.
var Kinds = []string{"apt", "go", "npm", "pypi", "docker"}

// AptHosts are the mirrors apt fetches through the cache (guests ask for
// http://, the cache fetches https://).
var AptHosts = []string{"archive.ubuntu.com", "security.ubuntu.com", "deb.debian.org", "security.debian.org"}

// Upstreams, by kind.
var (
	goProxy   = "https://proxy.golang.org"
	goSumDB   = "https://sum.golang.org"
	npmReg    = "https://registry.npmjs.org"
	pypiIndex = "https://pypi.org"
	pypiFiles = "https://files.pythonhosted.org"
	registry  = "https://registry-1.docker.io"
)

// Valid reports an error for an unknown kind.
func Valid(kind string) error {
	if !slices.Contains(Kinds, kind) {
		return fmt.Errorf("unknown cache %q (%s)", kind, strings.Join(Kinds, ", "))
	}
	return nil
}

type route struct {
	kind     string
	url      string
	store    bool
	key      string // what the object is stored as: url, or its digest
	digest   string // the sha256 (hex) the object must hash to, if known
	from, to string // pass-through bodies: upstream links (from) to the cache (to)
	local    bool   // answered here: the registry's /v2/ check, the sumdb probe
}

var (
	goFileRe   = regexp.MustCompile(`/@v/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+incompatible)?\.(info|mod|zip)$`)
	npmTgzRe   = regexp.MustCompile(`/-/[^/]+\.tgz$`)
	registryRe = regexp.MustCompile(`^/v2/([a-z0-9._/-]+)/(manifests|blobs)/([^/]+)$`)
	digestRe   = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	byHashRe   = regexp.MustCompile(`/by-hash/SHA256/([0-9a-f]{64})$`)
)

// resolve maps a guest request to its upstream; nil with an HTTP status when
// it isn't one the caches serve.
func resolve(r *http.Request, kinds map[string]bool) (*route, int) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return nil, http.StatusMethodNotAllowed
	}
	rt, code := routeFor(r)
	if rt == nil {
		return nil, code
	}
	if !kinds[rt.kind] {
		return nil, http.StatusNotFound
	}
	if rt.key == "" {
		rt.key = rt.url
	}
	return rt, 0
}

func routeFor(r *http.Request) (*route, int) {
	if r.URL.IsAbs() { // apt, using the cache as its HTTP proxy
		if r.URL.Scheme != "http" || r.URL.Port() != "" || !slices.Contains(AptHosts, strings.ToLower(r.URL.Hostname())) {
			return nil, http.StatusForbidden
		}
		p := r.URL.EscapedPath()
		plain := r.URL.RawQuery == "" && canonical(p)
		rt := &route{kind: "apt", url: "https://" + strings.ToLower(r.URL.Hostname()) + p + query(r),
			store: plain && strings.Contains(p, "/pool/")}
		if m := byHashRe.FindStringSubmatch(p); m != nil {
			rt.store, rt.digest = plain, m[1]
		}
		return rt, 0
	}
	p, q := r.URL.EscapedPath(), query(r)
	plain := r.URL.RawQuery == "" && canonical(p)
	switch {
	case strings.HasPrefix(p, "/go/sumdb/"):
		// The go command asks the proxy whether it proxies the checksum
		// database; proxy.golang.org doesn't, so the cache does.
		rest, ok := strings.CutPrefix(p, "/go/sumdb/sum.golang.org/")
		switch {
		case !ok:
			return nil, http.StatusNotFound
		case rest == "supported":
			return &route{kind: "go", local: true}, 0
		}
		return &route{kind: "go", url: goSumDB + "/" + rest + q, store: plain && strings.HasPrefix(rest, "tile/")}, 0
	case strings.HasPrefix(p, "/go/"):
		rest := strings.TrimPrefix(p, "/go")
		return &route{kind: "go", url: goProxy + rest + q, store: plain && goFileRe.MatchString(rest)}, 0
	case strings.HasPrefix(p, "/npm/"):
		rest := strings.TrimPrefix(p, "/npm")
		rt := &route{kind: "npm", url: npmReg + rest + q, store: plain && npmTgzRe.MatchString(rest)}
		if !rt.store {
			rt.from, rt.to = `"`+npmReg+`/`, `"`+Base+`/npm/`
		}
		return rt, 0
	case strings.HasPrefix(p, "/pypi/simple/"):
		return &route{kind: "pypi", url: pypiIndex + strings.TrimPrefix(p, "/pypi") + q, from: pypiFiles + "/", to: Base + "/pypi/files/"}, 0
	case strings.HasPrefix(p, "/pypi/files/"):
		rest := strings.TrimPrefix(p, "/pypi/files")
		return &route{kind: "pypi", url: pypiFiles + rest + q, store: plain && strings.HasPrefix(rest, "/packages/")}, 0
	case p == "/v2/" || p == "/v2":
		return &route{kind: "docker", local: true}, 0
	case strings.HasPrefix(p, "/v2/"):
		// containerd adds ?ns=docker.io for mirrors; Docker Hub needs no query.
		m := registryRe.FindStringSubmatch(p)
		if m == nil {
			return nil, http.StatusNotFound
		}
		rt := &route{kind: "docker", url: registry + p}
		if d := digestRe.FindStringSubmatch(m[3]); d != nil && canonical(p) {
			// Shared by every repository with the object.
			rt.store, rt.digest, rt.key = true, d[1], m[2]+"/"+m[3]
		}
		return rt, 0
	}
	return nil, http.StatusNotFound
}

// canonical reports whether an escaped path has no empty, . or .. segments
// and no encoded dots, slashes or backslashes: nothing an upstream may
// resolve to another object than the one stored under the path.
func canonical(p string) bool {
	l := strings.ToLower(p)
	if strings.Contains(l, "%2e") || strings.Contains(l, "%2f") || strings.Contains(l, "%5c") {
		return false
	}
	for _, seg := range strings.Split(p, "/")[1:] {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func query(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}
