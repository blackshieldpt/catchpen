# Templates

A template describes a VM: its distro, resources, software, network policy, agents,
credentials and repos. Software is baked into a **base** image built from the template;
VMs are thin copy-on-write overlays on it.

```
curral template ls                  # built-in and user templates
curral template show default        # a template after extends is applied
curral template build go [--rebuild]
curral template rebuild --all       # fresh cloud image and packages, as a new base
```

## Where templates live

User templates are `~/.config/curral/templates/<name>.yaml` and shadow a built-in of
the same name. `curral init` writes `example.yaml` there: the `default` template's
settings, with the options `default` doesn't set commented out. Names match
`^[a-z0-9][a-z0-9_-]{0,31}$`; an optional `name:` field must equal the file name.
Relative paths in `scripts` and `files[].src` resolve inside the template's own
directory; absolute paths and `..` are rejected. Unknown keys are errors.

Built-ins:

| Template | Contents |
|---|---|
| `base` | `ca-certificates curl git jq tmux ripgrep unzip less file htop netcat-openbsd`; `egress: allowlist` with the `anthropic` preset. |
| `minimal` | `base` with 2 CPUs, 4G memory, 20G disk. |
| `default` | `base` with 4 CPUs, 8G, 40G; `build-essential make python3 python3-venv pkg-config`; Docker; `gh`; Node 24 and uv; Claude Code (OAuth token), opencode, pi and Codex (installed with `auth: none`: no credentials until a template sets `agents.codex.auth`); the `github npm pypi go apt-ubuntu apt-debian docker-apt docker-hub ghcr` presets; all five package caches; all three canaries. |

## Example

```yaml
# ~/.config/curral/templates/go.yaml
description: Go development
extends: default
distro: debian-13
resources: { cpus: 8, memory: 16G, disk: 60G }
packages: [protobuf-compiler]
runtimes: { go: "1.27" }
docker: true
scripts: [scripts/setup.sh]      # run as root at base build
files:
  - { src: files/gitconfig, dst: ~/.gitconfig, mode: "0644" }
ports: [8080, "9000:9000"]
network:
  egress: allowlist
  presets: [go]
  allow: [registry.example.com:443]
  on_deny: log
cache: [apt, go, npm]
canaries: [aws, env, git]
idle_stop: 2h
```

## Fields

| Field | Values |
|---|---|
| `extends` | One parent template. Cycles are errors. |
| `description` | Free text; not inherited. |
| `distro` | `ubuntu-24.04`, `ubuntu-26.04`, `debian-13`, or a name from `images:` in config.yaml. |
| `resources` | `cpus` (0 to 256), `memory`, `disk`: sizes like `512M`, `8G`, `1T`. |
| `packages` | apt package names, optionally `name=version`. |
| `docker` | `true` installs Docker and configures it for the proxy and caches. |
| `runtimes` | `node: "24"` (a major version, quoted), `go: "1.27"` or `"1.27.1"`, `python: uv`. |
| `scripts` | Scripts run as root at base build, in order. |
| `files` | `{src, dst, mode}`: copied in at base build. `dst` is absolute or `~/`; `mode` like `"0644"`. |
| `tools` | `gh`, `tea`. |
| `ports` | `"guest"` or `"host:guest"`, bound to `127.0.0.1`. Host port 0 or omitted picks a free port at start. |
| `network` | `egress` (`allowlist`, `open`, `none`), `presets`, `allow`, `on_deny` (`log`, `prompt`), `prompt_timeout` (default `30s`), `allow_private`. See [egress.md](egress.md). |
| `agents` | `claude`, `opencode`, `pi`, `omp`, `codex`. See [agents.md](agents.md). |
| `env` | Environment for every ssh session: literal values or `secret:NAME`. Names are upper case; `HOME`, `USER`, `PATH`, `SHELL` and `LOGNAME` are refused, and so is `ANTHROPIC_API_KEY` in a template with Claude whose `auth` isn't `api-key`. |
| `mcp` | MCP servers for all agents. See [agents.md](agents.md#mcp-servers). |
| `sync` | `git_identity` (default `true`), `claude_config` (default `false`). |
| `ssh` | `agent` (`off`, `restricted`, `host`), `agent_keys`, `confirm` (default `true`). See [forges.md](forges.md#ssh-agent). |
| `repos` | Repos cloned into `/workspace/<repo>` at start. See [forges.md](forges.md). |
| `idle_stop` | `off`, `daily`, `weekly` or a duration of at least `1m`; overrides `config.yaml`. |
| `cache` | Any of `apt`, `go`, `npm`, `pypi`, `docker`; `[]` turns them off. See [package-caches.md](package-caches.md). |
| `canaries` | Any of `aws`, `env`, `git`; `[]` turns them off. See [canaries.md](canaries.md). |

## How `extends` merges

The chain is applied from the root parent down to the leaf, and the result is validated
once.

- **Scalars override** when the child sets them: `distro`, `docker`, `runtimes.*`,
  `network.egress`, `on_deny`, `prompt_timeout`, `allow_private`, `ssh.agent`,
  `ssh.confirm`, `sync.*`, `idle_stop`, and `resources` field by field.
- **Lists append**, without duplicates: `packages`, `scripts`, `network.presets`,
  `network.allow`, `tools`, `repos`, `ssh.agent_keys`. `files` and `ports` append as
  they are.
- **Lists replace**: `cache` and `canaries`. A child's `[]` clears them.
- **Agents** merge field by field (`version`, `auth`, `yolo` override; `keys` append). A
  child can add an agent but not remove one.
- **`env`** merges by key and **`mcp`** by server name.

## Bases and rebuilds

The first `curral up` of a template downloads and verifies the cloud image and builds
the base, which takes a few minutes. Later VMs of the template boot in seconds. On a
terminal, the build shows its current step with a spinner and how long it has run.

Changing the software in a template (packages, runtimes, scripts, files, agents' versions
and the like) means a new base, built on the next `up` of a new VM. Existing VMs keep
their base and software; only `reset --latest` (a fresh disk on the new base) gives
them the new software, while `rebase` moves a disk onto the new base without changing
its files. See [snapshots.md](snapshots.md). `curral template build <name> --rebuild`
forces a build, re-checking the cloud image upstream, and `curral template rebuild --all`
refreshes every base.

Base builds run with the same isolation as VMs, through a proxy that allows any public
host but no private address, and use the template's package caches (Docker's excepted).
Build logs are in `~/.local/share/curral/bases/`; see
[troubleshooting.md](troubleshooting.md#base-builds).
