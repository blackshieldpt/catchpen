package api

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"regexp"
)

// PublicKeyPin is how a certificate is pinned, the server's by its clients
// and an mTLS client's by the server: "sha256:" and the hex SHA-256 of its
// public key (SubjectPublicKeyInfo). A new certificate for the same key keeps
// its pin. A certificate that wasn't parsed (no SubjectPublicKeyInfo) has
// none: "".
func PublicKeyPin(c *x509.Certificate) string {
	if len(c.RawSubjectPublicKeyInfo) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var pinRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidPin reports whether s has PublicKeyPin's form.
func ValidPin(s string) bool { return pinRe.MatchString(s) }
