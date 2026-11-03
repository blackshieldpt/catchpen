# Snapshots, reset and base updates

```
catchpen snapshot dev [before-upgrade]    # running: memory and disk; --disk-only for the disk
catchpen snapshots dev
catchpen restore dev before-upgrade       # --disk-only: restore only the disk, booting afresh
catchpen rmsnap dev before-upgrade
catchpen template rebuild --all           # fresh cloud image and packages, as a new base
catchpen reset dev [--latest]             # fresh disk, first boot again (--latest: new base)
catchpen rebase dev                       # onto the current base, contents unchanged
catchpen gc [--dry-run]
```

## Snapshots

Snapshots are stored inside the VM's disk. A snapshot of a running VM saves its memory
too (the VM pauses while it's written), so restoring it resumes the VM where it was,
starting it if stopped. Afterwards catchpen sets the guest clock and pushes credentials
again. `--disk-only` saves just the disk, without pausing for memory. Restoring a
disk-only snapshot needs the VM off, so a running VM is stopped and started again.

Memory is only loaded into the QEMU version and machine type it was saved from. After a
QEMU upgrade, `restore --disk-only` still restores such a snapshot's disk.

Reset, rebase and a restore that stops the VM (a disk-only snapshot, or `--disk-only`)
stop a running VM and start it again afterwards, with two exceptions:

- A running VM a canary hit paused, or whose canaries were found in use since it
  started (even if it was resumed since), is refused, nothing changed: stop it first
  (`catchpen stop`), then run the command again. So is any VM whose canaries are found
  in use after the command began and before it stops the VM (as while it copies the
  transcripts or builds a base): run it again.
- A VM whose canaries are found in use while the command runs is left stopped once
  the work is done, and the command fails saying so. `catchpen start` starts it.

With `canary_action: log` a hit doesn't count (a paused VM is still refused).

Restore and reset copy the agents' transcripts out first. Restore, reset and rebase refuse while the VM runs a
task. Memory snapshots hold whatever was in the guest's memory, credentials included.

## Base generations

A rebuild never replaces a base in use. It adds a generation
(`<distro>-<hash>.2.qcow2`) that new VMs use; the one it supersedes is removed if no VM
uses it. `catchpen ls` marks VMs on an older generation `stale-base`.

To move a VM to new software:

- **`reset --latest`** gives the VM a fresh disk on the new base. Everything on its disk
  is lost, snapshots included; keys, forges and settings stay, and repos are cloned
  again. This is how a VM gets new packages.
- **`reset`** does the same on the VM's current base.
- **`rebase`** copies what differs into the VM's disk, so the guest sees exactly the same
  files, without new packages, and stops it using the old base, which `gc` can then
  remove. It deletes the VM's snapshots.

Both use the VM's `-t` as the category files are now. When that gives another distro
(say `distros.yaml` changed its default), they refuse: a disk can't change distro. Create
a new VM, or `reset` without `--latest` to stay on the current base.

The daemon can rebuild bases on a schedule (`rebuild:` in config.yaml); see
[deployment.md](deployment.md#scheduled-rebuilds).

## Over the API

With the daemon's API on (`server:` in [configuration.md](configuration.md)), each of
these runs as a job:

| Call | As | Needs |
|---|---|---|
| `POST /v1/vms/<name>/snapshots` | `snapshot` | `{"name": …}` (no default name); `"disk_only": true` for `--disk-only` |
| `POST /v1/vms/<name>/snapshots/<snap>/restore` | `restore` | `"confirm": true`, with the `canary_hit` that `GET /v1/vms/<name>` tells; `"disk_only": true` for `--disk-only` |
| `POST /v1/vms/<name>/snapshots/<snap>/delete` | `rmsnap` | `"confirm": true` |
| `POST /v1/vms/<name>/reset` | `reset` | `"confirm": true`, with the `snapshots` and `canary_hit` that `GET` on the same path tells; `"latest": true` for `--latest` |
| `POST /v1/vms/<name>/rebase` | `rebase` | with snapshots, `"confirm": true` and the `snapshots` that `GET` on the same path tells |
| `POST /v1/vms/<name>/push` | `creds push` | nothing |

Restore and snapshot delete ask for the confirmation the CLI doesn't. A reset whose
snapshots or canary hit, a restore whose canary hit, or a rebase whose snapshots,
changed since the client read them is refused (409): read them again. All six are
refused on a VM a canary hit paused when the call is made: resume, stop or destroy it
first (to keep a paused VM's memory and disk as evidence, use `catchpen snapshot`).
Reset, rebase and restore can start the VM, and push can clone a repo again through the
VM's SSH agent, so they are refused on a VM with `ssh.agent` other than `off`, which
loads or forwards your SSH keys: use the CLI there.

## Garbage collection

`catchpen gc` removes:

- bases no VM uses (the current base of each `-t` spec stays);
- leftovers of interrupted builds, downloads, resets and destroys;
- package cache objects past `cache_max`.

Recordings, transcripts, tasks and the audit log are never removed. `--dry-run` lists
what would go and how much it would free.
