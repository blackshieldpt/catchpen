package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/image"
	"github.com/blackshieldpt/catchpen/internal/task"
)

func TestValidSnap(t *testing.T) {
	for n, ok := range map[string]bool{
		"20261020-153000": true, "before-upgrade": true, "a.b_c": true,
		"": false, "12": false, "-x": false, "a b": false, "a;quit": false, strings.Repeat("a", 65): false,
	} {
		if err := validSnap(n); (err == nil) != ok {
			t.Errorf("validSnap(%q) = %v", n, err)
		}
	}
}

// qio writes pattern byte b at offset off of a qcow2 image.
func qio(t *testing.T, img string, b byte, off string) {
	t.Helper()
	if out, err := exec.Command("qemu-io", "-f", "qcow2", "-c", fmt.Sprintf("write -P 0x%02x %s 64k", b, off), img).CombinedOutput(); err != nil {
		t.Fatalf("qemu-io: %v %s", err, out)
	}
}

// view returns what a guest would read from img.
func view(t *testing.T, img string) []byte {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "v.raw")
	if out, err := exec.Command("qemu-img", "convert", "-U", "-O", "raw", img, raw).CombinedOutput(); err != nil {
		t.Fatalf("convert: %v %s", err, out)
	}
	b, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// offlineVM is a stopped VM of the minimal template on a real qcow2 base.
