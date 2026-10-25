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

Then install a release binary (or [build from source](#building-from-source)). A release
is a static linux/amd64 binary, also packaged as a `.tar.gz` with the license and
changelog and as a [Debian package](#debian-package), on the [releases page](https://github.com/blackshieldpt/curral/releases).
Download the binary, its `.sigstore.json` and `SHA256SUMS`, then check it was built and
signed by this repository's release workflow at that tag (needs
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/) v3, and the `gh`
CLI for the optional provenance check):

```
v=0.1.0   # the release to install
f=curral_${v}_linux_amd64
u=https://github.com/blackshieldpt/curral/releases/download/v$v
curl -fL -O $u/$f -O $u/$f.sigstore.json -O $u/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
cosign verify-blob --bundle $f.sigstore.json \
  --certificate-identity "https://github.com/blackshieldpt/curral/.github/workflows/release.yml@refs/tags/v$v" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com $f
gh attestation verify $f --repo blackshieldpt/curral \
  --signer-workflow blackshieldpt/curral/.github/workflows/release.yml   # build provenance; optional
install -D -m 755 $f ~/.local/bin/curral
curral doctor
```

Install it somewhere stable (here `~/.local/bin`, which should be on your `PATH`). QEMU
and ssh call the binary back for the egress proxy, package caches, notifications and SSH
proxying.

Releases are reproducible: building the tag with GoReleaser and the Go version
`curral version` prints gives the same bytes.

### Debian package

Releases after 0.1.0 also ship an amd64 `.deb`, signed like the binary. It installs
`/usr/bin/curral` and pulls in QEMU, `openssh-client` and `git` (the optional packages
are recommends):

```
v=0.2.0   # the release to install
f=curral_${v}_amd64.deb
u=https://github.com/blackshieldpt/curral/releases/download/v$v
curl -fL -O $u/$f -O $u/$f.sigstore.json -O $u/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
cosign verify-blob --bundle $f.sigstore.json \
  --certificate-identity "https://github.com/blackshieldpt/curral/.github/workflows/release.yml@refs/tags/v$v" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com $f
sudo apt install ./$f
sudo usermod -aG kvm $USER          # then log out and back in
curral doctor
```

To upgrade, install the new release's `.deb` the same way; `sudo apt remove curral`
removes it (your VMs and settings stay; see [Uninstalling](#uninstalling)).

`curral doctor` checks QEMU, ssh, `/dev/kvm` and curral's own setup, and prints a remedy
for each failure; it doesn't check `git`, `scp` or QEMU's seccomp support. See
[troubleshooting.md](troubleshooting.md#curral-doctor).

Optionally, let plain `ssh`, `scp` and editors reach VMs as `curral-<name>`:

```
curral setup-ssh          # adds an Include to ~/.ssh/config; --remove takes it out
```

## First VM

1. Give Claude Code a credential. With a Claude subscription, create a long-lived token
   once (`claude setup-token`, with Claude Code installed on the host). `curral init`
   writes a commented `config.yaml`, `secrets.env` (mode 0600) and
   `templates/example.yaml` under `~/.config/curral`, keeping files that exist.
   Uncomment `CLAUDE_CODE_OAUTH_TOKEN=` in `secrets.env` and put the token after it:

   ```
   curral init
   $EDITOR ~/.config/curral/secrets.env
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

Download and verify the new release as in [Install](#install), install it over the old
one, then restart the daemon if it is installed:

```
systemctl --user restart curral
```

`curral version` prints what you run. With the `.deb`, see
[Debian package](#debian-package). To upgrade a source build, see
[Building from source](#building-from-source).

Run `curral daemon install` again instead if the binary moved. Check the
[changelog](https://github.com/blackshieldpt/curral/blob/master/CHANGELOG.md) for changes that need action.

## Building from source

Building needs Go 1.27. Build to the same stable path as a release binary; `go run`
won't work, since QEMU and ssh call the binary back:

```
git clone https://github.com/blackshieldpt/curral.git
cd curral
go build -o ~/.local/bin/curral ./cmd/curral
curral doctor
```

To upgrade:

```
cd curral
git pull
go build -o ~/.local/bin/curral ./cmd/curral
systemctl --user restart curral      # if the daemon is installed
```

Tests and the release process are in [development.md](development.md).

## Uninstalling

```
curral daemon uninstall              # if installed
curral ls                            # then: curral destroy -y <name>... for each VM
curral pool drain
curral setup-ssh --remove
rm ~/.local/bin/curral               # or: sudo apt remove curral
rm -rf ~/.config/curral ~/.local/share/curral ~/.cache/curral
```

Destroying the VMs first removes the deploy keys and tokens they hold on your forges. The
last line also deletes recordings, transcripts, task output and the audit log; keep
`~/.local/share/curral` if you need them.
