# Deploying curral

curral runs on one Linux host, as your own user. There is no server to deploy: the
`curral` binary, QEMU and a systemd user service for the background jobs are all it
needs. This page covers setting up a host to run VMs unattended: a workstation, or a
dedicated build box you reach over SSH.

## Host sizing

| Resource | What uses it |
|---|---|
| CPU | Each VM's `resources.cpus` (default 4); base builds use up to 4. |
| Memory | Each running VM's `resources.memory` (default 8G); base builds use 4 GiB. Stopped and pool VMs use none. |
| Disk | Cloud images (a few hundred MiB each); bases (built on a 20 GiB disk, usually a few GiB used); VM overlays (grow up to `resources.disk`, default 40G); memory snapshots and pool VMs (their guest memory, saved to disk); package caches (`cache_max`, default 20G); the git repos of skills and plugins (`~/.cache/curral/sources/`, never trimmed; safe to delete, the next start fetches them again); recordings, transcripts and task output, which are never trimmed. |

Keep `tasks.max_vms` and `tasks.max_memory` in config.yaml within what the host can run
at once; all running curral VMs count against them, not just tasks. curral doesn't check
free disk space up front, so watch it: failures show up as `qemu-img` or write errors.

## Installing

Follow [installation.md](installation.md), then:

1. Make sure your user is in the `kvm` group, not just granted access by the login
   session's ACL: services and SSH sessions may not get the ACL. `curral doctor` warns
   about this.
2. Install the binary at a stable path, such as `~/.local/bin/curral`. QEMU, ssh and the
   systemd unit call it back by its absolute path.
3. Run `curral setup-ssh` if you want `ssh curral-<vm>` to work.

## The daemon

The daemon runs the background jobs. Everything works without it; you only lose those
jobs.

| Job | How often | What it does |
|---|---|---|
| idle stop | every minute | Stops VMs idle for `idle_stop`. |
| pool | every 2 minutes | Replaces stale pool VMs and creates missing ones, one at a time. After a failure it waits 30 minutes. |
| rebuild | checked every 10 minutes | When `rebuild:` has elapsed, rebuilds the bases in use, refills the pool and runs `gc`. |
| prompts | every 30 seconds | Answers egress prompts through desktop notifications for VMs with `on_deny: prompt` that no `curral egress watch` serves. |

It reads config.yaml again each round, so edits apply without a restart.

```
curral daemon install     # writes ~/.config/systemd/user/curral.service, enables and starts it
curral daemon status      # daemon: running / service: active
curral daemon uninstall
journalctl --user -u curral -f
systemctl --user restart curral
```

`curral daemon` runs it in the foreground instead, logging to the terminal. Only one
daemon runs at a time (a lock on `~/.local/share/curral/daemon.lock`, which can't go
stale).

The unit runs `curral daemon` by its absolute path, with `Restart=on-failure` and the
`PATH`, `CURRAL_HOME` and `XDG_*` variables of the shell you installed it from. Run
`curral daemon install` again after moving the binary or changing those variables.

The service doesn't see your shell's environment otherwise. Secrets it needs, such as
notification tokens, belong in `secrets.env` or a `command:` secret source; see
[configuration.md](configuration.md#secrets).

A user service stops when you log out. On a host you don't stay logged into, enable
lingering so it keeps running and starts at boot:

```
sudo loginctl enable-linger $USER
```

### Warm pool

```yaml
# config.yaml
pool:
  - { template: default, size: 2 }
  - { template: "debian-13,go-1.27", size: 1 }   # a -t spec
```

```
curral pool ls | fill | drain
```

Pool VMs go through first boot with their own keys, but no repos, agents' credentials or
allowlist, and are then saved with their memory and stopped. They use disk, not RAM.
`curral up` and `curral task run` claim one when the `-t` spec, written the same way
(`go-1.27,large` doesn't serve `-t large,go-1.27`), resources, base and the rest of the
VM's settings (egress mode, user, Docker, caches, canaries) match
(not with `--rebuild`): it is renamed, gets its allowlist, forges and
credentials, and resumes in seconds with its new hostname. Each saved memory is resumed
once.

`pool fill` (and the daemon) replaces pool VMs on an old base or template version. After
a QEMU upgrade, a pool VM boots normally instead of resuming; `curral pool drain` then
`curral pool fill` replaces them all. `curral ls` doesn't list pool VMs.

### Idle stop

```yaml
idle_stop: 30m          # config.yaml; -t idle-2h or -t idle-off overrides it per VM
```

A VM is idle when nothing is connected to its SSH or forwarded ports (sessions, `run`,
`forward`, editors over SSH), it runs no task, and QEMU used under 5% of a core since the
previous check. It is shut down gracefully, and `vm.idle_stop` is notified. Ephemeral,
pool and paused VMs are left alone. Idle clocks are kept in memory, so restarting the
daemon resets them.

### Scheduled rebuilds

```yaml
rebuild: weekly         # off (default), daily, weekly or a duration such as 72h
```

The daemon rebuilds the bases in use from a fresh cloud image, so new VMs get current
packages; existing VMs keep their base until you `reset --latest` or `rebase` them (see
[snapshots.md](snapshots.md)). A failed rebuild leaves the old base in place and is
retried at the next interval.

## Running headless

On a host without a desktop session:

- Use `ntfy` or `webhook` notification sinks; `desktop` needs `notify-send` and a session
  bus. See [notifications.md](notifications.md).
- Egress prompts: the daemon always tries to answer them with desktop notifications,
  and without `notify-send` it denies them at once. Either use `on_deny: log` and allow
  hosts with `curral egress allow`, or run `curral egress watch` in a terminal (in tmux,
  say) and start it before the daemon: the daemon leaves alone any VM a running watcher
  already serves.
- Restricted agents with `confirm: true` need an askpass program and a display. Without
  one, use `confirm: false` or a forge mode other than `agent`.
- Reach VMs by SSH-ing to the host first: `ssh -t buildhost curral attach dev`. VM ports
  bind to the host's `127.0.0.1`, so use `ssh -L` to the host for web previews.

## After a host reboot

VMs don't start on their own: run `curral start <name>` for the ones you need. Tasks
that were running show as `lost`; `curral task cancel <id>` destroys their VMs. Pool VMs
are still valid, and the daemon (with lingering enabled) starts again.

## Upgrading curral

1. Build and install the new binary over the old one (see
   [installation.md](installation.md#upgrading)).
2. `systemctl --user restart curral` if the daemon runs, or `curral daemon install` if
   the binary moved.
3. Check the [changelog](https://github.com/blackshieldpt/curral/blob/master/CHANGELOG.md). If the new version changes how bases are
   built, templates get a new base: the next `up` of each builds it (up to 45 minutes),
   the daemon replaces pool VMs, and old bases stay until `curral gc`. Running VMs keep
   running. QEMU starts
   `curral` for each proxy, cache and notification connection, so those use the new
   binary at once.

## Backups

VMs are disposable; the repos they work on live on the forges. Worth backing up:

- `~/.config/curral/`: config.yaml, templates, forges.yaml (with pinned host keys) and
  secrets.env (encrypt it);
- `~/.local/share/curral/audit.jsonl`, `recordings/`, `transcripts/` and `tasks/`, if you
  need the history;
- any VM you can't recreate: copy its `vms/<name>/` directory while it's stopped,
  together with the base its disk overlays (`qemu-img info vms/<name>/disk.qcow2` shows
  the backing file, by absolute path, so restore it under the same home directory).
  Snapshots live inside the VM's disk, so they aren't a backup.

Don't delete files in `bases/` by hand: VM disks are overlays on them. Use `curral gc`.
