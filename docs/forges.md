# Forges, repos and SSH

A forge is a GitHub or Gitea server that VMs clone from. catchpen gives each VM the
narrowest access the forge supports, and gets work out through the host so the VM
needs no push access.

New to this? The [GitHub tutorial](github.md) walks through a GitHub setup step by step.

## Adding a forge

```
catchpen forge add github --type github --host github.com --mode deploy-key --token-secret GH_ADMIN_TOKEN
catchpen forge add acme --type gitea --host git.acme.example --ssh-port 2222 \
    --mode minted-token --user bot --password-secret ACME_PASSWORD
catchpen forge test acme
catchpen forge ls
```

`forge add` pins the forge's SSH host key: GitHub's from `api.github.com/meta`, others
scanned and confirmed by you (`-y` trusts the scan without asking). Forges are stored in
`~/.config/catchpen/forges.yaml`. GitHub Enterprise and Gitea on another URL take
`--http-url`. An `http://` URL sends the forge's token or password to its API
unencrypted, and in `token` and `minted-token` modes the VM's token when it clones;
`forge add`, `forge test` and creating a VM warn about it. API calls refuse a redirect to
another host, from https to http, or one that would turn a request into a GET (a 301,
302 or 303 on a POST or DELETE, as some forges answer for a renamed repo): the error
names where it points, so you can fix `--http-url` or the repo name.

## Modes

| `--mode` | The VM gets | The host-side token is used for |
|---|---|---|
| `deploy-key` | a per-VM, per-repo SSH key (`--ro`: read-only) | adding and removing the key |
| `minted-token` (Gitea) | a per-VM token, minted with the account's password; it reaches every repo the account can, so use an account that can reach only those | minting and deleting it |
| `token` | your token (`--token-secret`), for HTTPS and `gh`/`tea` | — |
| `agent` | nothing; SSH signs with your key through a restricted agent | — |
| `none` | nothing; anonymous clone; work leaves through `pull`/`pr` | opening PRs |

In every mode, `catchpen pr` opens pull requests with the forge's token, or the account's
password for `minted-token` without one.

