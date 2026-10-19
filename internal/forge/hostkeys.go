package forge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// ParseHostKey parses "<type> <base64>" (authorized_keys format).
func ParseHostKey(s string) (ssh.PublicKey, error) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
	return k, err
}

func FormatHostKey(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// KnownHosts renders known_hosts lines for the forge's SSH endpoint.
func (f Forge) KnownHosts() string {
	name := f.Host
	if f.Port() != 22 {
		name = "[" + f.Host + "]:" + strconv.Itoa(f.Port())
	}
	var b strings.Builder
	for _, k := range f.HostKeys {
		fmt.Fprintf(&b, "%s %s\n", name, k)
	}
	return b.String()
}

var errGotKey = errors.New("got host key")

// Scan fetches the SSH host keys a server offers (ed25519, ECDSA, RSA).
func Scan(ctx context.Context, host string, port int) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	var lastErr error
	for _, algo := range []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512} {
		var got ssh.PublicKey
		cfg := &ssh.ClientConfig{
			User:              "git",
			HostKeyAlgorithms: []string{algo},
			HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
				got = k
				return errGotKey
			},
			Timeout: 10 * time.Second,
		}
		d := net.Dialer{Timeout: 10 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _, _, err = ssh.NewClientConn(conn, host, cfg)
		conn.Close()
		if got != nil {
			keys = append(keys, got)
		} else {
			lastErr = err
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no SSH host key from %s:%d: %v", host, port, lastErr)
	}
	return keys, nil
}

// Fingerprint is the SHA256 fingerprint shown to users.
func Fingerprint(k ssh.PublicKey) string { return k.Type() + " " + ssh.FingerprintSHA256(k) }

// GitHubKeys fetches GitHub's published SSH host keys from the /meta API.
func (c *Client) GitHubKeys(ctx context.Context) ([]string, error) {
	var meta struct {
		SSHKeys []string `json:"ssh_keys"`
	}
	if err := c.do(ctx, "GET", "/meta", nil, &meta, false); err != nil {
		return nil, err
	}
	var out []string
	for _, k := range meta.SSHKeys {
		pk, err := ParseHostKey(k)
		if err != nil {
			return nil, fmt.Errorf("github meta: %w", err)
		}
		out = append(out, FormatHostKey(pk))
	}
	if len(out) == 0 {
		return nil, errors.New("github meta: no ssh_keys")
	}
	return out, nil
}

// HostBound reports whether the forge's SSH server supports OpenSSH host-bound
// public key auth (publickey-hostbound-v00@openssh.com). Destination-constrained
// keys in a forwarded agent can only sign for such servers. It uses the system
// ssh and the pinned host keys; no authentication is attempted.
func HostBound(ctx context.Context, f Forge) (bool, error) {
	if len(f.HostKeys) == 0 {
		return false, fmt.Errorf("forge %s has no pinned host keys", f.Name)
	}
	dir, err := os.MkdirTemp("", "curral-probe")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(f.KnownHosts()), 0o600); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "ssh", "-v", "-F", "/dev/null", "-o", "BatchMode=yes",
		"-o", "PreferredAuthentications=none", "-o", "IdentityAgent=none", "-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile="+kh, "-o", "GlobalKnownHostsFile=/dev/null",
		"-p", strconv.Itoa(f.Port()), "git@"+f.Host).CombinedOutput()
	s := string(out)
	if !strings.Contains(s, "SSH2_MSG_SERVICE_ACCEPT") {
		lines := strings.Split(strings.TrimSpace(s), "\n")
		return false, fmt.Errorf("ssh to %s:%d failed: %s", f.Host, f.Port(), lines[len(lines)-1])
	}
	return strings.Contains(s, "publickey-hostbound@openssh.com") || strings.Contains(s, "publickey-hostbound-v00@openssh.com"), nil
}
