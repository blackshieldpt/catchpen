# The API

`catchpen daemon` serves an HTTP/JSON API: the operations of the CLI, for a web cockpit
or a script. It is off until you set `server:` in `config.yaml`; without it the CLI and
the daemon work as before.

There are two front doors:

- **The unix socket**, `api.sock` in the data directory, mode 0600: the daemon's own
  user, who has every scope. This is the same trust as running `catchpen` yourself.
- **TCP**, on `server.listen`, over TLS 1.3: clients enrolled with
  `catchpen daemon client add`, each by its certificate or an HMAC key, within the scopes
  it was enrolled with.

The wire contract is the `pkg/api` package (paths, types, error codes and HMAC signing)
and the typed Go client is `pkg/apiclient`. A `config:write` client is as powerful as
your shell: see [the security model](security-model.md#the-api).

## Turning it on

| `config.yaml` | What the daemon serves |
|---|---|
| nothing, or `server:` | nothing (a bare `server:` is the same as not setting it) |
| `server: {}` | the unix socket |
| `server: { listen: ":7443" }` | the unix socket, and TCP on `:7443` |

The port is a string: quote it, and write `"[::1]:7443"` to listen on IPv6 loopback only.
`server:` is read when the daemon starts, so restart it after a change
(`systemctl --user restart catchpen`).

```
$ catchpen daemon info
config:      /home/you/.config/catchpen
socket:      /home/you/.local/share/catchpen/api.sock
certificate: sha256:…
clients:     1
```

`config` is the config directory, which holds `clients.yaml` and the server's
`server.key` and `server.crt`; `socket` is the unix socket in the data directory.
`certificate` is the pin enrolled clients check instead of a CA (the key and certificate
are made on first use); `clients` counts those enrolled.

The daemon refuses to start `server.listen` with no client enrolled. The port is
reachable by anything that can route to it: the API authenticates each client, but does
not confine where it connects from, so keep it on a private network or behind a firewall
and don't expose it to the internet.

The unix socket is the CLI's trust: a process of the daemon's user can use every route
with every scope, as it can run `catchpen`. Its 0600 mode is what keeps it to that user.

## Enrol a client

```sh
catchpen daemon client add cockpit --cert cockpit.pem
catchpen daemon client add ci --hmac > ci.key
catchpen daemon client add admin --hmac --scope config:write
catchpen daemon client ls
catchpen daemon client rm cockpit
```

| Flag | Meaning |
|---|---|
| `--cert <file>` | The client's certificate (PEM). The server pins its public key; the client keeps the private key. |
| `--hmac` | Generate a 32-byte key and print it on stdout as 64 hex digits, once. |
| `--scope config:write` | Also grant the `config:write` scope (see [Scopes](#scopes)). |

Exactly one of `--cert` and `--hmac` is required. `--scope` may be repeated, but
`config:write` is the only scope there is. A client name has a VM's shape: lowercase
letters, digits and hyphens.

Enrolments go to `clients.yaml` in the config directory. `client ls` shows each client's
name, method, scopes and when it was added, never a key or certificate. `client add` and
`client rm` are audited as `api.client.add` and `api.client.rm`, with the client's name
(and, on add, its method, scopes and the certificate's pin; never a key). `client rm`
refuses the client's next request, and its open streams end within 2 s.

The pin is `sha256:` and the hex SHA-256 of the certificate's public key, not the
certificate's fingerprint. `openssl x509 -in server.crt -pubkey -noout | openssl pkey
-pubin -outform der | sha256sum` prints the same value. Remove `server.crt` and run
`daemon info` to make a new certificate for the same key, with the same pin.

## Scopes

The unix socket has every scope. An enrolled client has none unless it was enrolled with
`--scope config:write`; there is no read-only scope, and a client is one name, with no
per-user identity.

`config:write` lets a client:

- read and write `config.yaml` and the template category files, and show a template;
- list, set and unset the secrets in `secrets.env` (no call returns a value);
- scan, add and remove forges, and read the secret names those events carry;
- submit a headless task, and read its output;
- change a `task-…` VM: start, stop, resume, destroy, reset, rebase and push credentials
  to it, change its egress, open a forward, save, restore or delete a snapshot, and pull
  a branch or open a pull request from it, open a terminal or console on it, and answer
  its egress prompts;
- read a job's error and progress lines, which can quote its guest;
- read the audit detail that names host secrets or hashes a file.

Each of those can run host code (`config.yaml`'s `secrets:` commands), use a host
credential, or read a guest that holds the template's secrets. Enrol a `config:write`
client only if you would trust it with your shell: see
[the security model](security-model.md#the-api).

## Connecting

`pkg/apiclient` is the Go client:

```go
c, err := apiclient.New(apiclient.Options{
	URL:       "https://host:7443",
	ServerPin: "sha256:…",                                 // catchpen daemon info
	HMAC:      &apiclient.HMAC{Name: "cockpit", Key: key}, // or Cert: &cert
})
```

For the unix socket, `Socket` replaces `URL` and `ServerPin`:

```go
c, err := apiclient.New(apiclient.Options{Socket: "/home/you/.local/share/catchpen/api.sock"})
```

Then one method per call, typed from `pkg/api`: `VMs`, `VM`, `StartVM`, `Terminal`,
`Events`, and so on.

Over raw HTTP, the socket takes plain HTTP with the socket path as the address; TCP takes
HTTPS with the server's key pinned, and a client certificate or the HMAC headers below.
Bodies are JSON, and every `POST` needs `Content-Type: application/json`.

An HMAC request carries:

| Header | Value |
|---|---|
| `X-HMAC-Hash` | `<client name>.<hex HMAC-SHA256>` |
| `X-HMAC-Timestamp` | the time, RFC 3339, UTC |
| `X-HMAC-Nonce` | a random value, unique per request (a UUID) |
| `X-HMAC-Version` | `2` |

The MAC is HMAC-SHA256 with the key's 32 bytes, over:

```
<timestamp>:<nonce>:<method>:<target>:<hex SHA-256 of the body>
```

`target` is the path and query exactly as sent. A timestamp more than 60 s from the
server's clock is refused, and a nonce is remembered for that window, so sign each
request once. The body is read in full (at most 4 MiB) before the signature is checked.

## The routes

These tables are the map. Each feature's page has the request and response types, the
options and the checks its calls make: [configuration.md](configuration.md#over-the-api),
[snapshots.md](snapshots.md#over-the-api), [egress.md](egress.md#over-the-api),
[forges.md](forges.md#over-the-api), [tasks.md](tasks.md#over-the-api) and
[templates.md](templates.md#over-the-api).

Every route is under `/v1`. In the paths, `<name>` is a VM's name, `<id>` a task's or
job's, `<snap>` a snapshot's and `<category>` a template category.

"Any" means any enrolled client, and every client on the unix socket. "A task VM" means
any VM whose name starts with `task-`, including one made with the CLI. A scope check
runs before the VM, task or body is looked at: a client without the scope gets 403
whatever the name is.

### Info, audit, jobs and events

| Call | What it does | Scope |
|---|---|---|
| `GET /v1/info` | catchpen's version and the API's. | any |
| `GET /v1/cache` | What the package caches hold. | any |
| `GET /v1/audit` | A page of the audit log, newest first (`before`, `limit`). | any; detail naming host secrets needs `config:write` |
| `GET /v1/audit/follow` | Follow the audit log as NDJSON. | any; the same detail needs `config:write` |
| `GET /v1/jobs` | The jobs running and the last that ended, newest first. | any; a job's text needs `config:write` |
| `GET /v1/jobs/<id>` | One job, and its progress lines (`after`). | any; the same text needs `config:write` |
| `GET /v1/events` | Every VM's state at connect, then its changes, as NDJSON. | any |

### VMs

| Call | What it does | Scope |
|---|---|---|
| `GET /v1/vms` | Every VM, with its state, template, size and egress mode. | any |
| `POST /v1/vms` | Create and start a VM, as `catchpen up`. | any |
| `GET /v1/vms/<name>` | One VM. | any |
| `POST /v1/vms/<name>/start` | Start it. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/stop` | Stop it, with `force` to power it off. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/resume` | Resume a VM a canary hit paused. | any; a task VM needs `config:write` |
| `GET /v1/vms/<name>/destroy` | What destroying it loses. | any |
| `POST /v1/vms/<name>/destroy` | Destroy it (`confirm`). | any; a task VM needs `config:write` |
| `GET /v1/vms/<name>/reset` | What resetting it loses. | any |
| `POST /v1/vms/<name>/reset` | Reset it (`confirm`, `latest`). | any; a task VM needs `config:write` |
| `GET /v1/vms/<name>/rebase` | What rebasing it loses. | any |
| `POST /v1/vms/<name>/rebase` | Rebase it onto its template's current base. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/push` | Push the VM's credentials into it. | any; a task VM needs `config:write` |
| `GET /v1/vms/<name>/snapshots` | Its snapshots. | any |
| `POST /v1/vms/<name>/snapshots` | Save one (`name`, `disk_only`). | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/snapshots/<snap>/restore` | Restore one (`confirm`, `disk_only`). | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/snapshots/<snap>/delete` | Delete one (`confirm`). | any; a task VM needs `config:write` |
| `GET /v1/vms/<name>/terminal` | An interactive terminal, as a WebSocket. | any; a task VM needs `config:write` |
| `GET /v1/vms/<name>/console` | The serial console, as a WebSocket. | any; a task VM needs `config:write` |

### Egress, forwards and prompts

| Call | What it does | Scope |
|---|---|---|
| `GET /v1/vms/<name>/egress` | The VM's egress mode, settings and lists. | any |
| `POST /v1/vms/<name>/egress/allow` | Add entries to the allowlist. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/egress/unallow` | Remove entries from the allowlist. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/egress/deny` | Add entries to the denylist. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/egress/undeny` | Remove entries from the denylist. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/egress/config` | Change the VM's egress settings. | any; a task VM needs `config:write` |
| `GET /v1/vms/<name>/egress/log` | A page of its egress log (`before`, `limit`). | any |
| `GET /v1/vms/<name>/egress/log/follow` | Follow its egress log as NDJSON (`after`). | any |
| `GET /v1/vms/<name>/forwards` | Its live forward sessions. | any |
| `POST /v1/vms/<name>/forwards` | Open a forward, as a job. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/forwards/stop` | End all its forward sessions. | any |
| `GET /v1/egress/prompts` | Egress prompts, as a WebSocket. | any; answering a task VM's prompt needs `config:write` |

### Forges and work leaving a VM

| Call | What it does | Scope |
|---|---|---|
| `GET /v1/forges` | The forges added. | any |
| `POST /v1/forges` | Add a forge. | `config:write` |
| `POST /v1/forges/scan` | Scan a host for a forge. | `config:write` |
| `POST /v1/forges/<name>/test` | Test a forge's credentials. | any |
| `POST /v1/forges/<name>/delete` | Remove a forge. | `config:write` |
| `POST /v1/vms/<name>/pull` | Pull a branch from the VM, as a job. | any; a task VM needs `config:write` |
| `POST /v1/vms/<name>/pr` | Open a pull request from the VM, as a job. | any; a task VM needs `config:write` |

### Tasks and templates

| Call | What it does | Scope |
|---|---|---|
| `GET /v1/tasks` | The headless tasks, newest first. | any; the error is withheld without `config:write` |
| `POST /v1/tasks` | Submit one. | `config:write` |
| `GET /v1/tasks/<id>` | One task. | any; the error is withheld without `config:write` |
| `POST /v1/tasks/<id>/cancel` | Cancel it, as a job. | any |
| `POST /v1/tasks/<id>/delete` | Delete it, as a job. | any |
| `GET /v1/tasks/<id>/output` | Its output, as NDJSON, or a page (`after`, `limit`, `follow`). | `config:write` |
| `GET /v1/templates` | The template categories and their sections. | any |
| `GET /v1/templates/show` | The template a `-t` spec gives. | `config:write` |
| `POST /v1/templates/build` | Build a base, as a job. | any |
| `POST /v1/templates/rebuild` | Rebuild the bases in use, as a job. | any |
| `GET /v1/templates/files/<category>` | A template category file. | `config:write` |
| `POST /v1/templates/files/<category>` | Write or create one. | `config:write` |
| `POST /v1/templates/files/<category>/delete` | Delete one (`confirm`). | `config:write` |

### Config and secrets

| Call | What it does | Scope |
|---|---|---|
| `GET /v1/config` | `config.yaml`, its hash and its effective settings. | `config:write` |
| `POST /v1/config` | Write `config.yaml`, if it is still the file read (`base_hash`). | `config:write` |
| `GET /v1/secrets` | The names in `secrets.env`; never a value. | `config:write` |
| `POST /v1/secrets/<name>` | Set one (`confirm` to replace a value). | `config:write` |
| `POST /v1/secrets/<name>/delete` | Unset one (`confirm`). | `config:write` |

## Requests, jobs and errors

A body may be at most 4 MiB. Every `POST` needs `Content-Type: application/json`, with or
without a body; a parameter such as `charset=utf-8` may follow. Otherwise it is refused
`415 unsupported_media_type`: a web page can make a browser holding a client's
certificate send some other POST without asking, but not that type without a CORS
preflight, which the API never allows.

A call that starts long work — a VM's lifecycle, a snapshot, a forward, a pull or push, a
template build, a task's cancel or delete — runs as a job and answers `202` with it;
`GET /v1/jobs/<id>` follows it. The job runs on whether or not its client stays; stopping
the daemon cancels the running ones.

An error is `{"error": {"code": "...", "message": "..."}}`:

| Code | Status | Meaning |
|---|---|---|
| `bad_request` | 400 | The request's fields, or what they name, are wrong. |
| `unauthorized` | 401 | No valid client credentials. |
| `forbidden` | 403 | The client lacks the scope. |
| `not_found` | 404 | No such route, VM, task or snapshot. |
| `conflict` | 409 | The state changed, or doesn't allow it. |
| `vm_paused` | 409 | A canary hit paused the VM. |
| `too_large` | 413 | The body is over 4 MiB. |
| `unsupported_media_type` | 415 | A `POST` without `application/json`. |
| `confirm_required` | 428 | A destructive call without `"confirm": true`. |
| `rate_limited` | 429 | A per-client request or stream limit. |
| `internal` | 500 | The daemon failed. |

`confirm_required` is the API's `[y/N]`: a call that deletes data needs `"confirm": true`,
and some also need the `snapshots`, `paused` and `canary_hit` that `GET` on the same path
shows, so that what is lost is what the client saw. `vm_paused` is a canary hit: resume,
stop or destroy the VM first.

## Streams

The terminal, console and egress prompts are WebSockets. The audit follow, egress log
follow, task output and events are NDJSON (`application/x-ndjson`): one JSON value per
line, and an error after the stream started is a last line of the same `error` shape.

A terminal or console WebSocket carries the session's bytes as binary messages, in both
directions; text messages are one `Frame` of JSON: a client sends `resize` with `w` and
`h`, and the server's last message is `end` with `exit` and/or `error`. A terminal runs
`ssh` (the default), `run` or `attach`, with `kind`, `agent`, `arg`, `cols` and `rows` in
the query (`cols` and `rows` default to 80 and 24; both or neither), and `TERM` is
`xterm-256color`. A message is at most 1 MiB, and a size is 1 to 4096 each way.

A stream whose `Origin` is set must be the API's own origin; a client that isn't a
browser sends none, which is allowed. So a web page on another origin can't open one with
a certificate the browser holds. A paused VM refuses a terminal and console, the console
is one stream per VM, and a canary hit ends it. The terminal is raw, as `catchpen ssh`
is; the console is filtered as `catchpen console` filters it. Every client's terminal is
recorded and audited as `web-ssh`, `web-run` or `web-attach`.

Guest text from any stream is data: a client must not render it as markup, and a terminal
client must turn off OSC 52, OSC 8 and title handling.

## Limits

| What | Limit |
|---|---|
| Connections to `server.listen` | 64 at once, 16 from one address (IPv4, or an IPv6 /64) |
| Headers, and a TLS handshake | 10 s |
| Authenticating a connection | 60 s from the connection's open |
| The body, after the headers; the answer | 30 s each; 90 s for a call that reaches a forge |
| An idle connection | 2 minutes |
| A request body | 4 MiB |
| Streams, per client | 8 open at once; beyond it, `429` |
| Requests, per client | 30 in a burst, refilled 10/s; beyond it, `429` |
| Jobs | 16 running at once; the last 32 kept, 500 progress lines each |
| A removed client | its open streams end within 2 s |

The unix socket is exempt from the per-client request and stream limits. A refused
stream's message names the cap. The values are catchpen's, not configuration.

## Versioning

`GET /v1/info` answers `{"version": "...", "api": 1}`: the daemon's catchpen version and
the API's. A change a client could notice in a way that breaks it gets a new version under
a new prefix, so a client can refuse an API version it doesn't know. The API version is
one check on whether a host is new enough for a cockpit.

## What it never returns

No call returns a value from `secrets.env`, `canaries.json` or the private keys under
`ssh/`; a VM's resolved environment, forge credentials or agent settings; or a task's
prompt and command. `GET /v1/templates` never carries a section's settings, and showing a
template needs `config:write`. A job's error and lines, a task's error and the audit
detail that names host secrets are withheld from a client without the scope, since the
guest can print a secret into any of them.

See [sessions.md](sessions.md) for recordings and the audit log,
[canaries.md](canaries.md) for what a hit refuses, and
[deployment.md](deployment.md) for the daemon's jobs.
