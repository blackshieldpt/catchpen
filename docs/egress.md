# Egress

Each VM's network access is set by `network.egress`, from its `network.yaml` sections:

| Mode | Section | What the guest can reach |
|---|---|---|
| `allowlist` (default) | `allowlist` | Only hosts on the VM's allowlist, through catchpen's proxy. |
| `none` | `offline` | No network. The notification relay still carries agents' short notification texts to your sinks. |
| `open` | `open` | The internet, unfiltered, and the host's `127.0.0.1` services. `catchpen up` requires `--allow-open`. |

## How allowlist mode works

The VM has no direct network access: QEMU's user network runs with `restrict=on`, and
the exit for network traffic is an HTTP proxy at `10.0.2.100:3128`. catchpen serves it on
the host, one short-lived `catchpen egress-stdio` process per connection. The guest is preconfigured to
use it: the environment, apt, and the Docker daemon and containers. DNS resolves on the
host. Allowlisted names that resolve to private or loopback addresses are blocked
unless `allow_private` is set.

The package caches (`10.0.2.102`) and the notification relay (`10.0.2.101`) are separate
exits with their own rules; see [package-caches.md](package-caches.md) and
[notifications.md](notifications.md).

## Configuring it

`network.yaml`'s default is the `allowlist` section plus one section per preset (named
host lists): `anthropic`, `github`, `npm`, `pypi`, `go`, `apt-ubuntu`, `apt-debian`,
`docker-apt`, `docker-hub`, `ghcr`. Picks add to it, and `-name` drops a preset:

```
catchpen up dev -t prompt                  # on_deny: prompt
catchpen up dev -t -docker-hub,-ghcr       # no Docker Hub or GHCR
catchpen up dev -t offline                 # egress: none
catchpen up dev -t open --allow-open       # egress: open
```

Hosts of your own go in a section of `~/.config/catchpen/templates/network.yaml`:

```yaml
# ~/.config/catchpen/templates/network.yaml
registry:
  network:
    allow: [registry.example.com:443]
    on_deny: log                         # log | prompt
```

```
catchpen up dev -t registry
```

Entries are `host`, `host:port`, `*.example.com` (subdomains) or IP addresses. An entry
without a port means ports 80 and 443. A `default:` in your `network.yaml` replaces the
built-in one, such as `default: [allowlist, anthropic, github, go]` for fewer presets on
every VM.

catchpen adds hosts the VM needs on its own: Claude Code's and Codex's API hosts, the API
hosts of the agents' provider keys, remote MCP servers and the VM's forges.

## Changing a running VM

```
catchpen egress show dev                      # lists and settings
catchpen egress log dev -f                    # allowed and blocked requests
catchpen egress allow dev pypi.org files.pythonhosted.org   # applies immediately
catchpen egress deny dev evil.example.com     # the denylist wins over the allowlist
catchpen egress config dev --on-deny prompt   # ask instead of just logging
```

Allowlist edits are recorded in the audit log.

## Prompting on deny

With `on_deny: prompt`, a blocked request waits up to `prompt_timeout` (30 s by default)
for your decision. Answer prompts in a terminal or through desktop notifications:

```
catchpen egress watch [--desktop]
```

The daemon also answers prompts through desktop notifications, for every VM that no
`catchpen egress watch` already serves; a `watch` started after the daemon fails for the
VMs it serves. Without `notify-send`, the daemon's prompts are denied at once. With
nobody watching, blocked requests are denied immediately.

Prompts for requests that already timed out are dropped, and each VM can have at most 8
distinct destinations waiting (more are denied), so a guest can't bury you in prompts.
Answered prompts go to the audit log; timeouts are only in the VM's egress log.

## Logs

`vms/<name>/egress.log` records every request the proxy allows or blocks by policy, plus
`CACHE` entries for package cache fetches; requests refused before a policy check
(malformed, a disallowed method, over the connection cap) aren't logged. It rotates at
32 MiB, keeping one old file (`egress.log.1`). An allowed proxy connection whose log
entry can't be written is refused; a package cache fetch goes ahead.

## Limits

- Entries match the requested name, not the resolved address.
- CDN hosts on the allowlist (such as `*.githubusercontent.com`) can be used for domain
  fronting.
- A preset for a service that takes uploads (`github`, `ghcr`, `docker-hub`, `npm`) is
  also a way out: a guest holding a token or key for it, yours or its own, can push data
  there (to GitHub over HTTPS or SSH).
- Skills and plugins are fetched by the host and copied in over SSH, whatever the VM's
  mode, `none` included (pi's `bundled` plugins are files already in the VM).
- The proxy caps request headers at 64 KiB and concurrent connections at 128 per VM (for
  the proxy and the caches each), closes connections idle for 10 minutes, and accepts
  only standard HTTP methods.
