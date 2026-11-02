package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// testCert is a certificate for a new key.
func testCert(t *testing.T) *x509.Certificate {
	t.Helper()
	kp, cp := certPaths(t)
	c, err := Certificate(kp, cp)
	if err != nil {
		t.Fatal(err)
	}
	return c.Leaf
}

func TestAddListRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.yaml")
	key, err := AddHMAC(path, "cockpit", []string{ScopeConfigWrite})
	if err != nil || len(key) != KeyBytes {
		t.Fatalf("AddHMAC: %d bytes, %v", len(key), err)
	}
	cert := testCert(t)
	pin, err := AddMTLS(path, "laptop", cert, nil)
	if err != nil || pin != api.PublicKeyPin(cert) {
		t.Fatalf("AddMTLS: %s, %v", pin, err)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("clients file: mode %v", fi.Mode())
	}
	cs, err := LoadClients(path)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := cs.Get("cockpit")
	if !ok || c.Method != MethodHMAC || c.Key != hex.EncodeToString(key) || !slices.Equal(c.Scopes, []string{ScopeConfigWrite}) || time.Since(c.Added) > time.Minute {
		t.Errorf("cockpit: %+v", c)
	}
	if c, ok := cs.Get("laptop"); !ok || c.Method != MethodMTLS || c.Pin != pin || c.Key != "" || c.Scopes != nil {
		t.Errorf("laptop: %+v", c)
	}

	if err := RemoveClient(path, "laptop"); err != nil {
		t.Fatal(err)
	}
	if cs, err := LoadClients(path); err != nil || len(cs.Clients) != 1 {
		t.Errorf("after rm: %+v %v", cs, err)
	}
	if err := RemoveClient(path, "laptop"); err == nil || err.Error() != "no client laptop" {
		t.Errorf("rm twice: %v", err)
	}
}

func TestAddRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.yaml")
	cert := testCert(t)
	if _, err := AddMTLS(path, "laptop", cert, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for _, c := range []struct {
		what  string
		err   error
		wants string
	}{
		{"same name", second(AddHMAC(path, "laptop", nil)), "client laptop exists"},
		{"capital", second(AddHMAC(path, "Laptop", nil)), "invalid client name"},
		{"dot", second(AddHMAC(path, "a.b", nil)), "invalid client name"},
		{"unknown scope", second(AddHMAC(path, "c1", []string{"admin"})), "unknown scope"},
		{"scope twice", second(AddHMAC(path, "c2", []string{ScopeConfigWrite, ScopeConfigWrite})), "listed twice"},
		{"same certificate", second(AddMTLS(path, "desk", cert, nil)), "same certificate key"},
		{"CA certificate", second(AddMTLS(path, "ca", caCert(t), nil)), "CA certificate"},
		{"no key", second(AddMTLS(path, "nokey", &x509.Certificate{}, nil)), "can't be used"},
		{"short RSA key", second(AddMTLS(path, "rsa", &x509.Certificate{PublicKey: &rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), 1023), E: 65537}}, nil)), "1024 bits"},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.wants) {
			t.Errorf("%s: %v, want %q", c.what, c.err, c.wants)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("a refused add changed the file:\n%s", after)
	}
}

func second[T any](_ T, err error) error { return err }

// caCert is a CA certificate for a new key.
func caCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now(),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadClientsRefuses(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("ab", KeyBytes)
	pin := "sha256:" + strings.Repeat("cd", 32)
	for what, body := range map[string]string{
		"unknown field":   "clients:\n  - { name: a, method: hmac, key: " + key + ", admin: true }\n",
		"unknown method":  "clients:\n  - { name: a, method: password }\n",
		"short key":       "clients:\n  - { name: a, method: hmac, key: abcd }\n",
		"hmac with a pin": "clients:\n  - { name: a, method: hmac, key: " + key + ", pin: " + pin + " }\n",
		"mtls with a key": "clients:\n  - { name: a, method: mtls, pin: " + pin + ", key: " + key + " }\n",
		"bad pin":         "clients:\n  - { name: a, method: mtls, pin: sha256:xyz }\n",
		"bad name":        "clients:\n  - { name: a.b, method: hmac, key: " + key + " }\n",
		"same name":       "clients:\n  - { name: a, method: hmac, key: " + key + " }\n  - { name: a, method: mtls, pin: " + pin + " }\n",
		"same pin":        "clients:\n  - { name: a, method: mtls, pin: " + pin + " }\n  - { name: b, method: mtls, pin: " + pin + " }\n",
	} {
		path := filepath.Join(dir, strings.ReplaceAll(what, " ", "-")+".yaml")
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := LoadClients(path); err == nil {
			t.Errorf("%s: loaded", what)
		}
	}

	ok := filepath.Join(dir, "ok.yaml")
	os.WriteFile(ok, []byte("clients:\n  - { name: a, method: hmac, key: "+key+" }\n"), 0o600)
	os.Chmod(ok, 0o640) // whatever the umask
	if _, err := LoadClients(ok); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Errorf("group-readable: %v", err)
	}
	os.Chmod(ok, 0o600)
	if cs, err := LoadClients(ok); err != nil || len(cs.Clients) != 1 {
		t.Errorf("0600: %+v %v", cs, err)
	}
	if cs, err := LoadClients(filepath.Join(dir, "missing.yaml")); err != nil || len(cs.Clients) != 0 {
		t.Errorf("missing file: %+v %v", cs, err)
	}
}

// Two enrolments of one name at once: one wins.
func TestAddConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.yaml")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- second(AddHMAC(path, "cockpit", nil)) })
	}
	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		if err == nil {
			n++
		}
	}
	if cs, err := LoadClients(path); n != 1 || err != nil || len(cs.Clients) != 1 {
		t.Errorf("%d adds succeeded; file %+v %v", n, cs, err)
	}
}
