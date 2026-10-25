// Package gitsrc fetches folders of public git repos at pinned commits, on the
// host, for skills and plugins that curral copies into VMs. Each repo URL has
// its own cached bare repo; a commit already fetched needs no network. Files
// are read from git's object store (ls-tree, cat-file), never through archive
// or checkout, so the repo's .gitattributes can't change them.
package gitsrc

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blackshieldpt/curral/internal/fsutil"
	"github.com/blackshieldpt/curral/internal/sshx"
)

type Fetcher struct {
	Dir string // cache, e.g. paths.Sources(); created if missing
	// Protocols git may use; nil means https only. Tests use "file".
	Protocols []string
}

var (
	maxBytes int64 = 64 << 20 // per folder
	maxFiles       = 20000    // regular files per folder
)

var (
	refRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dirRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)
)

// passEnv are the host variables git gets, if set, so proxies and custom CAs work.
var passEnv = []string{"https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY", "no_proxy", "NO_PROXY", "all_proxy", "ALL_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"}

func (f Fetcher) protocols() []string {
	if f.Protocols == nil {
		return []string{"https"}
	}
	return f.Protocols
}

// git builds, but doesn't start, a git command with a fixed config and env.
func (f Fetcher) git(ctx context.Context, gitDir string, args ...string) *exec.Cmd {
	var argv []string
	if gitDir != "" {
		argv = append(argv, "--git-dir="+gitDir)
	}
	argv = append(argv, "-c", "protocol.allow=never")
	for _, p := range f.protocols() {
		argv = append(argv, "-c", "protocol."+p+".allow=always")
	}
	argv = append(argv,
		"-c", "core.hooksPath=/dev/null",
		"-c", "credential.helper=",
		"-c", "fetch.fsckObjects=true",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
		"-c", "http.lowSpeedLimit=1000",
		"-c", "http.lowSpeedTime=30")
	cmd := exec.CommandContext(ctx, "git", append(argv, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.Dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_ALLOW_PROTOCOL=" + strings.Join(f.protocols(), ":"),
	}
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Stderr = &bytes.Buffer{}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

func run(cmd *exec.Cmd) error { return gitErr(cmd, cmd.Run()) }

// gitErr names the git subcommand and adds the end of its stderr to err.
func gitErr(cmd *exec.Cmd, err error) error {
	if err == nil {
		return nil
	}
	sub := ""
	for i := 1; i < len(cmd.Args) && sub == ""; i++ {
		switch a := cmd.Args[i]; {
		case a == "-c":
			i++
		case strings.HasPrefix(a, "--git-dir="):
		default:
			sub = a
		}
	}
	tail := cmd.Stderr.(*bytes.Buffer).Bytes()
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	return fmt.Errorf("git %s: %w: %s", sub, err, strings.TrimSpace(sshx.Printable(string(tail))))
}

// File is one regular file of a fetched folder; Name is relative to it.
type File struct {
	Name string
	Exec bool // mode 0755 rather than 0644
	Data []byte
}

func (f Fetcher) repoDir(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(f.Dir, hex.EncodeToString(sum[:])[:16]+".git")
}

// listed is one file from ls-tree.
type listed struct {
	File
	id   string
	size int64
}

// Files returns the regular files under dir at commit ref of the repo at url.
func (f Fetcher) Files(ctx context.Context, url, ref, dir string) ([]File, error) {
	if !refRe.MatchString(ref) {
		return nil, fmt.Errorf("ref %q is not a full lower-case commit SHA", ref)
	}
	if dir != "." && (!dirRe.MatchString(dir) || path.Clean(dir) != dir || slices.Contains(strings.Split(dir, "/"), "..")) {
		return nil, fmt.Errorf("dir %q is not a clean relative folder path", dir)
	}
	scheme, _, ok := strings.Cut(url, "://")
	if strings.HasPrefix(url, "-") || !ok || !slices.Contains(f.protocols(), scheme) {
		return nil, fmt.Errorf("url %q doesn't use an allowed protocol (%s)", url, strings.Join(f.protocols(), ", "))
	}

	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return nil, err
	}
	repo := f.repoDir(url)
	unlock, err := fsutil.Lock(repo + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	// A git killed mid-fetch leaves lock files that fail every later fetch.
	// Holding the flock, no other curral git runs in repo.
	ents, err := os.ReadDir(repo)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".lock") {
			if err := os.Remove(filepath.Join(repo, e.Name())); err != nil {
				return nil, err
			}
		}
	}
	if err := run(f.git(ctx, "", "init", "--bare", "-q", repo)); err != nil {
		return nil, err
	}

	has := func() bool { return run(f.git(ctx, repo, "cat-file", "-e", ref+"^{commit}")) == nil }
	if !has() {
		ferr := run(f.git(ctx, repo, "fetch", "--depth=1", "--no-tags", "-q", "--", url, ref+":refs/pins/"+ref))
		if !has() {
			if ferr != nil {
				return nil, fmt.Errorf("%q has no commit %s: %w", url, ref, ferr)
			}
			return nil, fmt.Errorf("%q has no commit %s", url, ref)
		}
		if ferr != nil {
			return nil, ferr
		}
	}

	args := []string{"ls-tree", "-r", "-z", "-l", "--full-tree", ref}
	if dir != "." {
		args = append(args, "--", dir)
	}
	var out bytes.Buffer
	ls := f.git(ctx, repo, args...)
	ls.Stdout = &out
	if err := run(ls); err != nil {
		return nil, err
	}
	var files []listed
	var total int64
	for _, rec := range strings.Split(out.String(), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("%q at %s: bad ls-tree record %q", url, ref, rec)
		}
		name := p
		if dir != "." {
			var ok bool
			if name, ok = strings.CutPrefix(p, dir+"/"); !ok {
				return nil, fmt.Errorf("%q at %s: %q is a file, not a folder", url, ref, p)
			}
		}
		var fl listed
		switch fields[0] {
		case "100644":
		case "100755":
			fl.Exec = true
		case "120000":
			return nil, fmt.Errorf("%q at %s: %q is a symlink, which curral doesn't copy", url, ref, p)
		case "160000":
			return nil, fmt.Errorf("%q at %s: %q is a submodule, which curral doesn't copy", url, ref, p)
		default:
			return nil, fmt.Errorf("%q at %s: %q has unsupported mode %s", url, ref, p, fields[0])
		}
		if err := checkName(name); err != nil {
			return nil, fmt.Errorf("%q at %s: %w", url, ref, err)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%q at %s: bad size in ls-tree record %q", url, ref, rec)
		}
		fl.Name, fl.id, fl.size = name, fields[2], size
		total += size
		files = append(files, fl)
	}
	if total > maxBytes || len(files) > maxFiles {
		return nil, fmt.Errorf("%q at %s: %s is over %d MiB or %d files", url, ref, dir, maxBytes>>20, maxFiles)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%q at %s has no folder %q", url, ref, dir)
	}

	if err := f.read(ctx, repo, files); err != nil {
		return nil, fmt.Errorf("%q at %s: %w", url, ref, err)
	}
	res := make([]File, len(files))
	for i, fl := range files {
		res[i] = fl.File
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res, nil
}

