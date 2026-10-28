# Troubleshooting

Start with `catchpen doctor`, then the logs for the area that failed. Error messages are
printed as `catchpen: <error>` and usually name the log or command that helps.

## Where to look

| What | Where |
|---|---|
| Host requirements | `catchpen doctor` |
| QEMU output | `~/.local/share/catchpen/vms/<name>/qemu.log` (one `--- <time> start` line per start) |
| Boot log | `catchpen console <name>` (live; `Ctrl-]` detaches), or `vms/<name>/serial.sock.log` with `--debug` |
| Network requests | `catchpen egress log <name> [-f]` (`vms/<name>/egress.log`) |
| Base builds | `~/.local/share/catchpen/bases/<base>.build.log`, `.console.log`, `.egress.log` |
| Port forwards | `vms/<name>/forward.log` |
| Agent notifications | `vms/<name>/notify.log` |
| Tasks | `catchpen task show <id>`, `catchpen task logs <id>`; `tasks/<id>/supervisor.log`, `stderr.log`, `output.jsonl` |
| Daemon | `journalctl --user -u catchpen` |
| Everything catchpen did | `catchpen audit [<name>]` |

`--debug` streams the whole base build log instead of progress lines, writes the serial
console to `vms/<name>/serial.sock.log`, and leaves a VM that fails to boot running so
you can inspect it. Stop or destroy it afterwards.

## catchpen doctor

| Check | If it fails |
|---|---|
| `qemu-system-x86_64`, `qemu-img` | `sudo apt install qemu-system-x86 qemu-utils` |
| `ssh` | `sudo apt install openssh-client` |
| `/dev/kvm read/write` | `sudo usermod -aG kvm $USER`, then log in again. Check that virtualization is on in the firmware. |
| `kvm group` (warning) | Access comes only from the login session's ACL, which services and SSH sessions may lack. Add yourself to the group. |
| `config` | Fix the error it shows in `config.yaml`. Unknown keys are errors. |
| `data dir` | Check the permissions of `~/.local/share/catchpen`, `~/.config/catchpen` and `~/.cache/catchpen`. |
| `catchpen binary` (warning) | You're running from a temp directory (`go run`). Install the binary; QEMU and ssh call it back. |
| `notify-send` (warning) | `sudo apt install libnotify-bin` for desktop notifications and prompts. |
| `ssh-agent` (warning) | `ssh.agent: restricted` needs `ssh-agent` and `ssh-add`. |
| `ssh-askpass` (warning) | Restricted agents with `confirm: true` need an askpass program and a display. With no display (a headless host), set `confirm: false`; with no askpass program, `sudo apt install ssh-askpass` or set `SSH_ASKPASS`. |

doctor doesn't check QEMU's version, `git`, `scp`, `tmux` or free disk space.

## Installing and starting QEMU

**`qemu exited during startup ... (see .../qemu.log)`**: read `qemu.log`. Usual causes
are no access to `/dev/kvm` (catchpen needs KVM; there's no software emulation fallback)
or a QEMU build without seccomp sandbox support.

**`qemu did not open its QMP socket within 30s`**: the host is overloaded, or QEMU is
stuck. Check `qemu.log` and `catchpen stop -f <name>`.

**`<binary> path ... cannot be used in ssh_config`**: install catchpen at a path without
quotes, `%` or line breaks.

## Templates

**`unknown section "X" (catchpen template ls lists them)`**: `-t` names a section no
category file has. `catchpen template ls` lists them all.

**`...: X.yaml: old-style templates, which catchpen no longer reads`**: commands that read
templates (`up`, `task run`, `template`, `reset --latest`, `rebase`, `pool fill`) refuse while
`~/.config/catchpen/templates/` holds a `*.yaml` file whose name isn't a category's, such as
the `example.yaml` an older `catchpen init` wrote. Move its settings into sections of the
category files, then remove or rename it. A category file holding an old template gives
**`... looks like an old-style template`** (for `extends:`, or a setting such as `distro:`
or `packages:` at the top) or **`... doesn't belong in X.yaml`** (for one such as
`resources:`).

**`... created with the old-style template X`**: the VM predates category files.
`reset --latest` and `rebase` refuse it and `creds push` keeps its env, skills and
plugins; create a new VM
with `-t` picks.

**`defaults.distro, defaults.template, defaults.resources and pool distro are gone`**:
remove those keys from `config.yaml`; see [configuration.md](configuration.md).

**`"X" is in several categories; say which: ...`**: pick it as `category:X`.

**`<category> takes one section, got A and B`**: `distros`, `resources`, `ssh` and
`idle` take one pick each in `-t`, which replaces the default. Pick one, and don't
remove (`-name`) their default.

**`-X: not in <category>'s default (...)`**: `-name` in `-t` only drops a section that
is chosen; `catchpen template show -t <picks>` shows what a `-t` gives.

## Images

**`checksum mismatch for <url>`**: the download was corrupted, or the image changed
upstream while downloading. Retry; the partial file is removed.

**`<file> not listed in checksum file`** or **`bad digest`**: the distro moved its
image, or a custom `images:` entry is wrong. Fix `checksum_url`, or pin `sha256:` or
`sha512:`. A mismatch against a pinned digest means the image changed: update the pin
after checking the new image; retrying won't help.

**`refusing redirect to non-https`**: image and checksum URLs must stay on HTTPS.

**`unknown distro`**: a section's `distro:` names neither a built-in (`ubuntu-24.04`,
`ubuntu-26.04`, `debian-13`) nor an entry under `images:` in config.yaml.