Deploy keys and minted tokens are removed when the VM is destroyed. If the forge answers
that a key is gone while the token can no longer see the repo or is no longer its admin,
catchpen warns instead, and you remove the key in the repo's settings. A key or token the
forge creates without reporting it fully (no id or no secret) is looked up and removed,
and the VM isn't created; if it can't be removed, the error names it and the audit log
records it. Tokens and passwords are always `secret:` references; see
[configuration.md](configuration.md#secrets).

In the VM, each deploy-key repo and each agent-mode forge gets an SSH name, and one VM
can't take two with the same name. A repo's is `catchpen-` then its forge, owner and name
joined by `-`, lower-cased, with each run of other characters than letters, digits and
`-` turned into one `-` and none left at either end: `o/b.c` and `o/b_c` match, as do
`o/b-c` and `o-b/c`. An agent-mode forge's is `catchpen-forge-<forge name>`.

In the VM, git SSH goes through the egress proxy (`~/.ssh/catchpen_config`, with pinned
`known_hosts`), HTTPS tokens sit in `~/.config/catchpen/git-credentials`, `gh` gets
`GH_TOKEN` and `tea` a login. Forge hosts are added to the VM's allowlist. The CLIs are
the `gh` and `tea` sections of `tools.yaml`: `gh` is in the default, `-t tea` adds `tea`
and `-t -gh` drops `gh`.

## Repos

```
catchpen up dev --repo github:acme/api --repo https://git.acme.example/team/web [--ro]
```

Repos are `forge:owner/repo`, or a URL whose host matches a forge. They are cloned into
`/workspace/<repo>` at start. Repos you use often can go in a section of `repos.yaml`
instead of `--repo`:

```yaml
# ~/.config/catchpen/templates/repos.yaml
api: { repos: [github:acme/api] }
```

```
catchpen up dev -t api
```

## Getting work out

```
catchpen pull dev --repo api --into ~/src/api     # -> refs/catchpen/dev/<branch>
catchpen pull dev --repo api --into ~/src/api --patches ~/patches
catchpen pr dev --repo api --into ~/src/api       # pull, push with your git creds, open the PR
```

`pull` copies the branch out as a git bundle and fetches it into
`refs/catchpen/<vm>/<branch>` of the host repo, with hooks and fsmonitor off and object
checks on. The host working tree is untouched, and nothing from the guest runs on the
host. The commits are scanned for canaries first. `--patches` also writes the branch as
an mbox, made on the host from the scanned commits.

`pr` pushes `catchpen/<vm>/<branch>` through a remote of the host repo that points at the
same forge repo, then opens the PR with the forge's API token. `--title`, `--body-file`,
`--draft`, `--base` and `--head` adjust it.

## Over the API

With the daemon's API on (`server:` in [configuration.md](configuration.md)):

| Call | As | Takes |
|---|---|---|
| `GET /v1/forges` | `forge ls` | nothing; it names each forge's secrets, never their values |
| `POST /v1/forges/scan` | `forge add`'s scan | `type`, `host`, `http_url`, `ssh_port` |
| `POST /v1/forges` | `forge add` | `name`, `type`, `host`, `http_url`, `ssh_port`, `mode`, `token_secret`, `user`, `password_secret`, `scopes`, and the scan's `fingerprints` you confirmed |
| `POST /v1/forges/<name>/test` | `forge test` | nothing |
| `POST /v1/forges/<name>/delete` | `forge rm` | `"confirm": true` |
| `POST /v1/vms/<name>/pull` | `pull` | `into`, `repo`, `branch` |
| `POST /v1/vms/<name>/pr` | `pr` | `pull`'s, and `title`, `body`, `draft`, `base`, `head` |

Adding a forge takes two calls. The scan answers with the SSH host keys' fingerprints
(github.com's from its API, with `"published": true`); the add scans again and pins the
keys only if their fingerprints are those you sent (else 409: scan again). `deploy-key`
and `agent` modes need them; the others may send none and pin nothing. A forge names
host secrets that catchpen sends to the forge's host, so a client that can add one can
have any secret in `secrets.env` sent to a host it chooses: scanning, adding and
removing need a client enrolled with `--scope config:write` (403 otherwise; over the
unix socket, every call is allowed); listing and testing don't. Adds and removals are
audited (`forge.add`, with the secrets the forge names, and `forge.rm`, both with
`client`); the CLI's aren't. A test answers with what passed and, in `error`, the check
that failed, which can quote the forge's API (up to 300 characters) and its `login`.
Calls that reach a forge answer within 60 s, and a TCP client has 90 s to read the
answer.

Pull and pr run as jobs. `into` is the absolute path of a git repository on the daemon's
host, written clean (no trailing `/`, `.` or `..`; 400 otherwise). There is no
`--patches`, and pr takes its description as `body`, not a file, so `into` is the only
host path a call names. pr pushes with the daemon's git credentials, from its
environment, not your shell's, and only onto a branch under `catchpen/` (`head`, default
`catchpen/<vm>/<branch>`; 400 otherwise), so not onto the repo's default branch; a push
can still run the forge's CI on that branch, as the CLI's does. Both are refused (409)
on a VM a canary hit paused, and their audit events (`export.pull`, `export.pr`) carry `client`.

## SSH agent

A VM's agent comes from `ssh.yaml`, a one-choice category: the default is `ssh-off`
(no agent), the built-in `ssh-host` forwards your own agent, and a section of your own
sets up a restricted one:

```yaml
# ~/.config/catchpen/templates/ssh.yaml
my-key:
  ssh:
    agent: restricted          # off | restricted | host
    agent_keys: [~/.ssh/id_ed25519]
    confirm: true              # default: ask (ssh-askpass) on every use of a key
```

```
catchpen up dev -t my-key
```

With `restricted`, `catchpen start` runs a per-VM `ssh-agent` holding only `agent_keys`,
each destination-constrained (`ssh-add -h`), so it signs only on the path host → VM →
that VM's `agent`-mode forges. The VM can't use it to log in anywhere else, including
back into itself. `ssh catchpen-<vm>` forwards that agent, never your own, and it
ends when catchpen stops or destroys the VM (after a guest power-off, at the next
`stop`, `destroy` or `start`). `--ssh-agent` and `--agent-key` on `catchpen up`
override the VM's `ssh.yaml` section.

The constraints need the forge's SSH server to support OpenSSH host-bound
authentication: github.com and Gitea behind OpenSSH do; gitlab.com and Gitea's built-in
SSH server don't. `forge add` and `forge test` check this for `agent`-mode forges.

`host` forwards your own agent (`$SSH_AUTH_SOCK`): anything in the VM can use all of its
keys while you're connected. catchpen warns when you create such a VM, and `agent`-mode
forges require `restricted`, since start-time clones would otherwise forward your agent
unattended.

For a step-by-step setup of GitHub with your own SSH key, and a token for `gh` in the
VM, see the [GitHub tutorial](github.md).

## Port forwards

```
catchpen forward dev -L 3000 -L 8080:80      # guest ports on the host's 127.0.0.1
catchpen forward dev -R 5432                 # host 127.0.0.1:5432 into the guest
catchpen forward dev -D 1080                 # SOCKS proxy on the host into the guest
catchpen forward dev --list
catchpen forward dev --stop
```

Each forward is a background `ssh -N`, logged in `vms/<name>/forward.log`; all of them
end when the VM stops. `-R` bypasses the egress proxy, so it is recorded in the VM's
egress log. `ports:` from a `ports.yaml` section, such as
`web: { ports: [3000, "8080:80"] }` picked with `-t web`, are forwarded at start instead.

Over the daemon's API (`server:` in [configuration.md](configuration.md)),
`GET /v1/vms/<name>/forwards` lists the sessions, `POST /v1/vms/<name>/forwards` with
`{"local": [...], "remote": [...], "dynamic": [...]}` (`-L`, `-R` and `-D`'s forms, 16
at most) opens one as a job, and `POST /v1/vms/<name>/forwards/stop` ends them all. The
session's ssh runs on the daemon's host: `local` and `dynamic` listen on its 127.0.0.1,
and `remote` reaches what it can reach. Opening one is refused (409) on a VM a canary
hit paused, one that isn't running, a warm pool VM, and one with `ssh.agent` other than
`off`, whose agent the session would forward: use the CLI there.