// read fills in each file's Data from cat-file --batch, checking every
// object's header against the listing.
func (f Fetcher) read(ctx context.Context, repo string, files []listed) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var ids strings.Builder
	for _, fl := range files {
		ids.WriteString(fl.id + "\n")
	}
	cmd := f.git(ctx, repo, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(ids.String())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return gitErr(cmd, err)
	}
	if err := readBatch(bufio.NewReader(stdout), files); err != nil {
		cancel()
		cmd.Wait()
		return err
	}
	return gitErr(cmd, cmd.Wait())
}

func readBatch(r *bufio.Reader, files []listed) error {
	for i := range files {
		fl := &files[i]
		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("cat-file: reading %s: %w", fl.Name, err)
		}
		want := fmt.Sprintf("%s blob %d\n", fl.id, fl.size)
		if line != want {
			return fmt.Errorf("cat-file: %s: got header %q, want %q", fl.Name, strings.TrimSpace(line), strings.TrimSpace(want))
		}
		fl.Data = make([]byte, fl.size)
		if _, err := io.ReadFull(io.LimitReader(r, fl.size), fl.Data); err != nil {
			return fmt.Errorf("cat-file: reading %s: %w", fl.Name, err)
		}
		if b, err := r.ReadByte(); err != nil || b != '\n' {
			return fmt.Errorf("cat-file: %s: no newline after the object", fl.Name)
		}
	}
	return nil
}

// checkName rejects names that could leave the folder they are copied into,
// land in a .git, or carry control characters.
func checkName(n string) error {
	if n == "" {
		return errors.New("empty name")
	}
	if strings.HasPrefix(n, "/") {
		return fmt.Errorf("name %q is absolute", n)
	}
	for i := 0; i < len(n); i++ {
		if n[i] < 0x20 || n[i] == 0x7f {
			return fmt.Errorf("name %q has a control character", n)
		}
	}
	for _, e := range strings.Split(n, "/") {
		switch {
		case e == "", e == ".", e == "..":
			return fmt.Errorf("name %q has a %q element", n, e)
		case strings.EqualFold(e, ".git"):
			return fmt.Errorf("name %q has a .git element", n)
		}
	}
	return nil
}

// Tar packs files under root (a clean relative path such as "x" or
// "x/local"), creating directories as needed.
func Tar(root string, files []File) ([]byte, error) {
	if err := checkName(root); err != nil {
		return nil, fmt.Errorf("tar root: %w", err)
	}
	var dirs []string
	seen := map[string]bool{}
	var need func(d string)
	need = func(d string) {
		if seen[d] {
			return
		}
		if p := path.Dir(d); p != "." {
			need(p)
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	need(root)
	for _, f := range files {
		if err := checkName(f.Name); err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		need(path.Dir(root + "/" + f.Name))
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, d := range dirs {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755}); err != nil {
			return nil, err
		}
	}
	for _, f := range files {
		mode := int64(0o644)
		if f.Exec {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: root + "/" + f.Name, Mode: mode, Size: int64(len(f.Data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
