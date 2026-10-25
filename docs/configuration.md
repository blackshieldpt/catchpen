# Configuration

curral reads its settings from `~/.config/curral/config.yaml`. The file is optional: a
missing or empty file means the defaults below. Unknown keys are errors. `curral init`
writes a commented `config.yaml` and `secrets.env` listing the settings and secret
names, and the commented [template](templates.md) category files; it keeps files that
exist. What VMs contain (distro, size, software, network) is set in those category
files, not here.

## Directories

| Variable | Effect |
|---|---|
| `CURRAL_HOME` | When set, config goes in `$CURRAL_HOME/config`, data in `$CURRAL_HOME/data` and caches in `$CURRAL_HOME/cache`. |
| `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME` | Used when set to an absolute path; otherwise `~/.config`, `~/.local/share` and `~/.cache`. curral adds `/curral` to each. |

```
~/.config/curral/
  config.yaml                this file
  secrets.env                NAME=value secrets (must be 0600)
  forges.yaml                managed by curral forge add/rm
  ssh_config                 generated; curral setup-ssh includes it from ~/.ssh/config
  templates/<category>.yaml  your template sections (distros.yaml, runtimes.yaml, ...)
~/.local/share/curral/
  images/                    verified cloud images (+ .img.json: source URL and digest)
  bases/                     template bases, one file per generation, and build logs
  vms/<name>/                disk overlay, seed, keys, allowlist, egress.log, qemu.log, ...
  vms/pool-<id>/             warm pool VMs
  recordings/<name>/         session recordings (asciicast v2)
  transcripts/<name>/        agent session stores copied from the VM
  tasks/<id>/                task.json, prompt.md, output.jsonl, result.md, supervisor.log
  audit.jsonl                host audit log
  daemon.lock, rebuild.stamp curral daemon state
~/.cache/curral/pkg/         package caches (apt, go, npm, pypi, docker)
~/.cache/curral/sources/     pinned git repos of skills and plugins
```

curral creates these directories with mode 0700.

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
```

| Key | Default | Notes |
|---|---|---|
| `defaults.user` | `agent` | Matches `^[a-z_][a-z0-9_-]{0,31}$`; `root` is refused. |
| `images.<name>` | | `url` must be HTTPS, with exactly one of `checksum_url` (HTTPS), `sha256` or `sha512`. A custom name shadows a built-in distro. Use it with a section in `templates/distros.yaml`, e.g. `my-ubuntu: { distro: my-ubuntu }`, and `-t my-ubuntu`. |
| `secrets.<NAME>` | | Exactly one of `command` (argv, 30 s timeout, trailing newline trimmed) or `env`. |
| `record` | `true` | `false` turns session recording off. |
| `notify` | no sinks | See [notifications.md](notifications.md). |
| `tasks.max_vms` | `2` | All running curral VMs count against it. |
| `tasks.max_memory` | no limit | A size such as `24G`. |
| `idle_stop` | off | A template can override it (`idle.yaml`). See [deployment.md](deployment.md#idle-stop). |
| `rebuild` | off | Needs the daemon. |
| `pool` | none | `template` is a `-t` spec (default: every category's default), `size` is 0 to 32. Each template may appear once. A pool VM serves only the same spec, written the same way. |
| `cache_max` | `20G` | Least recently used objects go first. |
| `canary_action` | `pause` | See [canaries.md](canaries.md). |

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
3. `~/.config/curral/secrets.env`.

`secrets.env` holds `NAME=value` lines, optionally prefixed with `export ` and with the
value in matching quotes. Lines starting with `#` and blank lines are skipped; there is
no inline comment or escape handling. The file must have no group or other permission
bits (`chmod 600`). Values with a newline, CR or NUL are refused.

The daemon runs as a systemd user service and doesn't see your shell's environment, so
secrets it needs (notification tokens) belong in
`secrets.env` or a `command:` source.

## forges.yaml

`curral forge add` and `curral forge rm` manage `~/.config/curral/forges.yaml` (mode
0600). You can read it, but prefer the commands, which pin host keys for you. See
[forges.md](forges.md).
