package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/server"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// runDaemon runs catchpen daemon with args, as a person would.
func runDaemon(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out bytes.Buffer
	return runDaemonTo(t, &out, args...)
}

// runDaemonTo runs catchpen daemon with args, its stdout going to w.
func runDaemonTo(t *testing.T, w io.Writer, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var errOut bytes.Buffer
	c := daemonCmd()
	c.SetArgs(args)
	c.SetOut(w)
	c.SetErr(&errOut)
	c.SilenceUsage, c.SilenceErrors = true, true
	err = c.Execute()
	if b, ok := w.(*bytes.Buffer); ok {
		stdout = b.String()
	}
	return stdout, errOut.String(), err
}

type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestDaemonClientFlow(t *testing.T) {
	t.Setenv("CATCHPEN_HOME", t.TempDir())
	p := paths.Default()

	out, errOut, err := runDaemon(t, "client", "add", "cockpit", "--hmac", "--scope", "config:write")
	key := strings.TrimSuffix(out, "\n")
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) || errOut != "enrolled cockpit (hmac): give it the key above now; catchpen won't show it again\n" {
		t.Fatalf("add --hmac: %q %q %v", out, errOut, err)
	}

	// A PEM file holding a key before the certificate, as some tools write it.
	certFile := filepath.Join(t.TempDir(), "laptop.pem")
	leaf := newCert(t)
	os.WriteFile(certFile, append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not read")}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})...), 0o600)
	out, _, err = runDaemon(t, "client", "add", "laptop", "--cert", certFile)
	if want := "enrolled laptop (mtls), certificate key " + api.PublicKeyPin(leaf) + "\n"; err != nil || out != want {
		t.Errorf("add --cert: %q %v, want %q", out, err, want)
	}

	notPEM := filepath.Join(t.TempDir(), "x.pem")
	os.WriteFile(notPEM, []byte("not a certificate\n"), 0o600)
	for _, c := range []struct {
		args  []string
		wants string
	}{
		{[]string{"client", "add", "x"}, "at least one of the flags"},
		{[]string{"client", "add", "x", "--hmac", "--cert", certFile}, "were all set"},
		{[]string{"client", "add", "x", "--cert", filepath.Join(t.TempDir(), "none.pem")}, "no such file"},
		{[]string{"client", "add", "x", "--cert", notPEM}, "no PEM certificate"},
	} {
		if _, _, err := runDaemon(t, c.args...); err == nil || !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%v: %v, want %q", c.args, err, c.wants)
		}
	}

	// A key that can't be written out isn't kept.
	if _, _, err := runDaemonTo(t, brokenPipe{}, "client", "add", "lost", "--hmac"); err == nil || !strings.Contains(err.Error(), "client lost removed") {
		t.Errorf("unwritable key: %v", err)
	}

	out, _, err = runDaemon(t, "client", "ls")
	if err != nil || !regexp.MustCompile(`(?m)^NAME +METHOD +SCOPES +ADDED\ncockpit +hmac +config:write +\d{4}-\d\d-\d\d \d\d:\d\d\nlaptop +mtls +\d{4}-`).MatchString(out) || strings.Contains(out, key) {
		t.Errorf("ls:\n%s%v", out, err)
	}

	out, _, err = runDaemon(t, "info")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := readCert(p.ServerCert())
	if err != nil {
		t.Fatal(err)
	}
	if want := "config:      " + p.Config + "\nsocket:      " + p.APISocket() + "\ncertificate: " + api.PublicKeyPin(srv) + "\nclients:     2\n"; out != want {
		t.Errorf("info: %q, want %q", out, want)
	}

	if _, _, err := runDaemon(t, "client", "rm", "laptop"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p.Audit())
	if a := string(b); strings.Count(a, `"api.client.add"`) != 3 || strings.Count(a, `"api.client.rm"`) != 2 || strings.Contains(a, key) {
		t.Errorf("audit log:\n%s", a)
	}
}

// newCert is a certificate for a new key.
func newCert(t *testing.T) *x509.Certificate {
	t.Helper()
	dir := t.TempDir()
	c, err := server.Certificate(filepath.Join(dir, "k"), filepath.Join(dir, "c"))
	if err != nil {
		t.Fatal(err)
	}
	return c.Leaf
}
