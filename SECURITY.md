# Security policy

curral is a security tool: it exists to keep coding agents, and anything they run or
download, away from your host, your credentials and your network. Bugs that weaken
that boundary are the ones we most want to hear about.

## Reporting a vulnerability

Please report vulnerabilities privately, not in a public issue or pull request.
Use GitHub's private vulnerability reporting on
[blackshieldpt/curral](https://github.com/blackshieldpt/curral/security/advisories/new)
(the repository's **Security** tab, then **Report a vulnerability**).

Include what you can of:

- the curral commit (`git rev-parse HEAD`), host distro, QEMU version and guest distro;
- the template and `config.yaml` settings involved (with secrets removed);
- the steps to reproduce, and what the guest or attacker gains;
- whether you've seen it exploited.

We don't commit to a response time. Once the issue is understood, we'll agree a
disclosure date with you. Please give us a reasonable time to ship a fix before
you publish details.

## Supported versions

curral has no tagged releases yet. Security fixes land on `master`; run a recent build
of it. Once releases start, this section will list which ones get fixes.

## Threat model

curral assumes the **guest is hostile**: the agent, the code it writes and the
packages it installs may all try to escape, read host secrets, exfiltrate data or
reach your network. The host user running curral, the host kernel, QEMU/KVM and the
forges you configure are trusted.

In scope (please report):

- escaping the VM, or running code on the host from inside it (including through
  curral's host-side processes: the egress proxy, package caches, notification relay,
  bundle and patch import, transcript and recording handling);
- reaching the network, the host's `127.0.0.1` services or private addresses past the
  egress policy;
- credentials reaching a VM that the configuration doesn't give it, or reaching bases,
  seed images, logs or other VMs;
- one VM reading or affecting another VM's state, keys or traffic;
- guest data that alters host files, git refs or hooks, or terminal state, outside what
  is documented;
- exports (`pull`, `pr`, tasks) that let a canary through without a hit, beyond the
  limits listed in [docs/canaries.md](docs/canaries.md);
- denial of service against the host from a single guest, beyond the documented caps.

Out of scope:

- anything a guest can do inside its own VM (the agent user has passwordless sudo
  there; the VM is the boundary);
- what a VM does with network access you granted it (`egress: open`, allowlisted hosts,
  `curral forward -R`, `ssh.agent: host`), and domain fronting through allowlisted CDNs;
- local attackers already running as your host user;
- spotting the decoys: careful attackers can tell canaries from real credentials.

## Hardening and known limits

The protections curral applies, and the limits it doesn't cover (unsigned checksum
files, allowlists that match names rather than addresses, what canaries can't see,
memory snapshots holding guest secrets, and more), are in
[docs/security-model.md](docs/security-model.md). Read it before you rely on curral
for anything sensitive.
