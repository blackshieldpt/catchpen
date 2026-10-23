package vm

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/fsutil"
	"github.com/blackshieldpt/curral/internal/qemu"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/task"
)

const (
	savevmTimeout = 15 * time.Minute
	snapMetaFile  = "snapshots.json"
)

// Snapshot names are passed to HMP and qemu-img, which also match ids:
// all-digit names are refused so a name never means another snapshot's id.
var snapNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func validSnap(n string) error {
	if _, err := strconv.Atoi(n); err == nil || !snapNameRe.MatchString(n) {
		return fmt.Errorf("invalid snapshot name %q (letters, digits, . _ -, not only digits, at most 64)", n)
	}
	return nil
}

// snapMeta records what a memory snapshot needs to be loaded again: QEMU
// only loads saved memory into the same machine type.
type snapMeta struct {
	QEMU    string `json:"qemu"`
	Machine string `json:"machine"`
}

func (m *Manager) snapMetas(name string) map[string]snapMeta {
	md := map[string]snapMeta{}
	if b, err := os.ReadFile(m.vm(name).path(snapMetaFile)); err == nil {
		_ = json.Unmarshal(b, &md)
	}
	return md
}

func (m *Manager) setSnapMeta(name, snap string, v *snapMeta) error {
	md := m.snapMetas(name)
	if v == nil {
		delete(md, snap)
	} else {
		md[snap] = *v
	}
	return writeJSON(m.vm(name).path(snapMetaFile), md)
}

// Snapshots lists a VM's snapshots, oldest first.
func (m *Manager) Snapshots(name string) ([]qemu.Snapshot, error) {
	if _, err := m.Load(name); err != nil {
		return nil, err
	}
	return qemu.Snapshots(m.vm(name).path(diskFile))
}

func (m *Manager) findSnap(name, snap string) (*qemu.Snapshot, error) {
	if err := validSnap(snap); err != nil {
		return nil, err
	}
	ss, err := m.Snapshots(name)
	if err != nil {
		return nil, err
	}
	for i := range ss {
		if ss[i].Name == snap {
			return &ss[i], nil
		}
	}
	return nil, fmt.Errorf("%s has no snapshot %q (see: curral snapshots %s)", name, snap, name)
}

// qmp runs fn on the VM's QMP socket.
func (m *Manager) qmp(name string, fn func(q *qemu.QMP) error) error {
	q, err := qemu.DialQMP(m.vm(name).path(qmpSock), 5*time.Second)
	if err != nil {
		return err
	}
	defer q.Close()
	return fn(q)
}

// busyTask refuses changes under a VM's running headless task.
// Tasks is the task store (internal/runner runs them).
func (m *Manager) Tasks() task.Store { return task.Store{Dir: m.P.Tasks()} }

func (m *Manager) busyTask(s *State) error {
	if s.Task == "" {
		return nil
	}
	t, err := m.Tasks().Load(s.Task)
	if err == nil && !task.Final(t.State) && t.Alive() {
		return fmt.Errorf("%s runs task %s; cancel it first (curral task cancel %s)", s.Name, s.Task, s.Task)
	}
	return nil
}

