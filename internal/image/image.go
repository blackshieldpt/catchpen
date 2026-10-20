// Package image downloads and verifies upstream cloud images.
package image

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

// Spec describes a cloud image. Exactly one of ChecksumURL, SHA256, SHA512 must be set.
// Images must be cloud-init enabled and apt based (Debian/Ubuntu family).
type Spec struct {
	Name        string `yaml:"-"`
	URL         string `yaml:"url"`
	ChecksumURL string `yaml:"checksum_url"` // SHA256SUMS / SHA512SUMS style file
	SHA256      string `yaml:"sha256"`
	SHA512      string `yaml:"sha512"`
}

var builtin = map[string]Spec{
	"ubuntu-24.04": {
		URL:         "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img",
		ChecksumURL: "https://cloud-images.ubuntu.com/noble/current/SHA256SUMS",
	},
	"ubuntu-26.04": {
		URL:         "https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img",
		ChecksumURL: "https://cloud-images.ubuntu.com/resolute/current/SHA256SUMS",
	},
	"debian-13": {
		URL:         "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2",
		ChecksumURL: "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS",
	},
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	sha256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	sha512Re = regexp.MustCompile(`^[0-9a-fA-F]{128}$`)
)

func Builtin(name string) (Spec, error) {
	s, ok := builtin[name]
	if !ok {
		return Spec{}, fmt.Errorf("unknown distro %q (built-in: %s)", name, strings.Join(BuiltinNames(), ", "))
	}
	s.Name = name
	return s, nil
}

func BuiltinNames() []string {
	return []string{"debian-13", "ubuntu-24.04", "ubuntu-26.04"}
}

func (s Spec) Validate() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("invalid distro name %q", s.Name)
	}
	if !strings.HasPrefix(s.URL, "https://") {
		return fmt.Errorf("distro %s: url must be https", s.Name)
	}
	n := 0
	for _, v := range []string{s.ChecksumURL, s.SHA256, s.SHA512} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("distro %s: set exactly one of checksum_url, sha256, sha512", s.Name)
	}
	if s.ChecksumURL != "" && !strings.HasPrefix(s.ChecksumURL, "https://") {
		return fmt.Errorf("distro %s: checksum_url must be https", s.Name)
	}
	if s.SHA256 != "" && !sha256Re.MatchString(s.SHA256) {
		return fmt.Errorf("distro %s: sha256 must be 64 hex characters", s.Name)
	}
	if s.SHA512 != "" && !sha512Re.MatchString(s.SHA512) {
		return fmt.Errorf("distro %s: sha512 must be 128 hex characters", s.Name)
	}
	return nil
}

// Digest is "<algo>:<hex>".
type Digest string

func (d Digest) parts() (string, string) {
	a, h, _ := strings.Cut(string(d), ":")
	return a, h
}

func (d Digest) newHash() (hash.Hash, error) {
	switch a, _ := d.parts(); a {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported digest %q", d)
	}
}

