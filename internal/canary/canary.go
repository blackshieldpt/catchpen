// Package canary makes the decoy credentials curral plants in guests and
// finds them in use. Each VM gets its own values, pointing at hosts that
// exist nowhere: any connection to one, or a value seen on the host side
// (plain HTTP requests, imported commits, PR bodies), means something in
// the VM went looking for credentials and used them.
package canary

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

// Kinds a template can plant.
var Kinds = []string{"aws", "env", "git"}

// File holds a VM's canaries, in its directory.
const File = "canaries.json"

// Canary is one planted decoy: the hosts using it reaches and the values
// that give it away (hosts included).
type Canary struct {
	Kind   string   `json:"kind"`
	Hosts  []string `json:"hosts"`
	Values []string `json:"values"`
}

// Valid reports an error for an unknown kind.
func Valid(kind string) error {
	if !slices.Contains(Kinds, kind) {
		return fmt.Errorf("unknown canary %q (%s)", kind, strings.Join(Kinds, ", "))
	}
	return nil
}

func random(alphabet string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

const (
	alnum  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	secret = alnum + "+/"
)

// Generate makes new canaries of the given kinds.
func Generate(kinds []string) []Canary {
	rnd := make([]byte, 4)
	_, _ = rand.Read(rnd)
	id := hex.EncodeToString(rnd)
	var out []Canary
	for _, k := range kinds {
		switch k {
		case "aws":
			out = append(out, Canary{Kind: k, Hosts: []string{"aws-gw-" + id + ".corp.internal"},
				Values: []string{"AKIA" + random(upper, 16), random(secret, 40)}})
		case "env":
			out = append(out, Canary{Kind: k, Hosts: []string{"api-" + id + ".corp.internal", "db-" + id + ".corp.internal"},
				Values: []string{random(alnum, 24), "sk_live_" + random(alnum, 24), random(alnum, 32)}})
		case "git":
			out = append(out, Canary{Kind: k, Hosts: []string{"git-" + id + ".corp.internal"},
				Values: []string{"ghp_" + random(alnum, 36)}})
		}
	}
	return out
}

// GuestFile is a decoy file, its path relative to the user's home.
type GuestFile struct{ Path, Content string }

// Files are the decoys planting cs writes into the guest.
func Files(cs []Canary) []GuestFile {
	var out []GuestFile
	for _, c := range cs {
		switch c.Kind {
		case "aws":
			out = append(out,
				GuestFile{".aws/credentials", fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n", c.Values[0], c.Values[1])},
				GuestFile{".aws/config", fmt.Sprintf("[default]\nregion = us-east-1\nendpoint_url = https://%s\n", c.Hosts[0])})
		case "env":
			// Not ~/.env: omp loads that one looking for API keys.
			out = append(out, GuestFile{".env.production", fmt.Sprintf("# production\nDATABASE_URL=postgres://app:%s@%s:5432/app\nSTRIPE_SECRET_KEY=%s\n"+
				"INTERNAL_API_URL=https://%s\nINTERNAL_API_TOKEN=%s\n", c.Values[0], c.Hosts[1], c.Values[1], c.Hosts[0], c.Values[2])})
		case "git":
			out = append(out, GuestFile{".git-credentials", fmt.Sprintf("https://deploy-bot:%s@%s\n", c.Values[0], c.Hosts[0])})
		}
	}
	return out
}

// Save writes a VM's canaries into its directory.
func Save(dir string, cs []Canary) error {
	b, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, File), b, 0o600)
}

// Load reads a VM's canaries; none if it has no file.
func Load(dir string) ([]Canary, error) {
	b, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cs []Canary
	return cs, json.Unmarshal(b, &cs)
}

// Host returns the canary host is one of.
func Host(cs []Canary, host string) *Canary {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for i, c := range cs {
		if slices.Contains(c.Hosts, host) {
			return &cs[i]
		}
	}
	return nil
}

// In returns the canary whose value (or host) s contains.
func In(cs []Canary, s string) *Canary {
	for i, c := range cs {
		for _, v := range append(c.Values, c.Hosts...) {
			if strings.Contains(s, v) {
				return &cs[i]
			}
		}
	}
	return nil
}

// Scan reads r to the end, returning the first canary whose value (or
// host) it contains.
func Scan(r io.Reader, cs []Canary) (*Canary, error) {
	longest := 0
	for _, c := range cs {
		for _, v := range append(c.Values, c.Hosts...) {
			longest = max(longest, len(v))
		}
	}
	if longest == 0 {
		_, err := io.Copy(io.Discard, r)
		return nil, err
	}
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if c := In(cs, string(buf)); c != nil {
			return c, nil
		}
		// Keep what may be the start of a value split across reads.
		keep := min(len(buf), longest-1)
		buf = append(buf[:0], buf[len(buf)-keep:]...)
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