// Snapshot saves a snapshot inside the VM's disk: of a running VM with its
// RAM (processes resume where they were) unless diskOnly, of a stopped VM
// the disk. snap "" names it by the time.
func (m *Manager) Snapshot(ctx context.Context, name, snap string, diskOnly bool) (string, error) {
	if snap == "" {
		snap = time.Now().UTC().Format("20060102-150405")
	}
	if err := validSnap(snap); err != nil {
		return "", err
	}
	_, unlock, err := m.vm(name).lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := m.findSnap(name, snap); err == nil {
		return "", fmt.Errorf("%s already has a snapshot %q", name, snap)
	}
	ram := false
	_, running := m.Running(name)
	if running {
		// Flush the guest's page cache, so the snapshot's disk alone (restore
		// --disk-only) is consistent too: files written shortly before, such
		// as first boot's ssh host keys, are otherwise only in its memory.
		m.withGuest(ctx, name, func(c *ssh.Client) { _, _ = sshx.Run(ctx, c, "sync", nil) })
	}
	if !running {
		err = qemu.SnapshotOffline("-c", snap, m.vm(name).path(diskFile))
	} else if diskOnly {
		err = m.qmp(name, func(q *qemu.QMP) error {
			dev, err := q.BlockDevice(m.vm(name).path(diskFile))
			if err != nil {
				return err
			}
			_, err = q.Execute("blockdev-snapshot-internal-sync", map[string]any{"device": dev, "name": snap})
			return err
		})
	} else {
		ram = true
		m.logf("saving %s with its memory (the VM pauses meanwhile)", name)
		var meta snapMeta
		err = m.qmp(name, func(q *qemu.QMP) error {
			var err error
			if meta.QEMU, err = q.Version(); err != nil {
				return err
			}
			if meta.Machine, err = q.Machine(); err != nil {
				return err
			}
			out, err := q.HMP("savevm "+snap, savevmTimeout)
			if err != nil {
				return err
			}
			// Output may be a warning; the snapshot existing is what counts.
			sn, ferr := m.findSnap(name, snap)
			if ferr != nil || sn.VMStateMB == 0 {
				return fmt.Errorf("savevm: %s", cmp.Or(out, "no snapshot was written"))
			}
			if out != "" {
				m.logf("warning: savevm: %s", out)
			}
			return nil
		})
		if err == nil {
			err = m.setSnapMeta(name, snap, &meta)
			// The guest's clock stood still while it was paused.
			m.withGuest(ctx, name, func(c *ssh.Client) { m.syncClock(ctx, c) })
		}
	}
	if err != nil {
		return "", fmt.Errorf("snapshot of %s: %w", name, err)
	}
	m.audit(audit.VMSnapshot, name, map[string]any{"snapshot": snap, "ram": ram})
	return snap, nil
}

// Restore returns the VM to a snapshot. A snapshot with RAM resumes the VM
// as it was (starting it if stopped) unless diskOnly; a disk-only restore
// needs the VM stopped, so a running VM is stopped and started again.
// Credentials are pushed again afterwards: restored memory may hold ones
// since rotated.
func (m *Manager) Restore(ctx context.Context, name, snap string, diskOnly bool) error {
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	if err := m.busyTask(s); err != nil {
		return err
	}
	if s.Pool {
		return fmt.Errorf("%s is a warm pool VM", name)
	}
	sn, err := m.findSnap(name, snap)
	if err != nil {
		return err
	}
	m.saveTranscripts(name) // what the agents wrote since the snapshot
	if sn.VMStateMB == 0 || diskOnly {
		return m.whileStopped(ctx, name, func() error {
			return qemu.SnapshotOffline("-a", snap, m.vm(name).path(diskFile))
		}, func() { m.audit(audit.VMRestore, name, map[string]any{"snapshot": snap, "ram": false}) })
	}
	meta, ok := m.snapMetas(name)[snap]
	if !ok {
		return fmt.Errorf("snapshot %q wasn't saved by curral (no record of its machine type); restore --disk-only uses just its disk", snap)
	}
	held, unlock, err := m.vm(name).lock()
	if err != nil {
		return err
	}
	if _, running := m.Running(name); !running {
		unlock()
		if v, err := qemuVersion(); err == nil && v != meta.QEMU {
			m.logf("warning: %s was saved by QEMU %s, this is %s; if it fails to load, restore --disk-only", snap, meta.QEMU, v)
		}
		if _, err := m.start(ctx, name, &resume{snap: snap, machine: meta.Machine}); err != nil {
			return fmt.Errorf("resuming %s from %s: %w (restore --disk-only uses just its disk)", name, snap, err)
		}
		m.audit(audit.VMRestore, name, map[string]any{"snapshot": snap, "ram": true})
		return nil
	}
	err = m.qmp(name, func(q *qemu.QMP) error {
		v, err := q.Version()
		if err != nil {
			return err
		}
		mt, err := q.Machine()
		if err != nil {
			return err
		}
		if v != meta.QEMU || mt != meta.Machine {
			return fmt.Errorf("it was saved by QEMU %s (%s), the VM runs %s (%s): stop the VM and restore again, or restore --disk-only", meta.QEMU, meta.Machine, v, mt)
		}
		out, err := q.HMP("loadvm "+snap, savevmTimeout)
		if err != nil {
			return err
		}
		// loadvm leaves the VM paused when loading fails; a running VM is
		// what counts, output may be a warning.
		if st, err := q.Status(); err != nil || st != "running" {
			return fmt.Errorf("loadvm: %s", cmp.Or(out, "the VM did not resume"))
		}
		if out != "" {
			m.logf("warning: loadvm: %s", out)
		}
		return nil
	})
	if err != nil {
		if m.isPaused(name) { // its memory is half-loaded
			if serr := m.stopLocked(held, true); serr != nil {
				err = errors.Join(err, serr)
			}
			err = fmt.Errorf("%w; the VM was stopped (start it, or restore again)", err)
		}
		unlock()
		return fmt.Errorf("restoring %s: %w", name, err)
	}
	unlock()
	m.audit(audit.VMRestore, name, map[string]any{"snapshot": snap, "ram": true})
	return m.afterResume(ctx, name)
}