func offlineVM(t *testing.T, m *Manager, name string) (*State, *Resolved) {
	t.Helper()
	needQemuImg(t)
	r, err := m.Resolve("minimal")
	if err != nil {
		t.Fatal(err)
	}
	base := r.basePath(m.P, 1)
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", base, "4M").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	qio(t, base, 0xaa, "0")
	if err := os.MkdirAll(m.dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.keys(name).Generate(name); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", base, m.vm(name).path(diskFile)).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	s := &State{Name: name, Template: r.T.Name, Picks: true, Distro: r.Distro.Name, User: r.User, Base: base, DiskMB: 4, Booted: true, Created: time.Now()}
	if err := m.save(s); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestOfflineSnapshots(t *testing.T) {
	m := testManager(t)
	offlineVM(t, m, "v1")
	ctx := context.Background()
	disk := m.vm("v1").path(diskFile)
	qio(t, disk, 0x11, "64k")
	before := view(t, disk)
	if _, err := m.Snapshot(ctx, "v1", "s1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(ctx, "v1", "s1", false); err == nil {
		t.Error("duplicate snapshot name accepted")
	}
	if _, err := m.Snapshot(ctx, "v1", "7", false); err == nil {
		t.Error("numeric snapshot name accepted")
	}
	qio(t, disk, 0x22, "64k")
	ss, err := m.Snapshots("v1")
	if err != nil || len(ss) != 1 || ss[0].Name != "s1" || ss[0].VMStateMB != 0 {
		t.Fatalf("snapshots %+v %v", ss, err)
	}
	if err := m.Restore(ctx, "v1", "s1", false); err != nil {
		t.Fatal(err)
	}
	if string(view(t, disk)) != string(before) {
		t.Error("restore didn't bring back the snapshot's disk")
	}
	if err := m.Restore(ctx, "v1", "nope", false); err == nil {
		t.Error("restored a snapshot that doesn't exist")
	}
	if err := m.DeleteSnapshot("v1", "s1"); err != nil {
		t.Fatal(err)
	}
	if ss, _ := m.Snapshots("v1"); len(ss) != 0 {
		t.Errorf("after rmsnap: %+v", ss)
	}
}

func TestRebaseKeepsContents(t *testing.T) {
	m := testManager(t)
	s, r := offlineVM(t, m, "v1")
	ctx := context.Background()
	disk := m.vm("v1").path(diskFile)
	qio(t, disk, 0x33, "128k")
	before := view(t, disk)
	if _, err := m.Snapshot(ctx, "v1", "s1", false); err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(ctx, "v1"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("rebase onto the same base: %v", err)
	}
	// A rebuilt base with different contents.
	gen2 := r.basePath(m.P, 2)
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", gen2, "4M").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	qio(t, gen2, 0xbb, "0")
	qio(t, gen2, 0xbb, "128k")
	ps, _ := m.Load("v1") // a claimed pool VM not started yet
	ps.Resume = poolSnap
	m.save(ps)
	if err := m.Rebase(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if string(view(t, disk)) != string(before) {
		t.Error("rebase changed what the guest sees")
	}
	ns, _ := m.Load("v1")
	if ns.Base != gen2 || ns.Resume != "" {
		t.Errorf("base %s, want %s; resume %q", ns.Base, gen2, ns.Resume)
	}
	if ss, _ := m.Snapshots("v1"); len(ss) != 0 {
		t.Errorf("snapshots survived the rebase: %+v", ss)
	}
	if users, _ := m.baseUsers(s.Base); len(users) != 0 {
		t.Errorf("old base still used by %v", users)
	}
}

func TestResetFreshDisk(t *testing.T) {
	m := testManager(t)
	s, r := offlineVM(t, m, "v1")
	ctx := context.Background()
	disk := m.vm("v1").path(diskFile)
	qio(t, disk, 0x44, "0")
	gen2 := r.basePath(m.P, 2)
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", gen2, "8M").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	qio(t, gen2, 0xbb, "0")
	if err := m.Reset(ctx, "v1", false); err != nil {
		t.Fatal(err)
	}
	ns, _ := m.Load("v1")
	if ns.Booted || ns.Base != s.Base || string(view(t, disk)) != string(view(t, s.Base)) {
		t.Errorf("reset: booted %v base %s", ns.Booted, ns.Base)
	}
	if _, err := os.Stat(m.vm("v1").path(seedFile)); err != nil {
		t.Error("no seed for first boot")
	}
	if err := m.Reset(ctx, "v1", true); err != nil {
		t.Fatal(err)
	}
	if ns, _ = m.Load("v1"); ns.Base != gen2 || ns.DiskMB != 8 || string(view(t, disk)) != string(view(t, gen2)) {
		t.Errorf("reset --latest: base %s disk %d", ns.Base, ns.DiskMB)
	}
}

func TestGC(t *testing.T) {
	m := testManager(t)
	r, err := m.Resolve("minimal")
	if err != nil {
		t.Fatal(err)
	}
	b := m.P.Bases()
	g1, g2, g3 := r.basePath(m.P, 1), r.basePath(m.P, 2), r.basePath(m.P, 3)
	stale := filepath.Join(b, "minimal-0123456789abcdef.qcow2") // an old template version
	for _, p := range []string{g1, g2, g3, stale} {
		writeFile(t, p, "base")
	}
	if err := r.recordSpec(m.P); err != nil {
		t.Fatal(err)
	}
	writeFile(t, strings.TrimSuffix(g1, ".qcow2")+".build.log", "log")
	writeFile(t, strings.TrimSuffix(g3, ".qcow2")+".build.log", "log")
	failed := filepath.Join(b, "minimal-fedcba9876543210.build.log") // build that never produced a base
	building := r.basePath(m.P, 4) + ".building"                     // possibly still running
	writeFile(t, failed, "log")
	writeFile(t, building, "x")
	oldTime := time.Now().Add(-gcTempAge - time.Hour)
	os.Chtimes(failed, oldTime, oldTime)
	part := filepath.Join(m.P.Images(), "ubuntu.img.part")
	writeFile(t, part, "x")
	os.Chtimes(part, oldTime, oldTime)
	tomb := filepath.Join(m.P.VMs(), ".destroyed-x-1")
	writeFile(t, filepath.Join(tomb, "disk.qcow2"), "x")
	os.MkdirAll(m.dir("user"), 0o700)
	if err := writeJSON(filepath.Join(m.dir("user"), stateFile), State{Name: "user", Base: g2}); err != nil {
		t.Fatal(err)
	}

	names := func(items []GCItem) string {
		var n []string
		for _, it := range items {
			rel, _ := filepath.Rel(m.P.Data, it.Path)
			n = append(n, rel)
		}
		return strings.Join(n, " ")
	}
	want := "bases/minimal-0123456789abcdef.qcow2 bases/minimal-fedcba9876543210.build.log bases/" + filepath.Base(g1) +
		" bases/" + strings.TrimSuffix(filepath.Base(g1), ".qcow2") + ".build.log images/ubuntu.img.part vms/.destroyed-x-1"
	items, err := m.GC(true)
	if err != nil || names(items) != want {
		t.Fatalf("dry run:\n got %s\nwant %s (%v)", names(items), want, err)
	}
	if _, err := os.Stat(g1); err != nil {
		t.Fatal("dry run removed files")
	}
	if items, err = m.GC(false); err != nil || names(items) != want {
		t.Fatalf("gc: %s %v", names(items), err)
	}
	for _, p := range []string{g1, stale, failed, part, tomb} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s not removed", p)
		}
	}
	for _, p := range []string{g2, g3, building, strings.TrimSuffix(g3, ".qcow2") + ".build.log"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed", p)
		}
	}
	if r.BasePath(m.P) != g3 {
		t.Errorf("current base now %s", r.BasePath(m.P))
	}
}

