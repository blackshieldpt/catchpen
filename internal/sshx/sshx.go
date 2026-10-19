// Package sshx manages per-VM SSH keys, pinned host keys, ssh_config and a client.
package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

const AliasPrefix = "curral-"

// Keys is a VM's key directory: client identity + pinned host key.
type Keys struct{ Dir string }

func (k Keys) Identity() string   { return filepath.Join(k.Dir, "id_ed25519") }
func (k Keys) KnownHosts() string { return filepath.Join(k.Dir, "known_hosts") }
func (k Keys) hostPriv() string   { return filepath.Join(k.Dir, "host_ed25519") }

func genKey(comment string) (priv []byte, pub []byte, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(sk, comment)
	if err != nil {
		return nil, nil, err
	}
	sp, err := ssh.NewPublicKey(pk)
	if err != nil {
		return nil, nil, err
	}
	pub = bytes.TrimSpace(ssh.MarshalAuthorizedKey(sp))
	return pem.EncodeToMemory(block), append(pub, []byte(" "+comment+"\n")...), nil
}

// Generate creates the client key, host key and known_hosts for a VM.
func (k Keys) Generate(vm string) error {
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return err
	}
	alias := AliasPrefix + vm
	cpriv, cpub, err := genKey("curral@" + vm)
	if err != nil {
		return err
	}
	hpriv, hpub, err := genKey("host@" + vm)
	if err != nil {
		return err
	}
	hostFields := strings.Fields(string(hpub))
	known := fmt.Sprintf("%s %s %s\n", alias, hostFields[0], hostFields[1])
	for p, b := range map[string][]byte{
		k.Identity():          cpriv,
		k.Identity() + ".pub": cpub,
		k.hostPriv():          hpriv,
		k.hostPriv() + ".pub": hpub,
		k.KnownHosts():        []byte(known),
	} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (k Keys) ClientPub() (string, error) {
	b, err := os.ReadFile(k.Identity() + ".pub")
	return string(b), err
}

// HostKeyPair returns the host private key (PEM) and public key for cloud-init.
func (k Keys) HostKeyPair() (priv, pub string, err error) {
	a, err := os.ReadFile(k.hostPriv())
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(k.hostPriv() + ".pub")
	return string(a), string(b), err
}

