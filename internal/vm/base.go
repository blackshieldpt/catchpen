package vm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/blackshieldpt/catchpen/internal/cache"
	"github.com/blackshieldpt/catchpen/internal/cloudinit"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/image"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/qemu"
	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/internal/template"
)

type Resolved struct {
	T      *template.Template
	Distro image.Spec
	User   string
	Hash   string
}

var unsafeNameRe = regexp.MustCompile(`[^a-z0-9._-]`)

// stem names a base after its distro; with the hash it identifies the
// content, so -t lists that give the same software share one base. It is
// also the build VM's QEMU name, which can't hold commas.
func (r *Resolved) stem() string { return unsafeNameRe.ReplaceAllString(r.Distro.Name, "_") }

// specsPath lists the -t specs that used a base, for rebuild --all and gc.
func (r *Resolved) specsPath(p paths.Paths) string {
	return filepath.Join(p.Bases(), r.stem()+"-"+r.Hash+".specs")
}

func readSpecs(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return strings.Fields(string(b)), err
}

// recordSpec adds the template's spec to its base's list. Callers hold the
// build lock.
func (r *Resolved) recordSpec(p paths.Paths) error {
	specs, err := readSpecs(r.specsPath(p))
	if err != nil {
		return err
	}
	if slices.Contains(specs, r.T.Name) {
		return nil
	}
	return fsutil.WriteFileAtomic(r.specsPath(p), []byte(strings.Join(append(specs, r.T.Name), "\n")+"\n"), 0o600)
}

// Resolve builds the template for a -t spec and the base it needs.
func (m *Manager) Resolve(spec string) (*Resolved, error) {
	t, err := m.Tpl.Load(spec)
	if err != nil {
		return nil, err
	}
	if t.Distro == "" {
		return nil, fmt.Errorf("-t %s: no distro (distros.yaml has no default)", t.Name)
	}
	dspec, err := m.Cfg.Distro(t.Distro)
	if err != nil {
		return nil, err
	}
	user := m.Cfg.Defaults.User
	// The caches a build uses aren't baked in: they don't make another base.
	ud, err := cloudinit.BuildUserData(t, user, nil)
	if err != nil {
		return nil, err
	}
	return &Resolved{T: t, Distro: dspec, User: user, Hash: baseHash(dspec, user, ud)}, nil
}

// baseHash identifies a base: the source image (URL and pin) plus the rendered
// build seed, which carries everything from the template that is baked in.
func baseHash(spec image.Spec, user string, buildUserData []byte) string {
	h := sha256.New()
	for _, f := range []string{spec.URL, spec.ChecksumURL, spec.SHA256, spec.SHA512, user, string(buildUserData)} {
		fmt.Fprintf(h, "%d:%s\x00", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// basePath is generation gen of the template base: <distro>-<hash>.qcow2 for
// the first, <distro>-<hash>.<gen>.qcow2 for rebuilds. A rebuild never replaces a
// file VMs may be overlays on.
func (r *Resolved) basePath(p paths.Paths, gen int) string {
	if gen <= 1 {
		return filepath.Join(p.Bases(), fmt.Sprintf("%s-%s.qcow2", r.stem(), r.Hash))
	}
	return filepath.Join(p.Bases(), fmt.Sprintf("%s-%s.%d.qcow2", r.stem(), r.Hash, gen))
}

// baseGen returns the newest built generation (0 if none).
func (r *Resolved) baseGen(p paths.Paths) int {
	gen := 0
	if _, err := os.Stat(r.basePath(p, 1)); err == nil {
		gen = 1
	}
	matches, _ := filepath.Glob(filepath.Join(p.Bases(), fmt.Sprintf("%s-%s.*.qcow2", r.stem(), r.Hash)))
	for _, mt := range matches {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(mt), r.stem()+"-"+r.Hash+"."), ".qcow2"))
		if err == nil && n > gen {
			gen = n
		}
	}
	return gen
}

// BasePath is the template's current base (it may not be built yet).
func (r *Resolved) BasePath(p paths.Paths) string { return r.basePath(p, max(r.baseGen(p), 1)) }

