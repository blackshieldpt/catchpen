# curral

**KVM sandboxes for coding agents.**

![Go 1.27](https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white)
![Linux x86_64](https://img.shields.io/badge/platform-linux%20x86__64-informational?logo=linux&logoColor=white)
![QEMU/KVM](https://img.shields.io/badge/runs%20on-QEMU%2FKVM-orange?logo=qemu&logoColor=white)
![Agents](https://img.shields.io/badge/agents-claude%20%7C%20opencode%20%7C%20pi%20%7C%20omp%20%7C%20codex-blueviolet)
[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue)](LICENSE)
[![Security policy](https://img.shields.io/badge/security-policy-red)](SECURITY.md)

`curral` runs Claude Code, opencode, pi, omp and Codex in QEMU/KVM virtual machines built
from declarative templates, so an agent can install packages, run code and use sudo
without reaching your host, your credentials or your network.

## Features

| | |
|---|---|
| **Isolated VMs** | No host directories mounted; key-only SSH with pinned, per-VM host keys; QEMU under its seccomp sandbox. |
| **Default-deny egress** | The only ways out are an allowlist proxy and package caches on the host, with logging and optional prompts. |
| **Scoped credentials** | Agent tokens and per-VM forge deploy keys or tokens, pushed over SSH at start and never baked into images. |
| **Work leaves through the host** | `curral pull` and `curral pr` bring commits out as scanned git bundles, so the VM needs no push access. |
| **Headless tasks** | Run an agent on a prompt in a throwaway VM and get a PR, a bundle or a patch back. |
| **Canaries** | Decoy credentials planted per VM (the default template plants them); their use pauses the VM by default. |
| **Operations** | Snapshots, warm pool, idle stop, scheduled base rebuilds, shared package caches, session recordings and an audit log. |

## Quick start

From nothing to Claude Code in a VM, on Ubuntu or Debian. You need Linux on x86_64 with
KVM.

1. Check the host has KVM:

   ```sh
   ls -l /dev/kvm
   ```

2. Install the dependencies:

   ```sh
   sudo apt install qemu-system-x86 qemu-utils openssh-client git
   ```

3. Download the latest release binary, check it, and install it to a stable path (QEMU
   and ssh call it back):

   ```sh
   v=0.1.0
   f=curral_${v}_linux_amd64
   curl -fLO https://github.com/blackshieldpt/curral/releases/download/v$v/$f
   curl -fLO https://github.com/blackshieldpt/curral/releases/download/v$v/SHA256SUMS
   sha256sum -c --ignore-missing SHA256SUMS
   install -D -m 755 $f ~/.local/bin/curral
   ```

   To also check its signature and build provenance, see
   [docs/installation.md](docs/installation.md#install). To build it yourself, see
   [Building from source](#building-from-source).

4. Join the `kvm` group, then log out and back in, which also puts `~/.local/bin` on
   your `PATH`:

   ```sh
   sudo usermod -aG kvm $USER
   ```

5. Check the host, and fix anything it reports:

   ```sh
   curral doctor
   ```

6. Get a Claude Code token, on a machine with Claude Code installed:

   ```sh
   claude setup-token
   ```

7. Store it where curral finds it:

   ```sh
   mkdir -p ~/.config/curral
   touch ~/.config/curral/secrets.env && chmod 600 ~/.config/curral/secrets.env
   echo 'CLAUDE_CODE_OAUTH_TOKEN=<token>' >> ~/.config/curral/secrets.env
   ```

8. Create and start a VM. The first one takes a few minutes to download the image and
   build its base:

   ```sh
   curral up dev
   ```

9. Start Claude Code in it. Detach with `Ctrl-a d`; the same command reattaches:

   ```sh
   curral run dev claude
   ```

Release verification, optional packages, upgrading and uninstalling are in
[docs/installation.md](docs/installation.md).

## Building from source

With Go 1.27, build to a stable path instead of step 3 (QEMU and ssh call the binary
back, so `go run` won't work):

```sh
git clone https://github.com/blackshieldpt/curral.git && cd curral
go build -o ~/.local/bin/curral ./cmd/curral
```

See [docs/installation.md](docs/installation.md#building-from-source) for upgrading a
source build, and [docs/development.md](docs/development.md) for tests.

## Usage

### VMs

```sh
curral up dev                   # create and start a VM from the default template
curral run dev claude           # Claude Code in a guest tmux session (reattaches if running)
curral ssh dev                  # a shell; curral ssh dev -- docker ps runs a command
curral attach dev               # the guest's main tmux session
curral cp ./notes.md dev:/tmp/  # copy files either way (-r for directories)
curral ls
curral stop dev
curral destroy dev
```

The first `up` of a template downloads and verifies the cloud image and builds its base.
Later VMs from the template are copy-on-write overlays and boot in seconds.
`curral setup-ssh` lets plain `ssh curral-dev`, `scp` and VS Code Remote-SSH reach VMs.

### Repositories

Work on a repository and get the result out through the host:

```sh
curral forge add github --type github --host github.com --mode deploy-key --token-secret GH_ADMIN_TOKEN
curral up api --repo github:acme/api
curral pr api --repo api --into ~/src/api
```

### Headless tasks

Hand a prompt to an agent and get a pull request back:

```sh
curral task run --repo github:acme/api --prompt "fix issue #42" --into ~/src/api
curral task ls
```

### Templates

The `default` template has Docker, Node 24, uv, `gh`, Claude Code, opencode, pi and Codex
(Codex without credentials until you set `agents.codex.auth`). Write your own in
`~/.config/curral/templates/`; see [docs/templates.md](docs/templates.md).

## Documentation

| Page | Covers |
|---|---|
| [Installation](docs/installation.md) | Requirements, install, release verification, first VM, upgrading, building from source |
| [CLI reference](docs/cli-reference.md) | Every command and flag |
| [Templates](docs/templates.md), [configuration](docs/configuration.md) | What VMs contain; `config.yaml`, secrets, directories |
| [Agents](docs/agents.md), [forges](docs/forges.md), [tasks](docs/tasks.md) | Credentials, repos, `pull`/`pr`, SSH agents, headless runs |
| [Sessions](docs/sessions.md), [notifications](docs/notifications.md), [snapshots](docs/snapshots.md) | Recordings and audit, alerts, snapshots and base updates |
| [Security model](docs/security-model.md), [egress](docs/egress.md), [caches](docs/package-caches.md), [canaries](docs/canaries.md) | What curral protects, and its limits |
| [Deployment](docs/deployment.md) | The daemon, warm pool, idle stop, rebuilds, headless hosts, backups |
| [Troubleshooting](docs/troubleshooting.md) | Logs, error messages and recovery |
| [Development](docs/development.md) | Building, testing, CI and releases |

All pages are listed in [docs/README.md](docs/README.md).

## Project

- [Changelog](CHANGELOG.md)
- [Reporting vulnerabilities](SECURITY.md)
- License: [AGPL-3.0](LICENSE)