func (k Keys) signer() (ssh.Signer, error) {
	b, err := os.ReadFile(k.Identity())
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

func (k Keys) hostKey() (ssh.PublicKey, error) {
	b, err := os.ReadFile(k.hostPriv() + ".pub")
	if err != nil {
		return nil, err
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey(b)
	return pk, err
}

// Dial connects to the VM's sshd on 127.0.0.1:port, verifying the pinned host key.
func (k Keys) Dial(ctx context.Context, port int, user string) (*ssh.Client, error) {
	signer, err := k.signer()
	if err != nil {
		return nil, err
	}
	hk, err := k.hostKey()
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if !bytes.Equal(key.Marshal(), hk.Marshal()) {
				return ErrHostKey
			}
			return nil
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           10 * time.Second,
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// ErrHostKey is returned when the guest presents a different host key.
var ErrHostKey = errors.New("host key mismatch")

// WaitReady polls until SSH auth succeeds or ctx expires. alive reports whether
// the VM process still runs, so a crashed VM fails fast.
func (k Keys) WaitReady(ctx context.Context, port int, user string, alive func() bool) (*ssh.Client, error) {
	var last error
	for {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		c, err := k.Dial(dctx, port, user)
		cancel()
		if err == nil {
			return c, nil
		}
		if errors.Is(err, ErrHostKey) {
			return nil, ErrHostKey
		}
		last = err
		if alive != nil && !alive() {
			return nil, fmt.Errorf("VM exited during boot (last ssh error: %w)", last)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for ssh: %w", last)
		case <-time.After(2 * time.Second):
		}
	}
}

// ExitError reports a remote command's non-zero exit status. It carries no
// guest-supplied text (ssh.ExitError's message comes from the remote side).
type ExitError struct {
	Cmd    string
	Status int
}

func (e *ExitError) Error() string { return fmt.Sprintf("%s: exited with status %d", e.Cmd, e.Status) }

// Run executes cmd and returns its combined output, which is guest-controlled:
// errors never include it, so callers decide whether (and how safely) to show it.
// Cancelling ctx ends the session.
func Run(ctx context.Context, c *ssh.Client, cmd string, stdin io.Reader) ([]byte, error) {
	s, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	s.Stdin = stdin
	out, err := s.CombinedOutput(cmd)
	if ctx.Err() != nil {
		return out, fmt.Errorf("%s: %w", cmd, ctx.Err())
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return out, &ExitError{Cmd: cmd, Status: ee.ExitStatus()}
	}
	if err != nil {
		return out, fmt.Errorf("%s: %w", cmd, err)
	}
	return out, nil
}

// Entry is one VM in the generated ssh_config.
type Entry struct {
	Name string
	User string
	Keys Keys
}

// cfgQuote double-quotes a path for ssh_config (which has no escape syntax).
func cfgQuote(s string) (string, error) {
	if strings.ContainsAny(s, "\"\n\r%") { // ssh expands %-tokens in these paths
		return "", fmt.Errorf("path %q cannot be used in ssh_config", s)
	}
	return `"` + s + `"`, nil
}

// shQuote single-quotes s for the shell that runs ProxyCommand.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// WriteConfig regenerates the curral ssh_config with one Host block per VM.
func WriteConfig(path, curralBin string, entries []Entry) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if strings.ContainsAny(curralBin, "\n\r%") {
		return fmt.Errorf("curral binary path %q cannot be used in ssh_config", curralBin)
	}
	var b strings.Builder
	b.WriteString("# Managed by curral; regenerated on VM create/destroy. Do not edit.\n")
	for _, e := range entries {
		alias := AliasPrefix + e.Name
		fmt.Fprintf(&b, "\nHost %s\n", alias)
		fmt.Fprintf(&b, "  User %s\n", e.User)
		id, err := cfgQuote(e.Keys.Identity())
		if err != nil {
			return err
		}
		kh, err := cfgQuote(e.Keys.KnownHosts())
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "  IdentityFile %s\n", id)
		b.WriteString("  IdentitiesOnly yes\n")
		b.WriteString("  IdentityAgent none\n")
		b.WriteString("  ForwardAgent no\n")
		// Override forwarding a user's `Host *` block might enable.
		b.WriteString("  ForwardX11 no\n")
		b.WriteString("  ForwardX11Trusted no\n")
		b.WriteString("  ClearAllForwardings yes\n")
		b.WriteString("  Tunnel no\n")
		fmt.Fprintf(&b, "  HostKeyAlias %s\n", alias)
		fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", kh)
		b.WriteString("  StrictHostKeyChecking yes\n")
		b.WriteString("  HostKeyAlgorithms ssh-ed25519\n")
		b.WriteString("  PasswordAuthentication no\n")
		fmt.Fprintf(&b, "  ProxyCommand %s ssh-proxy %s\n", shQuote(curralBin), e.Name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte(b.String()), 0o600)
}

// Proxy pipes stdin/stdout to 127.0.0.1:port (ssh ProxyCommand).
func Proxy(port int, in io.Reader, out io.Writer) error {
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer conn.Close()
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, in)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		errc <- err
	}()
	go func() {
		_, err := io.Copy(out, conn)
		errc <- err
	}()
	return <-errc
}

const includeMarker = "# curral: VM host aliases (remove with: curral setup-ssh --remove)"

// SetupInclude adds or removes the Include line at the top of ~/.ssh/config.
func SetupInclude(userConfig, curralConfig string, remove bool) (changed bool, err error) {
	inc, err := cfgQuote(curralConfig)
	if err != nil {
		return false, err
	}
	line := "Include " + inc
	if real, err := filepath.EvalSymlinks(userConfig); err == nil {
		userConfig = real // edit the target, keep the symlink
	}
	b, err := os.ReadFile(userConfig)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var kept []string
	present, unmarked := false, false
	lines := strings.Split(string(b), "\n")
	for i := 0; i < len(lines); i++ {
		if lines[i] == includeMarker && i+1 < len(lines) && lines[i+1] == line {
			present = true
			i++
			if i+1 < len(lines) && lines[i+1] == "" { // the blank line we added
				i++
			}
			continue
		}
		if strings.TrimSpace(lines[i]) == line {
			unmarked = true
		}
		kept = append(kept, lines[i])
	}
	if remove {
		if !present {
			return false, nil
		}
		return true, writeUserConfig(userConfig, strings.Join(kept, "\n"))
	}
	if present || unmarked { // already included (by us or by hand)
		return false, nil
	}
	// Include must precede any Host block to apply globally.
	return true, writeUserConfig(userConfig, includeMarker+"\n"+line+"\n\n"+string(b))
}

func writeUserConfig(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return fsutil.WriteFileAtomic(path, []byte(content), mode)
}
