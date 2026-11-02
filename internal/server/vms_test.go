package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// decoy marks every secret the tests plant: no response may carry it.
const decoy = "DECOY-c4tchp3n"

var created = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// vmHost is a catchpen home in a temporary directory (root), served on a
// socket, with its log.
type vmHost struct {
	root string
	p    paths.Paths
	sock string
	log  *strings.Builder
	mu   *sync.Mutex
}

// home makes an empty catchpen home and serves it, with mgr as its Manager
// (nil: one for the home).
func home(t *testing.T, mgr func() (*vm.Manager, error)) *vmHost {
	t.Helper()
	h := &vmHost{root: t.TempDir(), log: &strings.Builder{}, mu: &sync.Mutex{}}
	h.p = paths.Paths{Config: filepath.Join(h.root, "config"), Data: filepath.Join(h.root, "data"), Cache: filepath.Join(h.root, "cache")}
	if err := h.p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if mgr == nil {
		m := &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}
		mgr = func() (*vm.Manager, error) { return m, nil }
	}
	h.sock = filepath.Join(t.TempDir(), "api.sock")
	l, err := Listen(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	s := New("1.2.3", "")
	s.Manager = mgr
	s.Logf = func(format string, a ...any) {
		h.mu.Lock()
		defer h.mu.Unlock()
		fmt.Fprintf(h.log, format+"\n", a...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, s.Handler()) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return h
}

// serveVMs is a home with four VMs:
//   - alpha, stopped, made from template picks whose base has changed since,
//     with every secret a VM's state and directory keep set to a decoy;
//   - beta, running, paused by a canary hit, with a forward;
//   - gamma, running, not paused, its canaries found in use;
//   - pool-1, a warm pool VM.
func serveVMs(t *testing.T) *vmHost {
	t.Helper()
	h := home(t, nil)
	m := &vm.Manager{P: h.p}
	h.writeVM(t, vm.State{
		Name: "alpha", Template: "minimal", Picks: true, Base: "/" + decoy + "/old-base.qcow2",
		Distro: "debian", User: "dev", CPUs: 2, MemoryMB: 2048, DiskMB: 10240,
		Ports: []template.Port{{Host: 8080, Guest: 80}}, Egress: "allowlist", Docker: true,
		Created: created, Booted: true, Ephemeral: true, Task: "0123abcd", IdleStop: "30m",
		Env:    map[string]string{"API_KEY": decoy},
		Agents: template.Agents{Claude: &template.Agent{Keys: []string{decoy}}, OpenCode: &template.Agent{Keys: []string{decoy}}},
		MCP: []template.MCP{{Name: "m", Type: "http", Command: []string{"run", "--token=" + decoy}, URL: "https://" + decoy,
			Env: map[string]string{"K": decoy}, Headers: map[string]string{"Authorization": decoy}}},
		Skills: []template.Skill{{Git: "https://" + decoy}}, Plugins: []template.Plugin{{Agent: "claude", Git: "https://" + decoy}},
		Repos: []vm.RepoState{{Repo: forge.Repo{Forge: "gh", Owner: decoy, Name: "r"}, Alias: decoy, DeployKeyID: 7}},
		Forges: map[string]forge.Forge{"gh": {Name: "gh", Type: "github", Host: "github.com", HTTPURL: "https://" + decoy,
			Auth: forge.Auth{Mode: "token", Token: decoy, Password: decoy}}},
		ForgeUsers: map[string]string{"gh": decoy},
		SSH:        template.SSH{Agent: "/" + decoy + "/agent.sock", AgentKeys: []string{"/" + decoy}},
		Instance:   decoy, Resume: decoy, Canaries: []string{decoy}, Cache: []string{decoy},
	})
	write(t, filepath.Join(h.p.VM("alpha"), "canaries.json"), `{"token":"`+decoy+`"}`)
	write(t, filepath.Join(h.p.VM("alpha"), "ssh", "id_ed25519"), decoy)
	write(t, filepath.Join(h.p.VM("alpha"), "forge", "tokens.json"), `[{"token":"`+decoy+`"}]`)
	write(t, h.p.SecretsFile(), "TOKEN="+decoy+"\n")

	vmtest.FakeQEMU(t, h.p.VM("beta"), "beta", 512, func() bool { _, ok := m.Running("beta"); return ok })
	h.writeVM(t, vm.State{Name: "beta", Template: "base", Distro: "ubuntu", User: "dev", CPUs: 4, MemoryMB: 4096,
		DiskMB: 20480, Egress: "open", Created: created, Booted: true})
	write(t, filepath.Join(h.p.VM("beta"), "runtime.json"), `{"pid":1,"ssh_port":2222,"forwards":[{"Host":3000,"Guest":3000}]}`)
	write(t, filepath.Join(h.p.VM("beta"), "paused.json"), "{}")

	vmtest.FakeQEMU(t, h.p.VM("gamma"), "gamma", 512, func() bool { _, ok := m.Running("gamma"); return ok })
	h.writeVM(t, vm.State{Name: "gamma", Template: "base", Distro: "ubuntu", Created: created})
	write(t, filepath.Join(h.p.VM("gamma"), "canary-hit.json"), "{}")

	h.writeVM(t, vm.State{Name: "pool-1", Template: "base", Distro: "ubuntu", Created: created, Pool: true})
	return h
}

// wantVMs is what serveVMs's VMs must be told as.
var wantVMs = []api.VMInfo{
	{Name: "alpha", State: api.VMStopped, Ephemeral: true, Task: "0123abcd", StaleBase: true, Template: "minimal",
		Distro: "debian", User: "dev", CPUs: 2, MemoryMB: 2048, DiskMB: 10240, Egress: "allowlist", Docker: true,
		IdleStop: "30m", Created: created, Booted: true, Ports: []api.PortInfo{{Host: 8080, Guest: 80}}, Forwards: []api.PortInfo{}},
	{Name: "beta", State: api.VMPaused, Template: "base", Distro: "ubuntu", User: "dev", CPUs: 4,
		MemoryMB: 4096, DiskMB: 20480, Egress: "open", Created: created, Booted: true, SSHPort: 2222,
		Ports: []api.PortInfo{}, Forwards: []api.PortInfo{{Host: 3000, Guest: 3000}}},
	{Name: "gamma", State: api.VMRunning, CanaryHit: true, Template: "base", Distro: "ubuntu", Created: created,
		Ports: []api.PortInfo{}, Forwards: []api.PortInfo{}},
	{Name: "pool-1", State: api.VMStopped, Pool: true, Template: "base", Distro: "ubuntu", Created: created,
		Ports: []api.PortInfo{}, Forwards: []api.PortInfo{}},
}

func (h *vmHost) writeVM(t *testing.T, s vm.State) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.p.VM(s.Name), "vm.json"), string(b))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *vmHost) client(t *testing.T) *apiclient.Client {
	t.Helper()
	c, err := apiclient.New(apiclient.Options{Socket: h.sock})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// raw GETs path, for its status and body as sent.
func (h *vmHost) raw(t *testing.T, path string) (int, string) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}
	resp, err := hc.Get("http://catchpen" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// disk gives a VM a disk with the named snapshots.
func (h *vmHost) disk(t *testing.T, name string, snaps ...string) {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("needs qemu-img")
	}
	disk := filepath.Join(h.p.VM(name), "disk.qcow2")
	args := [][]string{{"create", "-q", "-f", "qcow2", disk, "1M"}}
	for _, s := range snaps {
		args = append(args, []string{"snapshot", "-c", s, disk})
	}
	for _, a := range args {
		if out, err := exec.Command("qemu-img", a...).CombinedOutput(); err != nil {
			t.Fatalf("qemu-img %v: %v\n%s", a, err, out)
		}
	}
}

