# Configuration

catchpen reads its settings from `~/.config/catchpen/config.yaml`. The file is optional: a
missing or empty file means the defaults below. Unknown keys are errors. `catchpen init`
writes a commented `config.yaml` and `secrets.env` listing the settings and secret
names, and the commented [template](templates.md) category files; it keeps files that
exist. What VMs contain (distro, size, software, network) is set in those category
files, not here.

## Directories

| Variable | Effect |
|---|---|
| `CATCHPEN_HOME` | When set, config goes in `$CATCHPEN_HOME/config`, data in `$CATCHPEN_HOME/data` and caches in `$CATCHPEN_HOME/cache`. |
| `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME` | Used when set to an absolute path; otherwise `~/.config`, `~/.local/share` and `~/.cache`. catchpen adds `/catchpen` to each. |

```
~/.config/catchpen/
  config.yaml                this file
  secrets.env                NAME=value secrets (must be 0600)
  forges.yaml                managed by catchpen forge add/rm
  clients.yaml               API clients, managed by catchpen daemon client add/rm (0600: holds keys)
  server.key, server.crt     the API's TLS key and certificate, made by catchpen daemon info
  ssh_config                 generated; catchpen setup-ssh includes it from ~/.ssh/config
  templates/<category>.yaml  your template sections (distros.yaml, runtimes.yaml, ...)
~/.local/share/catchpen/
  images/                    verified cloud images (+ .img.json: source URL and digest)
  bases/                     template bases, one file per generation, and build logs
  vms/<name>/                disk overlay, seed, keys, allowlist, egress.log, qemu.log, ...
  vms/pool-<id>/             warm pool VMs
  recordings/<name>/         session recordings (asciicast v2)
  transcripts/<name>/        agent session stores copied from the VM
  tasks/<id>/                task.json, prompt.md, output.jsonl, result.md, supervisor.log
  audit.jsonl                host audit log
  daemon.lock, rebuild.stamp catchpen daemon state
  api.sock                   the API's socket, while catchpen daemon serves it (server:)
~/.cache/catchpen/pkg/         package caches (apt, go, npm, pypi, docker)
~/.cache/catchpen/sources/     pinned git repos of skills and plugins
```

catchpen creates these directories with mode 0700.

## Reference

```yaml
defaults:
  user: agent                   # the guest user; not root

images:                         # custom apt-based cloud images; pick one with a distros.yaml section
  my-ubuntu:
    url: https://images.example.com/my-ubuntu.qcow2
    checksum_url: https://images.example.com/SHA256SUMS   # or sha256: / sha512: <hex>

secrets:                        # where secret:NAME values come from
  CLAUDE_CODE_OAUTH_TOKEN: { command: [pass, show, claude/token] }
  ANTHROPIC_API_KEY: { env: MY_ANTHROPIC_KEY }

record: true                    # host-side recordings of interactive sessions

notify:                         # see notifications.md
  sinks: [desktop]
  events: [task.done, task.failed, egress.prompt, agent.waiting, vm.idle_stop, canary.hit]

tasks: { max_vms: 2, max_memory: 24G }   # budget for parallel tasks

idle_stop: off                  # stop idle VMs: off, daily, weekly or a duration >= 1m
rebuild: off                    # daemon rebuilds bases in use: off, daily, weekly or a duration >= 1m
pool:                           # warm pool kept by the daemon; template is a -t spec
  - { template: default, size: 2 }
  - { template: "go-1.27,large", size: 1 }

cache_max: 20G                  # package cache size
canary_action: pause            # pause, stop or log
server: {}                      # the daemon serves the API on its unix socket;
                                # server: { listen: "127.0.0.1:7443" } also serves it
                                # over TLS 1.3 to enrolled clients
```

