# Forges, repos and SSH

A forge is a GitHub or Gitea server that VMs clone from. curral gives each VM the
narrowest access the forge supports, and gets work out through the host so the VM
needs no push access.

## Adding a forge

```
curral forge add github --type github --host github.com --mode deploy-key --token-secret GH_ADMIN_TOKEN
curral forge add acme --type gitea --host git.acme.example --ssh-port 2222 \
    --mode minted-token --user bot --password-secret ACME_PASSWORD
curral forge test acme
curral forge ls
```

`forge add` pins the forge's SSH host key: GitHub's from `api.github.com/meta`, others
scanned and confirmed by you (`-y` trusts the scan without asking). Forges are stored in
`~/.config/curral/forges.yaml`. GitHub Enterprise and Gitea on another URL take
`--http-url`.

## Modes

| `--mode` | The VM gets | The host-side token is used for |
|---|---|---|
| `deploy-key` | a per-VM, per-repo SSH key (`--ro`: read-only) | adding and removing the key |
| `minted-token` (Gitea) | a per-VM token, minted with the account's password | minting and deleting it |
| `token` | your token (`--token-secret`), for HTTPS and `gh`/`tea` | — |
| `agent` | nothing; SSH signs with your key through a restricted agent | — |
| `none` | nothing; anonymous clone; work leaves through `pull`/`pr` | opening PRs |

In every mode, `curral pr` opens pull requests with the forge's token, or the account's
password for `minted-token` without one.

Deploy keys and minted tokens are removed when the VM is destroyed. Tokens and passwords
are always `secret:` references; see [configuration.md](configuration.md#secrets).

In the VM, git SSH goes through the egress proxy (`~/.ssh/curral_config`, with pinned
`known_hosts`), HTTPS tokens sit in `~/.config/curral/git-credentials`, `gh` gets
`GH_TOKEN` and `tea` a login. Forge hosts are added to the VM's allowlist. Install the
CLIs with `tools: [gh, tea]` in a template (the `default` template has `gh`).

## Repos

```
curral up dev --repo github:acme/api --repo https://git.acme.example/team/web [--ro]
```

Repos (`--repo`, or `repos:` in a template) are `forge:owner/repo`, or a URL whose host
matches a forge. They are cloned into `/workspace/<repo>` at start.

## Getting work out

```
curral pull dev --repo api --into ~/src/api     # -> refs/curral/dev/<branch>
curral pull dev --repo api --into ~/src/api --patches ~/patches
curral pr dev --repo api --into ~/src/api       # pull, push with your git creds, open the PR
```

`pull` copies the branch out as a git bundle and fetches it into
`refs/curral/<vm>/<branch>` of the host repo, with hooks and fsmonitor off and object
checks on. The host working tree is untouched, and nothing from the guest runs on the
host. The commits are scanned for canaries first. `--patches` also writes the branch as
an mbox, made on the host from the scanned commits.

`pr` pushes `curral/<vm>/<branch>` through a remote of the host repo that points at the
same forge repo, then opens the PR with the forge's API token. `--title`, `--body-file`,
`--draft`, `--base` and `--head` adjust it.

## SSH agent

```yaml
# template
ssh:
  agent: restricted          # off (default) | restricted | host
  agent_keys: [~/.ssh/id_ed25519]
  confirm: true              # default: ask (ssh-askpass) on every use of a key
```

With `restricted`, `curral start` runs a per-VM `ssh-agent` holding only `agent_keys`,
each destination-constrained (`ssh-add -h`), so it signs only on the path host → VM →
that VM's `agent`-mode forges. The VM can't use it to log in anywhere else, including
back into itself. `ssh curral-<vm>` forwards that agent, never your own, and it ends when
the VM stops. `--ssh-agent` and `--agent-key` on `curral up` override the template.

The constraints need the forge's SSH server to support OpenSSH host-bound
authentication: github.com and Gitea behind OpenSSH do; gitlab.com and Gitea's built-in
SSH server don't. `forge add` and `forge test` check this for `agent`-mode forges.

`host` forwards your own agent (`$SSH_AUTH_SOCK`): anything in the VM can use all of its
keys while you're connected. curral warns when you create such a VM, and `agent`-mode
forges require `restricted`, since start-time clones would otherwise forward your agent
unattended.

## Port forwards

```
curral forward dev -L 3000 -L 8080:80      # guest ports on the host's 127.0.0.1
curral forward dev -R 5432                 # host 127.0.0.1:5432 into the guest
curral forward dev -D 1080                 # SOCKS proxy on the host into the guest
curral forward dev --list
curral forward dev --stop
```

Each forward is a background `ssh -N`, logged in `vms/<name>/forward.log`; all of them
end when the VM stops. `-R` bypasses the egress proxy, so it is recorded in the VM's
egress log. Template `ports:` are forwarded at start instead.
