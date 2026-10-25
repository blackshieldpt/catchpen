# Templates

A VM's template is what it contains: distro, size, software, network policy, agents,
credentials and repos. You build one with `-t`, by picking **sections** from
**category files**:

```
curral up dev                                        # every category's default
curral up dev -t go-1.27,python-3.14                 # the defaults, plus Go and Python 3.14
curral up dev -t ubuntu-26.04,large,go-1.27,-docker  # another distro and size, Go, no Docker
```

Software is baked into a **base** image built from the template; VMs are thin
copy-on-write overlays on it.

```
curral template ls [category]        # every section, with the defaults marked
curral template show -t go-1.27      # the settings a -t gives, and where they come from
curral template build -t go-1.27 [--rebuild]
curral template rebuild --all        # fresh cloud image and packages, as a new base
```

## How `-t` works

Each category is a file of named sections, plus a `default`. For every category:

- if `-t` names none of its sections, its **default** applies;
- in most categories, the sections `-t` names are **added** to the default, and
  `-name` drops one of the default's sections (`-t -docker,-codex`);
- in the one-choice categories (`distros`, `resources`, `ssh`, `idle`), the section `-t`
  names **replaces** the default, and naming two is an error. A size may set only some
  of `cpus`, `memory` and `disk`; the default size gives the rest.

Picks apply left to right: `-t docker,-docker` has no Docker, and picking a section again
moves it last, so its settings win (`-t open,allowlist` is the allowlist). A pick named
in `-t` wins over one from a profile (`-t minimal,large` is minimal at the large size,
and `-t docker,minimal` has Docker), and a profile's `-name` of a section that isn't
chosen does nothing (in a one-choice category, unless it leaves none). `-name` of a list
in a category file removes each of its sections (`-t -tools:default` drops the default
tools); a profile can't be removed.

`-t` is a comma-separated list of at most 160 characters (put longer ones in a
profile). A name must be unique across categories; when it isn't
(say you add a `docker` section to `network.yaml`), say which: `-t runtimes:docker`. `category:default` is that category's default, so
`-t runtimes:default` is the same as no runtime picks.

Sections are merged in category order, below. Within a category, lists add up
(`packages`, `presets`, `cache`, ...), settings like `go:` or `egress:` take the last
section's value, agents merge field by field, `env` by name and `mcp` by server name. A
runtime picked with `-t` overrides the Node version the agents ask for (`-t node-22`).

A VM records its `-t` (`curral ls` shows it as TEMPLATE), and uses it again for
`curral reset --latest`, `rebase`, `template rebuild --all` and `creds push`. So editing a
section changes what those give for every VM that picked it.

## Categories

| File | Sections set | Built-in default | Built-in sections |
|---|---|---|---|
| `distros.yaml` | `distro` | `ubuntu-24.04` | `ubuntu-24.04`, `ubuntu-26.04`, `debian-13` |
| `resources.yaml` | `resources` | `medium` | `small` (2 CPUs, 4G, 20G), `medium` (4, 8G, 40G), `large` (8, 16G, 80G), `xlarge` (16, 32G, 160G) |
| `tools.yaml` | `packages`, `tools`, `scripts`, `files` | `core`, `build`, `gh` | `core` (git, curl, jq, tmux, ripgrep, ...), `build` (build-essential, make, python3, ...), `gh`, `tea` |
| `agents.yaml` | `agents`, and the `runtimes` they need | `claude`, `opencode`, `pi`, `codex` | `claude`, `claude-api-key`, `claude-yolo`, `opencode`, `pi`, `omp`, `codex`, `codex-api-key`, `codex-chatgpt` |
| `runtimes.yaml` | `runtimes`, `docker` | `docker`, `uv` | `docker`, `uv`, `node-22`, `node-24`, `go-1.26`, `go-1.27`, `python-3.12`, `python-3.13`, `python-3.14` |
| `network.yaml` | `network` | `allowlist` and the presets below | `allowlist`, `open`, `offline`, `prompt`; `anthropic`, `github`, `npm`, `pypi`, `go`, `apt-ubuntu`, `apt-debian`, `docker-apt`, `docker-hub`, `ghcr` (one per preset) |
| `caches.yaml` | `cache` | all five | `apt-cache`, `go-cache`, `npm-cache`, `pypi-cache`, `docker-cache` |
| `canaries.yaml` | `canaries` | all three | `aws-canary`, `env-canary`, `git-canary` |
| `env.yaml` | `env` | none | none |
| `mcp.yaml` | `mcp` | none | none |
| `ssh.yaml` | `ssh` | `ssh-off` | `ssh-off`, `ssh-host` |
| `sync.yaml` | `sync` | none (git identity is copied) | `claude-config`, `no-git-identity` |
| `repos.yaml` | `repos` | none | none |
| `ports.yaml` | `ports` | none | none |
| `idle.yaml` | `idle_stop` | none (`config.yaml` `idle_stop`) | `idle-30m`, `idle-1h`, `idle-2h`, `idle-off` |
| `profiles.yaml` | lists of picks | none | `minimal` |

`curral template ls` prints the full list with descriptions. A section may set only its
category's settings, and a `description`.

## Your sections

`curral init` writes every category file to `~/.config/curral/templates/`. Each one
explains itself, shows curral's built-in sections commented out, and most have examples.
To use one, uncomment it with all the lines under it:

```yaml
# ~/.config/curral/templates/ssh.yaml
github-ssh:
  description: Sign git over SSH with my key
  ssh: { agent: restricted, agent_keys: [~/.ssh/id_ed25519], confirm: false }
```

