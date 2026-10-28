package qemu

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/egress"
)

func netdevOf(t *testing.T, s Spec) string {
	t.Helper()
	a, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range a {
		if v == "-netdev" {
			return a[i+1]
		}
	}
	t.Fatal("no -netdev")
	return ""
}

func TestNetdevAllowlist(t *testing.T) {
	nd := netdevOf(t, Spec{
		CPUs: 1, MemoryMB: 512, Disk: "/d,isk.qcow2", Egress: "allowlist", SSHPort: 2222,
		Forwards: []Forward{{Host: 8080, Guest: 80}},
		ProxyCmd: []string{"/opt/my bin/catchpen", "egress-stdio", "--dir", "/v,m"}, ProxyIP: "10.0.2.100", ProxyPort: 3128,
		NotifyCmd: []string{"/opt/my bin/catchpen", "notify-stdio", "--vm", "v"}, NotifyIP: "10.0.2.101", NotifyPort: 80,
		CacheCmd: []string{"/opt/my bin/catchpen", "cache-stdio", "--kinds", "apt,npm"}, CacheIP: "10.0.2.102", CachePort: 80,
	})
	for _, want := range []string{
		"restrict=on",
		"ipv6=off",
		"hostfwd=tcp:127.0.0.1:2222-:22",
		"hostfwd=tcp:127.0.0.1:8080-:80",
		"guestfwd=tcp:10.0.2.100:3128-cmd:'/opt/my bin/catchpen' 'egress-stdio' '--dir' '/v,,m'",
		"guestfwd=tcp:10.0.2.101:80-cmd:'/opt/my bin/catchpen' 'notify-stdio' '--vm' 'v'",
		"guestfwd=tcp:10.0.2.102:80-cmd:'/opt/my bin/catchpen' 'cache-stdio' '--kinds' 'apt,,npm'",
	} {
		if !strings.Contains(nd, want) {
			t.Errorf("netdev %q missing %q", nd, want)
		}
	}
	if strings.Contains(nd, "hostfwd=tcp::") {
		t.Error("hostfwd must bind loopback")
	}
}

func TestNetdevModes(t *testing.T) {
	if nd := netdevOf(t, Spec{Egress: "open"}); strings.Contains(nd, "restrict") || strings.Contains(nd, "guestfwd") {
		t.Errorf("open: %q", nd)
	}
	if nd := netdevOf(t, Spec{Egress: "none"}); !strings.Contains(nd, "restrict=on") || strings.Contains(nd, "guestfwd") {
		t.Errorf("none: %q", nd)
	}
	if _, err := (Spec{Egress: "allowlist"}).Args(); err == nil {
		t.Error("allowlist without proxy command must fail")
	}
	if _, err := (Spec{Egress: "bogus"}).Args(); err == nil {
		t.Error("unknown mode must fail")
	}
}

func TestArgsEscapesPaths(t *testing.T) {
	a, err := Spec{Egress: "open", Disk: "/a,b.qcow2", Seed: "/s,eed.iso", QMP: "/q.sock", Serial: "/ser.sock", Pidfile: "/p"}.Args()
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(a, " ")
	for _, want := range []string{"file=/a,,b.qcow2,if=virtio", "file=/s,,eed.iso,media=cdrom,readonly=on", "unix:/q.sock,server=on,wait=off", "-pidfile /p", "-sandbox on,obsolete=deny,resourcecontrol=deny"} {
		if !strings.Contains(j, want) {
			t.Errorf("args missing %q:\n%s", want, j)
		}
	}
}

func needBin(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not installed", name)
	}
}

func TestConvertRefusesExternalReferences(t *testing.T) {
	needBin(t, "qemu-img")
	dir := t.TempDir()
	img := func(name string, args ...string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if out, err := exec.Command("qemu-img", append(append([]string{"create", "-q"}, args...), p, "1M")...).CombinedOutput(); err != nil {
			t.Fatalf("qemu-img create %s: %v %s", name, err, out)
		}
		return p
	}
	plain := img("plain.qcow2", "-f", "qcow2")
	cases := map[string]struct {
		src  string
		want string
	}{
		"backing":   {img("ov.qcow2", "-f", "qcow2", "-F", "qcow2", "-b", plain), "backing file"},
		"data-file": {img("df.qcow2", "-f", "qcow2", "-o", "data_file="+filepath.Join(dir, "df.raw")), "external data file"},
		"vmdk":      {img("x.vmdk", "-f", "vmdk"), "unsupported image format"},
	}
	for name, c := range cases {
		err := Convert(c.src, filepath.Join(dir, name+".out"), 2)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Convert = %v, want error containing %q", name, err, c.want)
		}
		if _, err := os.Stat(filepath.Join(dir, name+".out")); err == nil {
			t.Errorf("%s: output written despite refusal", name)
		}
	}
	out := filepath.Join(dir, "plain.out")
	if err := Convert(plain, out, 4); err != nil {
		t.Fatalf("plain qcow2: %v", err)
	}
	if mb, err := VirtualSizeMB(out); err != nil || mb != 4 {
		t.Errorf("converted size = %d, %v; want 4", mb, err)
	}
}

