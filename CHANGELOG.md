# Changelog

All notable changes to catchpen are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), newest first. Sections dated
before 0.1.0 describe work on `master` before the first tagged release.

## [Unreleased]

### Changed (breaking)

- curral is now catchpen: the binary, the `catchpen` command, the Go module
  `github.com/blackshieldpt/catchpen`, the `.deb` package, the `CATCHPEN_*` environment
  variables and the `~/.config/catchpen`, `~/.local/share/catchpen` and
  `~/.cache/catchpen` directories.
- Templates are now [category files](docs/templates.md) of named sections: `distros`,
  `resources`, `tools`, `agents`, `runtimes`, `network`, `caches`, `canaries`, `env`,
  `mcp`, `ssh`, `sync`, `repos`, `ports`, `idle` and `profiles`,
  each with a `default`.
  `-t` is a comma-separated list of picks, like `-t go-1.27,python-3.14` or
  `-t ubuntu-26.04,large,-docker`: categories not named keep their default, picks add to
  it (in `distros`, `resources`, `ssh` and `idle` they replace it), and `-name` drops a
  default section. Profiles are named lists of picks from any category. Template files
  with `extends:`, the built-in `base` and `default` templates (`minimal` is now a
  profile), `--distro` and config.yaml's `defaults.distro`, `defaults.template`,
  `defaults.resources` and pool `distro:` are gone; all but `--distro` give errors that
  say so. Commands that read templates refuse while old template files, such as the
  `example.yaml` an older `catchpen init` wrote, are in `~/.config/catchpen/templates/`. A
  pool entry's or task batch's `template:` is now a `-t` spec, so an old template name
  that is now a section's (`go`, `gh`) means that section on top of the defaults: check
  scripts and config with `catchpen template show -t <name>`. VMs made before keep
  running, but `reset --latest` and `rebase` refuse them and `creds push` leaves their
  env alone.
- opencode no longer reads `~/.claude/skills` in VMs, unless
  `sync: { opencode_claude_skills: true }` (`-t opencode-claude-skills`).
- Claude Code's skill and plugin sync from claude.ai is off in VMs, unless
  `sync: { claude_ai: true }` (`-t claude-ai-sync`).
- Bases are named after the distro and a hash of their content
  (`ubuntu-24.04-<hash>.qcow2`), so `-t` lists that give the same software share one.
- `catchpen init` writes every category file, commented, with catchpen's built-in sections
  and examples, instead of `templates/example.yaml`.
- `catchpen template ls [category]`, `show -t`, `build -t` and `rebuild -t` work on picks.

### Added

- A [GitHub tutorial](docs/github.md): your SSH key for git, and a token for `gh`, step
  by step.
- Go 1.26 and 1.27, Node 22 and 24, and Python 3.12, 3.13 and 3.14 runtime sections.
  `runtimes: { python: "3.14" }` installs that Python with uv, as `python3.14` and
  `python`.
