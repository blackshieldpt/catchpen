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
curral run dev claude         # agent in a guest tmux session (reattaches if running)
curral attach dev             # the guest's main tmux session
curral cp ./notes.md dev:/tmp/  # scp, either direction (-r for directories)
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
cache: [apt, go, npm]        # host package caches (default: all five); [] turns them off
canaries: [aws, env, git]    # decoy credentials (default template: all three)
```

Lists (packages, scripts, presets, allow, ...) append to the parent template; scalars
override, and so do `cache` and `canaries`. Software is baked into the base image; changing it triggers a rebuild on the
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

Templates install Claude Code, opencode and pi from npm, and omp (oh-my-pi) from its
release binary (checked against its SHA256SUMS), into the base (no self-updates;
rebuild the base for new versions). Credentials are resolved on the host
and pushed over SSH after every start (`curral creds push <name>|--all` for running
VMs); they never enter bases or seed images.

```yaml
agents:
  claude: { auth: oauth-token }          # or api-key; per VM: curral up --claude-auth api-key
  opencode: { keys: [ANTHROPIC_API_KEY, OPENROUTER_API_KEY] }
  pi: { version: "1.3.0", keys: [ANTHROPIC_API_KEY] }
  omp: { version: "18.12.5", keys: [OPENROUTER_API_KEY] }   # needs no runtimes.node
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
- opencode, pi and omp get provider API keys (`keys:`) in their own files, so an
  Anthropic API key for them doesn't override Claude's subscription token. Known keys:
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `GEMINI_API_KEY`,
  `GROQ_API_KEY`, `MISTRAL_API_KEY`, `XAI_API_KEY`, `DEEPSEEK_API_KEY`. Their API hosts,
  and remote MCP hosts, are added to the VM's allowlist.
- MCP servers are written to all the agents' configs (pi skips SSE servers).
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
`~/.pi/agent/{auth,mcp}.json`, omp in `~/.omp/agent/.env` and `mcp.json`, all 0600. Removing a secret on the host removes it from
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
keys while you're connected. curral warns when you create such a VM, and `agent`-mode
forges require `restricted` (start-time clones would otherwise forward your agent
unattended).

```
curral forward dev -L 3000 -L 8080:80      # guest ports on the host's 127.0.0.1
curral forward dev -R 5432                 # host 127.0.0.1:5432 into the guest
curral forward dev -D 1080                 # SOCKS proxy on the host into the guest
curral forward dev --list | --stop
```

Each `forward` is a background `ssh -N` (log in `vms/<name>/forward.log`); all end when
the VM stops. `-R` bypasses the egress proxy, so it is recorded in the VM's egress log.

## Sessions, recordings and audit

`curral run <vm> claude|opencode|pi|omp [-- args]` starts the agent in a guest tmux
session named after it, in the VM's repo when it has exactly one (`/workspace` with
several, home with none), and reattaches to it later (new arguments and `yolo` only
apply when the session starts); `curral attach <vm>` does the same for a `main` shell
session. VMs created before `curral run` existed need recreating for it. The guest tmux prefix is `C-a`, so it doesn't clash
with a host tmux. `agents.<name>.yolo: true` in a template starts claude
(`--dangerously-skip-permissions`) or opencode (`--auto`) without permission prompts;
pi doesn't ask.

`curral tmux [<vm>...]` opens a host tmux session `curral` with a window per running
VM, each running `curral attach`; `start`/`stop` add and close windows while it exists,
`curral tmux --sync` reconciles it. Windows you open there yourself are left alone.

`curral up <vm> --ephemeral [-- command]` creates a new VM, attaches (or runs the
command) and destroys it, with its deploy keys and tokens, when the session ends or
curral gets SIGINT/SIGTERM/SIGHUP. `ls` marks ephemeral VMs, so one left by a crash is
easy to spot.

