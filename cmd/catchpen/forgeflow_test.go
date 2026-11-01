package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/paths"
)

// sshHost serves SSH with a new ed25519 host key, and returns its port, the
// key as forges.yaml pins it, and its fingerprint.
func sshHost(t *testing.T) (port int, key, fp string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { ssh.NewServerConn(c, cfg); c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, forge.FormatHostKey(signer.PublicKey()), forge.Fingerprint(signer.PublicKey())
}

// runForge runs `catchpen forge <args>` with stdin, and returns what it
// printed to stdout and stderr.
func runForge(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte(stdin), 0o600); err != nil {
		t.Fatal(err)
	}
	inF, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer inF.Close()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inF, outW, errW
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }() // drained as it runs, so a full pipe can't block it
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()
	func() {
		defer func() { // restored even if the command panics, so the panic is seen
			os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
			outW.Close()
			errW.Close()
		}()
		cmd := forgeCmd()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	}()
	return <-outC, <-errC, err
}

// forge add and forge test, as a person sees them: what they print, what
// they ask, which error wins, and what forges.yaml ends up holding.
func TestForgeAddAndTest(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	home := t.TempDir()
	t.Setenv("CATCHPEN_HOME", home)
	if err := paths.Default().Ensure(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACME_TOK", "tok")
	pa, ka, fpa := sshHost(t)
	pb, _, fpb := sshHost(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/user" && r.Header.Get("Authorization") == "token tok" {
			w.Write([]byte(`{"login":"bot"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	a, b := strconv.Itoa(pa), strconv.Itoa(pb)
	scanned := func(port string, fp string) string {
		return "SSH host keys of 127.0.0.1 port " + port + ":\n  " + fp + "\n"
	}
	const ask = "Do these match the forge's published fingerprints? Trust them? [y/N] "
	cleartext := "warning: forge g1 uses an http:// URL, so its token or password crosses the network unencrypted; use https\n"
	hostBound := "'s SSH server does not support host-bound auth (publickey-hostbound@openssh.com), so a restricted agent can't sign for it; use deploy-key or minted-token mode instead"
	gitea := func(name, port string, more ...string) []string {
		return append([]string{"add", name, "--type", "gitea", "--host", "127.0.0.1", "--ssh-port", port}, more...)
	}
	step := func(stdin string, args []string, wantOut, wantErrOut, wantErr string) {
		t.Helper()
		out, errOut, err := runForge(t, stdin, args...)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if out != wantOut || errOut != wantErrOut || got != wantErr {
			t.Errorf("forge %s:\nstdout %q\n  want %q\nstderr %q\n  want %q\nerror  %q\n  want %q", strings.Join(args, " "), out, wantOut, errOut, wantErrOut, got, wantErr)
		}
	}

	step("n\n", gitea("a1", a, "--mode", "agent"), scanned(a, fpa)+ask, "", "host keys not trusted")
	step("no\n", gitea("w1", a, "--mode", "none"), scanned(a, fpa)+ask+"added forge w1 (https://127.0.0.1); check credentials with: catchpen forge test w1\n",
		"warning: host keys not trusted (only needed for deploy-key and agent modes)\n", "")
	step(" YES \n", gitea("g1", a, "--http-url", srv.URL, "--mode", "deploy-key", "--token-secret", "ACME_TOK"),
		scanned(a, fpa)+ask+"added forge g1 ("+srv.URL+"); check credentials with: catchpen forge test g1\n", cleartext, "")
	step("", gitea("g1", a, "--mode", "none", "-y"), "", "", "forge g1 exists (catchpen forge rm g1 first)")
	step("", gitea("a2", a, "--mode", "agent", "-y"), scanned(a, fpa), "", "a2"+hostBound)
	step("y\n", gitea("bad", b, "--mode", "token"), scanned(b, fpb)+ask, "", "forge bad: auth.mode token needs auth.token")
	step("", gitea("a3", "1", "--mode", "agent", "-y"), "", "", "dial tcp 127.0.0.1:1: connect: connection refused")

	if err := forge.Update(paths.Default().Forges(), func(r *forge.Registry) error {
		g1, _ := r.Get("g1")
		g2 := g1
		g2.Name, g2.SSHPort = "g2", pb // pins a's key, but b answers
		ag := forge.Forge{Name: "ag", Type: forge.Gitea, Host: "127.0.0.1", SSHPort: pa, HostKeys: []string{ka}, Auth: forge.Auth{Mode: forge.ModeAgent}}
		r.Forges = append(r.Forges, g2, ag)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	step("", []string{"test", "g1"}, "API ok: authenticated as bot\nSSH host keys match\n", cleartext, "")
	step("", []string{"test", "w1"}, "", "", "")
	step("", []string{"test", "g2"}, "API ok: authenticated as bot\n", strings.ReplaceAll(cleartext, "g1", "g2"),
		"SSH host key changed: "+fpb+" is not pinned (if expected: catchpen forge rm g2 && catchpen forge add ...)")
	step("", []string{"test", "ag"}, "SSH host keys match\n", "", "ag"+hostBound)
	step("", []string{"test", "nosuch"}, "", "", `no forge "nosuch"`)
	t.Setenv("ACME_TOK", "")
	step("", []string{"test", "g1"}, "", cleartext, "secret ACME_TOK not found on the host")

	reg, err := forge.Load(paths.Default().Forges())
	var names []string
	for _, f := range reg.Forges {
		names = append(names, f.Name)
	}
	g1, _ := reg.Get("g1")
	w1, _ := reg.Get("w1")
	if err != nil || !reflect.DeepEqual(names, []string{"w1", "g1", "g2", "ag"}) || !reflect.DeepEqual(g1.HostKeys, []string{ka}) || w1.HostKeys != nil {
		t.Errorf("forges.yaml: %v %v; g1 keys %q, w1 keys %q", names, err, g1.HostKeys, w1.HostKeys)
	}
}
