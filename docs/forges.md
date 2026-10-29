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
`--http-url`.

## Modes

| `--mode` | The VM gets | The host-side token is used for |
|---|---|---|
| `deploy-key` | a per-VM, per-repo SSH key (`--ro`: read-only) | adding and removing the key |
| `minted-token` (Gitea) | a per-VM token, minted with the account's password | minting and deleting it |
| `token` | your token (`--token-secret`), for HTTPS and `gh`/`tea` | — |
| `agent` | nothing; SSH signs with your key through a restricted agent | — |
| `none` | nothing; anonymous clone; work leaves through `pull`/`pr` | opening PRs |

In every mode, `catchpen pr` opens pull requests with the forge's token, or the account's
password for `minted-token` without one.

Deploy keys and minted tokens are removed when the VM is destroyed. Tokens and passwords
are always `secret:` references; see [configuration.md](configuration.md#secrets).

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