- `small`, `medium`, `large` and `xlarge` resource sections.
- [Skills and plugins](docs/agents.md#skills-and-plugins) for every agent, fetched on the
  host from pinned git commits (pi's `bundled` plugins come from the pi package in the VM)
  and copied in at each start. They are set in `agents.yaml` sections. The built-in `claude`
  section gives Claude Code three of Anthropic's skills (`skill-creator`, `mcp-builder`,
  `frontend-design`); `claude-vanilla` is Claude Code without them.

### Changed

- `catchpen creds push` takes the VM's `env:` from its template as the template is now, so
  variables added to the template after the VM was created reach it. It also refreshes
  the VM's skills and plugins from the template.
- Claude Code no longer clones its official plugin marketplace in VMs; no setting turns
  it back on.
- Codex's remote plugins are off.
- A `--repo` whose host has no forge says that repos clone only from added forges, lists
  the configured ones, and prints a complete `catchpen forge add` command for the host.

### Fixed

- A template's base built by a warm-pool fill now includes the template's coding agents.
  Before, if the daemon's pool fill was the first to build a template's base, the base
  had no agents, and every VM of that template used it. If a VM is missing its agent,
  run `catchpen template rebuild -t <template>`, then `catchpen reset --latest <vm>`
  (this gives the VM a fresh disk; its repos are cloned again) or recreate the VM. A
  rebuild alone doesn't change existing VMs, which keep their base.

## [0.1.1] - 2026-10-25

### Added

- `curral init` writes a commented `config.yaml` and `secrets.env` (mode 0600), listing
  the settings and secret names, and `templates/example.yaml`, the `default` template's
  settings with its other options commented out. It keeps files that exist.
- Releases ship an amd64 `.deb` that installs `/usr/bin/curral` and depends on QEMU,
  `openssh-client` and `git`; it is checksummed, signed and attested like the binary.

### Changed

- On a terminal, a base build shows its current step with a spinner and the step's
  elapsed time, and prints each finished step with the time it took.

### Fixed

- The egress proxy and the package caches race a host's IPv4 and IPv6 addresses
  (Happy Eyeballs) instead of trying them one at a time. On a host whose IPv6 resolves
  but doesn't route, every connection hung until it timed out, and base builds failed
  with apt's `Connection failed`.
- `curral doctor`'s `ssh-askpass` warning says whether the display or the askpass program
  is missing; with no display it suggests `confirm: false` instead of installing
  `ssh-askpass`.

## [0.1.0] - 2026-10-24

### Added

- `curral herdr` gives each running VM a [herdr](docs/herdr.md) workspace whose agent
  herdr tracks (working, blocked, idle) and resumes after a restart; `curral start` and
  `stop` keep the workspaces in step while herdr runs. herdr stays on the host.
- `curral version` prints the version, commit and commit date.
- Signed release binaries: a tag builds a static linux/amd64 binary and `.tar.gz` with
  GoReleaser, signs them with cosign keyless and attests build provenance. Verifying
  them is in [docs/installation.md](docs/installation.md#install).
- CI on every push and pull request (lint, staticcheck, govulncheck, tests, builds), the
  integration tests nightly, and the docs site on GitHub Pages.

- The Codex agent (`agents: { codex: {} }`), installed from npm. `auth: api-key` logs
  it in with `OPENAI_API_KEY` at each start; `auth: chatgpt` leaves the sign-in to
  `codex login --device-auth` inside each VM; `auth: none` installs it with no
  credentials. The `default` template installs it with `none`, so its base is rebuilt.
  `curral run <vm> codex`, `curral task run --agent codex` (with `auth: api-key`),
  notifications, transcripts and MCP servers (not SSE ones) work as for the other
  agents.

### Security

- Claude Code's token or API key is no longer in every session's environment. curral
  pushes it to `~/.config/curral/claude.env`, which only a `claude` wrapper in the base
  loads, so opencode, pi, omp and SSH sessions don't get it. Templates with Claude get
  a new base; existing VMs keep the old behaviour, with a warning at each start, until
  `curral reset --latest`.

- Patches exported by `curral pull --patches` and `curral task run --out patch` are now
  made on the host from commits it has already scanned, instead of being taken from
  the guest. A binary delta in a guest-written patch could rebuild a decoy credential
  from bytes the patch scan never saw.
- Commit scans also check messages and names after git re-encodes them to UTF-8, so an
  `encoding` header (ISO-2022-JP escapes, for example) can no longer hide a decoy that
  format-patch or a forge would show.
- The patch scanner decodes binary deltas and caps how much a patch may inflate to, so a
  guest can't exhaust host memory with one.
- Root commits are scanned for canaries too.
- When a VM's `canaries.json` can't be read, its proxy, cache and exports refuse
  everything, and its notifications and task results are withheld.
- Notifications and task results carrying a decoy are withheld.
- The package caches store only paths in canonical form.
- A task's whole result is scanned for canaries, not only its first line, and
  `curral task show` prints it withheld when it carries a decoy.
- Patch exports scan exactly the commits they export: `pull --patches` included commits
  the host already had, which the pull's scan skipped.
- Canary matching also decodes percent-encoded URLs, URL user info and `Basic`
  credentials.
- A canary hit on a VM that can't be paused (its QEMU control socket busy, as during a
  snapshot) stops it instead of leaving it running. Hits arriving together wait for
  each other rather than racing for the control socket.
- The package caches drop a guest that stops reading on every path, cap the Docker
  tokens they keep per connection, and don't store encoded responses.
- `curral egress log` strips control characters and newlines from hosts and reasons,
  and guest text shown on the host drops bidi controls.
- go-toml is updated to v2.4.3.

### Fixed

- A task whose VM a canary hit paused now fails at once instead of hanging on SSH to
  the frozen guest.
- Other findings from a review of the canary, cache, task and omp work.
- Host operations on a guest (exports, credential pushes, transcripts, clock syncs) no
  longer hang when a canary hit pauses the VM mid-way.
- A pool VM with a canary hit no longer fills a pool slot forever.
- Concurrent pulls of the same branch can't swap in each other's unscanned commits,
  and refs left by a pull that died are removed by the next one.
- Copying transcripts no longer reports a failure after copying everything (tar's
  end-of-archive padding hit a closed pipe).
- A `cache_max` (or any size) too large to count in bytes is refused, not wrapped
  negative, which made `curral gc` empty the cache.
- An agent config file over 4 MiB in the VM fails the push with a message saying so.

## 2026-10-21: Phase 6, canaries and oh-my-pi

### Added

- Canaries: decoy AWS, `.env` and git credentials planted per VM (`canaries:` in a
  template). Connections to the decoy hosts are detected at the egress proxy, decoy
  values in plain-HTTP request lines and headers and in package cache requests, and
  both in everything `pull`, `pr` and tasks export; a hit refuses the request, is audited and
  notified (`canary.hit`), and pauses the VM (`canary_action: pause | stop | log`)
  until `curral resume`.
- Commit authors, committers and branch names are scanned for canaries.
- The `omp` (oh-my-pi) agent, installed from its release binary and checked against the
  release's SHA256SUMS.

### Security

- Package cache redirects are checked against the VM's egress denylist.

## 2026-10-21: Phase 5c, package caches

### Added

- Pull-through caches on the host for apt, Go modules, npm, PyPI and Docker Hub, shared
  by all VMs and base builds (`cache:` in a template, `cache_max` in config.yaml).
- `curral cache` and `curral cache clear`.

## 2026-10-20: Phase 5b, daemon, warm pool and idle stop

### Added

- `curral daemon` (systemd user service via `curral daemon install`): refills the warm
  pool, stops idle VMs, rebuilds bases on a schedule and answers egress prompts through
  desktop notifications.
- Warm pool (`pool:` in config.yaml, `curral pool ls | fill | drain`): pre-booted VMs
  saved with their memory, claimed by `up` and `task` in seconds.
- Idle stop (`idle_stop:`), with a `vm.idle_stop` notification, and scheduled base
  rebuilds (`rebuild:`).

## 2026-10-20: Phase 5a, snapshots and base generations

### Added

- `curral snapshot`, `snapshots`, `restore` and `rmsnap`, with memory or disk-only
  snapshots.
- `curral reset [--latest]` and `curral rebase`.
- Base generations: `curral template rebuild` adds a new base without replacing one in
  use, and `curral ls` marks VMs on an older one `stale-base`.
- `curral gc`.

## 2026-10-20: Phase 4b, headless tasks, transcripts and notifications

### Added

- `curral task run | ls | show | logs | cancel | rm`: an agent runs a prompt in a fresh
  VM, and the work is exported from the host as a PR, a bundle or a patch. Batch files
  with a matrix, and a VM and memory budget for parallel tasks.
- Agent session stores copied to the host as transcripts (`curral transcripts`).
- Notifications to the desktop, ntfy or webhooks for task, egress-prompt and
  agent-waiting events.

## 2026-10-20: Phase 4a, sessions, recordings and audit

### Added

- `curral run` and `curral attach` (agents and a shell in guest tmux sessions),
  `curral tmux` on the host, `curral up --ephemeral` and `curral cp`.
- Host-side asciicast recordings of interactive sessions (`curral recordings`).
- A host audit log (`curral audit`).

## 2026-10-19: Phase 3, forges, repos and SSH

### Added

- GitHub and Gitea forges (`curral forge add | rm | test`) with `deploy-key`,
  `minted-token`, `token`, `agent` and `none` modes; repos cloned into `/workspace` at
  start.
- `curral pull` and `curral pr` export work through the host, so the VM needs no push
  access.
- A restricted, destination-constrained per-VM ssh-agent (`ssh.agent: restricted`) and
  `curral forward` for port forwards.

### Changed

- `agent`-mode forges require a restricted agent.

## 2026-10-19: Phase 2, agents and credentials

### Added

- Claude Code (OAuth token or API key), opencode and pi installed into template bases.
- Credentials resolved on the host and pushed over SSH after every start
  (`curral creds push`); they never enter bases or seed images.
- `secret:` references, MCP servers written to every agent's config, and git identity
  and Claude config sync.

## 2026-10-19: Phase 1, KVM sandboxes

### Added

- QEMU/KVM VMs from declarative templates with `extends`, built into copy-on-write
  bases from verified cloud images (Ubuntu 24.04 and 26.04, Debian 13, or custom images).
- Key-only SSH with a per-VM host key, generated on the host and pinned.
- A default-deny egress allowlist through a host-side proxy, with log or prompt on deny.
- `curral doctor`, `up`, `start`, `stop`, `destroy`, `ls`, `ssh`, `console` and
  `setup-ssh`.