func TestGCKeepsInUseAndCurrent(t *testing.T) {
	needQemuImg(t)
	m := testManager(t)
	m.Cfg.Images = map[string]image.Spec{"mydistro": {Name: "mydistro", URL: "https://example.com/x.img", SHA256: strings.Repeat("a", 64)}}
	writeFile(t, filepath.Join(m.P.Templates(), "distros.yaml"), "mydistro: { distro: mydistro }\n")
	custom, err := m.Resolve("minimal,mydistro")
	if err != nil {
		t.Fatal(err)
	}
	def, _ := m.Resolve("minimal")
	writeFile(t, custom.basePath(m.P, 1), "base") // current for a config-defined distro
	custom.recordSpec(m.P)
	def.recordSpec(m.P)
	// A base only the disk refers to: reset or rebase interrupted before its state was saved.
	byDisk := def.basePath(m.P, 1)
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", byDisk, "1M").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	writeFile(t, def.basePath(m.P, 2), "base")
	os.MkdirAll(m.dir("v1"), 0o700)
	if err := qemuCreateOverlay(m.vm("v1").path(diskFile), byDisk); err != nil {
		t.Fatal(err)
	}
	writeJSON(filepath.Join(m.dir("v1"), stateFile), State{Name: "v1", Base: def.basePath(m.P, 2)})
	// Leftovers old enough to be dead, and one interrupted reset.
	oldTime := time.Now().Add(-gcTempAge - time.Hour)
	stem := strings.TrimSuffix(def.basePath(m.P, 3), ".qcow2")
	var dead []string
	for _, p := range []string{def.basePath(m.P, 3) + ".building", def.basePath(m.P, 3) + ".seed.iso", stem + ".egress", m.vm("v1").path(diskFile + ".reset")} {
		writeFile(t, p, "x")
		os.Chtimes(p, oldTime, oldTime)
		dead = append(dead, p)
	}
	items, err := m.GC(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{custom.basePath(m.P, 1), byDisk, def.basePath(m.P, 2)} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed", filepath.Base(p))
		}
	}
	for _, p := range dead {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s kept", p)
		}
	}
	if len(items) != len(dead) {
		t.Errorf("removed %+v", items)
	}

	// An unreadable VM state keeps every base.
	writeFile(t, filepath.Join(m.dir("broken"), stateFile), "{garbage")
	writeFile(t, def.basePath(m.P, 4), "base")
	if items, _ := m.GC(false); len(items) != 0 {
		t.Errorf("removed with an unreadable VM around: %+v", items)
	}
}

