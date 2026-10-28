package sshagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/catchpen/internal/forge"
)

func keyPair(t *testing.T) (privPEM []byte, pub string) {
	t.Helper()
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	b, err := ssh.MarshalPrivateKey(sk, "test")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ssh.NewPublicKey(pk)
	return pemEncode(b), forge.FormatHostKey(p)
}

func TestStartStop(t *testing.T) {
	for _, b := range []string{"ssh-agent", "ssh-add"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b + " not installed")
		}
	}
	dir, err := os.MkdirTemp("", "ag") // short path: unix socket limit
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	priv, _ := keyPair(t)
	keyFile := filepath.Join(dir, "id")
	os.WriteFile(keyFile, priv, 0o600)
	_, vmKey := keyPair(t)
	_, forgeKey := keyPair(t)
	vmKH := filepath.Join(dir, "known_hosts")
	os.WriteFile(vmKH, []byte("catchpen-vm "+vmKey+"\n"), 0o600)

	a := Agent{Dir: dir}
	s := Spec{VMAlias: "catchpen-vm", VMKnownHosts: vmKH, Keys: []string{keyFile}, Confirm: true,
		Forges: []forge.Forge{{Name: "g", Host: "git.test", HostKeys: []string{forgeKey}}}}
	if err := a.Start(context.Background(), Spec{VMAlias: "x", VMKnownHosts: vmKH, Keys: []string{keyFile}}); err == nil {
		t.Error("agent without forges started")
	}
	if err := a.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	pid, ok := a.Running()
	if !ok {
		t.Fatal("agent not running")
	}
	cmd := exec.Command("ssh-add", "-l")
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+a.Socket())
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Count(string(out), "SHA256:") != 1 {
		t.Fatalf("ssh-add -l: %v\n%s", err, out)
	}
	kh, _ := os.ReadFile(a.knownHosts())
	if !strings.Contains(string(kh), "catchpen-forge-g "+forgeKey) || !strings.Contains(string(kh), "catchpen-vm "+vmKey) {
		t.Errorf("constraint known_hosts:\n%s", kh)
	}
	// A restart replaces the agent.
	if err := a.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if pid2, _ := a.Running(); pid2 == pid {
		t.Error("agent not replaced")
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Running(); ok {
		t.Error("still running after Stop")
	}
	if _, err := os.Stat(a.Socket()); !os.IsNotExist(err) {
		t.Error("socket left behind")
	}
	if err := a.Start(context.Background(), Spec{VMAlias: "catchpen-vm", VMKnownHosts: vmKH, Forges: s.Forges, Keys: []string{filepath.Join(dir, "missing")}}); err == nil {
		t.Error("missing key accepted")
	}
	if _, ok := a.Running(); ok {
		t.Error("agent left running after a failed start")
	}
}

func pemEncode(b *pem.Block) []byte { return pem.EncodeToMemory(b) }