func TestVMs(t *testing.T) {
	h := serveVMs(t)
	got, err := h.client(t).VMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(wantVMs) {
		t.Fatalf("got %d VMs, want %d: %+v", len(got), len(wantVMs), got)
	}
	for i := range wantVMs {
		if !reflect.DeepEqual(got[i], wantVMs[i]) {
			t.Errorf("VM %d:\n got %+v\nwant %+v", i, got[i], wantVMs[i])
		}
	}
}

func TestVM(t *testing.T) {
	h := serveVMs(t)
	c := h.client(t)
	for _, want := range wantVMs {
		if got, err := c.VM(context.Background(), want.Name); err != nil || !reflect.DeepEqual(*got, want) {
			t.Errorf("%s:\n got %+v, %v\nwant %+v", want.Name, got, err, want)
		}
	}
	for _, name := range []string{"nope", "../alpha", "alpha/snapshots", "", ".", ".."} {
		if _, err := c.VM(context.Background(), name); code(err) != api.CodeNotFound {
			t.Errorf("%q: %v, want not_found", name, err)
		}
	}
}

func TestSnapshots(t *testing.T) {
	h := serveVMs(t)
	h.disk(t, "alpha", "first", "second")
	c := h.client(t)
	ss, err := c.Snapshots(context.Background(), "alpha")
	if err != nil || len(ss) != 2 || ss[0].Name != "first" || ss[1].Name != "second" || ss[0].MemoryMB != 0 ||
		time.Since(ss[0].Created) > time.Minute {
		t.Errorf("alpha's snapshots: %+v, %v", ss, err)
	}
	if _, err := c.Snapshots(context.Background(), "nope"); code(err) != api.CodeNotFound {
		t.Errorf("no such VM: %v, want not_found", err)
	}
	// beta has no disk: reading it fails, and says nothing of why.
	if _, err := c.Snapshots(context.Background(), "beta"); code(err) != api.CodeInternal || strings.Contains(err.Error(), h.root) {
		t.Errorf("no disk: %v, want internal", err)
	}
}

