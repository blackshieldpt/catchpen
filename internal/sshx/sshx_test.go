package sshx

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// serve runs a minimal sshd on loopback that accepts the given client key and
// answers "exec" requests with the command text.
func serve(t *testing.T, hostKeyPEM []byte, clientPub ssh.PublicKey) int {
	t.Helper()
	hk, err := ssh.ParsePrivateKey(hostKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(clientPub.Marshal()) {
			return nil, nil
		}
		return nil, os.ErrPermission
	}}
	cfg.AddHostKey(hk)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, _ := nc.Accept()
					go func() {
						for r := range creqs {
							if r.Type == "exec" {
								_ = r.Reply(true, nil)
								_, _ = ch.Write(r.Payload[4:])
								status := byte(0)
								if string(r.Payload[4:]) == "fail" {
									status = 3
								}
								_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, status})
								ch.Close()
							}
						}
					}()
				}
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestGenerateAndDialPinned(t *testing.T) {
	k := Keys{Dir: filepath.Join(t.TempDir(), "ssh")}
	if err := k.Generate("vm1"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{k.Identity(), k.KnownHosts(), k.hostPriv()} {
		st, err := os.Stat(f)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, err, st)
		}
	}
	known, _ := os.ReadFile(k.KnownHosts())
	if !strings.HasPrefix(string(known), "catchpen-vm1 ssh-ed25519 ") {
		t.Errorf("known_hosts = %q", known)
	}
	pubB, _ := os.ReadFile(k.Identity() + ".pub")
	clientPub, _, _, _, err := ssh.ParseAuthorizedKey(pubB)
	if err != nil {
		t.Fatal(err)
	}
	hostPEM, _ := os.ReadFile(k.hostPriv())

	port := serve(t, hostPEM, clientPub)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := k.Dial(ctx, port, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := Run(ctx, c, "echo ok", nil)
	if err != nil || string(out.Raw()) != "echo ok" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	out, err = Run(ctx, c, "fail", nil)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Status != 3 || string(out.Raw()) != "fail" {
		t.Fatalf("Run(fail) = %q, %v; want ExitError status 3 with output returned separately", out, err)
	}
	if strings.Contains(err.Error(), "fail\n") || strings.Count(err.Error(), "fail") != 1 {
		t.Errorf("error must not embed guest output: %q", err)
	}

	// A server presenting another host key must be rejected.
	other := Keys{Dir: filepath.Join(t.TempDir(), "o")}
	if err := other.Generate("x"); err != nil {
		t.Fatal(err)
	}
	otherPEM, _ := os.ReadFile(other.hostPriv())
	port2 := serve(t, otherPEM, clientPub)
	if _, err := k.WaitReady(ctx, port2, "agent", nil); err != ErrHostKey {
		t.Fatalf("WaitReady with wrong host key = %v, want ErrHostKey", err)
	}
}

func TestWriteConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ssh_config")
	if err := WriteConfig(p, "/usr/bin/catchpen", []Entry{{Name: "b", User: "agent", Keys: Keys{Dir: "/k/b"}}, {Name: "a", User: "agent", Keys: Keys{Dir: "/k/a"}}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, want := range []string{"Host catchpen-a\n", "StrictHostKeyChecking yes", "ForwardAgent no", "IdentitiesOnly yes",
		"IdentityAgent none", "PasswordAuthentication no", "HostKeyAlgorithms ssh-ed25519", "HostKeyAlias catchpen-a",
		"ForwardX11 no", "ForwardX11Trusted no", "ClearAllForwardings yes", "Tunnel no", `ProxyCommand '/usr/bin/catchpen' ssh-proxy a`, `UserKnownHostsFile "/k/a/known_hosts"`} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh_config missing %q", want)
		}
	}
	if strings.Index(s, "catchpen-a") > strings.Index(s, "catchpen-b") {
		t.Error("entries not sorted")
	}
	for _, bad := range []struct{ bin, dir string }{{"/usr/bin/cur\nral", "/k"}, {"/bin/%h", "/k"}, {"/usr/bin/catchpen", `/k"x`}, {"/usr/bin/catchpen", "/k/%d"}} {
		if err := WriteConfig(p, bad.bin, []Entry{{Name: "a", User: "agent", Keys: Keys{Dir: bad.dir}}}); err == nil {
			t.Errorf("WriteConfig(%q, %q) accepted an unsafe path", bad.bin, bad.dir)
		}
	}
}

func TestSetupInclude(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".ssh", "config")
	orig := "Host example\n  User me\n"
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if ch, err := SetupInclude(p, "/c/ssh_config", false); err != nil || !ch {
		t.Fatalf("add: %v %v", ch, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), includeMarker+"\nInclude \"/c/ssh_config\"\n") || !strings.Contains(string(b), orig) {
		t.Fatalf("after add: %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Error("mode not preserved")
	}
	if ch, _ := SetupInclude(p, "/c/ssh_config", false); ch {
		t.Error("second add must be a no-op")
	}
	if ch, err := SetupInclude(p, "/c/ssh_config", true); err != nil || !ch {
		t.Fatalf("remove: %v %v", ch, err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != orig {
		t.Fatalf("after remove = %q, want original %q", b, orig)
	}

	// Repeated cycles must not accumulate blank lines.
	for i := 0; i < 3; i++ {
		SetupInclude(p, "/c/ssh_config", false)
		SetupInclude(p, "/c/ssh_config", true)
	}
	if b, _ = os.ReadFile(p); string(b) != orig {
		t.Fatalf("after cycles = %q", b)
	}

	// An Include the user added by hand counts as present.
	hand := "Include \"/c/ssh_config\"\n" + orig
	os.WriteFile(p, []byte(hand), 0o644)
	if ch, _ := SetupInclude(p, "/c/ssh_config", false); ch {
		t.Error("hand-written Include duplicated")
	}
}

func TestSetupIncludeKeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real_config")
	link := filepath.Join(dir, "config")
	os.WriteFile(target, []byte("Host x\n"), 0o600)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SetupInclude(link, "/c/ssh_config", false); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "Include") {
		t.Fatalf("target not updated: %q", b)
	}
}

func TestDialSanitizesGuestErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			// Valid version line, then a plaintext SSH_MSG_DISCONNECT with an escape sequence.
			c.Write([]byte("SSH-2.0-x\r\n"))
			msg := "bye\x1b]0;pwned\x07\x1b[2J"
			payload := []byte{1, 0, 0, 0, 2}
			payload = binary.BigEndian.AppendUint32(payload, uint32(len(msg)))
			payload = append(payload, msg...)
			payload = append(payload, 0, 0, 0, 0)
			pad := 8 - (5+len(payload))%8
			if pad < 4 {
				pad += 8
			}
			pkt := binary.BigEndian.AppendUint32(nil, uint32(1+len(payload)+pad))
			pkt = append(pkt, byte(pad))
			pkt = append(pkt, payload...)
			pkt = append(pkt, make([]byte, pad)...)
			c.Write(pkt)
			time.Sleep(200 * time.Millisecond)
			c.Close()
		}
	}()
	k := Keys{Dir: t.TempDir()}
	if err := k.Generate("a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = k.Dial(ctx, ln.Addr().(*net.TCPAddr).Port, "agent")
	if err == nil {
		t.Fatal("handshake with garbage server succeeded")
	}
	if !strings.Contains(err.Error(), "bye") {
		t.Fatalf("expected the guest's disconnect message, got %q", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07\r") {
		t.Fatalf("control characters in error: %q", err)
	}
}

func TestCapWriter(t *testing.T) {
	w := &capWriter{max: 10}
	for _, s := range []string{"12345", "6789", "abcdef"} {
		if n, err := w.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if w.b.String() != "123456789a" || !w.over {
		t.Fatalf("buffer %q over=%v", w.b.String(), w.over)
	}
}

func TestWriteConfigForwardAgent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ssh_config")
	err := WriteConfig(p, "/usr/bin/catchpen", []Entry{
		{Name: "off", User: "agent", Keys: Keys{Dir: "/k/off"}},
		{Name: "res", User: "agent", Keys: Keys{Dir: "/k/res"}, ForwardAgent: "/k/res/agent.sock"},
		{Name: "host", User: "agent", Keys: Keys{Dir: "/k/host"}, ForwardAgent: "$SSH_AUTH_SOCK"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	blocks := strings.Split(string(b), "\nHost ")
	want := map[string]string{"off": "ForwardAgent no\n", "res": `ForwardAgent "/k/res/agent.sock"` + "\n", "host": "ForwardAgent $SSH_AUTH_SOCK\n"}
	for _, blk := range blocks[1:] {
		name := strings.TrimPrefix(strings.SplitN(blk, "\n", 2)[0], "catchpen-")
		ia := strings.Replace(want[name], "ForwardAgent", "IdentityAgent", 1)
		if name == "off" {
			ia = "IdentityAgent none\n"
		}
		if !strings.Contains(blk, want[name]) || strings.Count(blk, "ForwardAgent") != 1 || !strings.Contains(blk, ia) || !strings.Contains(blk, "IdentitiesOnly yes") {
			t.Errorf("%s block:\n%s", name, blk)
		}
	}
	if err := WriteConfig(p, "/usr/bin/catchpen", []Entry{{Name: "x", User: "a", Keys: Keys{Dir: "/k"}, ForwardAgent: "/k/%h.sock"}}); err == nil {
		t.Error("percent token in agent socket path accepted")
	}
}

func TestRenamePinsTheSameHostKey(t *testing.T) {
	k := Keys{Dir: filepath.Join(t.TempDir(), "ssh")}
	if err := k.Generate("pool-1"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(k.KnownHosts())
	if err := k.Rename("work"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(k.KnownHosts())
	if string(after) != strings.Replace(string(before), "catchpen-pool-1 ", "catchpen-work ", 1) {
		t.Errorf("known_hosts %q -> %q", before, after)
	}
}

func TestPrintable(t *testing.T) {
	if got := Printable("a\x1b[2Jb\tc\nd\u009be\u202ef\u2067g\u200fh\u061ci"); got != "a[2Jb\tc\ndefghi" {
		t.Errorf("%q", got)
	}
}

// Guest output prints trimmed and without control characters; Raw keeps it.
func TestGuestOutput(t *testing.T) {
	g := Guest([]byte(" a\x1b]0;x\x07b\n"))
	if s := fmt.Sprintf("%s|%v|%s", g, g, fmt.Errorf("e: %s", g)); s != "a]0;xb|a]0;xb|e: a]0;xb" {
		t.Errorf("%q", s)
	}
	if string(g.Raw()) != " a\x1b]0;x\x07b\n" {
		t.Errorf("raw %q", g.Raw())
	}
}