func (m *Manager) isPaused(name string) bool {
	st := ""
	_ = m.qmp(name, func(q *qemu.QMP) error { var err error; st, err = q.Status(); return err })
	return st != "" && st != "running"
}

func qemuVersion() (string, error) {
	bin, err := qemu.Binary()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "", err
	}
	if mt := regexp.MustCompile(`version (\d+\.\d+\.\d+)`).FindSubmatch(out); mt != nil {
		return string(mt[1]), nil
	}
	return "", errors.New("unknown QEMU version")
}

// DeleteSnapshot removes a snapshot.
func (m *Manager) DeleteSnapshot(name, snap string) error {
	_, unlock, err := m.vm(name).lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := m.findSnap(name, snap); err != nil {
		return err
	}
	if _, ok := m.Running(name); ok {
		err = m.qmp(name, func(q *qemu.QMP) error {
			out, err := q.HMP("delvm "+snap, savevmTimeout)
			if err == nil {
				if _, ferr := m.findSnap(name, snap); ferr == nil {
					err = fmt.Errorf("delvm: %s", cmp.Or(out, "snapshot still there"))
				}
			}
			return err
		})
	} else {
		err = qemu.SnapshotOffline("-d", snap, m.vm(name).path(diskFile))
	}
	if err != nil {
		return err
	}
	_ = m.setSnapMeta(name, snap, nil)
	m.audit(audit.VMSnapshotRm, name, map[string]any{"snapshot": snap})
	return nil
}

// whileStopped runs fn with the VM stopped and locked; done runs if fn
// succeeded. A VM that was running is started again either way.
func (m *Manager) whileStopped(ctx context.Context, name string, fn func() error, done func()) error {
	held, unlock, err := m.vm(name).lock()
	if err != nil {
		return err
	}
	_, running := m.Running(name)
	if err := m.stopLocked(held, false); err != nil {
		unlock()
		return err
	}
	err = fn()
	unlock()
	if err == nil {
		done()
	}
	if running {
		if _, serr := m.Start(ctx, name); serr != nil {
			return errors.Join(err, serr)
		}
	}
	return err
}

// withGuest runs fn on an ssh connection to the running VM (best effort).
func (m *Manager) withGuest(ctx context.Context, name string, fn func(c *ssh.Client)) {
	if _, err := m.Load(name); err != nil {
		return
	}
	if _, err := m.SSHPort(name); err != nil {
		return
	}
	g, _, err := m.DialGuest(ctx, name)
	if err != nil {
		m.logf("warning: connecting to %s: %v", name, err)
		return
	}
	defer g.Close()
	fn(g.Client)
}

// afterResume fixes up a VM resumed from saved memory: its clock is the
// snapshot's, and its credentials may be stale.
func (m *Manager) afterResume(ctx context.Context, name string) error {
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	port, err := m.SSHPort(name)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	alive := func() bool { _, ok := m.Running(name); return ok }
	g, err := m.waitGuest(ctx, wctx, name, port, s.User, alive)
	if err != nil {
		return err
	}
	defer g.Close()
	c := g.Client
	m.syncClock(ctx, c)
	if err := m.push(ctx, g, s); err != nil {
		return fmt.Errorf("%s was restored, but pushing credentials failed: %w (retry: curral creds push %s)", name, err, name)
	}
	m.logf("%s restored", name)
	return nil
}

// syncClock sets the guest clock to the host's. Guests have no NTP (egress
// is filtered), so after a pause or a resume from a snapshot their clock lags.
func (m *Manager) syncClock(ctx context.Context, c *ssh.Client) {
	cmd := fmt.Sprintf("sudo -n date -u -s @%d >/dev/null", time.Now().Unix())
	if out, err := sshx.Run(ctx, c, cmd, nil); err != nil {
		m.logf("warning: setting the guest clock: %v %s", err, out)
	}
}

