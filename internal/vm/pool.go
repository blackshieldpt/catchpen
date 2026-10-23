package vm

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/egress"
	"github.com/blackshieldpt/curral/internal/fsutil"
	"github.com/blackshieldpt/curral/internal/qemu"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
)

// A warm pool VM has been through first boot with its own keys, but has no
// repos, agents or credentials. It waits stopped, with its memory saved in
// snapshot poolSnap: a claim renames it and resumes it from there. (A running
// VM can't be renamed: QEMU's command line names its files.)
const (
	poolSnap     = "curral-pool"
	poolLock     = ".pool.lock"      // in vms/: claims and removals
	poolFillLock = ".pool-fill.lock" // in vms/: fills, recycling and draining
)

// ErrFillRunning means another process is filling or draining the pool.
var ErrFillRunning = errors.New("another curral is filling or draining the pool")

// poolKey is what a pool VM must match to be claimed: what its disk, seed
// and QEMU command line were made with.
type poolKey struct {
	Template, Distro, Base string
	CPUs                   int
	MemoryMB, DiskMB       int64
	Egress, User           string
	Docker                 bool
	Cache                  string // the cache kinds, comma separated
	Canaries               string // the canary kinds, comma separated
}

func keyOf(s *State) poolKey {
	return poolKey{s.Template, s.Distro, s.Base, s.CPUs, s.MemoryMB, s.DiskMB, s.Egress, s.User, s.Docker, strings.Join(s.Cache, ","), strings.Join(s.Canaries, ",")}
}

// poolKeyFor is the key of a VM created now from r without overrides.
func (m *Manager) poolKeyFor(r *Resolved) (poolKey, error) {
	res := m.Cfg.Defaults.Resources.Override(r.T.Resources)
	memMB, err := template.ParseSize(res.Memory)
	if err != nil {
		return poolKey{}, err
	}
	diskMB, err := template.ParseSize(res.Disk)
	if err != nil {
		return poolKey{}, err
	}
	base := r.BasePath(m.P)
	if mb, err := qemu.VirtualSizeMB(base); err == nil {
		diskMB = max(diskMB, mb)
	}
	return poolKey{r.T.Name, r.Distro.Name, base, res.CPUs, memMB, diskMB,
		cmp.Or(r.T.Network.Egress, egress.ModeAllowlist), r.User, r.T.HasDocker(), strings.Join(vmCaches(r.T), ","),
		strings.Join(canaryKinds(r.T), ",")}, nil
}

// PoolVMs lists the warm pool: ready VMs and ones being filled.
func (m *Manager) PoolVMs() ([]Info, error) {
	vms, err := m.List()
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, v := range vms {
		if v.Pool {
			out = append(out, v)
		}
	}
	return out, nil
}

// Ready reports whether a pool VM can be claimed: never one whose canaries
// were used while it warmed.
func (i Info) Ready() bool { return i.Pool && i.Resume == poolSnap && !i.Running && !i.Hit }

// claimPool moves a ready pool VM matching key into name's directory, which
// the caller created empty, and returns its state; nil if none matches.
func (m *Manager) claimPool(name string, key poolKey) (*State, error) {
	unlock, err := fsutil.Lock(filepath.Join(m.P.VMs(), poolLock))
	if err != nil {
		return nil, err
	}
	defer unlock()
	vms, err := m.PoolVMs()
	if err != nil {
		return nil, err
	}
	for _, v := range vms {
		if !v.Ready() || keyOf(&v.State) != key {
			continue
		}
		_, vunlock, ok, err := m.vm(v.Name).tryLock()
		if err != nil || !ok {
			continue
		}
		s, err := m.Load(v.Name)
		_, running := m.Running(v.Name)
		// A canary hit while warming: not handed out.
		if err != nil || !s.Pool || s.Resume != poolSnap || keyOf(s) != key || running || m.vm(v.Name).hit() {
			vunlock()
			continue
		}
		// Replaces the empty directory, so name stays reserved throughout
		// (os.Rename refuses an existing directory).
		err = syscall.Rename(m.dir(v.Name), m.dir(name))
		vunlock()
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil, nil // something (a racing rm's lock file) is in name's directory
		}
		if err != nil {
			return nil, fmt.Errorf("claiming pool VM %s: %w", v.Name, err)
		}
		// No longer a pool VM, even before Create saves the full state.
		ns := *s
		ns.Name, ns.Pool = name, false
		if err := m.save(&ns); err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, nil
}

// takeResume decides how a claimed pool VM starts, with its lock held: from
// its saved memory when this QEMU can load it, else a normal boot. Later
// starts boot normally either way.
func (m *Manager) takeResume(_ heldVM, s *State) (*resume, error) {
	snap := s.Resume
	s.Resume = ""
	if err := m.save(s); err != nil {
		return nil, err
	}
	meta, ok := m.snapMetas(s.Name)[snap]
	if v, err := qemuVersion(); ok && err == nil && v == meta.QEMU {
		return &resume{snap: snap, machine: meta.Machine}, nil
	}
	m.logf("%s's saved memory is from another QEMU version: booting it instead", s.Name)
	if err := qemu.SnapshotOffline("-d", snap, m.vm(s.Name).path(diskFile)); err != nil {
		m.logf("warning: removing snapshot %s of %s: %v", snap, s.Name, err)
	}
	_ = m.setSnapMeta(s.Name, snap, nil)
	return nil, nil
}

