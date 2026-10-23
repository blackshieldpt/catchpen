package vm

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/curral/internal/canary"
	"github.com/blackshieldpt/curral/internal/config"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestCanariesPlantedAndScanned(t *testing.T) {
	// Not the developer's git config (signing, hooks) until the test sets one.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	m := poolManager(t)
	writeFile(t, filepath.Join(m.P.Templates(), "ag.yaml"), "extends: minimal\nruntimes: { node: \"24\" }\nagents:\n  claude: {}\nidle_stop: 2h\ncanaries: [git, env]\n")
	s, err := m.Create(context.Background(), "c1", CreateOpts{Template: "ag"})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := canary.Load(m.dir("c1"))
	if err != nil || len(cs) != 2 || !slices.Equal(s.Canaries, []string{"env", "git"}) {
		t.Fatalf("canaries %+v %v, state %v", cs, err, s.Canaries)
	}
	if seed, _ := os.ReadFile(m.vm("c1").path(seedFile)); !bytes.Contains(seed, []byte(cs[1].Values[0])) {
		t.Error("seed lacks the git canary")
	}

	m.Cfg.CanaryAction = config.CanaryLog // nothing running to pause
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a"), []byte("a\n"), 0o600)
	gitRun(t, repo, "add", "a")
	gitRun(t, repo, "commit", "-qm", "base")
	gitRun(t, repo, "checkout", "-qb", "work")
	os.WriteFile(filepath.Join(repo, "a"), []byte("a\nfixed\n"), 0o600)
	gitRun(t, repo, "commit", "-qam", "clean change")
	gitRun(t, repo, "update-ref", "refs/curral/c1/clean", "HEAD")
	os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN="+cs[0].Values[2]+"\n"), 0o600)
	gitRun(t, repo, "add", ".env")
	gitRun(t, repo, "commit", "-qm", "add config")
	gitRun(t, repo, "update-ref", "refs/curral-incoming/c1/leak", "HEAD")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "branch", "-qD", "work") // only refs/curral/ has the VM's commits
	if err := m.scanCommits(context.Background(), "c1", repo, "refs/curral/c1/clean"); err != nil {
		t.Errorf("clean commits: %v", err)
	}
	if err := m.scanCommits(context.Background(), "c1", repo, "refs/curral-incoming/c1/leak"); !errors.Is(err, ErrCanary) {
		t.Errorf("leaking commit: %v", err)
	}
	if b, _ := os.ReadFile(m.P.Audit()); !strings.Contains(string(b), `"canary.hit"`) || !strings.Contains(string(b), `"kind":"env"`) {
		t.Errorf("audit: %s", b)
	}
	if err := m.scanExport("c1", strings.NewReader("## Result\n\n"+cs[1].Hosts[0]), "in the pull request"); !errors.Is(err, ErrCanary) {
		t.Errorf("PR body: %v", err)
	}

	// A value added while resolving a merge, and one in a binary file.
	gitRun(t, repo, "checkout", "-qb", "side")
	os.WriteFile(filepath.Join(repo, "s"), []byte("side\n"), 0o600)
	gitRun(t, repo, "add", "s")
	gitRun(t, repo, "commit", "-qm", "side")
	gitRun(t, repo, "checkout", "-qb", "top", "main")
	os.WriteFile(filepath.Join(repo, "t"), []byte("top\n"), 0o600)
	gitRun(t, repo, "add", "t")
	gitRun(t, repo, "commit", "-qm", "top")
	gitRun(t, repo, "merge", "-q", "--no-ff", "--no-commit", "side")
	os.WriteFile(filepath.Join(repo, "m"), []byte(cs[1].Values[0]+"\n"), 0o600)
	gitRun(t, repo, "add", "m")
	gitRun(t, repo, "commit", "-qm", "merge")
	gitRun(t, repo, "update-ref", "refs/curral-incoming/c1/merge", "HEAD")
	gitRun(t, repo, "checkout", "-qb", "bin", "main")
	os.WriteFile(filepath.Join(repo, "b"), []byte("\x00\x01"+cs[1].Values[0]), 0o600)
	gitRun(t, repo, "add", "b")
	gitRun(t, repo, "commit", "-qm", "binary")
	gitRun(t, repo, "update-ref", "refs/curral-incoming/c1/bin", "HEAD")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "branch", "-qD", "side", "top", "bin")
	gitRun(t, repo, "checkout", "-qb", "who", "main")
	os.WriteFile(filepath.Join(repo, "w"), []byte("w\n"), 0o600)
	gitRun(t, repo, "add", "w")
	gitRun(t, repo, "-c", "user.email="+cs[0].Values[1]+"@example.com", "commit", "-qm", "clean")
	gitRun(t, repo, "update-ref", "refs/curral-incoming/c1/who", "HEAD")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "branch", "-qD", "who")

	// A message git would re-encode (an even-length UTF-16 commit), and a
	// root commit under a user config that hides root diffs.
	gitRun(t, repo, "checkout", "-qb", "enc", "main")
	for msg := "token " + cs[0].Values[1]; ; msg += "x" {
		gitRun(t, repo, "-c", "i18n.commitEncoding=UTF-16LE", "commit", "-q", "--allow-empty", "-m", msg)
		raw, _ := exec.Command("git", "-C", repo, "cat-file", "commit", "HEAD").Output()
		if len(raw)%2 == 0 {
			break
		}
		gitRun(t, repo, "reset", "-q", "--hard", "HEAD~1")
	}
	gitRun(t, repo, "update-ref", "refs/curral-incoming/c1/enc", "HEAD")
	gitRun(t, repo, "checkout", "-q", "--orphan", "root")
	os.WriteFile(filepath.Join(repo, "r"), []byte(cs[1].Values[0]+"\n"), 0o600)
	gitRun(t, repo, "add", "r")
	gitRun(t, repo, "commit", "-qm", "root")
	gitRun(t, repo, "update-ref", "refs/curral-incoming/c1/root", "HEAD")
	gitRun(t, repo, "checkout", "-qf", "main")
	gitRun(t, repo, "branch", "-qD", "enc", "root")
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(gcfg, []byte("[log]\n\tshowRoot = false\n[color]\n\tui = always\n"), 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)

	// refs/curral/ holds earlier pulls, excluded from what's new; what's
	// being pulled is under refs/curral-incoming/.
	gitRun(t, repo, "update-ref", "refs/curral/c1/work", "refs/curral/c1/clean")
	for _, ref := range []string{"refs/curral-incoming/c1/merge", "refs/curral-incoming/c1/bin", "refs/curral-incoming/c1/who",
		"refs/curral-incoming/c1/enc", "refs/curral-incoming/c1/root"} {
		if err := m.scanCommits(context.Background(), "c1", repo, ref); !errors.Is(err, ErrCanary) {
			t.Errorf("%s: %v", ref, err)
		}
	}

	// A binary file in a patch; a hit removes the file.
	patch := filepath.Join(t.TempDir(), "p.mbox")
	out, err := exec.Command("git", "-C", repo, "format-patch", "--stdout", "main..refs/curral-incoming/c1/bin").Output()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(patch, out, 0o600)
	if err := m.scanPatch("c1", patch, "in patches"); !errors.Is(err, ErrCanary) || fileExists(patch) {
		t.Errorf("binary patch: %v, kept %v", err, fileExists(patch))
	}

	// A binary delta that builds a value out of bytes copied from the old
	// file: the patch alone hides it, the commits don't, and patches are
	// made on the host from them.
	v := cs[1].Values[0]
	fill := make([]byte, 3<<10)
	rand.Read(fill)
	gitRun(t, repo, "checkout", "-qb", "delta", "main")
	os.WriteFile(filepath.Join(repo, "d"), slices.Concat(fill[:1<<10], []byte(v[:len(v)/2]), fill[1<<10:2<<10], []byte(v[len(v)/2:]), fill[2<<10:]), 0o600)
	gitRun(t, repo, "add", "d")
	gitRun(t, repo, "commit", "-qm", "halves")
	gitRun(t, repo, "update-ref", "refs/curral/c1/halves", "HEAD")
	os.WriteFile(filepath.Join(repo, "d"), slices.Concat(fill[:1<<10], []byte(v), fill[1<<10:]), 0o600)
	gitRun(t, repo, "commit", "-qam", "joined")
	gitRun(t, repo, "update-ref", "refs/curral-incoming/c1/delta", "HEAD")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "branch", "-qD", "delta")
	out, err = exec.Command("git", "-C", repo, "format-patch", "--stdout", "refs/curral/c1/halves..refs/curral-incoming/c1/delta").Output()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := canary.ScanPatch(bytes.NewReader(out), cs); c != nil || err != nil || !bytes.Contains(out, []byte("\ndelta ")) {
		t.Errorf("not a hidden delta: %v %v", c, err)
	}
	if err := m.scanCommits(context.Background(), "c1", repo, "refs/curral-incoming/c1/delta"); !errors.Is(err, ErrCanary) {
		t.Errorf("delta commit: %v", err)
	}
	// FormatPatch scans the commits it exports itself, whatever else the
	// host repository has.
	halves, _ := exec.Command("git", "-C", repo, "rev-parse", "refs/curral/c1/halves").Output()
	if err := m.FormatPatch(context.Background(), "c1", repo, "refs/curral-incoming/c1/delta", strings.Fields(string(halves)), patch, "in patches"); !errors.Is(err, ErrCanary) || fileExists(patch) {
		t.Errorf("delta patch: %v, written %v", err, fileExists(patch))
	}
	mainID, _ := exec.Command("git", "-C", repo, "rev-parse", "main").Output()
	if err := m.FormatPatch(context.Background(), "c1", repo, "refs/curral/c1/clean", strings.Fields(string(mainID)), patch, "in patches"); err != nil {
		t.Errorf("clean patch: %v", err)
	} else if b, _ := os.ReadFile(patch); !bytes.Contains(b, []byte("Subject: [PATCH] clean change")) {
		t.Errorf("clean patch:\n%s", b)
	}
	if err := m.FormatPatch(context.Background(), "c1", repo, "refs/curral/c1/clean", []string{"--output=x"}, patch, "in patches"); err == nil {
		t.Error("took an option as the base")
	}
	if err := m.FormatPatch(context.Background(), "c1", repo, "refs/curral/c1/clean", nil, patch, "in patches"); err != nil {
		t.Errorf("patch with no base: %v", err)
	} else if b, _ := os.ReadFile(patch); !bytes.Contains(b, []byte("Subject: [PATCH 1/2] base")) {
		t.Errorf("patch with no base:\n%s", b)
	}

	// A message git re-encodes into a value (an ISO-2022 escape that
	// decodes to nothing splits it).
	tree, _ := exec.Command("git", "-C", repo, "rev-parse", "main^{tree}").Output()
	raw := "tree " + strings.TrimSpace(string(tree)) + "\nparent " + strings.TrimSpace(string(mainID)) +
		"\nauthor a <a@b.test> 0 +0000\ncommitter a <a@b.test> 0 +0000\nencoding ISO-2022-JP\n\n" +
		"token " + v[:4] + "\x1b(B" + v[4:] + "\n"
	cmd := exec.Command("git", "-C", repo, "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
	cmd.Stdin = strings.NewReader(raw)
	id, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "update-ref", "refs/curral-incoming/c1/iso2022", strings.TrimSpace(string(id)))
	if err := m.scanCommits(context.Background(), "c1", repo, "refs/curral-incoming/c1/iso2022"); !errors.Is(err, ErrCanary) {
		t.Errorf("re-encoded message: %v", err)
	}

	// Ten hits in a minute: three audit records.
	b, _ := os.ReadFile(m.P.Audit())
	if n := strings.Count(string(b), `"canary.hit"`); n != 3 {
		t.Errorf("%d canary.hit audit records, want 3", n)
	}
	if !m.vm("c1").has(hitFile) {
		t.Error("no canary-hit marker")
	}
}