// baseFor returns the template's current base, building it if needed.
func (m *Manager) baseFor(ctx context.Context, s *State) (string, error) {
	r, err := m.Resolve(s.Template, s.Distro)
	if err != nil {
		return "", err
	}
	if r.baseGen(m.P) == 0 {
		m.logf("template %s changed since its base was built: building the new base first (this can take a while)", s.Template)
	}
	return m.EnsureBase(ctx, r, false)
}

// holdBase takes a shared lock on base, so gc leaves it alone until a VM's
// state names it; release is safe to call more than once.
func holdBase(base string) (release func(), err error) {
	r, err := fsutil.RLock(base + ".lock")
	if err != nil {
		return nil, err
	}
	return sync.OnceFunc(r), nil
}

// Reset gives the VM a fresh disk on its base (latest: the template's
// current base) and runs first boot again; keys, forges and settings stay,
// repos are cloned again at start. Its snapshots are lost.
func (m *Manager) Reset(ctx context.Context, name string, latest bool) error {
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	if err := m.busyTask(s); err != nil {
		return err
	}
	base := s.Base
	if latest {
		if base, err = m.baseFor(ctx, s); err != nil {
			return err
		}
	}
	release, err := holdBase(base)
	if err != nil {
		return err
	}
	defer release()
	if _, err := os.Stat(base); err != nil {
		return fmt.Errorf("base %s missing: %w", base, err)
	}
	baseMB, err := qemu.VirtualSizeMB(base)
	if err != nil {
		return err
	}
	m.saveTranscripts(name)
	return m.whileStopped(ctx, name, func() error {
		disk := m.vm(name).path(diskFile)
		tmp := disk + ".reset"
		_ = os.Remove(tmp)
		if err := qemu.CreateOverlay(tmp, base, max(s.DiskMB, baseMB)); err != nil {
			return err
		}
		id, err := m.writeSeed(name, "", s.User, s.Egress, s.Docker, s.Cache)
		if err != nil {
			os.Remove(tmp)
			return err
		}
		// State first: gc also keeps a base the disk still uses, so a crash
		// between the two leaves both bases in place.
		ns, err := m.Load(name)
		if err != nil {
			os.Remove(tmp)
			return err
		}
		ns.Base, ns.DiskMB, ns.Booted, ns.Instance, ns.Resume = base, max(s.DiskMB, baseMB), false, id, ""
		if err := m.save(ns); err != nil {
			os.Remove(tmp)
			return err
		}
		release()
		if err := os.Rename(tmp, disk); err != nil {
			return err
		}
		_ = os.Remove(m.vm(name).path(snapMetaFile))
		return nil
	}, func() {
		m.audit(audit.VMReset, name, map[string]any{"base": base})
		m.logf("reset %s on %s", name, base)
	})
}

// Rebase moves the VM's disk onto the template's current base without
// changing what the guest sees, so the old base can be removed. Snapshots
// refer to the old base and are deleted first.
func (m *Manager) Rebase(ctx context.Context, name string) error {
	s, err := m.Load(name)
	if err != nil {
		return err
	}
	if err := m.busyTask(s); err != nil {
		return err
	}
	base, err := m.baseFor(ctx, s)
	if err != nil {
		return err
	}
	if base == s.Base {
		return fmt.Errorf("%s is already on the current base", name)
	}
	release, err := holdBase(base)
	if err != nil {
		return err
	}
	defer release()
	if _, err := os.Stat(s.Base); err != nil {
		return fmt.Errorf("its base %s is missing: %w", s.Base, err)
	}
	return m.whileStopped(ctx, name, func() error {
		disk := m.vm(name).path(diskFile)
		ss, err := qemu.Snapshots(disk)
		if err != nil {
			return err
		}
		for _, sn := range ss {
			if err := qemu.SnapshotOffline("-d", sn.Name, disk); err != nil {
				return err
			}
		}
		_ = os.Remove(m.vm(name).path(snapMetaFile))
		m.logf("rebasing %s onto %s (copies what differs into its disk)", name, base)
		if err := qemu.Rebase(disk, base); err != nil {
			return err
		}
		ns, err := m.Load(name)
		if err != nil {
			return err
		}
		ns.Base, ns.Resume = base, ""
		err = m.save(ns) // if this fails, gc still sees the disk's backing file
		release()
		return err
	}, func() { m.audit(audit.VMRebase, name, map[string]any{"base": base}) })
}
