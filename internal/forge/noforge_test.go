package forge

import (
	"slices"
	"strings"
	"testing"
)

// The hint for a repo whose host has no forge offers every mode that host
// can use: minted-token only off github.com (it is Gitea only).
func TestNoForgeHintModes(t *testing.T) {
	for _, c := range []struct {
		host string
		want []string
	}{
		{"git.example.com", []string{ModeDeployKey, ModeMintedToken, ModeToken, ModeAgent, ModeNone}},
		{"github.com", []string{ModeDeployKey, ModeToken, ModeAgent, ModeNone}},
	} {
		err := Registry{}.noForge("o/r", c.host)
		_, after, ok := strings.Cut(err.Error(), "--mode ")
		if !ok {
			t.Fatalf("%s: no --mode in %q", c.host, err)
		}
		got := strings.Split(strings.Fields(after)[0], "|")
		slices.Sort(got)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: hint modes %q, want %q", c.host, got, want)
		}
	}
}
