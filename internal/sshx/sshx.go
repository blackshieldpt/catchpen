// Package sshx manages per-VM SSH keys, pinned host keys, ssh_config and a client.
package sshx

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
)

const AliasPrefix = "catchpen-"

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
// GenerateKey returns a new ed25519 key pair (OpenSSH private key, authorized_keys line).
func GenerateKey(comment string) (priv, pub []byte, err error) { return genKey(comment) }

func (k Keys) Generate(vm string) error {
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return err
	}
	alias := AliasPrefix + vm
	cpriv, cpub, err := genKey("catchpen@" + vm)
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

// Rename pins the host key under the alias of vm, for a VM that was renamed.
func (k Keys) Rename(vm string) error {
	pub, err := k.hostKey()
	if err != nil {
		return err
	}
	known := fmt.Sprintf("%s %s", AliasPrefix+vm, ssh.MarshalAuthorizedKey(pub))
	return fsutil.WriteFileAtomic(k.KnownHosts(), []byte(known), 0o600)
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
		if errors.Is(err, ErrHostKey) {
			return nil, ErrHostKey
		}
		// Handshake errors can carry guest text (version string, disconnect message).
		return nil, guestError{Printable(err.Error())}
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// guestError holds an error message that came from the guest, already sanitized.
type guestError struct{ msg string }

func (e guestError) Error() string { return e.msg }

// GuestOutput is output the guest controls. Raw gives its bytes, for
// parsing; printed (with %s or %v, or String) it is trimmed and has no control
// characters. It's a struct so that a plain conversion can't make it a string.
type GuestOutput struct{ b []byte }

// Guest wraps bytes that came from the guest.
func Guest(b []byte) GuestOutput { return GuestOutput{b} }

// Raw is the output as the guest sent it.
func (g GuestOutput) Raw() []byte { return g.b }

func (g GuestOutput) String() string { return Printable(strings.TrimSpace(string(g.b))) }

// Printable drops control characters (except newline and tab) from guest text
// before it reaches the host terminal.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		// Not bidi controls either: they reorder what follows on screen.
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && !(r >= 0x80 && r < 0xa0) && !(r >= 0x202a && r <= 0x202e) && !(r >= 0x2066 && r <= 0x2069) && r != 0x200e && r != 0x200f && r != 0x061c) {
			return r
		}
		return -1
	}, s)
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

// ExitCode is the command's exit status, as for exec.ExitError.
func (e *ExitError) ExitCode() int { return e.Status }

// Stream runs cmd and copies its stdout to w, failing once more than max bytes
// arrive. Stderr (capped) is returned for diagnostics.
func Stream(ctx context.Context, c *ssh.Client, cmd string, w io.Writer, max int64) (stderr GuestOutput, err error) {
	b, err := stream(ctx, c, cmd, w, max)
	return Guest(b), err
}

func stream(ctx context.Context, c *ssh.Client, cmd string, w io.Writer, max int64) ([]byte, error) {
	s, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	lw := &limitWriter{w: w, left: max}
	eb := &capWriter{max: 64 << 10}
	s.Stdout, s.Stderr = lw, eb
	err = s.Run(cmd)
	if lw.over {
		return eb.b.Bytes(), fmt.Errorf("%s: output exceeded %d MiB", cmd, max>>20)
	}
	if ctx.Err() != nil {
		return eb.b.Bytes(), fmt.Errorf("%s: %w", cmd, ctx.Err())
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return eb.b.Bytes(), &ExitError{Cmd: cmd, Status: ee.ExitStatus()}
	}
	if err != nil {
		return eb.b.Bytes(), fmt.Errorf("%s: %w", cmd, err)
	}
	return eb.b.Bytes(), nil
}

type limitWriter struct {
	w    io.Writer
	left int64
	over bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		l.over = true
		return 0, errors.New("output limit exceeded")
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}

// MaxOutput is the most guest output Run keeps.
const MaxOutput = 16 << 20

var ErrOutputTooLarge = fmt.Errorf("guest output exceeded %d MiB", MaxOutput>>20)

// capWriter keeps up to max bytes and discards the rest (so the remote side
// isn't blocked), remembering that it overflowed.
type capWriter struct {
	mu   sync.Mutex
	b    bytes.Buffer
	max  int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.max - w.b.Len(); len(p) > room {
		w.b.Write(p[:max(room, 0)])
		w.over = true
		return len(p), nil
	}
	return w.b.Write(p)
}

// Run executes cmd and returns its combined output, which is guest-controlled:
// errors never include it, so callers decide whether to show it.
// Cancelling ctx ends the session.
func Run(ctx context.Context, c *ssh.Client, cmd string, stdin io.Reader) (GuestOutput, error) {
	b, err := run(ctx, c, cmd, stdin)
	return Guest(b), err
}

