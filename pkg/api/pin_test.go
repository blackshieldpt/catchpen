package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"testing"
	"time"
)

func selfSigned(t *testing.T, key *ecdsa.PrivateKey, serial int64) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "x"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
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

func TestPublicKeyPin(t *testing.T) {
	k1, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a, b, other := selfSigned(t, k1, 1), selfSigned(t, k1, 2), selfSigned(t, k2, 1)
	spki, err := x509.MarshalPKIXPublicKey(&k1.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	if got, want := PublicKeyPin(a), "sha256:"+hex.EncodeToString(sum[:]); got != want {
		t.Errorf("pin %s, want %s", got, want)
	}
	if PublicKeyPin(a) != PublicKeyPin(b) {
		t.Error("two certificates for one key pin differently")
	}
	if PublicKeyPin(a) == PublicKeyPin(other) {
		t.Error("two keys pin the same")
	}
	if p := PublicKeyPin(&x509.Certificate{}); p != "" {
		t.Errorf("pin of a certificate with no key: %q", p)
	}
	if !ValidPin(PublicKeyPin(a)) {
		t.Errorf("ValidPin(%s) = false", PublicKeyPin(a))
	}
	for _, bad := range []string{"", "sha256:", "sha256:" + hex.EncodeToString(sum[:31]), "SHA256:" + hex.EncodeToString(sum[:]),
		"sha256:" + hex.EncodeToString(sum[:]) + "\n", "sha1:" + hex.EncodeToString(sum[:])} {
		if ValidPin(bad) {
			t.Errorf("ValidPin(%q) = true", bad)
		}
	}
}