Interactive `ssh`, `run` and `attach` sessions run the system ssh on a host pty and
record its output (not your keystrokes) as asciicast v2 in
`recordings/<vm>/<id>.cast` (0600, 256 MiB cap per session, kept after `destroy`).
The guest can't alter them. `curral recordings <vm>` lists them,
`curral recordings <vm> play <id> [--speed 2]` replays one with title and clipboard
sequences dropped. `asciinema play` also reads them but replays guest escape sequences
(window title, clipboard writes) unfiltered. `record: false` in `config.yaml` turns
recording off.

`curral audit [<vm>] [-f]` shows the host audit log (`audit.jsonl`, rotated at 32 MiB):
VM lifecycle, credentials pushed (secret names only), deploy keys and tokens added and
removed, restricted agents, allowlist edits and answered prompts (timeouts are only in
the VM's egress log), forwards, `cp`, `pull`/`pr`, and the start and end of every
`ssh`/`run`/`attach` session, with its recording id when recorded.

## Headless tasks

```
curral task run --repo github:acme/api --prompt "fix issue #42" [--agent claude|opencode|pi|omp]
    [--prompt-file f.md] [--timeout 2h] [--max-budget-usd 5] [--out pr|bundle|patch|none]
    [--into ~/src/api] [--rw] [--keep] [-f]
curral task run --agent cmd --cmd 'make fix' --prompt "..."   # any command, prompt on stdin
curral task run --file tasks.yaml [--parallel 2]
curral task ls | show <id> | logs <id> [-f] [--raw] | cancel <id> | rm <id>
```

Each task runs in a new VM (`task-<id>`, read-only deploy keys unless `--rw`) under a
background supervisor: the agent works on branch `curral/<id>` without permission
prompts (claude `-p --output-format stream-json`, `opencode run --format json --auto`,
`pi -p --mode json`), whatever it leaves uncommitted is committed, and the work leaves
from the host: `--out pr` (the default with a repo) pushes `curral/task-<id>/curral/<id>`
through a remote of `--into` (default: the current directory) with your credentials and
opens a PR with the prompt and result; `bundle` fetches into `refs/curral/...` of that
repo; `patch` writes an mbox next to the task. The VM is then destroyed (`--keep`
leaves it), except when the export fails: then it is kept with the commits and the
task's error says how to pull them. A task whose supervisor died shows as `lost`;
`cancel` or `rm` destroys its VM. The agent's output is captured on the host in
`tasks/<id>/output.jsonl`, its final message in `result.md`, and turns, tokens and cost
in `task.json`. Tasks wait for the budget in `config.yaml`:

```yaml
tasks: { max_vms: 2, max_memory: 24G }   # all running curral VMs count
```

A batch file has `defaults:`, `tasks:` (each with `prompt` or `prompt_file`, and any of
`agent`, `repo`, `template`, `timeout`, `out`, `into`, `title`, `rw`, `keep`, `cmd`,
`max_budget_usd`) and an optional `matrix: { agent: [...], repo: [...] }` that runs each
task once per combination.

## Transcripts

The agents' session stores (claude `~/.claude/projects`, opencode's `opencode.db`, pi
`~/.pi/agent/sessions`; not their credentials) are copied to
`transcripts/<vm>/<time>/` when a task ends, before `destroy`, and on
`curral transcripts <vm>`, which also lists earlier copies. They come from the guest,
so only regular files and directories are unpacked (no symlinks, nothing outside the
directory, at most 1 GiB and 200,000 entries).

## Notifications

```yaml
notify:
  sinks:
    - desktop                                   # notify-send
    - { ntfy: { url: https://ntfy.sh, topic: my-curral, token: secret:NTFY_TOKEN } }
    - { webhook: { url: https://hooks.example/curral, headers: { Authorization: secret:HOOK_AUTH } } }
  events: [task.done, task.failed, egress.prompt, agent.waiting, vm.idle_stop, canary.hit]   # default: all
```

`egress.prompt` fires while `curral egress watch` (or the daemon) waits for your answer. For
`agent.waiting`, Claude's `Notification`/`Stop` hooks, an opencode plugin and a pi
extension (written at each start) POST to `10.0.2.101:80` in the guest, which reaches
`curral notify-stdio` on the host. The guest holds no sink credentials; its text is
stripped of control characters, cut to 300 characters and limited to a burst of 5 per
VM, then one per 30 s. Task VMs' hooks are ignored (the task's result is notified).