| Key | Default | Notes |
|---|---|---|
| `defaults.user` | `agent` | Matches `^[a-z_][a-z0-9_-]{0,31}$`; `root` is refused. |
| `images.<name>` | | `url` must be HTTPS, with exactly one of `checksum_url` (HTTPS), `sha256` or `sha512`. A custom name shadows a built-in distro. Use it with a section in `templates/distros.yaml`, e.g. `my-ubuntu: { distro: my-ubuntu }`, and `-t my-ubuntu`. |
| `secrets.<NAME>` | | Exactly one of `command` (argv, 30 s timeout, trailing newline trimmed) or `env`. |
| `record` | `true` | `false` turns session recording off. |
| `notify` | no sinks | See [notifications.md](notifications.md). |
| `tasks.max_vms` | `2` | All running catchpen VMs count against it. |
| `tasks.max_memory` | no limit | A size such as `24G`. When no VM is running or starting for a task, a task starts anyway, even one that needs more. |
| `idle_stop` | off | A template can override it (`idle.yaml`). See [deployment.md](deployment.md#idle-stop). |
| `rebuild` | off | Needs the daemon. |
| `pool` | none | `template` is a `-t` spec (default: every category's default), `size` is 0 to 32. Each template may appear once. A pool VM serves only the same spec, written the same way. |
| `cache_max` | `20G` | Least recently used objects go first. |
| `canary_action` | `pause` | See [canaries.md](canaries.md). |
| `server` | not set | Set it (`server: {}`) and `catchpen daemon` serves the API on `api.sock` in the data directory, mode 0600, to its own user only. A bare `server:` is the same as not setting it. Read when the daemon starts: restart it after a change (`systemctl --user restart catchpen`). The API so far reads: its version (`GET /v1/info`), the VMs (`GET /v1/vms`, `GET /v1/vms/<name>`: state, template, size, egress mode, the ports QEMU forwards; warm pool VMs marked `pool`), a VM's snapshots (`GET /v1/vms/<name>/snapshots`), the template categories and their sections (`GET /v1/templates`, without the sections' settings), the headless tasks (`GET /v1/tasks`, `GET /v1/tasks/<id>`, without their prompts or commands), the audit log a page at a time, newest first (`GET /v1/audit?vm=<name>&limit=<1-1000, default 100>&before=<the previous page's next>`) and what the package caches hold (`GET /v1/cache`). It also creates, starts, stops (`force` to power off at once), destroys and resumes VMs (`POST /v1/vms`, `POST /v1/vms/<name>/start`, `/stop`, `/destroy`, `/resume`; create takes `catchpen up`'s options but `--ssh-agent`, `--agent-key` and `--ephemeral`, and no repo with any of `@ ? # %`; a destroy needs `"confirm": true` and the snapshots, pause and canary hit `GET /v1/vms/<name>/destroy` lists, with what else it loses; stopping a paused VM needs `"force": true`; a VM whose template sets `ssh.agent` other than `off` is neither created nor started, as that loads or forwards your SSH keys: use the CLI). It also resets, rebases and pushes credentials to VMs (`POST /v1/vms/<name>/reset`, `/rebase`, `/push`) and saves, restores and deletes their snapshots (`POST /v1/vms/<name>/snapshots`, `/snapshots/<snap>/restore`, `/snapshots/<snap>/delete`); [snapshots.md](snapshots.md#over-the-api) says what each needs. Each is a job (`GET /v1/jobs`, `GET /v1/jobs/<id>?after=<lines read>`) that runs on when its client goes. Jobs live in the daemon's memory: 16 run at most, the last 32 that ended are kept with their last 500 progress lines, a restart forgets them and stopping the daemon cancels those running. A job's error and progress can name host paths and quote the guest, as `catchpen up` prints them; once they are found to carry the VM's decoy credentials, every line and the error read `(withheld: …)` and the hit is acted on (see [canaries.md](canaries.md) for what is found). It also reads and changes a VM's egress lists and settings, and lists, opens (a job) and stops its forwards: see [egress.md](egress.md#over-the-api) and [forges.md](forges.md#port-forwards). The daemon doesn't see your shell's environment: a secret it reads from there for the CLI must be in `secrets.env` or a `command:` source for a VM the API creates or starts. A VM the API starts keeps running when the daemon stops. The audit events these calls cause carry `client`: the enrolled client's name, or `@local` over the socket. It never returns a VM's environment, forge credentials, agent settings or `canaries.json`, a template section's settings, or a task's prompt or command. A task's `--repo` with a user or password (any `@`, `git@host:o/r` included) is shown without them, and that task's error is withheld (`catchpen task show` prints it); other text loses a plain `scheme://user:password@` URL's user and password (not a token in a query). A task's output and errors, and audit details, can name host paths, as `catchpen task ls` and `catchpen audit` show them; a `canary.hit` event names the host the canary was sent to. |
| `server.listen` | not set | A `host:port`, quoted (`":7443"` for every interface; `"[::1]:7443"` for IPv6), where the daemon also serves the API over TLS 1.3, with the certificate `catchpen daemon info` shows, to clients enrolled with `catchpen daemon client add`: by their certificate, or by requests signed with their HMAC key. It refuses to start with no client enrolled. At most 64 connections at once, 16 from one IPv4 address or IPv6 /64; once a request's headers are in, it has 30 s to send its body and 30 s, from when it is authenticated, for its answer to be read, and a refused one closes its connection. A connection is closed 60 s after it opened unless a request on it has been authenticated by then. |

Sizes are a number followed by `M`, `G` or `T` (MiB, GiB, TiB).

## Distros

The built-in distros, each picked with the `distros.yaml` section of the same name
(`-t debian-13`):

| Name | Image |
|---|---|
| `ubuntu-24.04` | `cloud-images.ubuntu.com/noble/current`, checked against its `SHA256SUMS` |
| `ubuntu-26.04` | `cloud-images.ubuntu.com/resolute/current`, checked against its `SHA256SUMS` |
| `debian-13` | `cloud.debian.org/images/cloud/trixie/latest` (genericcloud), checked against its `SHA512SUMS` |

All are amd64 and apt-based with cloud-init; custom images must be too. A cached image
is reused only while its URL and pin are unchanged; editing either downloads and verifies
the new image and builds new bases.

## Secrets

`secret:NAME` values in templates, forges and notification sinks are resolved on the
host, in this order:

1. a `secrets.NAME` source in config.yaml (if it gives an empty value, the lookup stops
   there);
2. the environment variable `NAME`;
3. `~/.config/catchpen/secrets.env`.

`secrets.env` holds `NAME=value` lines, optionally prefixed with `export ` and with the
value in matching quotes. Lines starting with `#` and blank lines are skipped; there is
no inline comment or escape handling. The file must have no group or other permission
bits (`chmod 600`). Values with a newline, CR or NUL are refused.

The daemon runs as a systemd user service and doesn't see your shell's environment, so
secrets it needs (notification tokens) belong in
`secrets.env` or a `command:` source.

## forges.yaml

`catchpen forge add` and `catchpen forge rm` manage `~/.config/catchpen/forges.yaml` (mode
0600). You can read it, but prefer the commands, which pin host keys for you. See
[forges.md](forges.md).
