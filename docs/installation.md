# Installation

## Requirements

- Linux on x86_64 with KVM. `/dev/kvm` must be readable and writable by your user; there
  is no software-emulation fallback.
- QEMU (`qemu-system-x86_64`, `qemu-img`) built with seccomp sandbox support, as distro
  packages are.
- OpenSSH client (`ssh`, `scp`) and `git`.
- Go 1.27 to build.
- Disk space for cloud images, bases and VM disks; see
  [deployment.md](deployment.md#host-sizing).

Optional:

| Package | For |
|---|---|
| `libnotify-bin` (`notify-send`) | Desktop notifications and egress prompts. |
| `openssh-client`'s `ssh-agent`, and `ssh-askpass` | Restricted SSH agents with confirmation. |
| `tmux` | `curral tmux`. |
| systemd user session | `curral daemon install`. |

macOS, Windows, libvirt and multi-host setups aren't supported.

## Install

On Debian or Ubuntu:

```
sudo apt install qemu-system-x86 qemu-utils openssh-client git
sudo usermod -aG kvm $USER          # then log out and back in
sudo apt install libnotify-bin ssh-askpass tmux   # optional
```

Build and install the binary:

```
git clone git@github.com:blackshieldpt/curral.git
cd curral
go build -o ~/.local/bin/curral ./cmd/curral
curral doctor
```

Install it somewhere stable (here `~/.local/bin`, which should be on your `PATH`). QEMU
and ssh call the binary back for the egress proxy, package caches, notifications and SSH
proxying, so `go run` won't work.

`curral doctor` checks QEMU, ssh, `/dev/kvm` and curral's own setup, and prints a remedy
for each failure; it doesn't check `git`, `scp` or QEMU's seccomp support. See
[troubleshooting.md](troubleshooting.md#curral-doctor).

Optionally, let plain `ssh`, `scp` and editors reach VMs as `curral-<name>`:

```
curral setup-ssh          # adds an Include to ~/.ssh/config; --remove takes it out
```

## First VM

1. Give Claude Code a credential. With a Claude subscription, create a long-lived token
   once (`claude setup-token`, with Claude Code installed on the host) and store it:

   ```
   mkdir -p ~/.config/curral && touch ~/.config/curral/secrets.env && chmod 600 ~/.config/curral/secrets.env
   echo 'CLAUDE_CODE_OAUTH_TOKEN=<token>' >> ~/.config/curral/secrets.env
   ```

   With an API key, set `ANTHROPIC_API_KEY` there instead and create VMs with
   `--claude-auth api-key`. Other ways to supply secrets are in
   [configuration.md](configuration.md#secrets).

2. Create and start a VM from the `default` template:

   ```
   curral up dev
   ```

   The first `up` of a template downloads and verifies the cloud image and builds the
   template's base, which takes a few minutes. Later VMs of the template boot in
   seconds.

3. Use it:

   ```
   curral run dev claude          # Claude Code in a guest tmux session
   curral ssh dev                 # a shell
   curral ls
   curral stop dev
   curral destroy dev
   ```

To work on a repository, add a forge and pass `--repo`; see [forges.md](forges.md). To
run agents unattended, see [tasks.md](tasks.md).

## Upgrading

```
cd curral
git pull
go build -o ~/.local/bin/curral ./cmd/curral
systemctl --user restart curral      # if the daemon is installed
```

Run `curral daemon install` again instead if the binary moved. Check the
[changelog](https://github.com/blackshieldpt/curral/blob/master/CHANGELOG.md) for changes that need action.

## Uninstalling

```
curral daemon uninstall              # if installed
curral ls                            # then: curral destroy -y <name>... for each VM
curral pool drain
curral setup-ssh --remove
rm ~/.local/bin/curral
rm -rf ~/.config/curral ~/.local/share/curral ~/.cache/curral
```

Destroying the VMs first removes the deploy keys and tokens they hold on your forges. The
last line also deletes recordings, transcripts, task output and the audit log; keep
`~/.local/share/curral` if you need them.
