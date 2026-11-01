package vm

import (
	"fmt"
	"os"

	"github.com/blackshieldpt/catchpen/internal/qemu"
)

// Loss is what Reset, Rebase or Destroy would delete from a VM, for a
// prompt or an API caller to show before going ahead. It changes nothing.
type Loss struct {
	Disk      bool     // everything on the VM's disk, if it has one (Reset, Destroy)
	Snapshots []string // the snapshots deleted, oldest first
	// Running: the VM runs, and is stopped: Reset and Rebase start it again,
	// Destroy forces it off. A paused VM is forced off by all three, losing
	// its frozen memory, and Reset and Rebase start it unpaused.
	Running bool
}

// RebaseLoss is what Rebase would delete: the VM's snapshots. Like Rebase,
// it fails on a disk it can't read.
func (m *Manager) RebaseLoss(name string) (Loss, error) {
	ss, err := m.Snapshots(name)
	if err != nil {
		return Loss{}, err
	}
	l := Loss{Snapshots: snapNames(ss)}
	_, l.Running = m.Running(name)
	return l, nil
}

// ResetLoss is what Reset would delete: everything on the VM's disk, its
// snapshots included. Like Reset, it needs the state file but not the disk.
func (m *Manager) ResetLoss(name string) (Loss, error) {
	if _, err := m.Load(name); err != nil {
		return Loss{}, err
	}
	return m.wholeDisk(name), nil
}

// DestroyLoss is what Destroy would delete: the whole VM, its disk and
// snapshots included. Like Destroy, it works on a VM whose state file is
// missing or corrupt.
func (m *Manager) DestroyLoss(name string) (Loss, error) {
	if err := ValidName(name); err != nil {
		return Loss{}, err
	}
	if _, err := os.Stat(m.dir(name)); err != nil {
		return Loss{}, fmt.Errorf("no VM named %q (see: catchpen ls)", name)
	}
	return m.wholeDisk(name), nil
}

// wholeDisk is the loss of the VM's disk. Its snapshots are those qemu-img
// can read: none when the disk is missing or unreadable.
func (m *Manager) wholeDisk(name string) Loss {
	l := Loss{Disk: true}
	if ss, err := qemu.Snapshots(m.vm(name).path(diskFile)); err == nil {
		l.Snapshots = snapNames(ss)
	}
	_, l.Running = m.Running(name)
	return l
}

func snapNames(ss []qemu.Snapshot) []string {
	var names []string
	for _, s := range ss {
		names = append(names, s.Name)
	}
	return names
}
