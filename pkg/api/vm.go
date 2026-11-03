package api

import (
	"net/url"
	"strings"
	"time"
)

// PathVMs is GET: every VM, as []VMInfo by name, warm pool VMs included.
// It is also POST: create a VM from a CreateVMRequest, as catchpen up does,
// without starting it (a job).
const PathVMs = Prefix + "/vms"

// PathVM is GET: one VM's VMInfo. The name is one escaped path segment,
// dots included, so a router can't take "." or ".." for a directory.
func PathVM(name string) string {
	return PathVMs + "/" + strings.ReplaceAll(url.PathEscape(name), ".", "%2E")
}

// PathSnapshots is GET: a VM's snapshots, as []SnapshotInfo, oldest first.
// It is also POST, with a CreateSnapshotRequest: save a snapshot, as catchpen
// snapshot does (with its memory, of a running VM, unless DiskOnly), as a
// job; a name the VM has is refused with CodeConflict.
func PathSnapshots(name string) string { return PathVM(name) + "/snapshots" }

// A VM's state.
const (
	VMRunning = "running"
	VMPaused  = "paused" // by a canary hit: it stays so until resumed
	VMStopped = "stopped"
)

// VMInfo is what the API tells of a VM. It never carries what the VM's
// state on the host keeps from others: its environment, its forges'
// credentials, its agents' settings or its canaries.
type VMInfo struct {
	Name      string     `json:"name"`
	State     string     `json:"state"`          // VMRunning, VMPaused or VMStopped
	Pool      bool       `json:"pool"`           // a warm pool VM, not claimed yet
	Ephemeral bool       `json:"ephemeral"`      // destroyed when its session ends
	Task      string     `json:"task,omitempty"` // the id of the headless task it runs
	StaleBase bool       `json:"stale_base"`     // its template's base has changed since it was made
	CanaryHit bool       `json:"canary_hit"`     // its canaries were found in use
	Template  string     `json:"template"`       // as given to create
	Distro    string     `json:"distro"`
	User      string     `json:"user"` // the guest user
	CPUs      int        `json:"cpus"`
	MemoryMB  int64      `json:"memory_mb"`
	DiskMB    int64      `json:"disk_mb"`
	Egress    string     `json:"egress"` // its egress mode
	Docker    bool       `json:"docker"`
	IdleStop  string     `json:"idle_stop,omitempty"` // a duration
	Created   time.Time  `json:"created"`
	Booted    bool       `json:"booted"`             // its first boot finished
	SSHPort   int        `json:"ssh_port,omitempty"` // on the host's 127.0.0.1, while it runs
	Ports     []PortInfo `json:"ports"`              // the ports its template forwards (Host 0: any free port)
	Forwards  []PortInfo `json:"forwards"`           // those QEMU forwards now, while it runs
}

// PortInfo is a port QEMU forwards from the host's 127.0.0.1 to the guest
// (not the forwards of catchpen forward, which are ssh sessions).
type PortInfo struct {
	Host  int `json:"host"`
	Guest int `json:"guest"`
}

// SnapshotInfo is one of a VM's snapshots.
type SnapshotInfo struct {
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	MemoryMB int64     `json:"memory_mb"` // the saved memory; 0: a disk-only snapshot
}