Interrupted downloads leave `images/*.part`, which `catchpen gc` removes after 90 minutes.

## Base builds

**`base build failed: ... (logs: <build.log>, <console.log>)`** or **`base build script
failed; see <build.log>`**:

1. Read the end of `<base>.build.log`; it holds the build script's output.
2. If the script never started, read `<base>.console.log` (kernel and cloud-init).
3. Check `<base>.egress.log` for blocked downloads. The build VM can reach any public
   host but no private address, so a package mirror on your LAN won't work.
4. Retry with `catchpen --debug template build -t <picks> --rebuild` to watch it live.

**`timed out after 45m0s`**: the build took too long, usually a slow mirror or a script
waiting for input. **`build log exceeded 256 MiB`**: a script is printing far too much.

**`template X changed since its base was built: building the new base first`**: shown
by `reset --latest` and `rebase`; expected after you edit a section the VM's `-t` picks.

**`base ... missing`**: a base file was deleted by hand. VMs on it can't start or be
restored; destroy them, or put the file back. `catchpen gc` never removes a base a VM
uses.

**`stale-base` in `catchpen ls`**: a newer base exists. `catchpen reset <vm> --latest` (new
packages, fresh disk) or `catchpen rebase <vm>` (same files, snapshots deleted), then
`catchpen gc`.

## Booting and SSH

**`VM exited during boot ... (VM stopped; retry with --debug ...)`**: read `qemu.log`,
then retry with `catchpen --debug start <name>` and watch `catchpen console <name>`.

**`timed out waiting for ssh`** (after 10 minutes): first boot failed or is very slow.
Use `--debug` and the console as above.

**`first boot: ...`** followed by cloud-init status: cloud-init reported an error, and the
VM was stopped. Run `catchpen --debug start <name>` to keep it running, then
`catchpen ssh <name> -- cloud-init status --long` and
`catchpen ssh <name> -- sudo cat /var/log/cloud-init-output.log`.

**`host key mismatch`**: the guest presented a different host key than the pinned one.
`catchpen reset <name>` regenerates the VM from its seed.

**`... is running, but pushing credentials failed`**: fix the secret, then
`catchpen creds push <name>`. **`secrets not found on the host: A, B`**: set them in the
environment, `secrets.env` or config.yaml `secrets:`. **`... is readable by other
users`**: `chmod 600 ~/.config/catchpen/secrets.env`.

**`VM <name> is not running`**: `catchpen start <name>`. **`runtime state missing`**:
`catchpen stop <name>`, then start it again.

**`<agent> is not installed in <vm>`**: create a new VM with the agent's section in `-t`
(`-t omp`), or add it to the `agents.yaml` default: a VM's agents are fixed when it's created, so `catchpen reset` doesn't add one.

**`recording: ...`**: the recording couldn't be written. Fix the disk or permissions, or
set `record: false`.

## Network and egress

A blocked request gets HTTP 403 with
`catchpen: egress to host:port blocked (<reason>); see: catchpen egress log <name>`.

| Reason | Fix |
|---|---|
| `not in allowlist` | `catchpen egress allow <name> host[:port]`, or, for new VMs, put it in a `network.yaml` section's `allow` or `presets` and pick that section with `-t`. |
| `denylisted` | `catchpen egress undeny <name> host`. |
| a private address | The name resolves to a private or loopback address; set `allow_private` (`catchpen egress config <name> --allow-private`) if that's intended. |
| `prompt unavailable: no watcher` | `on_deny: prompt` but nothing is answering: run `catchpen egress watch` or the daemon. |
| `prompt unavailable: timeout` | Nobody answered within `prompt_timeout`. |
| `not allowed` | Shown to the guest when a canary was used or `canaries.json` can't be read; the egress log has the real reason (`canary (...)` or `canaries unreadable`). See [Canaries](#canaries-and-paused-vms). |