// fakeQEMU starts a process whose argv[0] looks like QEMU and returns a pidfile for it.
func fakeQEMU(t *testing.T, argv0, vmName string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh")
	// sh -c <script> <$0> <$1>: the -name pair is visible as separate argv entries.
	cmd.Args = []string{argv0, "-c", "sleep 30; :", "-name", "catchpen-" + vmName}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pf := filepath.Join(t.TempDir(), "qemu.pid")
	if err := os.WriteFile(pf, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return pf
}

func TestPidIdentity(t *testing.T) {
	pf := fakeQEMU(t, "qemu-system-x86_64", "ab")
	if _, ok := Pid(pf, "ab"); !ok {
		t.Error("exact VM name not recognised")
	}
	if _, ok := Pid(pf, "a"); ok {
		t.Error("prefix of the VM name accepted")
	}
	if _, ok := Pid(fakeQEMU(t, "/usr/bin/sh-qemu-system", "ab"), "ab"); ok {
		t.Error("process whose argv[0] is not qemu-system accepted")
	}
	self := filepath.Join(t.TempDir(), "self.pid")
	_ = os.WriteFile(self, []byte(strconv.Itoa(os.Getpid())), 0o600)
	if _, ok := Pid(self, "ab"); ok {
		t.Error("non-qemu process accepted")
	}
	dead := exec.Command("/bin/true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	deadPf := filepath.Join(t.TempDir(), "dead.pid")
	_ = os.WriteFile(deadPf, []byte(strconv.Itoa(dead.Process.Pid)), 0o600)
	if _, ok := Pid(deadPf, "ab"); ok {
		t.Error("dead pid accepted")
	}
	if _, ok := Pid(filepath.Join(t.TempDir(), "missing"), "ab"); ok {
		t.Error("missing pidfile accepted")
	}
}

// fakeQMPServer serves one connection with the given reply lines per command.
func fakeQMPServer(t *testing.T, replies map[string][]string, closeAfter string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(c, `{"QMP": {"version": {}, "capabilities": []}}`+"\n")
		dec := json.NewDecoder(c)
		for {
			var req struct {
				Execute string `json:"execute"`
			}
			if err := dec.Decode(&req); err != nil {
				return
			}
			if req.Execute == closeAfter {
				return
			}
			for _, l := range replies[req.Execute] {
				io.WriteString(c, l+"\n")
			}
		}
	}()
	return sock
}

func TestQMP(t *testing.T) {
	sock := fakeQMPServer(t, map[string][]string{
		"qmp_capabilities": {`{"return": {}}`},
		"system_powerdown": {`{"event": "POWERDOWN", "timestamp": {}}`, `{"return": {}}`},
		"bogus":            {`{"error": {"class": "CommandNotFound", "desc": "nope"}}`},
	}, "quit")
	q, err := DialQMP(sock, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if r, err := q.Execute("system_powerdown", nil); err != nil || string(r) != "{}" {
		t.Errorf("event not skipped: %s, %v", r, err)
	}
	if _, err := q.Execute("bogus", nil); err == nil || !strings.Contains(err.Error(), "CommandNotFound") {
		t.Errorf("error reply = %v", err)
	}
	if _, err := q.Execute("quit", nil); err != nil {
		t.Errorf("quit with EOF = %v, want nil", err)
	}
}

func TestLaunchFailures(t *testing.T) {
	if _, err := Launch(Spec{Egress: egress.ModeOpen}, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "QMP") {
		t.Errorf("Launch without QMP = %v", err)
	}
	needBin(t, "qemu-system-x86_64")
	dir := t.TempDir()
	log, err := os.Create(filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	// A missing disk makes QEMU exit during startup, before KVM is needed.
	s := Spec{Name: "t", CPUs: 1, MemoryMB: 64, Disk: filepath.Join(dir, "missing.qcow2"), Egress: egress.ModeNone, QMP: filepath.Join(dir, "qmp.sock")}
	if _, err := Launch(s, dir, log); err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Errorf("Launch with missing disk = %v", err)
	}
}
