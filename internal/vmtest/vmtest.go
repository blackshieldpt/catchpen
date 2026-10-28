//go:build integration

package vmtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/qemu"
)

// Home makes a catchpen home for one test, with the cached images linked in
// and a catchpen binary built (the supervisors and egress run it).
func Home(t *testing.T) (p paths.Paths, bin string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CATCHPEN_HOME", root)
	p = paths.Default()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	images := os.Getenv("CATCHPEN_TEST_IMAGES")
	if images == "" {
		home, _ := os.UserHomeDir()
		images = filepath.Join(home, ".local/share/catchpen/images")
	}
	if ents, err := os.ReadDir(images); err == nil {
		for _, e := range ents {
			_ = os.Symlink(filepath.Join(images, e.Name()), filepath.Join(p.Images(), e.Name()))
		}
	}
	bin = filepath.Join(root, "catchpen")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/blackshieldpt/catchpen/cmd/catchpen").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return p, bin
}

// Writer logs what's written to it to t.
type Writer struct{ T *testing.T }

func (w Writer) Write(b []byte) (int, error) {
	w.T.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// GiteaPassword is user bot's password.
const GiteaPassword = "catchpen-Test-pw-1"

// Gitea is a throwaway Gitea server.
type Gitea struct {
	t       *testing.T
	IP      string
	HTTP    string // http://<docker bridge ip>:port
	SSHPort int
}

// StartGitea runs a throwaway Gitea in Docker with user "bot" and repos app and lib.
// It runs Gitea with its built-in SSH server, or with OpenSSH (which
// supports host-bound auth, needed for the restricted agent).
func StartGitea(t *testing.T, openssh bool) *Gitea {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	image := os.Getenv("CATCHPEN_TEST_GITEA_IMAGE")
	if image == "" {
		image = "gitea/gitea:1.24"
	}
	// Publish on the Docker bridge address: the VM reaches it through the egress
	// proxy (allow_private), and unlike 127.0.0.1 it isn't the VM's own loopback.
	ipOut, err := exec.Command("docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil {
		t.Fatalf("docker bridge address: %v", err)
	}
	ip := strings.TrimSpace(string(ipOut))
	hp, _ := qemu.FreePort()
	sp, _ := qemu.FreePort()
	g := &Gitea{t: t, IP: ip, HTTP: fmt.Sprintf("http://%s:%d", ip, hp), SSHPort: sp}
	name := fmt.Sprintf("catchpen-test-gitea-%d", hp)
	sshArgs := []string{"-p", fmt.Sprintf("%s:%d:2222", ip, sp), "-e", "GITEA__server__START_SSH_SERVER=true", "-e", "GITEA__server__SSH_LISTEN_PORT=2222"}
	if openssh {
		sshArgs = []string{"-p", fmt.Sprintf("%s:%d:22", ip, sp), "-e", "GITEA__server__START_SSH_SERVER=false"}
	}
	args := append([]string{"run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("%s:%d:3000", ip, hp),
		"-e", "GITEA__security__INSTALL_LOCK=true",
		"-e", "GITEA__server__ROOT_URL=" + g.HTTP + "/",
		"-e", fmt.Sprintf("GITEA__server__SSH_PORT=%d", sp),
		"-e", "GITEA__server__DISABLE_SSH=false"}, sshArgs...)
	out, err := exec.Command("docker", append(args, image)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if resp, err := http.Get(g.HTTP + "/api/v1/version"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("gitea did not start")
		}
		time.Sleep(time.Second)
	}
	if out, err := exec.Command("docker", "exec", "-u", "git", name, "gitea", "admin", "user", "create",
		"--username", "bot", "--password", GiteaPassword, "--email", "bot@example.test", "--admin", "--must-change-password=false").CombinedOutput(); err != nil {
		t.Fatalf("create user: %v\n%s", err, out)
	}
	for _, repo := range []string{"app", "lib"} {
		g.API("POST", "/user/repos", map[string]any{"name": repo, "auto_init": true, "default_branch": "main"}, nil)
	}
	return g
}

// API calls Gitea's API as bot, decoding the answer into out.
func (g *Gitea) API(method, path string, body, out any) {
	g.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, g.HTTP+"/api/v1"+path, rd)
	req.SetBasicAuth("bot", GiteaPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode > 299 {
		g.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			g.t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
}