// No list is null: an empty one is [].
func TestVMsEmpty(t *testing.T) {
	h := home(t, nil)
	if st, body := h.raw(t, api.PathVMs); st != http.StatusOK || body != "[]\n" {
		t.Errorf("no VMs: %d %q", st, body)
	}
	h.writeVM(t, vm.State{Name: "solo", Created: created})
	h.disk(t, "solo")
	if st, body := h.raw(t, api.PathSnapshots("solo")); st != http.StatusOK || body != "[]\n" {
		t.Errorf("no snapshots: %d %q", st, body)
	}
}

// No response carries a secret the VM's state, its directory or the host
// keeps, or a path on the host (security requirement 2).
func TestVMsHideSecrets(t *testing.T) {
	h := serveVMs(t)
	h.disk(t, "alpha", "first")
	for _, path := range []string{api.PathVMs, api.PathVM("alpha"), api.PathSnapshots("alpha")} {
		st, body := h.raw(t, path)
		if st != http.StatusOK || !strings.Contains(body, "alpha") && !strings.Contains(body, "first") {
			t.Errorf("%s: %d %s", path, st, body)
		}
		if strings.Contains(body, decoy) || strings.Contains(body, h.root) {
			t.Errorf("%s gives away a secret or a host path: %s", path, body)
		}
	}
}

func TestVMsNoManager(t *testing.T) {
	c, err := apiclient.New(apiclient.Options{Socket: serve(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.VMs(context.Background()); code(err) != api.CodeInternal {
		t.Errorf("no Manager: %v, want internal", err)
	}
}

// A config.yaml that doesn't load fails the request: the client is told
// nothing of why, the log is.
func TestVMsBadConfig(t *testing.T) {
	h := home(t, func() (*vm.Manager, error) { return nil, errors.New("config.yaml: " + decoy) })
	if _, err := h.client(t).VMs(context.Background()); code(err) != api.CodeInternal || strings.Contains(err.Error(), decoy) {
		t.Errorf("bad config: %v, want internal", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !strings.Contains(h.log.String(), "api: GET /v1/vms: config.yaml: "+decoy) {
		t.Errorf("log: %q", h.log)
	}
}
