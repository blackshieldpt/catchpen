package egress

import (
	"fmt"
	"sort"
	"strings"
)

// Presets are named allowlist bundles. Entries without a port cover 80 and 443.
var Presets = map[string][]string{
	"anthropic": {
		"api.anthropic.com",
		"console.anthropic.com",
		"statsig.anthropic.com",
		"claude.ai",
		"*.claude.ai", // includes downloads.claude.ai (installer/updates)
	},
	"github": {
		"github.com",
		"github.com:22",
		"ssh.github.com:443",
		"api.github.com",
		"codeload.github.com",
		"uploads.github.com",
		"*.githubusercontent.com", // raw., objects. (release assets), avatars
	},
	"npm": {
		"registry.npmjs.org",
		"registry.yarnpkg.com",
	},
	"pypi": {
		"pypi.org",
		"files.pythonhosted.org",
	},
	"go": {
		"proxy.golang.org",
		"sum.golang.org",
	},
	"apt-ubuntu": {
		"archive.ubuntu.com",
		"*.archive.ubuntu.com",
		"security.ubuntu.com",
		"esm.ubuntu.com",
	},
	"apt-debian": {
		"deb.debian.org",
		"security.debian.org",
	},
	"docker-hub": {
		"registry-1.docker.io",
		"auth.docker.io",
		"index.docker.io",
		"production.cloudflare.docker.com",
		"production.cloudfront.docker.com",
		"docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com",
	},
	"docker-apt": {
		"download.docker.com",
	},
	"ghcr": {
		"ghcr.io",
		"pkg-containers.githubusercontent.com",
	},
}

// ExpandPresets returns the deduplicated entries of the named presets, in order.
func ExpandPresets(names []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		entries, ok := Presets[n]
		if !ok {
			known := make([]string, 0, len(Presets))
			for k := range Presets {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("unknown egress preset %q (known: %s)", n, strings.Join(known, ", "))
		}
		for _, e := range entries {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out, nil
}