**A tool times out instead of getting 403**: it doesn't use the proxy. Configure it to
use `http://10.0.2.100:3128` (the guest's `http_proxy` and `https_proxy` are already set);
clients that ignore them, like database drivers and Node's `fetch`, can't reach the
network in allowlist mode.

**`503 too many concurrent proxy connections`**: the VM has 128 connections open.
**`egress log unavailable; refusing connection`**: the log can't be written; check disk
space and permissions of `vms/<name>/`.

**`another egress watcher is already serving <vm>`**: the daemon or another
`egress watch` already handles it. Stop one of them.

**`lists only apply to allowlist mode`**: the VM uses `open` or `none`.

## Package caches

**`no room left in the store`**: the filesystem is within 5% of full. Free space or lower
`cache_max`; `catchpen cache clear` empties the caches.

**`denied by the VM's egress denylist`**: a cached host or one of its redirects is
denylisted for the VM.

**`unknown cache`**: valid kinds are `apt`, `go`, `npm`, `pypi` and `docker`.

**`503 cache unavailable`**: the VM's `canaries.json` can't be read; see below.

## Forges and exports

**`no forge for host X`**: `catchpen forge add <name> --type github|gitea --host X --mode <mode>`
(see [forges.md](forges.md#modes)).

**`SSH host key changed: <fp> is not pinned`**: if the forge really rotated its key,
`catchpen forge rm <name>` and `catchpen forge add` it again. Otherwise, treat it as an
attack.

**`... does not support host-bound auth`**: the forge's SSH server can't be used with a
restricted agent; use `deploy-key` or `minted-token` mode.

**`clone <repo>: ... (the restricted agent refused ...)`**: you declined the
confirmation, there was no askpass to ask with, or the forge lacks host-bound auth. Run
`catchpen forge test <forge>`.

**`forge X is on the host's loopback, which VMs can't reach`**: use an address or name
the host serves on another interface.

**`could not remove deploy key ...` / `could not delete ... token`** at destroy: catchpen
couldn't reach the forge. Remove them in the forge's settings; the audit log has their
ids.

**`<dir> is not a git repository (use --into)`**: pass the host repository with `--into`.

**`no remote in <dir> points at ...`** (pr): add a remote for the forge repo to the host
repository.

**`<vm> has N repos; pick one with --repo`**: name the repo.

## Tasks

**A task stays `queued`**: it waits for `tasks.max_vms` and `tasks.max_memory`, and all
running catchpen VMs count. Check `catchpen ls`, stop VMs you don't need, or raise the
limits.

**State `lost`**: its supervisor process died (a reboot or a kill). Read
`tasks/<id>/supervisor.log`, then `catchpen task cancel <id>` or `catchpen task rm <id>`,
which destroy its VM.

**`kept <vm> because its N commit(s) weren't exported`**: committing, counting or
exporting the task's commits failed.
`catchpen pull <vm> --branch <branch> --into <repo>`, then `catchpen destroy <vm>`.

**`<agent> timed out after <timeout>`**: raise `--timeout` (up to 48h) or split the task.

**`--out pr needs a host repository`**: run from the repository or pass `--into`.

**`--file can't be combined with other task flags`**: put those settings under
`defaults:` in the batch file.

**`<vm> runs task <id>; cancel it first`**: restore, reset and rebase refuse while a task
runs.

## Snapshots

**`it was saved by QEMU X ..., the VM runs Y`**: the running QEMU differs from the one
that saved the snapshot. Stop the VM and restore again, so it starts under the installed
QEMU. If that QEMU is also different (a warning says so) and loading fails, use
`catchpen restore <vm> <snapshot> --disk-only`, which keeps the disk and drops the memory.

**`invalid snapshot name`**: letters, digits, `.`, `_` and `-`, starting with a letter or
digit, not only digits, up to 64 characters.

## Daemon and pool

**`catchpen daemon is already running`**: the service is running; you don't need a second
one in the foreground.

**`daemon: not running`** while the service is installed: `systemctl --user status catchpen`
and `journalctl --user -u catchpen -e`. After moving the binary, `catchpen daemon install`.

**The daemon stops when you log out**: `sudo loginctl enable-linger $USER`.

**Pool logs `pool: <err> (next try in 30m0s)`**: fix the error (often a bad `-t` spec or a base
build failure), then `catchpen pool fill` to retry now.

**`another catchpen is filling or draining the pool`**: wait for it to finish.

**VMs aren't stopped when idle**: idle stop needs `idle_stop` set (it's off by default)
and the daemon running, and skips VMs with an SSH session, a forward in use, a task, or
paused, ephemeral and pool VMs.

**Notifications from the daemon don't arrive**: the service doesn't see your shell's
environment. Put tokens in `secrets.env` or a `command:` source.

## Canaries and paused VMs

**`VM <name> was paused by a canary hit`**: something in the VM used a decoy credential.
Investigate first (`catchpen audit <name>`, `catchpen egress log <name>`, see
[canaries.md](canaries.md#responding-to-a-hit)), then `catchpen resume <name>` or
`catchpen destroy <name>`.

**`... refused: it contains the VM's decoy credentials`**: an export carried a decoy and
was refused.

**`canaries unreadable`**: `vms/<name>/canaries.json` is damaged. The VM's proxy, cache
and exports refuse everything until it's fixed. There is no repair command: restore the
file from a backup, or destroy and recreate the VM. Deleting the file turns detection off
for that VM while the guest keeps the old decoys.

## Cleaning up after a crash

- `catchpen gc --dry-run`, then `catchpen gc`: removes unused bases and interrupted destroys,
  and leftovers of interrupted builds, downloads (`*.part`) and resets once they are 90
  minutes old.
- `catchpen destroy -y <name>` works even when a VM's state file is missing or damaged; it
  warns about deploy keys it might have left on the forge.
- `catchpen stop -f <name>` for a QEMU that won't shut down.
- `catchpen forward <name> --stop` for leftover forwards.
- Don't delete files under `bases/` by hand.
