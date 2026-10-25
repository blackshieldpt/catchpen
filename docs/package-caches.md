# Package caches

VMs whose template lists `cache:` fetch packages through curral on the host, which
keeps what never changes for every other VM and base build.

Each cache is a section of `caches.yaml`: `apt-cache`, `go-cache`, `npm-cache`,
`pypi-cache` and `docker-cache`. The default has all five; `-name` drops one:

```
curral up dev -t -npm-cache,-docker-cache
```

The docker cache needs Docker in the VM, which the `docker` section of `runtimes.yaml`
(a default) installs. The built-in `minimal` profile has no caches.

```yaml
# config.yaml
cache_max: 20G          # default; least recently used objects go first
```

```
curral cache                                   # what each cache holds
curral cache clear [apt|go|npm|pypi|docker ...]
```

## What is cached

The guest reaches the caches at `10.0.2.102`. The store is
`~/.cache/curral/pkg/<kind>/`.

| Cache | Stored | Fetched every time |
|---|---|---|
| `apt` | `.deb` files from the Ubuntu and Debian archive and security mirrors; apt lists fetched by hash | `InRelease` |
| `go` | module files and checksum database tiles from proxy.golang.org and sum.golang.org | version lists |
| `npm` | tarballs from registry.npmjs.org | package metadata |
| `pypi` | files from files.pythonhosted.org | indexes from pypi.org |
| `docker` | Docker Hub layers and manifests by digest | tags |

The guest is preconfigured: apt, `GOPROXY`, npm, yarn, pnpm, pip, uv, and Docker's
registry mirror (merged into the VM's `daemon.json` at first boot). Other mirrors, registries and private
indexes still go through the egress proxy.

Cached hosts need no allowlist entry, but `egress deny` still applies, redirects
included. Cache fetches show in `curral egress log` as `CACHE`. Base builds use the
template's caches too (Docker's excepted) without changing the base. VMs with
`egress: none` use none.

## Size and disk space

The caches trim themselves to `cache_max` every 10 minutes while fetching (and `gc`
does), so they can be over it in between. A download stops being stored when the
filesystem would drop below 5% free. This is checked as the file is written, so parallel
downloads count; the guest's response is cut short, as when the upstream fails. Objects
sent without a length, and paths that aren't in canonical form, are passed through but
not stored.

## Security

- The caches fetch over HTTPS from their fixed upstreams and the CDNs those redirect to,
  never from private addresses, and accept only GET and HEAD. A cache is about as open as
  allowlisting its preset; proxy.golang.org and sum.golang.org fetch any module path
  they're asked for.
- Guests can't put anything in the store. Docker objects and apt by-hash files are
  checked against their digest, the rest by the clients (apt signatures, the Go checksum
  database, lockfile hashes).
- The store is shared by all VMs, so a VM can tell from timing whether a package was
  already cached.
- A canary value in a cache request counts as a hit; see [canaries.md](canaries.md).
- Each VM has at most 128 cache connections. A guest that stops reading for 30 s is
  dropped (a download it started finishes for the store).
- A response with a `Content-Encoding` is passed through, not stored: the store keeps
  only identity bodies.
