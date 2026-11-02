package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"time"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
)

// certLife is how long a generated certificate is valid. Clients pin its
// public key, so its expiry matters only to clients that also check dates.
const certLife = 10 * 365 * 24 * time.Hour

// Certificate returns the server's TLS certificate, made on first use: an
// ECDSA P-256 key (keyPath, mode 0600) and a self-signed certificate for it
// (certPath). Clients pin its key (api.PublicKeyPin); there is no CA. With
// the key but no certificate, it makes a new certificate for that key, which
// keeps the pin; with a certificate but no key, it refuses.
func Certificate(keyPath, certPath string) (tls.Certificate, error) {
	unlock, err := fsutil.Lock(keyPath + ".lock")
	if err != nil {
		return tls.Certificate{}, err
	}
	defer unlock()
	_, kerr := os.Lstat(keyPath)
	_, cerr := os.Lstat(certPath)
	var key *ecdsa.PrivateKey
	switch {
	case kerr == nil && cerr == nil:
		return loadCertificate(keyPath, certPath)
	case kerr == nil && errors.Is(cerr, fs.ErrNotExist):
		if key, err = loadKey(keyPath); err != nil {
			return tls.Certificate{}, err
		}
	case errors.Is(kerr, fs.ErrNotExist) && errors.Is(cerr, fs.ErrNotExist):
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return tls.Certificate{}, err
		}
		kder, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return tls.Certificate{}, err
		}
		if err := fsutil.WriteFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
			return tls.Certificate{}, err
		}
	case errors.Is(kerr, fs.ErrNotExist):
		return tls.Certificate{}, fmt.Errorf("%s exists but its key %s doesn't: restore the key, or remove the certificate to make a new key (clients must then pin it again)", certPath, keyPath)
	default:
		return tls.Certificate{}, errors.Join(kerr, cerr)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "catchpen"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := fsutil.WriteFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return loadCertificate(keyPath, certPath)
}

// private refuses a key file other users can read.
func private(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %o); run: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	return nil
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	if err := private(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s: no PEM private key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA key", path)
	}
	return ek, nil
}

func loadCertificate(keyPath, certPath string) (tls.Certificate, error) {
	if err := private(keyPath); err != nil {
		return tls.Certificate{}, err
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("server certificate: %w", err)
	}
	return c, nil
}