```yaml
# ~/.config/curral/templates/env.yaml
gh-token: { env: { GH_TOKEN: "secret:GH_TOKEN" } }
```

```
curral up dev -t github-ssh,gh-token --repo github:acme/api
```

Your file is read on top of curral's: a section with a built-in name replaces it, and
`default:` replaces the built-in default. Sections you don't redefine keep tracking
curral's, so new built-in sections (a new Go version, say) reach you on upgrade.

Section names are lower case letters, digits, `.`, `_` and `-`, start with a letter or
digit, are at most 48 characters, and are defined once per file. Any other `*.yaml` in
that folder, such as an old-style template, is refused until you move or rename it.
Relative paths in `scripts` and `files[].src` resolve inside
`~/.config/curral/templates/` (symlinks there are followed); absolute paths and `..` are
rejected. Unknown settings are errors.

### Profiles

A section can also be a list of picks, used like one section. `profiles.yaml` holds lists
that span categories:

```yaml
# ~/.config/curral/templates/profiles.yaml
acme-api: [github-ssh, gh-token, api, go-1.27, large]
```

```
curral up dev -t acme-api
```

Lists may include `-name` and other lists. In a category file other than
`profiles.yaml`, a list names sections of that category: `runtimes.yaml` could hold
`web: [node-24, python-3.14]`.

The built-in `minimal` profile is the defaults minus everything optional: `small`, the
`core` tools, the allowlist with only the `anthropic` preset, and no agents, runtimes,
caches or canaries. It removes curral's built-in default sections by name, so sections
you add to a default stay: with `default: [claude, omp]` in agents.yaml, `-t minimal`
has omp.

### Defaults

`default:` is a section name, a list of names, or settings of its own:

```yaml
# ~/.config/curral/templates/resources.yaml
default: large                     # every VM gets 8 CPUs unless -t says otherwise
```

```yaml
# ~/.config/curral/templates/agents.yaml
default: [claude, omp]             # no opencode, pi or codex by default
```

## Settings

| Setting | Values |
|---|---|
| `distro` | `ubuntu-24.04`, `ubuntu-26.04`, `debian-13`, or a name from `images:` in config.yaml. |
| `resources` | `cpus` (0 to 256), `memory`, `disk`: sizes like `512M`, `8G`, `1T`. `--cpus`, `--mem` and `--disk` on `curral up` override them. |
| `packages` | apt package names, optionally `name=version`. |
| `docker` | `true` installs Docker and configures it for the proxy and caches. |
| `runtimes` | `node: "24"` (a major version, quoted), `go: "1.27"` or `"1.27.1"`, `python: uv` or a version like `"3.14"` (installed with uv, as `python3.14` and `python`). |
| `scripts` | Scripts run as root at base build, in order. |
| `files` | `{src, dst, mode}`: copied in at base build. `dst` is absolute or `~/`; `mode` like `"0644"`. |
| `tools` | `gh`, `tea`. |
| `ports` | `"guest"` or `"host:guest"`, bound to `127.0.0.1`. Host port 0 or omitted picks a free port at start. |
| `network` | `egress` (`allowlist`, `open`, `none`), `presets`, `allow`, `on_deny` (`log`, `prompt`), `prompt_timeout` (default `30s`), `allow_private`. See [egress.md](egress.md). |
| `agents` | `claude`, `opencode`, `pi`, `omp`, `codex`. See [agents.md](agents.md). |
| `env` | Environment for every ssh session: literal values or `secret:NAME`. Names are upper case; `HOME`, `USER`, `PATH`, `SHELL` and `LOGNAME` are refused, and so is `ANTHROPIC_API_KEY` with Claude whose `auth` isn't `api-key`. |
| `mcp` | MCP servers for all agents. See [agents.md](agents.md#mcp-servers). |
| `sync` | `git_identity` (default `true`), `claude_config` (default `false`). |
| `ssh` | `agent` (`off`, `restricted`, `host`), `agent_keys`, `confirm` (default `true`). See [forges.md](forges.md#ssh-agent). |
| `repos` | Repos cloned into `/workspace/<repo>` at start. See [forges.md](forges.md). |
| `idle_stop` | `off`, `daily`, `weekly` or a duration of at least `1m`; overrides `config.yaml`. |
| `cache` | Any of `apt`, `go`, `npm`, `pypi`, `docker`. See [package-caches.md](package-caches.md). |
| `canaries` | Any of `aws`, `env`, `git`. See [canaries.md](canaries.md). |

## Bases and rebuilds

The first `curral up` of a template downloads and verifies the cloud image and builds
the base, which takes a few minutes. Later VMs of the template boot in seconds. On a
terminal, the build shows its current step with a spinner and how long it has run.

Bases are named after the distro and a hash of what they contain
(`ubuntu-24.04-<hash>.qcow2`), so `-t` lists that give the same software share one
base: `-t large` or `-t ssh-host` don't build another.

Changing the software in a template (packages, runtimes, scripts, files, agents' versions
and the like) means a new base, built on the next `up` of a new VM. Existing VMs keep
their base and software; only `reset --latest` (a fresh disk on the new base) gives
them the new software, while `rebase` moves a disk onto the new base without changing
its files. See [snapshots.md](snapshots.md). `curral template build -t <picks> --rebuild`
forces a build, re-checking the cloud image upstream, and `curral template rebuild --all`
refreshes every base.

Base builds run with the same isolation as VMs, through a proxy that allows any public
host but no private address, and use the template's package caches (Docker's excepted).
Build logs are in `~/.local/share/curral/bases/`; see
[troubleshooting.md](troubleshooting.md#base-builds).
