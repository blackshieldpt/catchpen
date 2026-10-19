# curral

KVM sandboxes for coding agents. `curral` launches QEMU/KVM VMs from declarative
templates, with key-only SSH, pinned host keys and a default-deny egress allowlist.
See [PLAN.md](PLAN.md) for the full design and roadmap; this README covers what is
implemented (phase 1).

## Requirements

- Linux x86_64 with KVM (`/dev/kvm` read/write; add yourself to the `kvm` group)
- `qemu-system-x86`, `qemu-utils`, `openssh-client`
- Go 1.27 to build

```
sudo apt install qemu-system-x86 qemu-utils
go build -o ~/.local/bin/curral ./cmd/curral
curral doctor
```

Install the binary somewhere stable: QEMU and ssh call it back (`egress-stdio`,
`ssh-proxy`), so `go run` won't work.

## Quick start

```
curral up dev                 # default template: Docker, Node 24, uv, Claude Code, opencode, pi
curral ssh dev                # shell
curral ssh dev -- docker ps   # command
curral ls
curral stop dev
curral destroy dev
```

The first `up` for a template downloads and verifies the cloud image and builds a
template base (a few minutes). Later VMs from the same template are thin
copy-on-write overlays and boot in seconds.

Optional: `curral setup-ssh` adds an `Include` to `~/.ssh/config`, so `ssh curral-dev`,
`scp`, VS Code Remote-SSH etc. work directly.

## Templates

Built-in: `base`, `minimal`, `default` (`curral template ls`, `curral template show default`).
User templates live in `~/.config/curral/templates/<name>.yaml` and override built-ins by name.

```yaml
# ~/.config/curral/templates/go.yaml
description: Go development
extends: default
distro: debian-13            # ubuntu-24.04 | ubuntu-26.04 | debian-13 | custom (config.yaml)
resources: { cpus: 8, memory: 16G, disk: 60G }
packages: [protobuf-compiler]
runtimes: { go: "1.27" }     # node: "24", python: uv
docker: true
scripts: [scripts/setup.sh]  # run as root at base build, relative to this directory
files:
  - { src: files/gitconfig, dst: ~/.gitconfig, mode: "0644" }
ports: [8080, "9000:9000"]   # guest port, or host:guest; bound to 127.0.0.1
network:
  egress: allowlist          # allowlist | open | none
  presets: [go]
  allow: [registry.example.com:443]
  on_deny: log               # log | prompt
```

Lists (packages, scripts, presets, allow, ...) append to the parent template; scalars
override. Software is baked into the base image; changing it triggers a rebuild on the
next `up` of a new VM (`curral template build <name> --rebuild` to force).

Global defaults go in `~/.config/curral/config.yaml`:

```yaml
defaults:
  distro: ubuntu-24.04
  template: default
  resources: { cpus: 4, memory: 8G, disk: 40G }
  user: agent
images:                      # custom apt-based cloud images
  my-ubuntu:
    url: https://images.example.com/my-ubuntu.qcow2
    checksum_url: https://images.example.com/SHA256SUMS   # or sha256: / sha512: <hex>
```

A cached image is reused only while its URL (and pin, if any) is unchanged; editing
either downloads and verifies the new image, and builds a new base.

## Agents and credentials

Templates install Claude Code, opencode and pi from npm into the base (no
self-updates; rebuild the base for new versions). Credentials are resolved on the host
and pushed over SSH after every start (`curral creds push <name>|--all` for running
VMs); they never enter bases or seed images.

```yaml
agents:
  claude: { auth: oauth-token }          # or api-key; per VM: curral up --claude-auth api-key
  opencode: { keys: [ANTHROPIC_API_KEY, OPENROUTER_API_KEY] }
  pi: { version: "1.3.0", keys: [ANTHROPIC_API_KEY] }
env: { GH_TOKEN: "secret:GH_TOKEN", TZ: Europe/Lisbon }   # environment for every ssh session
mcp:
  - { name: docs, type: http, url: "https://mcp.example.com/mcp", headers: { Authorization: "secret:DOCS_AUTH" } }
  - { name: fs, type: stdio, command: [npx, -y, "@modelcontextprotocol/server-filesystem", /workspace] }
sync:
  git_identity: true                     # host git user.name/email (default)
  claude_config: false                   # host ~/.claude settings, CLAUDE.md, agents, skills, commands
```

- Claude Code: `oauth-token` uses `CLAUDE_CODE_OAUTH_TOKEN` (create it once with
  `claude setup-token`); `api-key` uses `ANTHROPIC_API_KEY`. The subscription token is
  only given to Claude Code.
- opencode and pi get provider API keys (`keys:`) in their own auth files, so an
  Anthropic API key for them doesn't override Claude's subscription token. Known keys:
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `GEMINI_API_KEY`,
  `GROQ_API_KEY`, `MISTRAL_API_KEY`, `XAI_API_KEY`, `DEEPSEEK_API_KEY`. Their API hosts,
  and remote MCP hosts, are added to the VM's allowlist.
