# Security model

This page lists what catchpen does to keep a VM contained and what it leaves to you. For
reporting vulnerabilities, see [SECURITY.md](https://github.com/blackshieldpt/catchpen/blob/master/SECURITY.md).

catchpen treats the guest as hostile. Everything in a VM (the agent, its tools, the code
it writes and the packages it installs) may try to escape, read host secrets,
exfiltrate data or reach your network. The VM, not the guest user, is the boundary:
the agent user has passwordless sudo inside it.

## Isolation

- **No host mounts.** No host directory is shared with a VM. Repositories are cloned
  inside it (`/workspace/<repo>`), and work leaves through the host (`pull`, `pr`,
  tasks), never by the VM pushing on its own unless you give it push credentials.
- **QEMU sandbox.** QEMU runs under its seccomp sandbox
  (`-sandbox on,obsolete=deny,resourcecontrol=deny`). QEMU's user network has IPv6 off,
  so the guest has no IPv6 route out. catchpen doesn't turn IPv6 off inside the guest,
  which keeps its link-local addresses.
- **Base builds are isolated too.** The build VM gets out only through the proxy, which
  allows any public host but no private or loopback address, and the package caches. Its requests are logged to
  `bases/<base>.egress.log`, and once that passes 32 MiB, the older entries to
  `bases/<base>.egress.log.1`.

## SSH

- Guest login is SSH public key only; passwords are locked. Each VM has its own client
  key and host key. The host key is generated on the host and pinned
  (`StrictHostKeyChecking yes`).
- `seed.iso` (0600) holds the VM's SSH host private key, so the key can be pinned; it
  holds no user credentials.
- SSH and port forwards bind to `127.0.0.1`. Other local users can connect to the port
  but cannot authenticate.
- The generated `ssh_config` disables agent forwarding (unless `ssh.agent` is set), X11,
  port forwarding and tunnels for VM hosts, allows only public-key login (no password,
  keyboard-interactive or PKCS#11 keys), and keeps the VM from updating its pinned host key
  and catchpen's key from being added to an agent. catchpen's own `ssh`, `run`, `attach`,
  `cp` and `forward` read only that file; `catchpen forward` turns port forwarding back on
  only for its own sessions, and agent forwarding off for them. A manual `ssh catchpen-<vm>`
  (after `catchpen setup-ssh`) reads
  your own ssh config too: catchpen's settings beat a block after its Include but not one
  before it, and a block's `IdentityFile`, `CertificateFile`, `SendEnv` and `SetEnv` still
  apply, so the VM's sshd sees those public keys and variables. `catchpen setup-ssh` reports
  both; `Host * !catchpen-*` or `Match !host catchpen-*` keeps a block off catchpen's hosts.
- catchpen's `ssh_config` settings are client-side. The guest's sshd allows TCP, agent and
  Unix-socket forwarding (`catchpen forward` and `ssh.agent` rely on it), and the guest
  has root, so nothing in the VM is relied on to refuse them.
- `ssh.agent: restricted` gives the VM a per-VM `ssh-agent` holding only the keys you
  list, each destination-constrained so it signs only for that VM's `agent`-mode forges,
  and asking for confirmation on each use by default. `ssh.agent: host` forwards your
  own agent: anything in the VM can use all of its keys while you're connected. See
  [forges.md](forges.md#ssh-agent).

## Network

- In `allowlist` mode (the default) the VM has no direct network access. QEMU's user
  network runs with `restrict=on`; the exits are an HTTP proxy at `10.0.2.100:3128`, the
  package caches at `10.0.2.102` and the notification relay at `10.0.2.101`, all served
  by catchpen on the host. DNS resolves on the host. In `none` mode only the notification
  relay is left.
- Allowlisted names that resolve to private or loopback addresses are blocked unless
  `allow_private` is set. The denylist wins over the allowlist.
- The proxy caps request headers (64 KiB), concurrent connections per VM (128) and idle
  tunnels (10 min), and accepts only standard HTTP methods. Each guest connection,
  rejected ones included, starts one short-lived `catchpen egress-stdio` process (about
  3 ms and 11 MiB RSS each, so at most about 1.4 GiB per VM at the connection cap).
  The package caches have their own cap of 128 connections and `catchpen cache-stdio`
  processes per VM, so about twice that with both in use.
- A connection that is allowed but whose log entry can't be written is refused.
  `egress.log` rotates at 32 MiB, keeping one old file, so a guest making many denied
  requests can push older entries out.
- `network.egress: open` gives the guest unfiltered internet and the host's `127.0.0.1`
  services (the `open` section of `network.yaml`). `catchpen up` requires `--allow-open`
  for it.
- `catchpen forward -R` bypasses the proxy. It is recorded in the audit log, and in the
  egress log of allowlist-mode VMs.

## Credentials

- Credentials are resolved on the host and pushed over SSH after every start. They
  never enter bases, seed images or the warm pool.
- Claude Code's token or API key goes into a file only the guest's `claude` command
  loads, so other agents and SSH sessions don't get it in their environment. Other
  agents' provider keys go only to the agents you list them for, and Codex's OpenAI key
  only to Codex, in their own files.
  This keeps credentials from being picked up or logged by the wrong tool; it is not a
  boundary inside the VM, where anything running as the agent user can read all of
  them. Don't give a VM a credential you wouldn't give to everything that runs in it.
- Forge credentials are scoped per VM where the forge allows it: per-VM, per-repo deploy
  keys (read-only with `--ro`) or per-VM minted Gitea tokens (which reach every repo the
  account can), removed when the VM is destroyed. Task VMs get read-only deploy keys
  unless you pass `--rw`.
- The notification relay holds the sink credentials on the host; the guest never sees
  them.

## The API

`catchpen daemon` can serve an API that does what the CLI does; it is off unless
`server:` is set. Its two front doors have different trust:

- **The unix socket** (`api.sock`, mode 0600) is the daemon's own user: any process of
  that user can call every route with every scope, as it can run `catchpen` itself. The
  socket is only as private as that account.
- **`server.listen`** serves TLS 1.3 to clients enrolled by certificate (mTLS) or HMAC
  key. An enrolled client is trusted as far as its scopes go: with none it controls VMs,
  and `config:write` makes it as powerful as your shell. It can connect from any address
  that reaches the port. There is no CA: the server's certificate is self-signed and
  clients pin its key. A certificate or key is a host credential; a leaked one is an
  account takeover until `catchpen daemon client rm`, which refuses its next request and
  ends its open streams within 2 s.

`config:write` is host code execution: it writes `config.yaml`, which can run host
commands (`secrets.<name>.command`), turn off canary pauses and recordings, and send host
secrets into VMs; it changes forge credentials; and it submits tasks whose guests hold
the template's secrets. Enrol a `config:write` client only if you would give it your
shell. Everything else is VM control.

The API is bounded and leaves traces: 64 TCP connections, the per-client request and
stream rates, 4 MiB bodies and timeouts (see [server.md](server.md#limits)); the audit
events these calls cause carry `client` (`@local` over the socket), while a read writes
none, and API sessions are recorded as
`web-ssh`, `web-run` and `web-attach` like CLI ones. It never returns `secrets.env`
values, `canaries.json`, private keys or resolved secrets, and guest text is data. A
canary pause refuses a terminal and console and the calls that would work on its guest
(reset, rebase, push, snapshots, forwards, pull and pr), as it does at the CLI; resuming
it is its own audited call. CORS is never allowed, and a stream whose `Origin` isn't the
API's own is refused, so a web page can't drive a client's certificate.

## Data coming from the guest

- Bundles from `pull`, `pr` and tasks are fetched into `refs/catchpen/...` with hooks off
  and fsmonitor off, and every object the host repository receives is checked
  (`fetch.fsckObjects`, under the repository's own fsck settings), on any git version.
  Nothing from the guest runs on the host, and the host working tree is untouched.
- Exported commits, branch names, PR titles and bodies, task results and agent
  notifications are scanned for canaries. Patches are made on the host from the scanned
  commits, not taken from the guest. See [canaries.md](canaries.md).
- Transcripts are unpacked as regular files and directories only: no symlinks, nothing
  outside the target directory, at most 1 GiB and 200,000 entries.
- Notification text from the guest is stripped of control characters, cut to 300
  characters and rate-limited (a burst of 5 per VM, then one every 30 s).
- Guest output shown on the host is filtered: first-boot errors and the task output
  commands (`task logs`, `task show`, `task run -f`) drop control characters, and
  `catchpen console` and `catchpen recordings play` drop OSC, DCS and APC sequences
  (window title, clipboard). `asciinema play` does not filter them.
- Session recordings are written on the host, from the host side of the pty, so the
  guest can't alter what they hold. It can end one early by filling its 256 MiB cap;
  `catchpen recordings` marks such a recording `(truncated)` and `recordings play`
  says so when the replay ends.
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
- Skills and plugins are fetched on the host by `git`, over `https` only, at a pinned
  full commit, with a scrubbed environment and config (no hooks, no credential helpers,
  `fsckObjects` on); pi's `bundled` plugins are files of the pi package in the VM
  instead. Symlinks, submodules, `.git` and other odd names are refused. A fetch is
  stopped once it has added 512 MiB on disk (git and the helpers it started are
  killed), each git process it runs may use at most 1 GiB of memory, and a folder over 64 MiB or 20000 files is refused
  while git lists it, before any file is read. The fetched files go only into the guest; nothing from them runs on the host. A pin fixes
  the bytes, not who wrote them: on GitHub, a commit from a fork can be fetched through
  the parent repo's URL, so pin commits you have read. See
  [agents.md](agents.md#skills-and-plugins).

## Known limits

- Allowlist entries match the requested name, not the resolved address, and CDN hosts on
  the allowlist (such as `*.githubusercontent.com`) can be used for domain fronting.
- An allowed upload service (the `github`, `ghcr`, `docker-hub` or `npm` preset) lets a
  guest that holds a token or key for it, its own included, push data out.
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
  never removed by `gc`. They can contain secrets an agent printed. The audit log rotates
  at 32 MiB, keeping one old file (`audit.jsonl.1`), so its oldest entries do go.
- An enrolled API client is a host credential: a leaked certificate or HMAC key can do
  anything its scopes allow until `catchpen daemon client rm`. There is one scope, so a
  client either has `config:write` or not, and no per-user identity is carried.
- `server.listen` has no account lockout and no per-source ban beyond the 64-connection
  and 16-per-address caps and the 60 s pre-auth timeout; keep it on a private network.
- The unix socket's protection is the daemon user's account and its 0600 mode: any
  process of that user, or root, can use every scope.

## Recommendations

- Keep `egress: allowlist` and add hosts as you need them; prefer `on_deny: prompt` while
  you learn what a project needs.
- Prefer `deploy-key` or `minted-token` forges over `token`, and `--ro` where the VM only
  reads. Use `pull` or `pr` to get work out rather than giving the VM push access.
- Prefer `ssh.agent: restricted` with `confirm: true` over `host`.
- Pin `version:` for agents installed from release binaries.
- Keep canaries on (the `canaries.yaml` default plants all three) and
  `canary_action: pause`.
- Review `catchpen audit` and `catchpen egress log` for VMs that ran unattended.
