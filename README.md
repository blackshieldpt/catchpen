# curral

KVM sandboxes for coding agents. `curral` runs Claude Code, opencode, pi, omp and Codex in
QEMU/KVM virtual machines built from declarative templates, so an agent can install
packages, run code and use sudo without reaching your host, your credentials or your
network.

- **Isolated VMs**: no host directories mounted; key-only SSH with pinned, per-VM host
  keys; QEMU under its seccomp sandbox.
- **Default-deny egress**: by default the only ways out are an allowlist proxy and
  package caches on the host, with logging and optional prompts.
- **Scoped credentials**: agent tokens and per-VM forge deploy keys or tokens, pushed
  over SSH at start and never baked into images.
- **Work leaves through the host**: `curral pull` and `curral pr` bring commits out as
  scanned git bundles, so the VM needs no push access.
- **Headless tasks**: run an agent on a prompt in a throwaway VM and get a PR, a bundle
  or a patch back.
- **Canaries**: decoy credentials planted per VM (the default template plants them);
  their use pauses the VM by default.
- **Operations**: snapshots, warm pool, idle stop, scheduled base rebuilds, shared
  package caches, session recordings and an audit log.

## Quick start

From nothing to Claude Code in a VM, on Ubuntu or Debian (details in the sections below):

1. Check the host has KVM: `ls -l /dev/kvm`.
2. Install the dependencies: `sudo apt install qemu-system-x86 qemu-utils openssh-client git`, and Go 1.27.
3. Clone curral: `git clone git@github.com:blackshieldpt/curral.git && cd curral`.
4. Build it to a stable path: `go build -o ~/.local/bin/curral ./cmd/curral`.
5. Join the `kvm` group (`sudo usermod -aG kvm $USER`), then log out and back in, which also puts `~/.local/bin` on your `PATH`.
6. Run `curral doctor` and fix anything it reports.
7. Get a Claude Code token on a machine with Claude Code installed: `claude setup-token`.
8. Store it: `mkdir -p ~/.config/curral && touch ~/.config/curral/secrets.env && chmod 600 ~/.config/curral/secrets.env && echo 'CLAUDE_CODE_OAUTH_TOKEN=<token>' >> ~/.config/curral/secrets.env`.
9. Create and start a VM: `curral up dev`. The first one takes a few minutes to download the image and build its base.
10. Start Claude Code in it: `curral run dev claude`. Detach with `Ctrl-a d`; the same command reattaches.

## Requirements

- Linux on x86_64 with KVM (`/dev/kvm` readable and writable; add yourself to the `kvm`
  group)
- `qemu-system-x86`, `qemu-utils`, `openssh-client`, `git`
- Go 1.27 to build

## Install

```
sudo apt install qemu-system-x86 qemu-utils openssh-client git
sudo usermod -aG kvm $USER          # then log out and back in

git clone git@github.com:blackshieldpt/curral.git
cd curral
go build -o ~/.local/bin/curral ./cmd/curral
curral doctor
```

Install the binary at a stable path: QEMU and ssh call it back, so `go run` won't work.
`curral doctor` checks the host and says how to fix what's missing. Optional packages,
upgrading and uninstalling are in [docs/installation.md](docs/installation.md).

## Run

Store a Claude Code token (from `claude setup-token`) where curral can find it:

```
mkdir -p ~/.config/curral && touch ~/.config/curral/secrets.env && chmod 600 ~/.config/curral/secrets.env
echo 'CLAUDE_CODE_OAUTH_TOKEN=<token>' >> ~/.config/curral/secrets.env
```

Then:

```
curral up dev                   # create and start a VM from the default template
curral run dev claude           # Claude Code in a guest tmux session (reattaches if running)
curral ssh dev                  # a shell; curral ssh dev -- docker ps runs a command
curral attach dev               # the guest's main tmux session
curral cp ./notes.md dev:/tmp/  # copy files either way (-r for directories)
curral ls
curral stop dev
curral destroy dev
```

The first `up` of a template downloads and verifies the cloud image and builds its base,
which takes a few minutes. Later VMs from the template are copy-on-write overlays and
boot in seconds. `curral setup-ssh` lets plain `ssh curral-dev`, `scp` and VS Code
Remote-SSH reach VMs.

Work on a repository and get the result out through the host:

```
curral forge add github --type github --host github.com --mode deploy-key --token-secret GH_ADMIN_TOKEN
curral up api --repo github:acme/api
curral pr api --repo api --into ~/src/api
```

Or hand a prompt to an agent and get a pull request back:

```
curral task run --repo github:acme/api --prompt "fix issue #42" --into ~/src/api
curral task ls
```

The `default` template has Docker, Node 24, uv, `gh`, Claude Code, opencode, pi and Codex
(Codex without credentials until you set `agents.codex.auth`).
Write your own in `~/.config/curral/templates/`; see
[docs/templates.md](docs/templates.md).

## Documentation

| | |
|---|---|
| [Installation](docs/installation.md) | Requirements, install, first VM, upgrading |
| [CLI reference](docs/cli-reference.md) | Every command and flag |
| [Templates](docs/templates.md) and [configuration](docs/configuration.md) | What VMs contain; `config.yaml`, secrets, directories |
| [Agents](docs/agents.md), [forges](docs/forges.md), [tasks](docs/tasks.md) | Credentials, repos, `pull`/`pr`, SSH agents, headless runs |
| [Sessions](docs/sessions.md), [notifications](docs/notifications.md), [snapshots](docs/snapshots.md) | Recordings and audit, alerts, snapshots and base updates |
| [Security model](docs/security-model.md), [egress](docs/egress.md), [caches](docs/package-caches.md), [canaries](docs/canaries.md) | What curral protects, and its limits |
| [Deployment](docs/deployment.md) | The daemon, warm pool, idle stop, rebuilds, headless hosts, backups |
| [Troubleshooting](docs/troubleshooting.md) | Logs, error messages and recovery |
| [Development](docs/development.md) | Building and testing |

All pages: [docs/README.md](docs/README.md). Changes: [CHANGELOG.md](CHANGELOG.md).
Reporting vulnerabilities: [SECURITY.md](SECURITY.md). Design and history:
[PLAN.md](PLAN.md).