- MCP servers are written to all three agents' configs (pi skips SSE servers).
- `secret:NAME` values come from, in order: `secrets:` in config.yaml, the environment
  variable `NAME`, or `~/.config/curral/secrets.env` (`NAME=value` lines, must be 0600).

```yaml
# config.yaml
secrets:
  CLAUDE_CODE_OAUTH_TOKEN: { command: [pass, show, claude/token] }
  ANTHROPIC_API_KEY: { env: MY_ANTHROPIC_KEY }
```

In the guest: env in `~/.ssh/environment`, Claude in `~/.claude.json`, opencode in
`~/.local/share/opencode/auth.json` and `~/.config/opencode/opencode.json`, pi in
`~/.pi/agent/{auth,mcp}.json`, all 0600. Removing a secret on the host removes it from
the environment on the next push; provider keys already in auth files stay until
replaced. With `sync.claude_config`, the guest's `~/.claude/settings.json` is replaced
on every push (minus `env`, hooks, credential helpers, status line and plugins).

## Forges and repos

```
curral forge add github --type github --host github.com --mode deploy-key --token-secret GH_ADMIN_TOKEN
curral forge add acme --type gitea --host git.acme.example --ssh-port 2222 \
    --mode minted-token --user bot --password-secret ACME_PASSWORD
curral forge test acme
curral up dev --repo github:acme/api --repo https://git.acme.example/team/web [--ro]
```

Forges live in `~/.config/curral/forges.yaml` (managed by `curral forge add/rm`). `forge
add` pins the SSH host key: GitHub's from `api.github.com/meta`, others scanned and
confirmed by you. Repos (`--repo`, or `repos:` in a template) are `forge:owner/repo` or a
URL whose host matches a forge, and are cloned into `/workspace/<repo>` at start.

| `--mode` | VM gets | Host-side token used for |
|---|---|---|
| `deploy-key` | a per-VM, per-repo SSH key (`--ro`: read-only) | adding/removing the key |
| `minted-token` (Gitea) | a per-VM token, minted with the account's password | minting/deleting it |
| `token` | your token (`--token-secret`), for HTTPS and `gh`/`tea` | — |
| `agent` | nothing; SSH signs with your key through a restricted agent (below) | — |
| `none` | nothing; anonymous clone, work leaves via `pull`/`pr` | opening PRs |

Deploy keys and minted tokens are removed when the VM is destroyed. In the VM, git SSH
goes through the egress proxy (`~/.ssh/curral_config`, pinned `known_hosts`), HTTPS
tokens sit in `~/.config/curral/git-credentials`, `gh` gets `GH_TOKEN` and `tea` a
login. Forge hosts are added to the VM's allowlist. Install the CLIs with
`tools: [gh, tea]` in a template (the default template has `gh`).

Getting work out without giving the VM push access:

```
curral pull dev --repo api --into ~/src/api     # -> refs/curral/dev/<branch>, hooks off, fsck on
curral pr dev --repo api --into ~/src/api       # pull, push with your git creds, open the PR
```

`pr` pushes `curral/<vm>/<branch>` through a remote of the host repo that points at the
same forge repo, then opens the PR with the forge's API token.

## SSH agent and port forwards

```yaml
# template
ssh:
  agent: restricted          # off (default) | restricted | host
  agent_keys: [~/.ssh/id_ed25519]
  confirm: true              # default: ask (ssh-askpass) on every use of a key
```

With `restricted`, `curral start` runs a per-VM `ssh-agent` holding only `agent_keys`,
each destination-constrained (`ssh-add -h`) so it signs only on the path
host → VM → that VM's `agent`-mode forges: the VM can't use it to log in anywhere else,
including back into itself. `ssh curral-<vm>` forwards that agent, never your own; it
ends when the VM stops. `--ssh-agent` / `--agent-key` on `curral up` override the
template. Constraints need the forge's SSH server to support OpenSSH host-bound auth:
github.com and Gitea behind OpenSSH do, gitlab.com and Gitea's built-in SSH server
don't. `forge add` / `forge test` check this for `agent`-mode forges.

`host` forwards your own agent (`$SSH_AUTH_SOCK`): anything in the VM can use all its
keys while you're connected. curral warns when you create such a VM.

```
curral forward dev -L 3000 -L 8080:80      # guest ports on the host's 127.0.0.1
curral forward dev -R 5432                 # host 127.0.0.1:5432 into the guest
curral forward dev -D 1080                 # SOCKS proxy on the host into the guest
curral forward dev --list | --stop
```

Each `forward` is a background `ssh -N` (log in `vms/<name>/forward.log`); all end when
the VM stops. `-R` bypasses the egress proxy, so it is recorded in the VM's egress log.

## Egress

