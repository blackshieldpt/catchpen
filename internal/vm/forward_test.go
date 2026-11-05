package vm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/template"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestForwardSpecs(t *testing.T) {
	for in, want := range map[string]string{"3000": "127.0.0.1:3000:localhost:3000", "8080:80": "127.0.0.1:8080:localhost:80", "9000:db:5432": "127.0.0.1:9000:db:5432"} {
		if got, err := LocalSpec(in); err != nil || got != want {
			t.Errorf("LocalSpec(%q) = %q, %v", in, got, err)
		}
	}
	for in, want := range map[string]string{"5432": "127.0.0.1:5432:localhost:5432", "15432:5432": "127.0.0.1:15432:localhost:5432", "6379:cache.lan:6379": "127.0.0.1:6379:cache.lan:6379"} {
		if got, err := RemoteSpec(in); err != nil || got != want {
			t.Errorf("RemoteSpec(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "70000", "a:b", "0.0.0.0:80:x:80", "1:-oProxyCommand=x:2", "1:a b:2", "x"} {
		if _, err := LocalSpec(bad); err == nil {
			t.Errorf("LocalSpec(%q) accepted", bad)
		}
		if _, err := RemoteSpec(bad); err == nil {
			t.Errorf("RemoteSpec(%q) accepted", bad)
		}
	}
	if got, err := DynamicSpec("1080"); err != nil || got != "127.0.0.1:1080" {
		t.Errorf("DynamicSpec = %q, %v", got, err)
	}
	if _, err := DynamicSpec("1080:x"); err == nil {
		t.Error("bad -D accepted")
	}
}

func TestForwardNeedsRunningVM(t *testing.T) {
	m := testManager(t)
	writeFile(t, m.vm("a").path(stateFile), `{"name":"a"}`)
	if _, err := m.Forward(context.Background(), "a", []string{"3000"}, nil, nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("err = %v", err)
	}
	if fs, err := m.Forwards("a"); err != nil || len(fs) != 0 {
		t.Errorf("Forwards = %v, %v", fs, err)
	}
}

// A forward's ssh never lets the guest use the VM's SSH agent. The guest's
// sshd is hostile: it can open an agent channel whenever it likes, not only
// when a session asked to forward the agent, and ssh accepts it whenever
// ForwardAgent is on.
func TestForwardKeepsAgentFromGuest(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh")
	}
	m := testManager(t)
	fakeQEMU(t, m, "r", 512)
	writeFile(t, m.vm("r").path(runtimeFile), `{"ssh_port":2222}`)
	writeFile(t, m.vm("r").path(stateFile), `{"name":"r","user":"agent","ssh":{"agent":"restricted"}}`)
	k := m.keys("r")
	if err := k.Generate("r"); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSSHConfig(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(m.P.SSHConfig()); !strings.Contains(string(b), "  ForwardAgent \""+m.agent("r").Socket()+"\"\n") {
		t.Fatalf("the VM's ssh_config doesn't forward its agent:\n%s", b)
	}
	// The VM's restricted agent, holding one key.
	_, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: sk}); err != nil {
		t.Fatal(err)
	}
	al, err := net.Listen("unix", m.agent("r").Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	go func() {
		for {
			c, err := al.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(kr, c); c.Close() }()
		}
	}()
	// The guest's sshd: once the forward has logged in, it asks for the agent.
	hostPriv, _, err := k.HostKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	hk, err := ssh.ParsePrivateKey([]byte(hostPriv))
	if err != nil {
		t.Fatal(err)
	}
	cpub, err := k.ClientPub()
	if err != nil {
		t.Fatal(err)
	}
	client, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cpub))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(client.Marshal()) {
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
	got, done := make(chan string, 1), make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		c, err := l.Accept()
		if err != nil {
			got <- "accept: " + err.Error()
			return
		}
		sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			got <- "login: " + err.Error()
			return
		}
		defer func() { <-done; sc.Close() }() // ssh ends when its connection does
		go ssh.DiscardRequests(reqs)
		go func() {
			for nc := range chans {
				_ = nc.Reject(ssh.Prohibited, "no")
			}
		}()
		ch, creqs, err := sc.OpenChannel("auth-agent@openssh.com", nil)
		var oe *ssh.OpenChannelError
		if errors.As(err, &oe) {
			got <- "refused"
			return
		}
		if err != nil {
			got <- "agent channel: " + err.Error()
			return
		}
		go ssh.DiscardRequests(creqs)
		keys, err := agent.NewClient(ch).List()
		got <- fmt.Sprintf("the guest used the VM's agent: %d key(s), %v", len(keys), err)
	}()
	// Forward's ssh, to that sshd instead of through catchpen ssh-proxy.
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/bash\nexec -a ssh '%s' -o ProxyCommand=none -o HostName=127.0.0.1 -o Port=%d \"$@\"\n", sshBin, l.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(pl.Addr().(*net.TCPAddr).Port)
	pl.Close()
	fs, err := m.Forward(context.Background(), "r", []string{port}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(fs.PID, syscall.SIGTERM) })
	select {
	case g := <-got:
		if g != "refused" {
			t.Error(g)
		}
	case <-time.After(10 * time.Second):
		t.Error("the guest's sshd saw no login")
	}
}

func TestCheckAgent(t *testing.T) {
	agentForge := map[string]forge.Forge{"g": {Name: "g", Auth: forge.Auth{Mode: forge.ModeAgent}}}
	tokenForge := map[string]forge.Forge{"t": {Name: "t", Auth: forge.Auth{Mode: forge.ModeToken}}}
	restricted := template.SSH{Agent: template.AgentRestricted, AgentKeys: []string{"~/.ssh/id"}}
	cases := []struct {
		ssh  template.SSH
		fs   map[string]forge.Forge
		want string
	}{
		{template.SSH{}, agentForge, "set ssh.agent"},
		{restricted, tokenForge, "needs a repo on a forge with auth mode agent"},
		{restricted, nil, "needs a repo"},
		{restricted, agentForge, ""},
		{template.SSH{Agent: template.AgentHost}, agentForge, "set ssh.agent: restricted"},
		{template.SSH{Agent: template.AgentHost}, tokenForge, ""},
		{template.SSH{}, tokenForge, ""},
	}
	for i, c := range cases {
		err := checkAgent(c.ssh, c.fs)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("case %d: err = %v, want %q", i, err, c.want)
		}
	}
}
