package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// How an enrolled client authenticates over TCP.
const (
	MethodMTLS = "mtls" // a client certificate, pinned by its public key
	MethodHMAC = "hmac" // requests signed with a key the server generated
)

// ScopeConfigWrite lets a client change config.yaml and templates, which can
// run host commands, and the forges, whose credentials are host secrets, and
// submit tasks, whose guests hold the template's secrets.
const ScopeConfigWrite = "config:write"

// KeyBytes is the size of a generated HMAC key.
const KeyBytes = 32

// Client is one enrolled API client. Key is secret.
type Client struct {
	Name   string    `yaml:"name"`
	Method string    `yaml:"method"`
	Pin    string    `yaml:"pin,omitempty"` // mtls: api.PublicKeyPin of its certificate
	Key    string    `yaml:"key,omitempty"` // hmac: KeyBytes, hex
	Scopes []string  `yaml:"scopes,omitempty"`
	Added  time.Time `yaml:"added"`
}

// Clients is the clients file (clients.yaml in the config directory).
type Clients struct {
	Clients []Client `yaml:"clients"`
}

func (c Client) validate() error {
	if !api.ValidClientName(c.Name) {
		return fmt.Errorf("invalid client name %q: a lowercase letter, then lowercase letters, digits and -, up to 32 in all, not ending in -", c.Name)
	}
	switch c.Method {
	case MethodMTLS:
		if !api.ValidPin(c.Pin) || c.Key != "" {
			return fmt.Errorf("client %s: an mtls client has a pin (sha256:<hex>) and no key", c.Name)
		}
	case MethodHMAC:
		if k, err := hex.DecodeString(c.Key); err != nil || len(k) != KeyBytes || c.Pin != "" {
			return fmt.Errorf("client %s: an hmac client has a %d-byte hex key and no pin", c.Name, KeyBytes)
		}
	default:
		return fmt.Errorf("client %s: method must be mtls or hmac", c.Name)
	}
	for i, s := range c.Scopes {
		if s != ScopeConfigWrite {
			return fmt.Errorf("client %s: unknown scope %q (the only one is %s)", c.Name, s, ScopeConfigWrite)
		}
		if slices.Contains(c.Scopes[:i], s) {
			return fmt.Errorf("client %s: scope %s listed twice", c.Name, s)
		}
	}
	return nil
}

func (cs Clients) validate() error {
	names, pins := map[string]bool{}, map[string]bool{}
	for _, c := range cs.Clients {
		if err := c.validate(); err != nil {
			return err
		}
		if names[c.Name] {
			return fmt.Errorf("client %s listed twice", c.Name)
		}
		names[c.Name] = true
		if c.Pin != "" && pins[c.Pin] {
			return fmt.Errorf("client %s: another client has the same certificate key", c.Name)
		}
		if c.Pin != "" {
			pins[c.Pin] = true
		}
	}
	return nil
}

// Get returns the client called name.
func (cs Clients) Get(name string) (Client, bool) {
	for _, c := range cs.Clients {
		if c.Name == name {
			return c, true
		}
	}
	return Client{}, false
}

// LoadClients reads the clients file at path; a missing file has no clients.
// The file holds keys, so one other users can read is refused.
func LoadClients(path string) (Clients, error) {
	var cs Clients
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cs, nil
	}
	if err != nil {
		return cs, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cs, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return cs, fmt.Errorf("%s is readable by other users (mode %o); run: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cs); err != nil && !errors.Is(err, io.EOF) {
		return cs, fmt.Errorf("%s: %w", path, err)
	}
	if err := cs.validate(); err != nil {
		return cs, fmt.Errorf("%s: %w", path, err)
	}
	return cs, nil
}

// updateClients applies fn to the clients file under its lock and saves it.
func updateClients(path string, fn func(*Clients) error) error {
	unlock, err := fsutil.Lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	cs, err := LoadClients(path)
	if err != nil {
		return err
	}
	if err := fn(&cs); err != nil {
		return err
	}
	if err := cs.validate(); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString("# managed by catchpen daemon client add/rm; holds secret keys\n")
	enc := yaml.NewEncoder(&b)
	if err := enc.Encode(cs); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, b.Bytes(), 0o600)
}

func addClient(path string, c Client) error {
	c.Added = time.Now().UTC().Truncate(time.Second)
	return updateClients(path, func(cs *Clients) error {
		if _, ok := cs.Get(c.Name); ok {
			return fmt.Errorf("client %s exists (catchpen daemon client rm %s first)", c.Name, c.Name)
		}
		cs.Clients = append(cs.Clients, c)
		return nil
	})
}

// AddHMAC enrols name as an HMAC client and returns its new key.
func AddHMAC(path, name string, scopes []string) ([]byte, error) {
	key := make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := addClient(path, Client{Name: name, Method: MethodHMAC, Key: hex.EncodeToString(key), Scopes: scopes}); err != nil {
		return nil, err
	}
	return key, nil
}

// AddMTLS enrols name as an mTLS client with cert's public key, and returns
// its pin. The key must be one a TLS 1.3 client can sign with: ECDSA, Ed25519
// or RSA of 2048 bits or more. A CA certificate is refused: a client
// certificate is pinned, not trusted to sign others.
func AddMTLS(path, name string, cert *x509.Certificate, scopes []string) (string, error) {
	switch k := cert.PublicKey.(type) {
	case *ecdsa.PublicKey, ed25519.PublicKey:
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return "", fmt.Errorf("the certificate's RSA key has %d bits; use 2048 or more", k.N.BitLen())
		}
	default:
		return "", fmt.Errorf("the certificate's key (%v) can't be used: use ECDSA, Ed25519 or RSA", cert.PublicKeyAlgorithm)
	}
	if cert.IsCA {
		return "", errors.New("that is a CA certificate: enrol the client's own certificate")
	}
	pin := api.PublicKeyPin(cert)
	if err := addClient(path, Client{Name: name, Method: MethodMTLS, Pin: pin, Scopes: scopes}); err != nil {
		return "", err
	}
	return pin, nil
}

// RemoveClient removes name from the clients file.
func RemoveClient(path, name string) error {
	return updateClients(path, func(cs *Clients) error {
		i := slices.IndexFunc(cs.Clients, func(c Client) bool { return c.Name == name })
		if i < 0 {
			return fmt.Errorf("no client %s", name)
		}
		cs.Clients = slices.Delete(cs.Clients, i, i+1)
		return nil
	})
}
