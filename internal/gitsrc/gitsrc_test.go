package gitsrc

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo commits files (names ending in ".sh" are executable) to the repo in dir,
// a new one if dir is "", and returns its file:// URL and the commit.
func repo(t *testing.T, dir string, files map[string]string) (string, string) {
	t.Helper()
	// Not the developer's git config (signing, hooks).
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if dir == "" {
		dir = t.TempDir()
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		gitIn(t, dir, "init", "-q")
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "c")
	return "file://" + dir, gitIn(t, dir, "rev-parse", "HEAD")
}

func fetcher(t *testing.T) Fetcher {
	return Fetcher{Dir: filepath.Join(t.TempDir(), "sub", "cache"), Protocols: []string{"file"}}
}

func names(fs []File) []string {
	var n []string
	for _, f := range fs {
		n = append(n, f.Name)
	}
	return n
}

func mustFiles(t *testing.T, f Fetcher, url, ref, dir string) []File {
	t.Helper()
	fs, err := f.Files(context.Background(), url, ref, dir)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestFilesOfTheFolder(t *testing.T) {
	url, sha := repo(t, "", map[string]string{
		"skills/a/SKILL.md":       "a skill",
		"skills/a/scripts/run.sh": "#!/bin/sh\n",
		"skills/b/SKILL.md":       "b skill",
	})
	f := fetcher(t)
	fs := mustFiles(t, f, url, sha, "skills/a")
	if len(fs) != 2 || fs[0].Name != "SKILL.md" || fs[1].Name != "scripts/run.sh" {
		t.Fatalf("files = %q", names(fs))
	}
	if string(fs[0].Data) != "a skill" || fs[0].Exec {
		t.Errorf("SKILL.md = %q exec %v", fs[0].Data, fs[0].Exec)
	}
	if string(fs[1].Data) != "#!/bin/sh\n" || !fs[1].Exec {
		t.Errorf("run.sh = %q exec %v", fs[1].Data, fs[1].Exec)
	}

	all := mustFiles(t, f, url, sha, ".")
	if got := strings.Join(names(all), ","); got != "skills/a/SKILL.md,skills/a/scripts/run.sh,skills/b/SKILL.md" {
		t.Errorf("files of . = %s", got)
	}
}

func TestFilesAreTheCommittedBytes(t *testing.T) {
	url, sha := repo(t, "", map[string]string{
		".gitattributes":    "skills/a/SKILL.md export-subst\nskills/a/hidden export-ignore\n",
		"skills/a/SKILL.md": "commit $Format:%H$\n",
		"skills/a/hidden":   "still here",
	})
	fs := mustFiles(t, fetcher(t), url, sha, "skills/a")
	if got := strings.Join(names(fs), ","); got != "SKILL.md,hidden" {
		t.Fatalf("files = %s", got)
	}
	if string(fs[0].Data) != "commit $Format:%H$\n" {
		t.Errorf("SKILL.md = %q", fs[0].Data)
	}
}

func TestFilesPinTheCommit(t *testing.T) {
	url, sha1 := repo(t, "", map[string]string{"skills/a/SKILL.md": "one"})
	dir := strings.TrimPrefix(url, "file://")
	_, sha2 := repo(t, dir, map[string]string{"skills/a/SKILL.md": "two"})
	f := fetcher(t)
	for _, c := range []struct{ sha, want string }{{sha1, "one"}, {sha2, "two"}, {sha1, "one"}} {
		fs := mustFiles(t, f, url, c.sha, "skills/a")
		if len(fs) != 1 || string(fs[0].Data) != c.want {
			t.Errorf("at %s: %+v", c.sha, fs)
		}
	}
}

func TestFilesUseTheCache(t *testing.T) {
	url, sha1 := repo(t, "", map[string]string{"skills/a/SKILL.md": "one"})
	dir := strings.TrimPrefix(url, "file://")
	_, sha2 := repo(t, dir, map[string]string{"skills/a/SKILL.md": "two"})
	f := fetcher(t)
	mustFiles(t, f, url, sha1, "skills/a")
	if err := os.Rename(dir, dir+".gone"); err != nil {
		t.Fatal(err)
	}
	fs := mustFiles(t, f, url, sha1, "skills/a")
	if len(fs) != 1 || string(fs[0].Data) != "one" {
		t.Errorf("files = %+v", fs)
	}
	if refs := gitIn(t, dir+".gone", "--git-dir="+f.repoDir(url), "show-ref"); !strings.Contains(refs, "refs/pins/"+sha1) {
		t.Errorf("show-ref = %s", refs)
	}
	if _, err := f.Files(context.Background(), url, sha2, "skills/a"); err == nil {
		t.Error("an unfetched commit of a gone repo worked")
	}
}

func TestFilesRecoverFromAStaleLock(t *testing.T) {
	url, sha1 := repo(t, "", map[string]string{"skills/a/SKILL.md": "one"})
	_, sha2 := repo(t, strings.TrimPrefix(url, "file://"), map[string]string{"skills/a/SKILL.md": "two"})
	f := fetcher(t)
	mustFiles(t, f, url, sha1, "skills/a")
	if err := os.WriteFile(filepath.Join(f.repoDir(url), "shallow.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fs := mustFiles(t, f, url, sha2, "skills/a")
	if len(fs) != 1 || string(fs[0].Data) != "two" {
		t.Errorf("files = %+v", fs)
	}
}

func TestFilesReject(t *testing.T) {
	url, sha := repo(t, "", map[string]string{"skills/a/SKILL.md": "a", "skills/file": "f"})

	symDir := t.TempDir()
	gitIn(t, symDir, "init", "-q")
	if err := os.MkdirAll(filepath.Join(symDir, "skills/a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(symDir, "skills/a/link")); err != nil {
		t.Fatal(err)
	}
	symURL, symSHA := repo(t, symDir, map[string]string{"skills/a/SKILL.md": "a"})

	other, _ := repo(t, "", map[string]string{"x": "x"})
	subDir := t.TempDir()
	gitIn(t, subDir, "init", "-q")
	gitIn(t, subDir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", other, "skills/a/sub")
	subURL, subSHA := repo(t, subDir, map[string]string{"skills/a/SKILL.md": "a"})

	f := fetcher(t)
	mustFiles(t, f, url, sha, "skills/a") // so FETCH_HEAD and the commit are in the cache

	for _, c := range []struct {
		name, url, ref, dir, want string
	}{
		{"symlink", symURL, symSHA, "skills/a", "is a symlink"},
		{"submodule", subURL, subSHA, "skills/a", "is a submodule"},
		{"missing folder", url, sha, "skills/nope", "has no folder"},
		{"dir is a file", url, sha, "skills/file", "not a folder"},
		{"glob", url, sha, "skills/*", "dir"},
		{"unknown commit", url, strings.Repeat("0", 40), "skills/a", "has no commit"},
		{"upper-case commit", url, strings.ToUpper(sha), "skills/a", "ref"},
		{"FETCH_HEAD", url, "FETCH_HEAD", "skills/a", "ref"},
		{"dot-dot dir", url, sha, "../x", "dir"},
		{"option url", "-oProxyCommand=x", sha, "skills/a", "url"},
		{"other scheme", "ssh://example.com/x", sha, "skills/a", "url"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs, err := f.Files(context.Background(), c.url, c.ref, c.dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Files = %q, %v; want an error with %q", names(fs), err, c.want)
			}
		})
	}
}

func TestCheckName(t *testing.T) {
	for _, n := range []string{"", "/x", "a/../b", "./a", ".git/config", "a/.git", "a\nb", "a\x1bb", "a\x7fb", "a//b", "a/", ".GIT/config", "x/.Git"} {
		if checkName(n) == nil {
			t.Errorf("checkName(%q) accepted", n)
		}
	}
	for _, n := range []string{"a/b.c", ".claude-plugin/plugin.json"} {
		if err := checkName(n); err != nil {
			t.Errorf("checkName(%q) = %v", n, err)
		}
	}
}

func TestTarRoots(t *testing.T) {
	files := []File{{Name: "SKILL.md", Data: []byte("s")}, {Name: "scripts/run.sh", Exec: true, Data: []byte("r")}}
	b, err := Tar("x/local", files)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, h.Name+" "+os.FileMode(h.Mode).String())
	}
	want := "x/ -rwxr-xr-x,x/local/ -rwxr-xr-x,x/local/scripts/ -rwxr-xr-x,x/local/SKILL.md -rw-r--r--,x/local/scripts/run.sh -rwxr-xr-x"
	if strings.Join(got, ",") != want {
		t.Errorf("tar = %s\nwant  %s", strings.Join(got, ","), want)
	}

	for _, root := range []string{"../x", "."} {
		if _, err := Tar(root, files); err == nil {
			t.Errorf("Tar(%q) worked", root)
		}
	}
	for _, n := range []string{"../evil", "a/../../evil", "/evil"} {
		if _, err := Tar("x", []File{{Name: n}}); err == nil {
			t.Errorf("Tar of %q worked", n)
		}
	}
}

func TestFilesRefuseOtherProtocols(t *testing.T) {
	url, sha := repo(t, "", map[string]string{"skills/a/SKILL.md": "a"})
	f := Fetcher{Dir: t.TempDir()}
	if _, err := f.Files(context.Background(), url, sha, "skills/a"); err == nil {
		t.Error("an https-only fetcher fetched over file://")
	}
}

func TestFilesIgnoreUserGitConfig(t *testing.T) {
	url, sha := repo(t, "", map[string]string{"skills/a/SKILL.md": "a"})
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[url \"file:///nonexistent/\"]\n\tinsteadOf = file://\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.file:///nonexistent/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "file://")
	t.Setenv("GIT_DIR", "/nonexistent")
	fs := mustFiles(t, fetcher(t), url, sha, "skills/a")
	if len(fs) != 1 || string(fs[0].Data) != "a" {
		t.Errorf("files = %+v", fs)
	}
}

func TestFilesSizeLimit(t *testing.T) {
	mb, mf := maxBytes, maxFiles
	t.Cleanup(func() { maxBytes, maxFiles = mb, mf })
	maxFiles = 1
	url, sha := repo(t, "", map[string]string{"skills/a/one": "1", "skills/a/two": "2"})
	if _, err := fetcher(t).Files(context.Background(), url, sha, "skills/a"); err == nil || !strings.Contains(err.Error(), "is over") {
		t.Errorf("Files = %v; want is over", err)
	}
}
