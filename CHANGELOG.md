# Changelog

All notable changes to curral are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). curral has no tagged releases
yet; everything below is on `master`, newest first.

## [Unreleased]

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

### Fixed

- A task whose VM a canary hit paused now fails at once instead of hanging on SSH to
  the frozen guest.
- Other findings from a review of the canary, cache, task and omp work.

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
