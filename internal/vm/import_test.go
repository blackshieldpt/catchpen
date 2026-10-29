package vm

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// importFixture makes a guest repo with commit base and, on main, a commit
// built by mk; a bundle of main without base; and a host repo with base.
func importFixture(t *testing.T, mk func(t *testing.T, guest, base string) string) (host, bundle string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	guest, host := t.TempDir(), t.TempDir()
	gitRun(t, guest, "init", "-q", "-b", "base")
	gitRun(t, guest, "commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(t, guest, "rev-parse", "HEAD")
	gitRun(t, guest, "update-ref", "refs/heads/main", mk(t, guest, base))
	bundle = filepath.Join(t.TempDir(), "b.bundle")
	gitRun(t, guest, "bundle", "create", "-q", bundle, "main", "^"+base)
	gitRun(t, host, "init", "-q", "-b", "trunk")
	gitRun(t, host, "fetch", "-q", guest, "base:refs/heads/base")
	return host, bundle
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestImportBundle(t *testing.T) {
	host, bundle := importFixture(t, func(t *testing.T, guest, base string) string {
		gitRun(t, guest, "commit", "-q", "--allow-empty", "-m", "good")
		return gitOut(t, guest, "rev-parse", "HEAD")
	})
	if err := importBundle(context.Background(), host, bundle, "main", "refs/catchpen/vm/main"); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, host, "log", "-1", "--format=%s", "refs/catchpen/vm/main"); got != "good" {
		t.Errorf("imported %q", got)
	}
}

// Malformed objects from the guest are refused whatever the host's git
// version (before 2.48, git didn't fsck objects read from a bundle).
func TestImportBundleRefusesMalformedObjects(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T, guest, base string) string{
		"bad email": func(t *testing.T, guest, base string) string {
			tree := gitOut(t, guest, "rev-parse", base+"^{tree}")
			return gitIn(t, guest, "tree "+tree+"\nparent "+base+"\nauthor a <a@b 1 +0000\ncommitter a <a@b> 1 +0000\n\nbad\n",
				"hash-object", "-t", "commit", "-w", "--stdin", "--literally")
		},
		".git in a tree": func(t *testing.T, guest, base string) string {
			blob := gitIn(t, guest, "x\n", "hash-object", "-w", "--stdin")
			tree := gitIn(t, guest, "100644 blob "+blob+"\t.GIT\n", "mktree")
			return gitIn(t, guest, "", "commit-tree", tree, "-p", base, "-m", "bad")
		},
	} {
		host, bundle := importFixture(t, mk)
		err := importBundle(context.Background(), host, bundle, "main", "refs/catchpen/vm/main")
		if err == nil || !strings.Contains(err.Error(), "fsck") {
			t.Errorf("%s: err = %v, want an fsck error", name, err)
		}
		if exec.Command("git", "-C", host, "rev-parse", "-q", "--verify", "refs/catchpen/vm/main").Run() == nil {
			t.Errorf("%s: the ref was created", name)
		}
	}
}

// Task exports import a whole branch into an empty bare repository.
func TestImportBundleIntoBareRepo(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	guest, host := t.TempDir(), t.TempDir()
	gitRun(t, guest, "init", "-q", "-b", "main")
	gitRun(t, guest, "commit", "-q", "--allow-empty", "-m", "one")
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	gitRun(t, guest, "bundle", "create", "-q", bundle, "main")
	gitRun(t, host, "init", "-q", "--bare")
	if err := importBundle(context.Background(), host, bundle, "main", "refs/catchpen-incoming/vm/main"); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, host, "log", "-1", "--format=%s", "refs/catchpen-incoming/vm/main"); got != "one" {
		t.Errorf("imported %q", got)
	}
}

// A host repo that turned fetch.fsckObjects off still gets the guest's
// objects checked.
func TestImportBundleChecksDespiteHostConfig(t *testing.T) {
	host, bundle := importFixture(t, func(t *testing.T, guest, base string) string {
		tree := gitOut(t, guest, "rev-parse", base+"^{tree}")
		return gitIn(t, guest, "tree "+tree+"\nparent "+base+"\nauthor a <a@b 1 +0000\ncommitter a <a@b> 1 +0000\n\nbad\n",
			"hash-object", "-t", "commit", "-w", "--stdin", "--literally")
	})
	gitRun(t, host, "config", "fetch.fsckObjects", "false")
	if err := importBundle(context.Background(), host, bundle, "main", "refs/catchpen/vm/main"); err == nil || !strings.Contains(err.Error(), "fsck") {
		t.Errorf("err = %v, want an fsck error", err)
	}
}

// A real pull: a bundle of the branch's whole history into a host that has
// most of it, including an old object the host's fsck settings allow. Only
// the new objects are checked, under the host's settings.
func TestImportBundleWholeHistory(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	guest, host := t.TempDir(), t.TempDir()
	gitRun(t, guest, "init", "-q", "-b", "main")
	blob := gitIn(t, guest, "x\n", "hash-object", "-w", "--stdin")
	raw, _ := hex.DecodeString(blob)
	tree := gitIn(t, guest, "0100644 f\x00"+string(raw), "hash-object", "-t", "tree", "-w", "--stdin", "--literally")
	old := gitIn(t, guest, "", "commit-tree", tree, "-m", "old")
	gitRun(t, guest, "update-ref", "refs/heads/main", old)
	gitRun(t, host, "init", "-q", "-b", "trunk")
	gitRun(t, host, "config", "fetch.fsck.zeroPaddedFilemode", "ignore")
	gitRun(t, host, "fetch", "-q", guest, "main:refs/heads/old")
	gitRun(t, guest, "commit", "-q", "--allow-empty", "-m", "new")
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	gitRun(t, guest, "bundle", "create", "-q", bundle, "main")
	if err := importBundle(context.Background(), host, bundle, "main", "refs/catchpen/vm/main"); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, host, "log", "-1", "--format=%s", "refs/catchpen/vm/main"); got != "new" {
		t.Errorf("imported %q", got)
	}
}

// A SHA-256 host repository gets a SHA-256 scratch repository.
func TestImportBundleSHA256(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	guest, host := t.TempDir(), t.TempDir()
	gitRun(t, guest, "init", "-q", "-b", "main", "--object-format=sha256")
	gitRun(t, guest, "commit", "-q", "--allow-empty", "-m", "one")
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	gitRun(t, guest, "bundle", "create", "-q", bundle, "main")
	gitRun(t, host, "init", "-q", "--object-format=sha256")
	if err := importBundle(context.Background(), host, bundle, "main", "refs/catchpen/vm/main"); err != nil {
		t.Fatal(err)
	}
}
