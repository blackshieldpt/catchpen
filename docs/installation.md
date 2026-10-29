# Installation

## Requirements

- Linux on x86_64 with KVM. `/dev/kvm` must be readable and writable by your user; there
  is no software-emulation fallback.
- QEMU (`qemu-system-x86_64`, `qemu-img`) built with seccomp sandbox support, as distro
  packages are.
- OpenSSH client (`ssh`, `scp`) and `git`.
- Disk space for cloud images, bases and VM disks; see
  [deployment.md](deployment.md#host-sizing).

Optional:

| Package | For |
|---|---|
| `libnotify-bin` (`notify-send`) | Desktop notifications and egress prompts. |
| `openssh-client`'s `ssh-agent`, and `ssh-askpass` | Restricted SSH agents with confirmation. |
| `tmux` | `catchpen tmux`. |
| systemd user session | `catchpen daemon install`. |

macOS, Windows, libvirt and multi-host setups aren't supported.

## Install

Each [release](https://github.com/blackshieldpt/catchpen/releases) has an amd64 `.deb` for
Debian and Ubuntu, and a static linux/amd64 binary (also as a `.tar.gz` with the license,
README and changelog). Each file is listed in `SHA256SUMS`, signed with cosign keyless by
this repository's release workflow at that tag (a `.sigstore.json` bundle), and has a
build provenance attestation. Checking them needs
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/) v3, and the `gh`
CLI for the optional provenance check. Or [build from source](#building-from-source).

### Debian package

The `.deb` installs `/usr/bin/catchpen` and pulls in QEMU, `openssh-client` and `git`.
`libnotify-bin`, `ssh-askpass` and `tmux` come too, as recommends, unless you pass
`--no-install-recommends`.

```
v=0.1.2   # the release to install
f=catchpen_${v}_amd64.deb
u=https://github.com/blackshieldpt/catchpen/releases/download/v$v
curl -fL -O $u/$f -O $u/$f.sigstore.json -O $u/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
cosign verify-blob --bundle $f.sigstore.json \
  --certificate-identity "https://github.com/blackshieldpt/catchpen/.github/workflows/release.yml@refs/tags/v$v" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com $f
gh attestation verify $f --repo blackshieldpt/catchpen \
  --signer-workflow blackshieldpt/catchpen/.github/workflows/release.yml   # build provenance; optional
sudo apt install ./$f
sudo usermod -aG kvm $USER          # then log out and back in
catchpen doctor
```

To upgrade, install the new release's `.deb` the same way. `sudo apt remove catchpen`
removes the package; your VMs and settings stay (see [Uninstalling](#uninstalling)).
0.1.0 has no `.deb`.

### Release binary

Install the [requirements](#requirements) first; on Debian or Ubuntu:

```
sudo apt install qemu-system-x86 qemu-utils openssh-client git
sudo usermod -aG kvm $USER          # then log out and back in
sudo apt install libnotify-bin ssh-askpass tmux   # optional
```

Then download the binary and check it the same way:

```
v=0.1.2   # the release to install
f=catchpen_${v}_linux_amd64
u=https://github.com/blackshieldpt/catchpen/releases/download/v$v
curl -fL -O $u/$f -O $u/$f.sigstore.json -O $u/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
cosign verify-blob --bundle $f.sigstore.json \
  --certificate-identity "https://github.com/blackshieldpt/catchpen/.github/workflows/release.yml@refs/tags/v$v" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com $f
gh attestation verify $f --repo blackshieldpt/catchpen \
  --signer-workflow blackshieldpt/catchpen/.github/workflows/release.yml   # build provenance; optional
install -D -m 755 $f ~/.local/bin/catchpen
catchpen doctor
```

Install it somewhere stable (here `~/.local/bin`, which should be on your `PATH`). QEMU
and ssh call the binary back for the egress proxy, package caches, notifications and SSH
proxying.

Releases are reproducible: building the tag with GoReleaser and the Go version
`catchpen version` prints gives the same bytes.

`catchpen doctor` checks QEMU, ssh, `/dev/kvm` and catchpen's own setup, and prints a remedy
for each failure; it doesn't check `git`, `scp` or QEMU's seccomp support. See
[troubleshooting.md](troubleshooting.md#catchpen-doctor).

Optionally, let plain `ssh`, `scp` and editors reach VMs as `catchpen-<name>`:

```
catchpen setup-ssh          # adds an Include to ~/.ssh/config; --remove takes it out
```

## First VM

1. Give Claude Code a credential. With a Claude subscription, create a long-lived token
   once (`claude setup-token`, with Claude Code installed on the host). `catchpen init`
   writes a commented `config.yaml`, `secrets.env` (mode 0600) and the template
   category files in `templates/` under `~/.config/catchpen`, keeping files that exist.
   Uncomment `CLAUDE_CODE_OAUTH_TOKEN=` in `secrets.env` and put the token after it:

   ```
   catchpen init
   $EDITOR ~/.config/catchpen/secrets.env
   ```

   With an API key, set `ANTHROPIC_API_KEY` there instead and create VMs with
   `--claude-auth api-key` (or `-t claude-api-key`). Other ways to supply secrets are in
   [configuration.md](configuration.md#secrets).

2. Create and start a VM with the default template (every category's default):

   ```
   catchpen up dev
   ```

   The first `up` of a template downloads and verifies the cloud image and builds the
   template's base, which takes a few minutes. Later VMs of the template boot in
   seconds.

3. Use it:

   ```
   catchpen run dev claude          # Claude Code in a guest tmux session
   catchpen ssh dev                 # a shell
   catchpen ls
   catchpen stop dev
   catchpen destroy dev
   ```

To work on a repository, add a forge and pass `--repo`; see [forges.md](forges.md). To
run agents unattended, see [tasks.md](tasks.md).

## Upgrading

Download and verify the new release as in [Install](#install), install it over the old
one, then restart the daemon if it is installed:

```
systemctl --user restart catchpen
```

`catchpen version` prints what you run. For the `.deb`, see
[Debian package](#debian-package). To upgrade a source build, see
[Building from source](#building-from-source).

Run `catchpen daemon install` again instead if the binary moved. Check the
[changelog](https://github.com/blackshieldpt/catchpen/blob/master/CHANGELOG.md) for changes that need action.

## Building from source

Building needs Go 1.27. Build to the same stable path as a release binary; `go run`
won't work, since QEMU and ssh call the binary back:

```
git clone https://github.com/blackshieldpt/catchpen.git
cd catchpen
go build -o ~/.local/bin/catchpen ./cmd/catchpen
catchpen doctor
```

To upgrade:

```
cd catchpen
git pull
go build -o ~/.local/bin/catchpen ./cmd/catchpen
systemctl --user restart catchpen      # if the daemon is installed
```

Tests and the release process are in [development.md](development.md).

## Uninstalling

```
catchpen daemon uninstall              # if installed
catchpen ls                            # then: catchpen destroy -y <name>... for each VM
catchpen pool drain
catchpen setup-ssh --remove
rm ~/.local/bin/catchpen               # or: sudo apt remove catchpen
rm -rf ~/.config/catchpen ~/.local/share/catchpen ~/.cache/catchpen
```

Destroying the VMs first removes the deploy keys and tokens they hold on your forges. The
last line also deletes recordings, transcripts, task output and the audit log; keep
`~/.local/share/catchpen` if you need them.