// A paused VM: ssh refused with a hint, listed as paused, left alone by idle
// stop; resume refuses VMs a canary hit didn't pause.
func TestPausedVM(t *testing.T) {
	m := testManager(t)
	m.Cfg.IdleStop = "1m"
	if err := m.Resume(context.Background(), "p1"); err == nil {
		t.Error("resumed a VM that isn't running")
	}
	for _, n := range []string{"p1", "p2"} {
		fakeQEMU(t, m, n, 512)
		writeFile(t, m.vm(n).path(runtimeFile), `{"ssh_port":2222}`)
	}
	if err := m.Resume(context.Background(), "p1"); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Errorf("resume of a running VM: %v", err)
	}
	writeFile(t, m.vm("p1").path(pausedFile), `{"kind":"git"}`)
	if _, err := m.SSHPort("p1"); err == nil || !strings.Contains(err.Error(), "curral resume p1") {
		t.Errorf("ssh to a paused VM: %v", err)
	}
	if p, err := m.SSHPort("p2"); err != nil || p != 2222 {
		t.Errorf("ssh port %d %v", p, err)
	}
	vms, err := m.List()
	if err != nil || len(vms) != 2 || !vms[0].Paused || vms[1].Paused {
		t.Fatalf("list %+v %v", vms, err)
	}
	var it IdleTracker
	if _, err := it.Check(m, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := it.vms["p1"]; ok || it.vms["p2"] == nil {
		t.Errorf("idle tracker watches %v, want only p2", it.vms)
	}
}