// ParseSums finds the digest for file in a SHA*SUMS listing ("<hex> [ *]<name>").
func ParseSums(r io.Reader, file string) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			if _, err := hex.DecodeString(f[0]); err != nil {
				return "", fmt.Errorf("bad digest for %s", file)
			}
			return strings.ToLower(f[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s not listed in checksum file", file)
}

var client = &http.Client{
	Timeout: 30 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to non-https %s", req.URL)
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// pinned returns the digest fixed in the spec, or "" when it comes from a checksum file.
func (s Spec) pinned() Digest {
	switch {
	case s.SHA256 != "":
		return Digest("sha256:" + strings.ToLower(s.SHA256))
	case s.SHA512 != "":
		return Digest("sha512:" + strings.ToLower(s.SHA512))
	}
	return ""
}

// Expected returns the upstream digest for the image.
func (s Spec) Expected(ctx context.Context) (Digest, error) {
	if d := s.pinned(); d != "" {
		return d, nil
	}
	resp, err := get(ctx, s.ChecksumURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h, err := ParseSums(io.LimitReader(resp.Body, 1<<20), path.Base(s.URL))
	if err != nil {
		return "", err
	}
	switch len(h) {
	case 64:
		return Digest("sha256:" + h), nil
	case 128:
		return Digest("sha512:" + h), nil
	}
	return "", fmt.Errorf("%s: digest for %s is neither sha256 nor sha512", s.ChecksumURL, path.Base(s.URL))
}

// meta records where a local image came from (<name>.img.json).
type meta struct {
	URL    string `json:"url"`
	Digest Digest `json:"digest"`
}

// Ensure returns the local path of a verified image, downloading it if absent
// or if the spec's URL or pinned digest no longer matches what was fetched.
func Ensure(ctx context.Context, dir string, s Spec, progress io.Writer) (string, Digest, error) {
	dst := filepath.Join(dir, s.Name+".img")
	if d, ok, err := cached(dst, s); err != nil || ok {
		return dst, d, err
	}
	unlock, err := fsutil.Lock(dst + ".lock")
	if err != nil {
		return "", "", err
	}
	defer unlock()
	if d, ok, err := cached(dst, s); err != nil || ok {
		return dst, d, err // fetched by a concurrent curral
	}
	want, err := s.Expected(ctx)
	if err != nil {
		return "", "", fmt.Errorf("checksum for %s: %w", s.Name, err)
	}
	// image present but unrecorded (fetched manually, or by an older curral)
	if ok, _ := fileMatches(dst, want); !ok {
		if err := download(ctx, s.URL, dst, want, progress); err != nil {
			return "", "", err
		}
	}
	b, _ := json.Marshal(meta{URL: s.URL, Digest: want})
	if err := fsutil.WriteFileAtomic(dst+".json", append(b, '\n'), 0o600); err != nil {
		return "", "", err
	}
	_ = os.Remove(dst + ".digest") // pre-metadata format
	return dst, want, nil
}

// EnsureLatest is Ensure, but also checks upstream for a newer image (a
// changed checksum) and fetches it.
func EnsureLatest(ctx context.Context, dir string, s Spec, progress io.Writer) (string, Digest, error) {
	dst := filepath.Join(dir, s.Name+".img")
	if d, ok, err := cached(dst, s); err == nil && ok && s.pinned() == "" {
		want, err := s.Expected(ctx)
		if err != nil {
			return "", "", fmt.Errorf("checksum for %s: %w", s.Name, err)
		}
		if want != d {
			_ = os.Remove(dst + ".json") // makes Ensure fetch and verify it again
		}
	}
	return Ensure(ctx, dir, s, progress)
}

// cached reports whether dst is a recorded download of s.
func cached(dst string, s Spec) (Digest, bool, error) {
	b, err := os.ReadFile(dst + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	var m meta
	if json.Unmarshal(b, &m) != nil || m.URL != s.URL || m.Digest == "" {
		return "", false, nil
	}
	if p := s.pinned(); p != "" && p != m.Digest {
		return "", false, nil
	}
	if _, err := os.Stat(dst); err != nil {
		return "", false, nil
	}
	return m.Digest, true, nil
}

func fileMatches(p string, want Digest) (bool, error) {
	h, err := want.newHash()
	if err != nil {
		return false, err
	}
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	_, wantHex := want.parts()
	return hex.EncodeToString(h.Sum(nil)) == wantHex, nil
}

func download(ctx context.Context, url, dst string, want Digest, progress io.Writer) error {
	h, err := want.newHash()
	if err != nil {
		return err
	}
	resp, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	pr := &progressReader{r: resp.Body, total: resp.ContentLength, w: progress, name: path.Base(url)}
	if _, err := io.Copy(io.MultiWriter(f, h), pr); err != nil {
		f.Close()
		return fmt.Errorf("download %s: %w", url, err)
	}
	pr.done()
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, wantHex := want.parts()
	if got := hex.EncodeToString(h.Sum(nil)); got != wantHex {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, got, wantHex)
	}
	return os.Rename(tmp, dst)
}

type progressReader struct {
	r     io.Reader
	w     io.Writer
	name  string
	total int64
	n     int64
	last  time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.w != nil && time.Since(p.last) > 2*time.Second {
		p.last = time.Now()
		if p.total > 0 {
			fmt.Fprintf(p.w, "downloading %s: %d%%\n", p.name, p.n*100/p.total)
		} else {
			fmt.Fprintf(p.w, "downloading %s: %d MiB\n", p.name, p.n>>20)
		}
	}
	return n, err
}

func (p *progressReader) done() {
	if p.w != nil {
		fmt.Fprintf(p.w, "downloaded %s (%d MiB), verifying\n", p.name, p.n>>20)
	}
}