## Snapshots, reset and base updates

```
curral snapshot dev [before-upgrade]    # running: memory + disk; --disk-only for the disk
curral snapshots dev
curral restore dev before-upgrade
curral rmsnap dev before-upgrade
curral template rebuild --all           # fresh cloud image + packages, as a new base
curral reset dev [--latest]             # fresh disk, first boot again (--latest: new base)
curral rebase dev                       # onto the current base, contents unchanged
curral gc [--dry-run]
```

Snapshots are stored inside the VM's disk. Restoring a memory snapshot resumes the VM
where it was (starting it if stopped), sets the guest clock and pushes credentials
again; a disk-only snapshot needs the VM off, so a running VM is restarted. Memory is
only loaded into the QEMU version and machine type it was saved from: after a QEMU
upgrade, `restore --disk-only` still restores such a snapshot's disk. Restore and
reset copy the agents' transcripts out first, and refuse while the VM runs a task.

A rebuild never replaces a base in use: it adds a generation (`<tpl>-<hash>.2.qcow2`)
that new VMs use (the one it supersedes is removed if no VM uses it), and `curral ls`
marks VMs on an older one `stale-base`. `reset`
discards the VM's disk (keys, forges and settings stay; repos are cloned again);
`reset --latest` does so on the new base, which is how a VM gets the new packages.
`rebase` copies what differs into the VM's disk so the guest sees exactly the same
files, without new packages, and frees the old base; it deletes the VM's snapshots.
`gc` removes bases no VM uses (each template's current base stays) and leftovers of
interrupted builds, downloads, resets and destroys, and trims the package caches to
`cache_max`; recordings, transcripts, tasks and the audit log are never removed.

## Daemon, warm pool and idle stop

```yaml
# config.yaml
idle_stop: 30m          # stop VMs idle this long (default off; templates: idle_stop: 2h | off)
rebuild: weekly         # rebuild the bases in use: off (default), daily, weekly or e.g. 72h
pool:
  - { template: default, size: 2 }
  - { template: go, distro: debian-13, size: 1 }
```

```
curral daemon install   # systemd user service (curral daemon runs it in the foreground)
curral daemon status | uninstall
curral pool ls | fill | drain
```

The daemon refills the pool, stops idle VMs, rebuilds bases on schedule and answers
egress prompts of VMs with `on_deny: prompt` through desktop notifications (VMs a
`curral egress watch` started earlier serves are left to it; one started later fails
for the VMs the daemon serves). Everything works without it; it reads config.yaml
again each round. The service doesn't see your shell's environment: notification
tokens it needs belong in `secrets.env` or a `command:` source.

Pool VMs go through first boot with their own keys but no repos, agents, credentials
or allowlist, then are saved with their memory and stopped, so they use disk, not RAM.
`up` and `task` claim one when the template, distro, resources and base match (not
with `--rebuild`): it is renamed, gets its allowlist, forges and credentials, and
resumes in seconds with the new hostname; each saved memory is resumed once. After a
QEMU upgrade a pool VM boots normally instead. `pool fill` (and the daemon) replaces
pool VMs on an old base or template version; `curral ls` doesn't list them. After a
scheduled rebuild the daemon also runs `gc`.

A VM is idle when nothing is connected to its ssh or forwarded ports (sessions, `run`,
`forward`, editors over ssh), it runs no task and QEMU used under 5% of a core since
the previous check (every minute). It is shut down gracefully; ephemeral VMs are left
alone.

## Package caches

```yaml
# config.yaml
cache_max: 20G          # default; least recently used objects are removed past it
```

```
curral cache            # what each cache holds
curral cache clear [apt|go|npm|pypi|docker ...]
```

VMs of templates listing `cache:` fetch packages through curral on the host at
`10.0.2.102`, which keeps what never changes in `~/.cache/curral/pkg/` for every other VM
and base build: `.deb` files (from the Ubuntu and Debian archive and security mirrors),
Go module files and checksum database tiles, npm tarballs, PyPI files and Docker Hub
layers and manifests by digest. Indexes (apt lists, npm and PyPI metadata, tags, Go
version lists) are fetched every time. The guest is preconfigured: apt, `GOPROXY`,
npm, yarn and pnpm, pip and uv, and Docker's registry mirror (added to the base's
`daemon.json`). Cached hosts need no allowlist entry, but `egress deny` applies; their
fetches appear in `egress log` as `CACHE`. Other mirrors, registries and private indexes
still go through the egress proxy. Base builds use the template's caches too (Docker
excepted) without changing the base; VMs with `egress: none` use none.