func run(ctx context.Context, c *ssh.Client, cmd string, stdin io.Reader) ([]byte, error) {
	s, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	s.Stdin = stdin
	// Guest output is untrusted: cap what the host buffers.
	buf := &capWriter{max: MaxOutput}
	s.Stdout, s.Stderr = buf, buf
	err = s.Run(cmd)
	out := buf.b.Bytes()
	if buf.over {
		return out, fmt.Errorf("%s: %w", cmd, ErrOutputTooLarge)
	}
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
	// ForwardAgent is "" (no forwarding), an agent socket path, or "$SSH_AUTH_SOCK".
	ForwardAgent string
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

// WriteConfig regenerates the catchpen ssh_config with one Host block per VM.
func WriteConfig(path, catchpenBin string, entries []Entry) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if strings.ContainsAny(catchpenBin, "\n\r%") {
		return fmt.Errorf("catchpen binary path %q cannot be used in ssh_config", catchpenBin)
	}
	var b strings.Builder
	b.WriteString("# Managed by catchpen; regenerated on VM create/destroy. Do not edit.\n")
	for _, e := range entries {
		alias := AliasPrefix + e.Name
		fmt.Fprintf(&b, "\nHost %s\n", alias)
		if e.User != "" {
			fmt.Fprintf(&b, "  User %s\n", e.User)
		}
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
		// ssh doesn't forward an agent when IdentityAgent is none, so a forwarding
		// VM names the forwarded agent there too; IdentitiesOnly keeps its keys
		// from being offered to the VM itself.
		switch e.ForwardAgent {
		case "":
			b.WriteString("  IdentityAgent none\n")
			b.WriteString("  ForwardAgent no\n")
		case "$SSH_AUTH_SOCK":
			b.WriteString("  IdentityAgent $SSH_AUTH_SOCK\n")
			b.WriteString("  ForwardAgent $SSH_AUTH_SOCK\n")
		default:
			sock, err := cfgQuote(e.ForwardAgent)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "  IdentityAgent %s\n", sock)
			fmt.Fprintf(&b, "  ForwardAgent %s\n", sock)
		}
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
		// The first value wins, so these also beat a `Host *` block that a manual
		// `ssh catchpen-<vm>` reads after this file. A PKCS#11 provider's keys
		// would be offered despite IdentitiesOnly.
		b.WriteString("  PreferredAuthentications publickey\n")
		b.WriteString("  UpdateHostKeys no\n")
		b.WriteString("  AddKeysToAgent no\n")
		b.WriteString("  PKCS11Provider none\n")
		fmt.Fprintf(&b, "  ProxyCommand %s ssh-proxy %s\n", shQuote(catchpenBin), e.Name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte(b.String()), 0o600)
}

// ConfigCheck is what the ssh config does to `ssh catchpen-<vm>` besides
// catchpen's own Host block, as `ssh -G` resolves it.
type ConfigCheck struct {
	NotRead    bool     // ssh doesn't read catchpen's block for this VM
	Added      []string // identity and certificate files, SendEnv and SetEnv from other blocks
	Overridden []string // catchpen's settings that a block read before it replaced
}

// localeEnv are SendEnv patterns distributions set by default; LC_ prefixes too.
var localeEnv = []string{"LANG", "LANGUAGE", "COLORTERM", "NO_COLOR", "XMODIFIERS"}

// pins are the settings WriteConfig writes for e, as `ssh -G` prints them.
func pins(e Entry) []string {
	fwd := "no"
	if e.ForwardAgent != "" {
		fwd = e.ForwardAgent
	}
	return []string{"identitiesonly yes", "passwordauthentication no", "preferredauthentications publickey",
		"updatehostkeys false", "addkeystoagent false", "forwardagent " + fwd, "forwardx11 no",
		"forwardx11trusted no", "clearallforwardings yes", "tunnel false", "stricthostkeychecking true",
		"hostkeyalias " + AliasPrefix + e.Name, "userknownhostsfile " + e.Keys.KnownHosts()}
}

// CheckConfig runs `ssh -G` for e's alias with the ssh config ssh itself reads
// (cfg "" means ~/.ssh/config and /etc/ssh/ssh_config) and reports what other
// blocks add or override. Identity and certificate files and SendEnv add up,
// so no setting of catchpen's can stop them; the others first-value-win, so
// they are lost only to a block read before catchpen's Include. It returns a
// zero ConfigCheck when ssh isn't installed.
func CheckConfig(ctx context.Context, cfg string, e Entry) (ConfigCheck, error) {
	var c ConfigCheck
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return c, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // a Match exec can hang
	defer cancel()
	args := []string{"-G"}
	if cfg != "" {
		args = append(args, "-F", cfg)
	}
	out, err := exec.CommandContext(ctx, bin, append(args, AliasPrefix+e.Name)...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, Printable(strings.TrimSpace(string(ee.Stderr))))
		}
		return c, fmt.Errorf("ssh -G %s%s: %w", AliasPrefix, e.Name, err)
	}
	lines := strings.Split(string(out), "\n")
	if !slices.Contains(lines, "identityfile "+e.Keys.Identity()) {
		return ConfigCheck{NotRead: true}, nil
	}
	got := map[string]string{}
	for _, l := range lines {
		k, v, _ := strings.Cut(l, " ")
		got[k] = l
		switch {
		case (k == "identityfile" || k == "certificatefile") && v != e.Keys.Identity(),
			k == "sendenv" && !slices.Contains(localeEnv, v) && !strings.HasPrefix(v, "LC_"),
			k == "setenv":
			if !slices.Contains(c.Added, l) {
				c.Added = append(c.Added, l)
			}
		}
	}
	for _, p := range pins(e) {
		if k, _, _ := strings.Cut(p, " "); got[k] != p {
			c.Overridden = append(c.Overridden, cmp.Or(got[k], k+" (unset)"))
		}
	}
	if l, ok := got["pkcs11provider"]; ok {
		c.Overridden = append(c.Overridden, l)
	}
	return c, nil
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

const includeMarker = "# catchpen: VM host aliases (remove with: catchpen setup-ssh --remove)"

// SetupInclude adds or removes the Include line at the top of ~/.ssh/config.
func SetupInclude(userConfig, catchpenConfig string, remove bool) (changed bool, err error) {
	inc, err := cfgQuote(catchpenConfig)
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
