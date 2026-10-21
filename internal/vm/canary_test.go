package vm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if seed, _ := os.ReadFile(m.file("c1", seedFile)); !bytes.Contains(seed, []byte(cs[1].Values[0])) {
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
	gitRun(t, repo, "update-ref", "refs/curral/c1/leak", "HEAD")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "branch", "-qD", "work") // only refs/curral/ has the VM's commits
	if err := m.scanCommits(context.Background(), "c1", repo, "refs/curral/c1/clean"); err != nil {
		t.Errorf("clean commits: %v", err)
	}
	if err := m.scanCommits(context.Background(), "c1", repo, "refs/curral/c1/leak"); !errors.Is(err, ErrCanary) {
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
	gitRun(t, repo, "update-ref", "refs/curral/c1/merge", "HEAD")
	gitRun(t, repo, "checkout", "-qb", "bin", "main")
	os.WriteFile(filepath.Join(repo, "b"), []byte("\x00\x01"+cs[1].Values[0]), 0o600)
	gitRun(t, repo, "add", "b")
	gitRun(t, repo, "commit", "-qm", "binary")
	gitRun(t, repo, "update-ref", "refs/curral/c1/bin", "HEAD")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "branch", "-qD", "side", "top", "bin")
	gitRun(t, repo, "checkout", "-qb", "who", "main")
	os.WriteFile(filepath.Join(repo, "w"), []byte("w\n"), 0o600)
	gitRun(t, repo, "add", "w")
	gitRun(t, repo, "-c", "user.email="+cs[0].Values[1]+"@example.com", "commit", "-qm", "clean")
	gitRun(t, repo, "update-ref", "refs/curral/c1/who", "HEAD")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "branch", "-qD", "who")
	for _, ref := range []string{"refs/curral/c1/merge", "refs/curral/c1/bin", "refs/curral/c1/who"} {
		if err := m.scanCommits(context.Background(), "c1", repo, ref); !errors.Is(err, ErrCanary) {
			t.Errorf("%s: %v", ref, err)
		}
	}
}
