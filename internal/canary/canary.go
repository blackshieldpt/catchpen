// Package canary makes the decoy credentials curral plants in guests and
// finds them in use. Each VM gets its own values, pointing at hosts that
// exist nowhere: any connection to one, or a value seen on the host side
// (plain HTTP requests, imported commits, PR bodies), means something in
// the VM went looking for credentials and used them.
package canary

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/bits"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// In returns the canary whose value (or host, in any case) s contains.
func In(cs []Canary, s string) *Canary {
	lower := strings.ToLower(s)
	for i, c := range cs {
		for _, v := range c.Values {
			if strings.Contains(s, v) {
				return &cs[i]
			}
		}
		for _, h := range c.Hosts {
			if strings.Contains(lower, h) {
				return &cs[i]
			}
		}
	}
	return nil
}

// InRequest returns the canary in r's request line or headers (Host
// included; the body isn't read). The request URI is also matched
// percent-decoded and with its userinfo, and Basic credentials decoded.
func InRequest(cs []Canary, r *http.Request) *Canary {
	var b strings.Builder
	b.WriteString(r.Host + "\n" + r.RequestURI + "\n")
	// Decoded leniently (a bad escape elsewhere doesn't stop it), a few times
	// over for values encoded more than once; user info included.
	for _, s := range []string{r.RequestURI, strings.ReplaceAll(r.RequestURI, "+", " ")} {
		for range 3 {
			d := unescape(s)
			if d == s {
				break
			}
			s = d
			b.WriteString(s + "\n")
		}
	}
	for _, k := range []string{"Authorization", "Proxy-Authorization"} {
		for _, v := range r.Header.Values(k) {
			scheme, cred, _ := strings.Cut(strings.TrimSpace(v), " ")
			if !strings.EqualFold(scheme, "Basic") {
				continue
			}
			cred = strings.TrimRight(strings.TrimSpace(cred), "=")
			if d, err := base64.RawStdEncoding.DecodeString(cred); err == nil {
				b.WriteString(string(d) + "\n")
			} else if d, err := base64.RawURLEncoding.DecodeString(cred); err == nil {
				b.WriteString(string(d) + "\n")
			}
		}
	}
	_ = r.Header.Write(&b)
	return In(cs, b.String())
}

// unescape decodes s's valid %XX escapes and leaves the rest as they are.
func unescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ScanPatch scans a patch file like Scan, and the contents of its binary
// files too: git's base85 "GIT binary patch" hunks, whole files ("literal")
// and the bytes deltas insert ("delta"; what a delta copies from the old
// file isn't seen).
func ScanPatch(f io.ReadSeeker, cs []Canary) (*Canary, error) {
	if c, err := Scan(f, cs); c != nil || err != nil {
		return c, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	h := &hunk{br: bufio.NewReader(f)}
	budget := maxInflate
	for {
		l, err := h.line()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		delta, size, ok := hunkHeader(l)
		if !ok {
			continue
		}
		h.in = true
		var r io.Reader
		if zr, err := zlib.NewReader(h); err == nil {
			r = &spend{zr, &budget}
			r = io.LimitReader(r, size)
			if delta {
				r = &inserts{br: bufio.NewReader(r)}
			}
		} else {
			r = bytes.NewReader(nil)
		}
		c, err := Scan(readFailOK{r}, cs)
		if c != nil || err != nil {
			return c, err
		}
		if budget < 0 {
			return nil, errors.New("its binary files inflate to more than can be scanned")
		}
		if _, err := io.Copy(io.Discard, h); err != nil { // the rest of the hunk
			return nil, err
		}
	}
}

// maxInflate caps what a patch's binary hunks inflate to, in all.
var maxInflate int64 = 4 << 30

// spend reads r, taking what it reads from budget; past it, r ends.
type spend struct {
	r      io.Reader
	budget *int64
}

func (s *spend) Read(p []byte) (int, error) {
	if *s.budget < 0 {
		return 0, io.EOF
	}
	n, err := s.r.Read(p)
	*s.budget -= int64(n)
	return n, err
}

// hunkHeader parses "literal <size>" or "delta <size>" as git apply does
// (leading blanks skipped, anything after the digits ignored).
func hunkHeader(l string) (delta bool, size int64, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(l, "literal "):
		rest = l[len("literal "):]
	case strings.HasPrefix(l, "delta "):
		delta, rest = true, l[len("delta "):]
	default:
		return false, 0, false
	}
	rest = strings.TrimLeft(rest, " \t")
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	size, err := strconv.ParseInt(rest[:n], 10, 64)
	return delta, size, err == nil
}

