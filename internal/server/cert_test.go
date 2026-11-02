package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

func certPaths(t *testing.T) (key, cert string) {
	dir := t.TempDir()
	return filepath.Join(dir, "server.key"), filepath.Join(dir, "server.crt")
}

func TestCertificate(t *testing.T) {
	kp, cp := certPaths(t)
	c, err := Certificate(kp, cp)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{kp, cp} {
		if fi, err := os.Stat(f); err != nil {
			t.Error(err)
		} else if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", f, fi.Mode())
		}
	}
	l := c.Leaf
	if pub, ok := l.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("key %T", l.PublicKey)
	}
	if l.IsCA || l.Subject.CommonName != "catchpen" || len(l.ExtKeyUsage) != 1 || l.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("certificate CA %v, CN %q, ext key usage %v", l.IsCA, l.Subject.CommonName, l.ExtKeyUsage)
	}
	if time.Until(l.NotAfter) < 9*365*24*time.Hour || time.Now().Before(l.NotBefore) {
		t.Errorf("valid %s to %s", l.NotBefore, l.NotAfter)
	}

	// Made once: the next call loads the same certificate.
	again, err := Certificate(kp, cp)
	if err != nil || !bytes.Equal(again.Leaf.Raw, l.Raw) {
		t.Errorf("second call: %v, same certificate %v", err, err == nil && bytes.Equal(again.Leaf.Raw, l.Raw))
	}

	// A client pinning the key completes a TLS 1.3 handshake.
	sc, cc := net.Pipe()
	defer sc.Close()
	defer cc.Close()
	srv := tls.Server(sc, &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS13})
	go srv.Handshake()
	pin := api.PublicKeyPin(l)
	cli := tls.Client(cc, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
		VerifyConnection: func(s tls.ConnectionState) error {
			if api.PublicKeyPin(s.PeerCertificates[0]) != pin {
				return errors.New("pin mismatch")
			}
			return nil
		}})
	if err := cli.Handshake(); err != nil || cli.ConnectionState().Version != tls.VersionTLS13 {
		t.Errorf("handshake: %v", err)
	}
}

// Without the certificate, a new one is made for the same key: the pin
// stays. Without the key, nothing is made: clients pin it.
func TestCertificateHalfMissing(t *testing.T) {
	kp, cp := certPaths(t)
	c, err := Certificate(kp, cp)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(kp)
	os.Remove(cp)
	again, err := Certificate(kp, cp)
	if err != nil || api.PublicKeyPin(again.Leaf) != api.PublicKeyPin(c.Leaf) || bytes.Equal(again.Leaf.Raw, c.Leaf.Raw) {
		t.Errorf("without the certificate: %v; pin kept %v", err, err == nil && api.PublicKeyPin(again.Leaf) == api.PublicKeyPin(c.Leaf))
	}
	if after, _ := os.ReadFile(kp); !bytes.Equal(key, after) {
		t.Error("without the certificate, the key changed")
	}

	cert, _ := os.ReadFile(cp)
	os.Remove(kp)
	if _, err := Certificate(kp, cp); err == nil || !strings.Contains(err.Error(), "exists but its key") {
		t.Errorf("without the key: %v", err)
	}
	if after, _ := os.ReadFile(cp); !bytes.Equal(cert, after) {
		t.Error("without the key, the certificate changed")
	}
	if _, err := os.Stat(kp); err == nil {
		t.Error("without the key, a key was made")
	}
}

func TestCertificateKeyMode(t *testing.T) {
	kp, cp := certPaths(t)
	if _, err := Certificate(kp, cp); err != nil {
		t.Fatal(err)
	}
	os.Chmod(kp, 0o640)
	if _, err := Certificate(kp, cp); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Errorf("group-readable key: %v", err)
	}
}
