# Snapshots, reset and base updates

```
curral snapshot dev [before-upgrade]    # running: memory and disk; --disk-only for the disk
curral snapshots dev
curral restore dev before-upgrade       # --disk-only: restore only the disk, booting afresh
curral rmsnap dev before-upgrade
curral template rebuild --all           # fresh cloud image and packages, as a new base
curral reset dev [--latest]             # fresh disk, first boot again (--latest: new base)
curral rebase dev                       # onto the current base, contents unchanged
curral gc [--dry-run]
```

## Snapshots

Snapshots are stored inside the VM's disk. A snapshot of a running VM saves its memory
too (the VM pauses while it's written), so restoring it resumes the VM where it was,
starting it if stopped. Afterwards curral sets the guest clock and pushes credentials
again. `--disk-only` saves just the disk, without pausing for memory. Restoring a
disk-only snapshot needs the VM off, so a running VM is stopped and started again.

Memory is only loaded into the QEMU version and machine type it was saved from. After a
QEMU upgrade, `restore --disk-only` still restores such a snapshot's disk.

Restore and reset copy the agents' transcripts out first. Restore, reset and rebase refuse while the VM runs a
task. Memory snapshots hold whatever was in the guest's memory, credentials included.

## Base generations

A rebuild never replaces a base in use. It adds a generation
(`<distro>-<hash>.2.qcow2`) that new VMs use; the one it supersedes is removed if no VM
uses it. `curral ls` marks VMs on an older generation `stale-base`.

To move a VM to new software:

- **`reset --latest`** gives the VM a fresh disk on the new base. Everything on its disk
  is lost, snapshots included; keys, forges and settings stay, and repos are cloned
  again. This is how a VM gets new packages.
- **`reset`** does the same on the VM's current base.
- **`rebase`** copies what differs into the VM's disk, so the guest sees exactly the same
  files, without new packages, and stops it using the old base, which `gc` can then
  remove. It deletes the VM's snapshots.

The daemon can rebuild bases on a schedule (`rebuild:` in config.yaml); see
[deployment.md](deployment.md#scheduled-rebuilds).

## Garbage collection

`curral gc` removes:

- bases no VM uses (the current base of each `-t` spec stays);
- leftovers of interrupted builds, downloads, resets and destroys;
- package cache objects past `cache_max`.

Recordings, transcripts, tasks and the audit log are never removed. `--dry-run` lists
what would go and how much it would free.