func qemuCreateOverlay(disk, base string) error {
	out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", base, disk).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v %s", err, out)
	}
	return nil
}

func TestRebuildTargetsUseTheBasesDistro(t *testing.T) {
	m := testManager(t)
	m.Cfg.Images = map[string]image.Spec{"mydistro": {Name: "mydistro", URL: "https://example.com/x.img", SHA256: strings.Repeat("a", 64)}}
	writeFile(t, filepath.Join(m.P.Templates(), "distros.yaml"), "mydistro: { distro: mydistro }\n")
	custom, _ := m.Resolve("minimal,mydistro")
	writeFile(t, custom.basePath(m.P, 2), "base")
	custom.recordSpec(m.P)
	writeFile(t, filepath.Join(m.P.Bases(), "minimal-0123456789abcdef.qcow2"), "old version")
	got, err := m.RebuildTargets()
	if err != nil || len(got) != 1 || got[0] != "minimal,mydistro" {
		t.Errorf("targets %v %v", got, err)
	}
}

// -t lists that give the same software share one base, named after the
// distro (QEMU splits options on a -t's commas), and rebuild --all covers each.
func TestSameContentSharesABase(t *testing.T) {
	m := testManager(t)
	a, err := m.Resolve("minimal")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Resolve("minimal,large,ssh-host")
	if err != nil {
		t.Fatal(err)
	}
	if a.basePath(m.P, 1) != b.basePath(m.P, 1) || !strings.HasPrefix(filepath.Base(a.basePath(m.P, 1)), "ubuntu-24.04-") {
		t.Fatalf("bases %s %s", a.basePath(m.P, 1), b.basePath(m.P, 1))
	}
	writeFile(t, a.basePath(m.P, 1), "base")
	a.recordSpec(m.P)
	b.recordSpec(m.P)
	a.recordSpec(m.P)
	got, err := m.RebuildTargets()
	if err != nil || !slices.Equal(got, []string{"minimal", "minimal,large,ssh-host"}) {
		t.Errorf("targets %v %v", got, err)
	}
	// A spec that now gives other content no longer counts for this base.
	writeFile(t, filepath.Join(m.P.Templates(), "resources.yaml"), "large: { resources: { cpus: 8, memory: 16G, disk: 80G }, description: x }\n")
	writeFile(t, filepath.Join(m.P.Templates(), "profiles.yaml"), "")
	if items, err := m.GC(false); err != nil || len(items) != 0 {
		t.Errorf("gc of a current base: %+v %v", items, err)
	}
	// A template file that doesn't load keeps every base.
	writeFile(t, filepath.Join(m.P.Templates(), "tools.yaml"), "x: [")
	os.Remove(a.specsPath(m.P))
	if items, err := m.GC(false); err != nil || len(items) != 0 {
		t.Errorf("gc with broken templates: %+v %v", items, err)
	}
	os.Remove(filepath.Join(m.P.Templates(), "tools.yaml"))
	if items, err := m.GC(false); err != nil || len(items) != 1 {
		t.Errorf("gc of a base no spec gives: %+v %v", items, err)
	}
}

func TestBusyTaskRefusesReset(t *testing.T) {
	m := testManager(t)
	s, _ := offlineVM(t, m, "v1")
	tk := &task.Task{ID: task.NewID(), State: task.Running, Created: time.Now()}
	tk.PID = fakeSupervisor(t, tk.ID)
	m.Tasks().Create(tk, "p")
	s.Task = tk.ID
	m.save(s)
	ctx := context.Background()
	if err := m.Reset(ctx, "v1", false); err == nil || !strings.Contains(err.Error(), "cancel it first") {
		t.Errorf("reset under a running task: %v", err)
	}
	if err := m.Rebase(ctx, "v1"); err == nil || !strings.Contains(err.Error(), "cancel it first") {
		t.Errorf("rebase under a running task: %v", err)
	}
}