In `allowlist` mode the VM has no direct network access: QEMU's user network runs with
`restrict=on`, and the only exit is an HTTP proxy at `10.0.2.100:3128` that curral serves
on the host per connection. The guest is preconfigured to use it (environment, apt,
Docker daemon and containers). DNS resolves on the host. Allowlisted names that resolve
to private/loopback addresses are blocked unless `allow_private` is set.

```
curral egress show dev                      # lists and settings
curral egress log dev -f                    # allowed and blocked requests
curral egress allow dev pypi.org files.pythonhosted.org   # applies immediately
curral egress deny dev evil.example.com     # denylist wins over allowlist
curral egress config dev --on-deny prompt   # ask instead of just logging
curral egress watch [--desktop]             # answer prompts (terminal or notifications)
```

Entries: `host`, `host:port`, `*.example.com` (subdomains), IPs. No port means 80 and 443.
Presets: `anthropic`, `github`, `npm`, `pypi`, `go`, `apt-ubuntu`, `apt-debian`,
`docker-apt`, `docker-hub`, `ghcr`.

With `on_deny: prompt`, a blocked request waits up to `prompt_timeout` (30s) for a
decision from `curral egress watch`; with no watcher it is denied immediately. Prompts
for requests that already timed out are dropped, and each VM can have at most 8
distinct destinations waiting (more are denied), so a guest can't bury you in prompts.

## Security notes

- Guest login is SSH public key only; passwords are locked. Each VM has its own client
  key and host key; the host key is generated on the host and pinned.
- SSH and port forwards bind to `127.0.0.1`. Other local users can connect to the port
  but cannot authenticate.
- No host directories are mounted into the VM.
- QEMU runs under its seccomp sandbox (`-sandbox on,obsolete=deny,resourcecontrol=deny`)
  and IPv6 is disabled on the guest network.
- `seed.iso` (0600) holds the VM's SSH host private key, generated on the host so it can
  be pinned; it holds no user credentials.
- Guest output shown on the host is filtered: first-boot errors drop control
  characters, and `curral console` drops OSC/DCS/APC sequences (title, clipboard).
- The generated `ssh_config` disables agent (unless `ssh.agent` is set), X11 and port
  forwarding and tunnels for VM hosts, overriding a `Host *` block in your own config.
  `curral forward` turns port forwarding back on only for its own sessions.
- Template bases are built with the same isolation: the build VM's only exit is the
  proxy, which allows any public host but no private/loopback addresses. Its requests
  are logged to `bases/<base>.egress.log`.
- `network.egress: open` gives the guest unfiltered internet and the host's 127.0.0.1
  services; `curral up` requires `--allow-open` for such templates.
- The proxy caps request headers (64 KiB), concurrent connections per VM (128) and idle
  tunnels (10 min), and accepts only standard HTTP methods. Each guest connection,
  including rejected ones, starts one short-lived `curral egress-stdio` process (about
  3 ms and 11 MiB RSS each; so at most ~1.4 GiB per VM at the connection cap).
- `egress.log` rotates at 32 MiB, keeping one old file (`egress.log.1`): a guest
  making many denied requests can push older entries out. An allowed connection whose
  log entry can't be written is refused.
- Cloud images are verified against the distro's checksum file over HTTPS (or a pinned
  digest); GPG signatures of the checksum files are not checked yet. Images with a
  backing file or a qcow2 external data file are refused. Node, Go and uv are checked
  against checksums from their own download sites (no extra protection beyond TLS);
  Docker's apt key is fingerprint-pinned.
- Allowlist entries match the requested name, not the resolved address, and CDN hosts
  on the allowlist (e.g. `*.githubusercontent.com`) can be used for domain fronting.
- The agent user has passwordless sudo inside the VM; the VM is the boundary.
- `--rebuild` refuses to replace a base that existing VMs use (their disks are overlays
  on it); destroy those VMs first.

## Layout

```
~/.config/curral/            config.yaml, templates/, ssh_config
~/.local/share/curral/
  images/                    verified cloud images (+ .img.json: source URL and digest)
  bases/                     template bases (+ build logs)
  vms/<name>/                disk overlay, seed, keys, allowlist, egress.log, qemu.log
```

Build problems: `curral --debug up ...` streams the whole build-script log instead of
just progress lines. Logs are in `~/.local/share/curral/bases/`: `*.build.log` (build
script output), `*.console.log` (kernel and cloud-init console, useful when the build
fails before the script starts) and `*.egress.log` (the build's network requests).
A build is aborted if either serial log passes 256 MiB. Boot problems:
`curral console <name>`, or `curral --debug start <name>`, which logs the serial console
to `vms/<name>/serial.sock.log` and leaves a VM that fails to boot running for inspection.

## Development

```
go test ./...                                         # unit tests
go test -tags integration -timeout 30m ./internal/vm/ # boots a real VM (KVM + network)
```
