package vm

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/egress"
	"github.com/blackshieldpt/curral/internal/fsutil"
	"github.com/blackshieldpt/curral/internal/qemu"
	"github.com/blackshieldpt/curral/internal/template"
)

// poolManager has template "ag" (agents, a repo-less allowlist) with a fake
// base, and ready pool VMs named in ready.
func poolManager(t *testing.T, ready ...string) *Manager {
	t.Helper()
	needQemuImg(t)
	m := testManager(t)
	writeFile(t, filepath.Join(m.P.Templates(), "ag.yaml"), "extends: minimal\nruntimes: { node: \"24\" }\nagents:\n  claude: {}\nidle_stop: 2h\n")
	r, err := m.Resolve("ag", "")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", r.BasePath(m.P), "1G").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	m.Cfg.Pool = []config.PoolEntry{{Template: "ag", Size: 1}}
	for _, n := range ready {
		s, err := m.Create(context.Background(), n, CreateOpts{Template: "ag", pool: true})
		if err != nil {
			t.Fatal(err)
		}
		if !s.Pool || s.Agents.Any() || s.Booted {
			t.Fatalf("pool VM state = %+v", s)
		}
		s.Booted, s.Resume = true, poolSnap
		if err := m.save(s); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestPoolVMHasNothingToSteal(t *testing.T) {
	m := poolManager(t, "pool-a")
	if b, _ := os.ReadFile(m.file("pool-a", "allowlist")); len(bytes.TrimSpace(b)) != 0 {
		t.Errorf("pool VM allowlist = %q", b)
	}
	if b, _ := os.ReadFile(m.P.SSHConfig()); strings.Contains(string(b), "pool-a") {
		t.Error("pool VM is in ssh_config")
	}
	if _, err := m.Start(context.Background(), "pool-a"); err == nil || !strings.Contains(err.Error(), "pool VM") {
		t.Errorf("start of a pool VM: %v", err)
	}
	if err := m.Restore(context.Background(), "pool-a", poolSnap, false); err == nil || !strings.Contains(err.Error(), "pool VM") {
		t.Errorf("restore of a pool VM: %v", err)
	}
	if cfg, err := egress.ReadConfig(m.dir("pool-a")); err != nil || cfg.OnDeny != egress.OnDenyLog {
		t.Errorf("pool VM egress config %+v %v", cfg, err)
	}
}

func TestClaimSkipsLockedPoolVM(t *testing.T) {
	m := poolManager(t, "pool-a")
	unlock, err := fsutil.Lock(m.file("pool-a", lockFile))
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Create(context.Background(), "work", CreateOpts{Template: "ag"})
	unlock()
	if err != nil || s.Resume != "" {
		t.Fatalf("claimed a locked pool VM: %v %+v", err, s)
	}
	// claimPool alone leaves a state that no longer says pool VM.
	if err := os.Mkdir(m.dir("w2"), 0o700); err != nil {
		t.Fatal(err)
	}
	pool, _ := m.Load("pool-a")
	got, err := m.claimPool("w2", keyOf(pool))
	if err != nil || got == nil || got.Name != "pool-a" {
		t.Fatalf("claimPool = %+v, %v", got, err)
	}
	if ns, _ := m.Load("w2"); ns.Name != "w2" || ns.Pool || ns.Resume != poolSnap {
		t.Errorf("state after the rename %+v", ns)
	}
	if vms, _ := m.PoolVMs(); len(vms) != 0 {
		t.Errorf("pool after claim %+v", vms)
	}
}

func TestCreateClaimsMatchingPoolVM(t *testing.T) {
	m := poolManager(t, "pool-a")
	pool, _ := m.Load("pool-a")
	// Other resources: no match, a new VM.
	s, err := m.Create(context.Background(), "big", CreateOpts{Template: "ag", CPUs: 3})
	if err != nil {
		t.Fatal(err)
	}
	if s.Resume != "" || s.Booted {
		t.Errorf("big came from the pool: %+v", s)
	}
	s, err = m.Create(context.Background(), "work", CreateOpts{Template: "ag"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.dir("pool-a")); !os.IsNotExist(err) {
		t.Errorf("pool-a still there: %v", err)
	}
	if s.Pool || !s.Booted || s.Resume != poolSnap || s.Instance != pool.Instance || s.Agents.Claude == nil || s.IdleStop != "2h" {
		t.Errorf("claimed state = %+v", s)
	}
	if b, _ := os.ReadFile(m.keys("work").KnownHosts()); !strings.HasPrefix(string(b), "curral-work ssh-ed25519 ") {
		t.Errorf("known_hosts = %q", b)
	}
	seed, _ := os.ReadFile(m.file("work", seedFile))
	if !bytes.Contains(seed, []byte("instance-id: "+pool.Instance+"\nlocal-hostname: work\n")) {
		t.Error("seed keeps neither the instance nor the new name")
	}
	if b, _ := os.ReadFile(m.file("work", "allowlist")); !strings.Contains(string(b), "api.anthropic.com:443") {
		t.Errorf("claimed allowlist = %q", b)
	}
	if b, _ := os.ReadFile(m.P.SSHConfig()); !strings.Contains(string(b), "Host curral-work\n") {
		t.Error("claimed VM missing from ssh_config")
	}
	// The pool is empty now.
	s, err = m.Create(context.Background(), "work2", CreateOpts{Template: "ag"})
	if err != nil || s.Resume != "" {
		t.Errorf("work2: %v %+v", err, s)
	}
}

func TestRecyclePool(t *testing.T) {
	m := poolManager(t, "pool-a", "pool-b")
	// pool-c's fill died: not ready.
	if _, err := m.Create(context.Background(), "pool-c", CreateOpts{Template: "ag", pool: true}); err != nil {
		t.Fatal(err)
	}
	wants, err := m.poolWants()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.recyclePool(wants, true); err != nil {
		t.Fatal(err)
	}
	vms, _ := m.PoolVMs()
	if len(vms) != 1 || !vms[0].Ready() {
		t.Fatalf("after recycling: %+v", vms)
	}
	// The template changed: its pool VMs are on the old base.
	writeFile(t, filepath.Join(m.P.Templates(), "ag.yaml"), "extends: minimal\nruntimes: { node: \"24\" }\nagents:\n  claude: {}\npackages: [zsh]\n")
	if wants, err = m.poolWants(); err != nil {
		t.Fatal(err)
	}
	if err := m.recyclePool(wants, true); err != nil {
		t.Fatal(err)
	}
	if vms, _ := m.PoolVMs(); len(vms) != 0 {
		t.Errorf("stale pool VM kept: %+v", vms)
	}
}

func TestTakeResume(t *testing.T) {
	m := poolManager(t, "pool-a", "pool-b")
	v, err := qemuVersion()
	if err != nil {
		t.Skip("no qemu-system:", err)
	}
	for name, ver := range map[string]string{"pool-a": v, "pool-b": "1.0.0"} {
		if err := qemu.SnapshotOffline("-c", poolSnap, m.file(name, diskFile)); err != nil {
			t.Fatal(err)
		}
		if err := m.setSnapMeta(name, poolSnap, &snapMeta{QEMU: ver, Machine: "pc-q35-9.2"}); err != nil {
			t.Fatal(err)
		}
		s, _ := m.Load(name)
		from, err := m.takeResume(s)
		if err != nil {
			t.Fatal(err)
		}
		if s, _ = m.Load(name); s.Resume != "" {
			t.Errorf("%s: resume kept", name)
		}
		ss, _ := m.Snapshots(name)
		if name == "pool-a" && (from == nil || from.snap != poolSnap || from.machine != "pc-q35-9.2" || len(ss) != 1) {
			t.Errorf("same QEMU: resume %+v, snapshots %v", from, ss)
		}
		if name == "pool-b" && (from != nil || len(ss) != 0) {
			t.Errorf("other QEMU: resume %+v, snapshots %v", from, ss)
		}
	}
}

func TestCachesAndThePool(t *testing.T) {
	m := poolManager(t, "pool-a")
	before, err := m.Resolve("ag", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(m.P.Templates(), "ag.yaml"), "extends: minimal\nruntimes: { node: \"24\" }\nagents:\n  claude: {}\nidle_stop: 2h\ncache: [npm, docker, apt]\n")
	after, err := m.Resolve("ag", "")
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash != after.Hash {
		t.Error("listing caches made another base")
	}
	s, err := m.Create(context.Background(), "work", CreateOpts{Template: "ag"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Resume != "" || !slices.Equal(s.Cache, []string{"apt", "npm"}) { // docker: the template has none
		t.Errorf("state = %+v", s)
	}
	if seed, _ := os.ReadFile(m.file("work", seedFile)); !bytes.Contains(seed, []byte("npm_config_registry=http://10.0.2.102/npm/")) {
		t.Error("seed lacks the npm cache")
	}
	wants, err := m.poolWants()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.recyclePool(wants, true); err != nil {
		t.Fatal(err)
	}
	if vms, _ := m.PoolVMs(); len(vms) != 0 {
		t.Errorf("pool VM without the caches kept: %+v", vms)
	}
}

func TestVMCaches(t *testing.T) {
	yes := true
	for _, c := range []struct {
		t    template.Template
		want []string
	}{
		{template.Template{Cache: []string{"npm", "docker", "apt", "npm"}}, []string{"apt", "npm"}},
		{template.Template{Cache: []string{"npm", "docker"}, Docker: &yes}, []string{"docker", "npm"}},
		{template.Template{Cache: []string{"npm"}, Network: template.Network{Egress: "none"}}, nil},
	} {
		if got := vmCaches(&c.t); !slices.Equal(got, c.want) {
			t.Errorf("%v: %v, want %v", c.t.Cache, got, c.want)
		}
	}
	if got := buildCaches(&template.Template{Cache: []string{"docker", "go"}, Docker: &yes}); !slices.Equal(got, []string{"go"}) {
		t.Errorf("build caches %v", got)
	}
}

func TestCanaryKinds(t *testing.T) {
	tp := template.Template{Canaries: []string{"git", "aws", "env", "git"}}
	if got := canaryKinds(&tp); !slices.Equal(got, []string{"aws", "env", "git"}) {
		t.Errorf("%v", got)
	}
	tp.Env = map[string]string{"AWS_REGION": "eu-west-1"}
	if got := canaryKinds(&tp); !slices.Equal(got, []string{"env", "git"}) {
		t.Errorf("with AWS settings: %v", got)
	}
}

// A pool VM made before canaries: changed isn't handed out.
func TestCanariesAndThePool(t *testing.T) {
	m := poolManager(t, "pool-a")
	writeFile(t, filepath.Join(m.P.Templates(), "ag.yaml"), "extends: minimal\nruntimes: { node: \"24\" }\nagents:\n  claude: {}\nidle_stop: 2h\ncanaries: [git]\n")
	wants, err := m.poolWants()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.recyclePool(wants, true); err != nil {
		t.Fatal(err)
	}
	if vms, _ := m.PoolVMs(); len(vms) != 0 {
		t.Errorf("pool VM without the canaries kept: %+v", vms)
	}
}

func TestPoolVMWithCanaryHitNotClaimed(t *testing.T) {
	m := poolManager(t, "pool-a")
	writeFile(t, m.file("pool-a", hitFile), "{}")
	if err := os.Mkdir(m.dir("w1"), 0o700); err != nil {
		t.Fatal(err)
	}
	pool, _ := m.Load("pool-a")
	if got, err := m.claimPool("w1", keyOf(pool)); err != nil || got != nil {
		t.Errorf("claimed a pool VM with a canary hit: %+v %v", got, err)
	}
}
