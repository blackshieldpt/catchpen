// Package gitsrc fetches folders of public git repos at pinned commits, on the
// host, for skills and plugins that catchpen copies into VMs. Each repo URL has
// its own cached bare repo; a commit already fetched needs no network. Files
// are read from git's object store (ls-tree, cat-file), never through archive
// or checkout, so the repo's .gitattributes can't change them.
package gitsrc

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/sshx"
)

type Fetcher struct {
	Dir string // cache, e.g. paths.Sources(); created if missing
	// Protocols git may use; nil means https only. Tests use "file".
	Protocols []string
}

var (
	maxBytes int64 = 64 << 20  // per folder
	maxFiles       = 20000     // regular files per folder
	maxRepo  int64 = 512 << 20 // what one fetch may add on disk; over it, git is stopped and the repo dropped
	// how often a fetch's size on disk is checked
	sizeEvery = time.Second
)

var (
	refRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dirRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)
)

// IsCommit reports whether ref is a full lower-case commit SHA.
func IsCommit(ref string) bool { return refRe.MatchString(ref) }

// CheckPath checks that p is a clean relative path inside a repo or package,
// safe to pass to git or a shell.
func CheckPath(p string) error {
	if !dirRe.MatchString(p) {
		return errors.New("must be letters, digits, . _ - and /, not starting with - or /")
	}
	if path.Clean(p) != p {
		return errors.New("must be a clean path")
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return errors.New(`must not contain ".."`)
	}
	return nil
}

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
	cmd.Stderr = &tail{}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	// catchpen watches a fetch's size: git doesn't outlive it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

func run(cmd *exec.Cmd) error { return gitErr(cmd, cmd.Run()) }

// tail keeps the end of what git writes to stderr, which a server can fill.
type tail struct{ b []byte }

const tailSize = 4096

func (t *tail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > tailSize {
		p = p[len(p)-tailSize:]
	}
	t.b = append(t.b, p...)
	if len(t.b) > tailSize {
		t.b = append(t.b[:0], t.b[len(t.b)-tailSize:]...)
	}
	return n, nil
}

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
	end := cmd.Stderr.(*tail).b
	if len(end) > 500 {
		end = end[len(end)-500:]
	}
	return fmt.Errorf("git %s: %w: %s", sub, err, strings.TrimSpace(sshx.Printable(string(end))))
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
	if !IsCommit(ref) {
		return nil, fmt.Errorf("ref %q is not a full lower-case commit SHA", ref)
	}
	if dir != "." && CheckPath(dir) != nil {
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
	unlock, err := lockCtx(ctx, repo+".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	// A git killed mid-fetch leaves lock files that fail every later fetch,
	// and unfinished packs. Holding the flock, no other catchpen git runs in repo.
	err = filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() && (strings.HasSuffix(d.Name(), ".lock") || strings.HasPrefix(d.Name(), "tmp_")) {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err != nil { // new, or a half-made one
		if err := run(f.git(ctx, "", "init", "--bare", "-q", repo)); err != nil {
			return nil, err
		}
	}

	has := func() bool { return run(f.git(ctx, repo, "cat-file", "-e", ref+"^{commit}")) == nil }
	if !has() {
		ferr := f.fetch(ctx, repo, url, ref)
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

	files, err := f.list(ctx, repo, url, ref, dir)
	if err != nil {
		return nil, err
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

// lockCtx takes the exclusive flock on path, giving up when ctx is done.
func lockCtx(ctx context.Context, path string) (func(), error) {
	for {
		unlock, ok, err := fsutil.TryLock(path)
		if err != nil || ok {
			return unlock, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %s: %w", path, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// fetch fetches ref into repo, stopping git and dropping the repo once the
// fetch adds more than maxRepo on disk: a server can send any amount.
func (f Fetcher) fetch(ctx context.Context, repo, url, ref string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	limit := size(repo) + maxRepo
	cmd := f.git(ctx, repo, "fetch", "--depth=1", "--no-tags", "-q", "--", url, ref+":refs/pins/"+ref)
	if err := cmd.Start(); err != nil {
		return gitErr(cmd, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(sizeEvery)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if size(repo) > limit {
				return dropRepo(url, repo)
			}
			return gitErr(cmd, err)
		case <-tick.C:
			if size(repo) > limit {
				cancel()
				<-done
				return dropRepo(url, repo)
			}
		}
	}
}

func dropRepo(url, repo string) error {
	if err := os.RemoveAll(repo); err != nil {
		return err
	}
	return fmt.Errorf("%q sent over %d MiB; not kept", url, maxRepo>>20)
}

// size is the bytes of the regular files under dir.
func size(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// maxRecord bounds one ls-tree record: a path Linux can open plus the fields.
const maxRecord = 4096 + 256

// list runs ls-tree on dir and parses its output as it comes, stopping git
// once the folder is over the limits: a tree that reuses subtrees lists far
// more files than its objects take on disk.
func (f Fetcher) list(ctx context.Context, repo, url, ref, dir string) ([]listed, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"ls-tree", "-r", "-z", "-l", "--full-tree", ref}
	if dir != "." {
		args = append(args, "--", dir)
	}
	cmd := f.git(ctx, repo, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, gitErr(cmd, err)
	}
	files, err := parseList(bufio.NewReaderSize(stdout, maxRecord), url, ref, dir)
	if err != nil {
		cancel()
		cmd.Wait()
		return nil, err
	}
	return files, gitErr(cmd, cmd.Wait())
}

// parseList reads ls-tree -r -z -l records from r, failing as soon as the
// folder is over maxFiles or maxBytes, before reading the rest.
func parseList(r *bufio.Reader, url, ref, dir string) ([]listed, error) {
	var files []listed
	var total int64
	for {
		b, err := r.ReadSlice(0)
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("%q at %s: ls-tree record over %d bytes", url, ref, maxRecord)
		}
		if err == io.EOF {
			if len(b) != 0 {
				return nil, fmt.Errorf("%q at %s: truncated ls-tree record %q", url, ref, b)
			}
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		rec := string(b[:len(b)-1])
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
			return nil, fmt.Errorf("%q at %s: %q is a symlink, which catchpen doesn't copy", url, ref, p)
		case "160000":
			return nil, fmt.Errorf("%q at %s: %q is a submodule, which catchpen doesn't copy", url, ref, p)
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
		if total > maxBytes || len(files) > maxFiles {
			return nil, fmt.Errorf("%q at %s: %s is over %d MiB or %d files", url, ref, dir, maxBytes>>20, maxFiles)
		}
	}
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

// WriteTar writes files to tw under root (a clean relative path such as "x"
// or "x/local"), creating directories as needed.
func WriteTar(tw *tar.Writer, root string, files []File) error {
	if err := checkName(root); err != nil {
		return fmt.Errorf("tar root: %w", err)
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
			return fmt.Errorf("tar: %w", err)
		}
		need(path.Dir(root + "/" + f.Name))
	}

	for _, d := range dirs {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755}); err != nil {
			return err
		}
	}
	for _, f := range files {
		mode := int64(0o644)
		if f.Exec {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: root + "/" + f.Name, Mode: mode, Size: int64(len(f.Data))}); err != nil {
			return err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return err
		}
	}
	return nil
}