func TestGuestText(t *testing.T) {
	m := testManager(t)
	m.Cfg.CanaryAction = config.CanaryLog
	if err := os.MkdirAll(m.dir("g1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := m.GuestText("g1", "all good", "x"); got != "all good" {
		t.Errorf("no canaries: %q", got)
	}
	cs := canary.Generate([]string{"git"})
	if err := canary.Save(m.dir("g1"), cs); err != nil {
		t.Fatal(err)
	}
	v := cs[0].Values[0]
	for _, s := range []string{"token " + v, "token " + v[:8] + "\x01" + v[8:], "token " + v[:8] + "​" + v[8:]} {
		if got := m.GuestText("g1", s, "in the task's result"); !strings.Contains(got, "decoy credentials") {
			t.Errorf("%q: %q", s, got)
		}
	}
	if got := m.GuestText("g1", "fine", "x"); got != "fine" {
		t.Errorf("clean text: %q", got)
	}
	writeFile(t, m.vm("g1").path(canary.File), "{")
	if got := m.GuestText("g1", "fine", "x"); !strings.Contains(got, "couldn't be read") {
		t.Errorf("unreadable canaries: %q", got)
	}
}

// canary_action stop stops the VM; a pause that can't reach QMP stops it
// rather than leave it running.
func TestCanaryHitStops(t *testing.T) {
	for _, action := range []string{config.CanaryStop, config.CanaryPause} {
		m := testManager(t)
		m.Cfg.CanaryAction = action
		fakeQEMU(t, m, "h1", 512) // with no QMP socket
		m.CanaryHit("h1", &canary.Canary{Kind: "git"}, "in a test")
		if _, ok := m.Running("h1"); ok || m.Paused("h1") || !m.vm("h1").has(hitFile) {
			t.Errorf("%s: running %v, paused %v, hit recorded %v", action, ok, m.Paused("h1"), m.vm("h1").has(hitFile))
		}
		if b, _ := os.ReadFile(m.P.Audit()); !strings.Contains(string(b), `"action":"`+action+`"`) || !strings.Contains(string(b), `"vm.stop"`) {
			t.Errorf("%s: audit log:\n%s", action, b)
		}
	}
}

// Incoming refs of dead pulls and older versions go; a live pull's stays.
func TestPruneIncoming(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "x")
	live := fmt.Sprintf("refs/curral-incoming/%d-1/vm/main", os.Getpid())
	for _, ref := range []string{live, "refs/curral-incoming/999999999-1/vm/main", "refs/curral-incoming/vm/main"} {
		gitRun(t, repo, "update-ref", ref, "HEAD")
	}
	pruneIncoming(context.Background(), repo)
	out, _ := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/curral-incoming/").Output()
	if strings.TrimSpace(string(out)) != live {
		t.Errorf("left: %q", out)
	}
}