func TestSnapshotMetaAndDiskOnlyRestore(t *testing.T) {
	m := testManager(t)
	offlineVM(t, m, "v1")
	ctx := context.Background()
	if _, err := m.Snapshot(ctx, "v1", "s1", false); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(ctx, "v1", "7", false); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("numeric name: %v", err)
	}
	if err := m.DeleteSnapshot("v1", "a b"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("bad name: %v", err)
	}
	m.setSnapMeta("v1", "s1", &snapMeta{QEMU: "1.0.0", Machine: "pc-q35-1.0"})
	if err := m.DeleteSnapshot("v1", "s1"); err != nil {
		t.Fatal(err)
	}
	if md := m.snapMetas("v1"); len(md) != 0 {
		t.Errorf("metadata kept: %v", md)
	}
	if err := m.Reset(ctx, "v1", false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(m.vm("v1").path(diskFile)); st.Mode().Perm() != 0o600 {
		t.Errorf("reset disk mode %v", st.Mode().Perm())
	}
}

// reset --latest refuses to move a VM to another distro when the distros
// default changes.
func TestResetLatestKeepsTheDistro(t *testing.T) {
	m := testManager(t)
	s, _ := offlineVM(t, m, "v1")
	writeFile(t, filepath.Join(m.P.Templates(), "distros.yaml"), "default: debian-13\n")
	err := m.Reset(context.Background(), "v1", true)
	if err == nil || !strings.Contains(err.Error(), "can't change distro") {
		t.Errorf("reset --latest: %v", err)
	}
	if ns, _ := m.Load("v1"); ns.Base != s.Base {
		t.Errorf("the VM moved to %s", ns.Base)
	}
}

// gc removes interrupted writes of a base's list once they're old.
func TestGCRemovesOldSpecsTemps(t *testing.T) {
	m := testManager(t)
	tmp := filepath.Join(m.P.Bases(), ".ubuntu-24.04-0123456789abcdef.specs.123")
	writeFile(t, tmp, "x")
	if err := os.Chtimes(tmp, time.Now().Add(-2*gcTempAge), time.Now().Add(-2*gcTempAge)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GC(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("an old .specs temp file survived gc")
	}
}

// gc keeps a base's list while its first build runs, so the base stays current.
func TestGCDuringAFirstBuild(t *testing.T) {
	m := testManager(t)
	r, err := m.Resolve("minimal")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := fsutil.Lock(strings.TrimSuffix(r.basePath(m.P, 1), ".qcow2") + ".build.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.recordSpec(m.P); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GC(false); err != nil {
		t.Fatal(err)
	}
	unlock()
	writeFile(t, r.basePath(m.P, 1), "base") // the build lands
	if _, err := m.GC(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.basePath(m.P, 1)); err != nil {
		t.Error("gc removed the base a build had just made")
	}
}

// A file that parses but doesn't load keeps the bases, like one that doesn't
// parse; a spec naming a section that's gone frees its base.
func TestGCKeepsBasesWhileTemplatesAreBroken(t *testing.T) {
	m := testManager(t)
	r, err := m.Resolve("minimal")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.recordSpec(m.P); err != nil {
		t.Fatal(err)
	}
	writeFile(t, r.basePath(m.P, 1), "base")
	tpl := filepath.Join(m.P.Templates(), "distros.yaml")
	writeFile(t, tpl, "default: [ubuntu-24.04, debian-13]\n")
	if _, err := m.GC(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.specsPath(m.P)); err != nil {
		t.Fatal("gc removed a base's list while distros.yaml was broken")
	}
	os.Remove(tpl)
	writeFile(t, r.specsPath(m.P), "gone-section\n")
	if _, err := m.GC(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.basePath(m.P, 1)); err == nil {
		t.Error("gc kept a base only a spec naming a missing section used")
	}
}