The caches trim themselves to `cache_max` every 10 minutes while fetching (and `gc`
does), and store nothing that would leave less than 5% of the filesystem free; objects
the server sends without a length are passed through, not stored.

## Canaries

```yaml
# config.yaml
canary_action: pause    # default; stop | log
```

Templates listing `canaries:` get decoy credentials in the agent user's home, made for
each VM and pointing at hosts that exist nowhere: `aws` (`~/.aws/credentials` and an
`endpoint_url` in `~/.aws/config`; left out when the template's `env:` sets `AWS_*`),
`env` (`~/.env.production` with a database URL, an API URL and token, a Stripe-style key)
and `git` (`~/.git-credentials`). Nothing in the VM uses them, so their use means
something went looking for credentials:

- a connection to a canary host through the egress proxy, allowlisted or not;
- a canary value in a plain-HTTP request's line or headers (not its body), or in a
  package cache request;
- a value or canary host in the commits (merges and binary files included) and patches
  `pull`, `pr` and tasks bring out, or in a PR's title and body.

The request or export is refused, `canary.hit` goes to the audit log and to
notifications (a few per minute at most), and the VM is paused, frozen as it was, until
`curral resume <name>` (which also sets its clock); `curral ls` shows it `paused`. A task
whose VM is paused fails at once and keeps the VM.

What goes unseen: values sent encoded, or over HTTPS to an allowed host; clients that
don't use the proxy (database drivers for the `DATABASE_URL`, Node's and Bun's `fetch`),
whose connections just fail; everything in VMs with `egress: open` or `none` except the
exports; `curral cp` and `forward`. The decoys can be told from real ones by a careful
attacker (the hosts don't resolve, the GitHub token's checksum is wrong, the seed's
user-data lists them), and if an agent prints them they end up in transcripts and
recordings, where secret scanners may flag them.

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
- The package caches fetch over HTTPS from their fixed upstreams and the CDNs those
  redirect to, never from private addresses, and accept only GET and HEAD: a cache is
  about as open as allowlisting its preset. proxy.golang.org and sum.golang.org fetch
  any module path they're asked for, as with the `go` preset. Guests can't put anything
  in the store; Docker objects and apt by-hash files are checked against their digest,
  the rest by the clients (apt signatures, Go checksum database, lockfile hashes). The
  store is shared by all VMs: a VM can tell whether a package was cached (timing).
- The agent user has passwordless sudo inside the VM; the VM is the boundary.
- Memory snapshots contain whatever was in the guest's memory, credentials included;
  they live inside the VM's disk (in its `0700` directory) and go away with it, `reset`
  or `rebase`.

## Layout

```
~/.config/curral/            config.yaml, templates/, ssh_config
~/.local/share/curral/
  images/                    verified cloud images (+ .img.json: source URL and digest)
  bases/                     template bases, one file per generation (+ build logs)
  vms/<name>/                disk overlay, seed, keys, allowlist, egress.log, qemu.log
  vms/pool-<id>/             warm pool VMs
  daemon.lock, rebuild.stamp curral daemon
  recordings/<name>/         host-side session recordings (asciicast v2)
  transcripts/<name>/        agent session stores copied from the VM
  tasks/<id>/                task.json, prompt.md, output.jsonl, result.md, supervisor.log
  audit.jsonl                host audit log
~/.cache/curral/pkg/         package caches (apt, go, npm, pypi, docker)
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