// EnsureBase builds the template base image if it does not exist yet.
func (m *Manager) EnsureBase(ctx context.Context, r *Resolved, rebuild bool) (string, error) {
	if err := m.P.Ensure(); err != nil {
		return "", err
	}
	// Builds of one template version serialize on their own lock, apart from
	// the per-base locks VMs and gc take.
	unlock, err := fsutil.Lock(strings.TrimSuffix(r.basePath(m.P, 1), ".qcow2") + ".build.lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := r.recordSpec(m.P); err != nil {
		return "", err
	}
	gen := r.baseGen(m.P)
	if gen > 0 && !rebuild {
		return r.basePath(m.P, gen), nil
	}
	// A rebuild is a new generation: VM disks are overlays on the old file.
	dst := r.basePath(m.P, gen+1)
	ensure := image.Ensure
	if rebuild {
		ensure = image.EnsureLatest
	}
	img, digest, err := ensure(ctx, m.P.Images(), r.Distro, m.Out)
	if err != nil {
		return "", err
	}
	m.logf("building base %s (%s, %s)", filepath.Base(dst), r.Distro.Name, digest)
	stem := strings.TrimSuffix(dst, ".qcow2")
	work := dst + ".building"
	seed := dst + ".seed.iso"
	logf := stem + ".build.log"
	consolef := stem + ".console.log"
	defer os.Remove(work)
	defer os.Remove(seed)
	if err := qemu.Convert(img, work, buildDiskMB); err != nil {
		return "", err
	}
	caches := buildCaches(r.T)
	ud, err := cloudinit.BuildUserData(r.T, r.User, caches)
	if err != nil {
		return "", err
	}
	if err := cloudinit.WriteSeed(seed, map[string][]byte{
		"user-data": ud,
		"meta-data": cloudinit.MetaData("catchpen-build-"+r.Hash, "catchpen-build"),
	}); err != nil {
		return "", err
	}
	_ = os.Remove(logf)
	_ = os.Remove(consolef)
	// The build VM gets the same isolation as runtime VMs, but its proxy allows
	// any public destination (package mirrors, installers). Private ranges stay blocked.
	edir := stem + ".egress"
	_ = os.RemoveAll(edir)
	if err := os.Mkdir(edir, 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(edir)
	if err := egress.WriteConfig(edir, egress.Config{AllowAll: true}); err != nil {
		return "", err
	}
	spec := qemu.Spec{
		Name: "build-" + r.stem(), CPUs: min(runtime.NumCPU(), 4), MemoryMB: 4096,
		Disk: work, Seed: seed, Egress: egress.ModeAllowlist, SerialFile: consolef, LogFile: logf,
		ProxyCmd: []string{m.Bin, "egress-stdio", "--dir", edir, "--vm", "build-" + r.stem()},
		ProxyIP:  cloudinit.ProxyAddr, ProxyPort: cloudinit.ProxyPort,
	}
	if len(caches) > 0 {
		spec.CacheCmd, spec.CacheIP, spec.CachePort = m.cacheCmd(edir, "build-"+r.stem(), caches), cache.Addr, cache.Port
	}
	err = m.runForeground(ctx, spec, logf, consolef, buildTimeout)
	_ = os.Rename(filepath.Join(edir, egress.LogFile), stem+".egress.log")
	if err != nil {
		return "", fmt.Errorf("base build failed: %w (logs: %s, %s)", err, logf, consolef)
	}
	tail, err := readTail(logf, 4096)
	if err != nil {
		return "", err
	}
	switch lastLine(tail) {
	case cloudinit.BuildOK:
	case cloudinit.BuildFail:
		return "", fmt.Errorf("base build script failed; see %s", logf)
	default:
		return "", fmt.Errorf("base build ended without a result marker; see %s", logf)
	}
	if err := os.Rename(work, dst); err != nil {
		return "", err
	}
	if err := os.Chmod(dst, 0o400); err != nil {
		return "", err
	}
	m.logf("base ready: %s", dst)
	// The generation this one supersedes goes if nothing uses it, so repeated
	// rebuilds don't pile up bases.
	if gen > 0 {
		if prev := r.basePath(m.P, gen); m.removeBase(prev, false) {
			m.logf("removed the unused previous base %s", filepath.Base(prev))
		}
	}
	return dst, nil
}

func (m *Manager) baseUsers(base string) ([]string, error) {
	ents, err := os.ReadDir(m.P.VMs())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var users []string
	for _, e := range ents {
		if !e.IsDir() || ValidName(e.Name()) != nil {
			continue
		}
		s, err := m.Load(e.Name())
		if err != nil {
			users = append(users, e.Name()+" (unreadable state)")
			continue
		}
		// The disk is checked too: a reset or rebase interrupted between
		// changing the disk and saving the state leaves them disagreeing.
		backing, err := qemu.BackingFile(m.vm(s.Name).path(diskFile))
		if s.Base == base || backing == base || (err != nil && m.vm(s.Name).has(diskFile)) {
			users = append(users, s.Name)
		}
	}
	return users, nil
}

// runForeground runs QEMU until it exits, echoing build progress lines.
func (m *Manager) runForeground(ctx context.Context, spec qemu.Spec, logf, consolef string, timeout time.Duration) error {
	bin, err := qemu.Binary()
	if err != nil {
		return err
	}
	args, err := spec.Args()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done, followed := make(chan struct{}), make(chan struct{})
	go func() { m.follow(logf, done); close(followed) }()
	go capLogs(done, abort, logf, consolef)
	err = cmd.Wait()
	close(done)
	<-followed
	if c := context.Cause(ctx); errors.Is(c, errLogTooLarge) {
		return c
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

var errLogTooLarge = fmt.Errorf("build log exceeded %d MiB", maxBuildLog>>20)

// capLogs aborts the build when a serial log QEMU writes grows past maxBuildLog.
func capLogs(done <-chan struct{}, abort context.CancelCauseFunc, files ...string) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		for _, f := range files {
			if st, err := os.Stat(f); err == nil && st.Size() > maxBuildLog {
				abort(errLogTooLarge)
				return
			}
		}
	}
}

// follow tails a serial log, printing catchpen-build lines (or everything in debug mode).
// On a terminal, the current step shows with a spinner and its elapsed time, and each
// step ends as a line with the time it took.
func (m *Manager) follow(path string, done <-chan struct{}) {
	o, ok := m.Out.(*os.File)
	tty := ok && !m.Debug && term.IsTerminal(int(o.Fd()))
	step, since, n := "starting the build VM", time.Now(), 0
	spin := func() {
		if tty {
			show := step // a line that wraps can't be redrawn in place
			if r := []rune(show); len(r) > 60 {
				show = string(r[:60]) + "..."
			}
			fmt.Fprintf(m.Out, "\r\033[K  %c %s (%s)", `-\|/`[n%4], show, time.Since(since).Round(time.Second))
			n++
		}
	}
	endStep := func() {
		if tty {
			fmt.Fprintf(m.Out, "\r\033[K  %s (%s)\n", step, time.Since(since).Round(time.Second))
		}
	}
	defer endStep()
	var f *os.File
	for f == nil {
		var err error
		if f, err = os.Open(path); err != nil {
			spin()
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var partial string
	for {
		line, err := r.ReadString('\n')
		partial += line
		if err == nil {
			l := strings.TrimRight(partial, "\r\n")
			partial = ""
			if m.Debug || strings.Contains(l, "catchpen-build:") {
				if i := strings.Index(l, "catchpen-build:"); i >= 0 && !m.Debug {
					l = "  " + strings.TrimSpace(l[i+len("catchpen-build:"):])
				}
				if tty {
					endStep()
					step, since = strings.TrimSpace(sshx.Printable(l)), time.Now()
					spin()
					continue
				}
				m.logf("%s", sshx.Printable(l))
			}
			continue
		}
		spin()
		select {
		case <-done:
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}