// claimed renames the guest of a pool VM resumed as name, and drops the
// snapshot it was resumed from. (A later boot gets the name from the seed.)
func (m *Manager) claimed(ctx context.Context, c *ssh.Client, name, snap string) {
	cmd := fmt.Sprintf(`old=$(hostname) && sudo -n hostnamectl set-hostname %[1]s && sudo -n sed -i "s/\<$old\>/%[1]s/g" /etc/hosts`, name)
	if out, err := sshx.Run(ctx, c, cmd, nil); err != nil {
		m.logf("warning: setting the hostname of %s: %v %s", name, err, out)
	}
	if err := m.DeleteSnapshot(name, snap); err != nil {
		m.logf("warning: removing the pool snapshot of %s: %v", name, err)
	}
}

type poolWant struct {
	entry config.PoolEntry
	key   poolKey
}

// poolWants resolves the configured pool entries.
func (m *Manager) poolWants() ([]poolWant, error) {
	var out []poolWant
	var errs []error
	seen := map[poolKey]bool{}
	for _, e := range m.Cfg.Pool {
		r, err := m.Resolve(e.Template, e.Distro)
		if err == nil {
			var k poolKey
			if k, err = m.poolKeyFor(r); err == nil && seen[k] {
				err = errors.New("another pool entry makes the same VMs")
			}
			if err == nil {
				seen[k] = true
				out = append(out, poolWant{e, k})
				continue
			}
		}
		errs = append(errs, fmt.Errorf("pool template %s: %w", e.Template, err))
	}
	return out, errors.Join(errs...)
}

// FillPool removes pool VMs nothing wants any more (an old base or template
// version, beyond the configured size, a fill that died), then creates the
// missing ones one at a time. It returns how many it created.
func (m *Manager) FillPool(ctx context.Context) (int, error) {
	unlock, err := m.fillLock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	wants, werr := m.poolWants()
	if err := m.recyclePool(wants, werr == nil); err != nil {
		return 0, err
	}
	vms, err := m.PoolVMs()
	if err != nil {
		return 0, err
	}
	created := 0
	errs := []error{werr}
	for _, w := range wants {
		have := 0
		for _, v := range vms {
			if v.Ready() && keyOf(&v.State) == w.key {
				have++
			}
		}
		for ; have < w.entry.Size; have++ {
			if err := m.fillOne(ctx, w.entry); err != nil {
				errs = append(errs, fmt.Errorf("pool template %s: %w", w.entry.Template, err))
				break
			}
			created++
		}
	}
	return created, errors.Join(errs...)
}

func (m *Manager) fillLock() (func(), error) {
	if err := os.MkdirAll(m.P.VMs(), 0o700); err != nil {
		return nil, err
	}
	unlock, ok, err := fsutil.TryLock(filepath.Join(m.P.VMs(), poolFillLock))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrFillRunning
	}
	return unlock, nil
}

// recyclePool destroys the pool VMs wants doesn't keep. With complete false
// (some entries didn't resolve) only VMs that aren't ready are removed.
func (m *Manager) recyclePool(wants []poolWant, complete bool) error {
	unlock, err := fsutil.Lock(filepath.Join(m.P.VMs(), poolLock))
	if err != nil {
		return err
	}
	defer unlock()
	vms, err := m.PoolVMs()
	if err != nil {
		return err
	}
	left := map[poolKey]int{}
	for _, w := range wants {
		left[w.key] += w.entry.Size
	}
	var errs []error
	for _, v := range vms {
		k := keyOf(&v.State)
		if v.Hit {
			continue // kept for inspection, and not counted (curral pool ls)
		}
		if v.Ready() && (left[k] > 0 || !complete) {
			left[k]--
			continue
		}
		m.logf("removing pool VM %s", v.Name)
		if err := m.destroy(v.Name, false); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fillOne creates, boots and saves one pool VM.
func (m *Manager) fillOne(ctx context.Context, e config.PoolEntry) (err error) {
	rnd := make([]byte, 4)
	_, _ = rand.Read(rnd)
	name := "pool-" + hex.EncodeToString(rnd)
	m.logf("filling the pool: %s (template %s)", name, e.Template)
	// AllowOpen: listing the template in pool: is the consent, and a pool VM
	// has nothing to leak; claiming it still needs --allow-open.
	if _, err := m.Create(ctx, name, CreateOpts{Template: e.Template, Distro: e.Distro, AllowOpen: true, pool: true}); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = m.destroy(name, false)
		}
	}()
	if _, err := m.start(ctx, name, nil); err != nil {
		return err
	}
	if _, err := m.Snapshot(ctx, name, poolSnap, false); err != nil {
		return err
	}
	held, unlock, err := m.vm(name).lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.stopLocked(held, true); err != nil { // its memory is saved: no shutdown
		return err
	}
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	s.Resume = poolSnap
	if err := m.save(s); err != nil {
		return err
	}
	m.audit(audit.PoolFill, name, map[string]any{"template": s.Template, "distro": s.Distro})
	return nil
}

// DrainPool destroys every pool VM and returns how many.
func (m *Manager) DrainPool() (int, error) {
	unlock, err := m.fillLock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	vms, err := m.PoolVMs()
	if err != nil {
		return 0, err
	}
	if err := m.recyclePool(nil, true); err != nil {
		return 0, err
	}
	var errs []error
	for _, v := range vms {
		if v.Hit { // recycling keeps these for inspection; draining doesn't
			m.logf("removing pool VM %s (its canaries were used)", v.Name)
			errs = append(errs, m.destroy(v.Name, false))
		}
	}
	return len(vms), errors.Join(errs...)
}