// hunk reads a patch line by line; while in a binary hunk, Read returns
// its decoded bytes, ending at the first line that isn't base85 (which
// line then returns again).
type hunk struct {
	br      *bufio.Reader
	in      bool
	pending *string
	buf     []byte
}

func (h *hunk) line() (string, error) {
	if h.pending != nil {
		l := *h.pending
		h.pending = nil
		return l, nil
	}
	l, err := h.br.ReadString('\n')
	if err == io.EOF && l != "" {
		err = nil
	}
	return strings.TrimRight(l, "\r\n"), err
}

func (h *hunk) Read(p []byte) (int, error) {
	for len(h.buf) == 0 {
		if !h.in {
			return 0, io.EOF
		}
		l, err := h.line()
		if err != nil {
			h.in = false
			return 0, err
		}
		b, ok := decode85(l)
		if !ok {
			h.in, h.pending = false, &l
			return 0, io.EOF
		}
		h.buf = b
	}
	n := copy(p, h.buf)
	h.buf = h.buf[n:]
	return n, nil
}

// inserts reads a git delta (from its sizes and copy/insert ops) as the
// bytes it inserts, one after the other.
type inserts struct {
	br     *bufio.Reader
	header bool
	left   int // of the current insert
}

func (d *inserts) Read(p []byte) (int, error) {
	if !d.header {
		for range 2 { // source and target sizes, base-128 varints
			for {
				b, err := d.br.ReadByte()
				if err != nil {
					return 0, err
				}
				if b&0x80 == 0 {
					break
				}
			}
		}
		d.header = true
	}
	for d.left == 0 {
		op, err := d.br.ReadByte()
		if err != nil {
			return 0, err
		}
		switch {
		case op&0x80 != 0: // copy from the source: skip its offset and size
			if _, err := d.br.Discard(bits.OnesCount8(op & 0x7f)); err != nil {
				return 0, err
			}
		case op == 0:
			return 0, io.EOF // invalid
		default:
			d.left = int(op)
		}
	}
	n, err := d.br.Read(p[:min(len(p), d.left)])
	d.left -= n
	return n, err
}

type readFailOK struct{ r io.Reader }

// Read ends a corrupt stream as if it ended: what decoded so far is scanned.
func (r readFailOK) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		err = io.EOF
	}
	return n, err
}

const base85 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"

// decode85 decodes one line of a git binary hunk: a length character
// (A-Z: 1-26, a-z: 27-52) then base85 in groups of five.
func decode85(l string) ([]byte, bool) {
	if len(l) < 6 || (len(l)-1)%5 != 0 {
		return nil, false
	}
	var n int
	switch c := l[0]; {
	case c >= 'A' && c <= 'Z':
		n = int(c-'A') + 1
	case c >= 'a' && c <= 'z':
		n = int(c-'a') + 27
	default:
		return nil, false
	}
	out := make([]byte, 0, (len(l)-1)/5*4)
	for i := 1; i < len(l); i += 5 {
		var acc uint64
		for _, c := range []byte(l[i : i+5]) {
			d := strings.IndexByte(base85, c)
			if d < 0 {
				return nil, false
			}
			acc = acc*85 + uint64(d)
		}
		if acc > 0xffffffff {
			return nil, false
		}
		out = append(out, byte(acc>>24), byte(acc>>16), byte(acc>>8), byte(acc))
	}
	if n > len(out) {
		return nil, false
	}
	return out[:n], true
}

// Scan reads r to the end, returning the first canary whose value (or
// host) it contains.
func Scan(r io.Reader, cs []Canary) (*Canary, error) {
	longest := 0
	for _, c := range cs {
		for _, v := range slices.Concat(c.Values, c.Hosts) {
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
