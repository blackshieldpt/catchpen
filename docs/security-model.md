# Security model

This page lists what curral does to keep a VM contained and what it leaves to you. For
reporting vulnerabilities, see [SECURITY.md](../SECURITY.md).

curral treats the guest as hostile. Everything in a VM (the agent, its tools, the code
it writes and the packages it installs) may try to escape, read host secrets,
exfiltrate data or reach your network. The VM, not the guest user, is the boundary:
the agent user has passwordless sudo inside it.

## Isolation

- **No host mounts.** No host directory is shared with a VM. Repositories are cloned
  inside it (`/workspace/<repo>`), and work leaves through the host (`pull`, `pr`,
  tasks), never by the VM pushing on its own unless you give it push credentials.
- **QEMU sandbox.** QEMU runs under its seccomp sandbox
  (`-sandbox on,obsolete=deny,resourcecontrol=deny`). IPv6 is disabled on the guest
  network.
- **Base builds are isolated too.** The build VM gets out only through the proxy, which
  allows any public host but no private or loopback address, and the package caches. Its requests are logged to
  `bases/<base>.egress.log`.

## SSH

- Guest login is SSH public key only; passwords are locked. Each VM has its own client
  key and host key. The host key is generated on the host and pinned
  (`StrictHostKeyChecking yes`).
- `seed.iso` (0600) holds the VM's SSH host private key, so the key can be pinned; it
  holds no user credentials.
- SSH and port forwards bind to `127.0.0.1`. Other local users can connect to the port
  but cannot authenticate.
- The generated `ssh_config` disables agent forwarding (unless `ssh.agent` is set), X11,
  port forwarding and tunnels for VM hosts, overriding a `Host *` block in your own
  config. `curral forward` turns port forwarding back on only for its own sessions.
- `ssh.agent: restricted` gives the VM a per-VM `ssh-agent` holding only the keys you
  list, each destination-constrained so it signs only for that VM's `agent`-mode forges,
  and asking for confirmation on each use by default. `ssh.agent: host` forwards your
  own agent: anything in the VM can use all of its keys while you're connected. See
  [forges.md](forges.md#ssh-agent).

## Network

- In `allowlist` mode (the default) the VM has no direct network access. QEMU's user
  network runs with `restrict=on`; the exits are an HTTP proxy at `10.0.2.100:3128`, the
  package caches at `10.0.2.102` and the notification relay at `10.0.2.101`, all served
  by curral on the host. DNS resolves on the host. In `none` mode only the notification
  relay is left.
- Allowlisted names that resolve to private or loopback addresses are blocked unless
  `allow_private` is set. The denylist wins over the allowlist.
- The proxy caps request headers (64 KiB), concurrent connections per VM (128) and idle
  tunnels (10 min), and accepts only standard HTTP methods. Each guest connection,
  rejected ones included, starts one short-lived `curral egress-stdio` process (about
  3 ms and 11 MiB RSS each, so at most about 1.4 GiB per VM at the connection cap).
- A connection that is allowed but whose log entry can't be written is refused.
  `egress.log` rotates at 32 MiB, keeping one old file, so a guest making many denied
  requests can push older entries out.
- `network.egress: open` gives the guest unfiltered internet and the host's `127.0.0.1`
  services. `curral up` requires `--allow-open` for such templates.
- `curral forward -R` bypasses the proxy. It is recorded in the audit log, and in the
  egress log of allowlist-mode VMs.

## Credentials

- Credentials are resolved on the host and pushed over SSH after every start. They
  never enter bases, seed images or the warm pool.
- Claude Code's token or API key goes into a file only the guest's `claude` command
  loads, so other agents and SSH sessions don't get it in their environment. Other
  agents' provider keys go only to the agents you list them for, in their own files.
  This keeps credentials from being picked up or logged by the wrong tool; it is not a
  boundary inside the VM, where anything running as the agent user can read all of
  them. Don't give a VM a credential you wouldn't give to everything that runs in it.
- Forge credentials are scoped per VM where the forge allows it: per-VM, per-repo deploy
  keys (read-only with `--ro`) or per-VM minted Gitea tokens, removed when the VM is
  destroyed. Task VMs get read-only deploy keys unless you pass `--rw`.
- The notification relay holds the sink credentials on the host; the guest never sees
  them.

## Data coming from the guest

- Bundles from `pull`, `pr` and tasks are fetched into `refs/curral/...` with hooks off,
  fsmonitor off and `transfer.fsckObjects` on. Nothing from the guest runs on the host,
  and the host working tree is untouched.
- Exported commits, branch names, PR titles and bodies, task results and agent
  notifications are scanned for canaries. Patches are made on the host from the scanned
  commits, not taken from the guest. See [canaries.md](canaries.md).
- Transcripts are unpacked as regular files and directories only: no symlinks, nothing
  outside the target directory, at most 1 GiB and 200,000 entries.
- Notification text from the guest is stripped of control characters, cut to 300
  characters and rate-limited (a burst of 5 per VM, then one every 30 s).
- Guest output shown on the host is filtered: first-boot errors drop control
  characters, and `curral console` and `curral recordings play` drop OSC, DCS and APC
  sequences (window title, clipboard). `asciinema play` does not filter them.
- Session recordings are written on the host, from the host side of the pty, so the
  guest can't alter them.
- Live `ssh`, `run` and `attach` sessions pass guest output to your terminal unfiltered,
  as plain ssh does. Use a terminal you trust with untrusted output.

## Supply chain

- Cloud images are verified against the distro's checksum file over HTTPS, or a pinned
  digest. GPG signatures of the checksum files are not checked yet. Images with a
  backing file or a qcow2 external data file are refused.
- Node, Go and uv are checked against checksums from their own download sites, which
  adds nothing beyond TLS. Docker's apt key is fingerprint-pinned.
- omp is checked against its release's SHA256SUMS. That catches a broken download, not a
  tampered release; pin `version:` to control what you get.
- The package caches fetch over HTTPS from fixed upstreams and the CDNs those redirect
  to, never from private addresses, and accept only GET and HEAD. Redirects are checked
  against the VM's denylist. Guests can't put anything in the store; Docker objects and
  apt by-hash files are checked against their digest, the rest by the clients (apt
  signatures, the Go checksum database, lockfile hashes).

## Known limits

- Allowlist entries match the requested name, not the resolved address, and CDN hosts on
  the allowlist (such as `*.githubusercontent.com`) can be used for domain fronting.
- A package cache is about as open as allowlisting its preset. proxy.golang.org and
  sum.golang.org fetch any module path they're asked for, as with the `go` preset.
- The cache store is shared by all VMs, so a VM can tell from timing whether a package
  was already cached.
- Memory snapshots hold whatever was in the guest's memory, credentials included. They
  live inside the VM's disk (in its `0700` directory) and go away with it, `reset` or
  `rebase`.
- Canaries don't see everything; [canaries.md](canaries.md#what-goes-unseen) lists the
  gaps.
- Recordings, transcripts, task output and the audit log are kept after `destroy` and
  never removed by `gc`. They can contain secrets an agent printed.

## Recommendations

- Keep `egress: allowlist` and add hosts as you need them; prefer `on_deny: prompt` while
  you learn what a project needs.
- Prefer `deploy-key` or `minted-token` forges over `token`, and `--ro` where the VM only
  reads. Use `pull` or `pr` to get work out rather than giving the VM push access.
- Prefer `ssh.agent: restricted` with `confirm: true` over `host`.
- Pin `version:` for agents installed from release binaries.
- Keep canaries on (the default template plants all three) and `canary_action: pause`.
- Review `curral audit` and `curral egress log` for VMs that ran unattended.
