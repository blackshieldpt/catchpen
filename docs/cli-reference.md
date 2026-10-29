# CLI reference

`catchpen <command> --help` prints the same information. The one global flag is
`--debug`: it streams the whole base build log instead of progress lines, logs the VM
serial console, and leaves a VM that fails to boot running for inspection.

Exit status is 0 on success and 1 on error (printed as `catchpen: <error>`). `ssh`, `run`,
`attach`, `cp` and `up --ephemeral` pass on the exit status of what they ran. Commands
that take several names (`stop`, `destroy`, `rmsnap`, `task cancel`, `task rm`,
`creds push`) try them all and fail if any failed. A first SIGINT, SIGTERM or SIGHUP
cancels the running command cleanly; a second one kills catchpen.

## VM lifecycle

### `catchpen up <name> [--ephemeral [-- command...]]`

Create a VM if it doesn't exist, and start it.

| Flag | Meaning |
|---|---|
| `-t, --template` | Picks from the [template](templates.md) category files, comma separated: `go-1.27,large`, `-docker`, `debian-13`. Default: every category's default. |
| `--cpus`, `--mem`, `--disk` | Resources, e.g. `--mem 16G --disk 60G`. |
| `--rebuild` | Rebuild the template base first. |
| `--allow-open` | Confirm a template with network egress `open` (the `open` section). |
| `--repo` | Clone a repo into `/workspace` (`forge:owner/repo` or a URL; repeatable). |
| `--ro` | Make deploy keys read-only. |
| `--ssh-agent` | `off`, `restricted` or `host` (default: the template's). |
| `--agent-key` | Host key file for the restricted agent (repeatable). |
| `--claude-auth` | `oauth-token` or `api-key` for this VM. |
| `--ephemeral` | Create a new VM, attach (or run the command) and destroy it when the session ends. |

The creation flags are refused for a VM that already exists. With `--ephemeral -- cmd`,
each argument is shell-quoted.

### `catchpen start <name>`

Start an existing VM.

### `catchpen stop <name>... [-f]`

Shut VMs down (ACPI power-off, forced after a timeout). `-f, --force` powers off at once.

### `catchpen destroy <name>... [-y]`

Stop and delete VMs, their disks, deploy keys and minted tokens. Asks for confirmation
unless `-y, --yes`. Recordings, transcripts, tasks and audit entries are kept.

### `catchpen ls`

List VMs: name, state, template, distro, CPUs, memory, egress mode, SSH address, ports
and age. States are `stopped`, `running` or `paused`, with `,ephemeral`, `,task` or
`,stale-base` added where they apply. Pool VMs are listed by `catchpen pool ls` instead.

### `catchpen resume <name>`

Continue a VM a canary hit paused. Look at what it did first (`catchpen audit`,
`catchpen egress log`). See [canaries.md](canaries.md).

### `catchpen reset <vm> [--latest] [-y]`

Give a VM a fresh disk and run first boot again. Keys, forges and settings stay; repos
are cloned again; snapshots are deleted. `--latest` uses the template's current base. A
running VM is started again afterwards.

### `catchpen rebase <vm> [-y]`

Move a VM's disk onto the template's current base, keeping its contents. New packages
need `reset --latest`. Snapshots are deleted (it asks first when there are any); a
running VM is restarted.

### `catchpen gc [-n]`

Remove bases no VM uses (each template's current base stays), leftovers of interrupted
builds, downloads, resets and destroys, and trim the package caches to `cache_max`.
`-n, --dry-run` lists what would go.

## Access

### `catchpen ssh <name> [-- command...]`

Open a shell in a VM (recorded when stdin and stdout are a terminal, unless `record: false`), or run
a command (no tty, not recorded). The command's
words are joined with spaces, not quoted, so quote for the remote shell yourself.

### `catchpen run <name> claude|opencode|pi|omp|codex [-- agent args...]`

Start an agent in a guest tmux session named after it, or reattach to it. It starts in
the VM's repo when it has exactly one, `/workspace` with several, or home with none.
Arguments and `yolo` apply only when the session starts.

### `catchpen attach <name>`

Attach to the VM's `main` guest tmux session, creating it if needed. The guest tmux
prefix is `C-a`.

### `catchpen tmux [<name>...|--all] | catchpen tmux --sync`

Open a host tmux session `catchpen` with a window per VM, each running `catchpen attach`.
With no names, or `--all`, every running VM except task VMs gets one; naming a task
VM is an error. `--sync` adds windows for running
VMs and closes catchpen's windows for stopped ones, without attaching.

### `catchpen herdr [<name>...|--all] [--agent A] | catchpen herdr --sync`

Give each running VM a [herdr](herdr.md) workspace running an agent through
`catchpen run`, then open herdr (or focus the workspace, inside herdr). Starts a herdr
server if none is running. With no names, or `--all`, every running VM except task VMs
gets one. `--agent` picks the agent for new workspaces (default: `claude` if installed,
else the VM's first agent); a VM without it keeps its default, with a note, and naming
one is an error. `--sync` adds workspaces for running VMs and closes catchpen's
workspaces for stopped ones, without opening herdr.

### `catchpen cp [-r] <src>... <dst>`

Copy files to or from VMs with scp; remote paths are `vm:path`. `-r` copies directories.
Recorded in the audit log.

### `catchpen forward <name> [-L ...] [-R ...] [-D ...] | --list | --stop`

Forward ports over SSH in the background.

| Flag | Meaning |
|---|---|
| `-L, --local PORT\|H:G\|H:HOST:G` | A guest port on the host's `127.0.0.1`. |
| `-R, --remote PORT\|G:H\|G:HOST:H` | A host service into the guest. Bypasses the egress proxy; logged in the VM's egress log in allowlist mode. |
| `-D, --dynamic PORT` | A SOCKS proxy on the host into the guest. |
| `--list` | List forward sessions. |
| `--stop` | Stop all of the VM's forward sessions. |

Forwards also end when the VM stops.

### `catchpen console <name>`

Attach to the VM's serial console (boot log). `Ctrl-]` detaches. There is no password
login; use `catchpen ssh`.

### `catchpen setup-ssh [--remove]`

Add (or remove) an `Include` of catchpen's `ssh_config` in `~/.ssh/config`, so
`ssh catchpen-<name>`, `scp` and editors' remote SSH work directly.

## Templates

| Command | Meaning |
|---|---|
| `catchpen template ls [category]` | List every category's sections, marking the defaults, with their descriptions and the file each comes from. |
| `catchpen template show [-t picks]` | Print the settings a `-t` gives, the sections each category contributed, and its base. |
| `catchpen template build [-t picks] [--rebuild]` | Build the template's base (`--rebuild`: even if it exists). |
| `catchpen template rebuild -t picks... \| --all` | Build a new base generation from a fresh cloud image. `-t` is repeatable (`-t default` for the defaults); `--all` covers every template with a base or a VM, and can't be combined with `-t`. |

`show`, `build` and `rebuild` also take the picks as an argument: `catchpen template show go-1.27`.
`show` and `build` take them one way or the other, not both.

## Credentials

### `catchpen creds push <name>... | --all`

Push credentials, environment and agent config into running VMs. catchpen also does this
at every start. `creds push` first takes the VM's `env:`, `skills:` and `plugins:` from
its template as the template is now, so what was added to the template after the VM was
created reaches it; it prints the env names it adds (`+`), changes (`~`) and drops (`-`).

## Forges and exports

| Command | Meaning |
|---|---|
| `catchpen forge ls` | List forges. |
| `catchpen forge add <name> --type github\|gitea --host H --mode M [...]` | Add a forge, pinning its SSH host key. |
| `catchpen forge rm <name>` | Remove a forge. Existing VMs keep their copy of it. |
| `catchpen forge test <name>` | Check a forge's credentials and host key. |

`forge add` flags:

| Flag | Meaning |
|---|---|
| `--type` | `github` or `gitea` (required). |
| `--host` | Forge host name, e.g. `github.com` (required). |
| `--http-url` | Web and API base URL (default `https://<host>`). |
| `--ssh-port` | SSH port (default 22). |
| `--mode` | `deploy-key`, `minted-token` (Gitea only), `token`, `agent` or `none` (required). `--help` doesn't list `agent`, but it's accepted. |
| `--token-secret` | Secret holding the API token (`deploy-key`, `token`). |
| `--user`, `--password-secret` | Gitea account and the secret holding its password (`minted-token`). |
| `--scopes` | Minted token scopes (default `write:repository,read:user`). |
| `-y, --yes` | Trust the scanned host key without asking. |

### `catchpen pull <name>`

Fetch a branch from a VM's repo into `refs/catchpen/<vm>/<branch>` of a host repository.

| Flag | Meaning |
|---|---|
| `--repo` | Repo in the VM (name or `forge:owner/repo`; default: the only one). |
| `-b, --branch` | Branch (default: the clone's current branch). |
| `--into` | Host git repository to fetch into (default `.`). |
| `--patches DIR` | Also write the branch as an mbox of patches into `DIR`. |

### `catchpen pr <name>`

Pull a branch, push it with your host git credentials and open a pull request. Takes
`pull`'s flags plus:

| Flag | Meaning |
|---|---|
| `--title` | PR title (default: the last commit's subject). |
| `--body-file` | PR description file. |
| `--draft` | Open as a draft (Gitea: a WIP title). |
| `--base` | Target branch (default: the repo's default branch). |
| `--head` | Branch name on the forge (default `catchpen/<vm>/<branch>`). |

## Egress

| Command | Meaning |
|---|---|
| `catchpen egress show <name>` | Print the allowlist, denylist and settings. |
| `catchpen egress log <name> [-f]` | Show allowed and blocked requests. |
| `catchpen egress allow <name> <host[:port]>...` | Add allowlist entries (applies to new connections at once). |
| `catchpen egress unallow <name> <host[:port]>...` | Remove allowlist entries. |
| `catchpen egress deny <name> <host[:port]>...` | Add denylist entries (they win over the allowlist). |
| `catchpen egress undeny <name> <host[:port]>...` | Remove denylist entries. |
| `catchpen egress config <name> [--on-deny log\|prompt] [--prompt-timeout 30s] [--allow-private]` | Change settings. Only the flags you pass are changed; `--allow-private=false` clears it. |
| `catchpen egress watch [<name>...] [--desktop]` | Answer prompts for allowlist-mode VMs, in the terminal or through desktop notifications. |

All but `show` and `log` need a VM in allowlist mode.

## Tasks

### `catchpen task run`

Queue a headless task, or a batch of them (`--file`). See [tasks.md](tasks.md).

| Flag | Meaning |
|---|---|
| `-t, --template` | Picks from the template category files, as for `catchpen up`. |
| `--repo` | Repo to clone and work on. |
| `--agent` | `claude` (default), `opencode`, `pi`, `omp`, `codex` or `cmd`. |
| `--cmd` | Shell command for `--agent cmd`. The prompt is on stdin, and its path in `$CATCHPEN_PROMPT_FILE`. |
| `--prompt`, `--prompt-file` | The task. |
| `--timeout` | Stop the agent after this long (default `2h`). |
| `--max-budget-usd` | Claude: stop after spending this much. |
| `--out` | `pr`, `bundle`, `patch` or `none` (default `pr` with a repo). |
| `--into` | Host repository for `pr` and `bundle` (default: the current directory). |
| `--title` | PR title. |
| `--rw` | Give the VM a read-write deploy key. |
| `--keep` | Keep the VM afterwards for inspection. |
| `--file` | A batch file. Only `--parallel` may be combined with it, and `--follow` for a batch of one task. |
| `--parallel` | With `--file`: at most this many of the batch at once. |
| `-f, --follow` | Follow the task's output until it ends; exit 1 if it didn't succeed. |

### Other task commands

| Command | Meaning |
|---|---|
| `catchpen task ls` | List tasks with state, agent, repo, duration, cost and output. |
| `catchpen task show <id>` | Show a task and its result. |
| `catchpen task logs <id> [-f] [--raw]` | The agent's output, rendered, or as the raw event stream (`--raw`). |
| `catchpen task cancel <id>...` | Cancel tasks and destroy their VMs. |
| `catchpen task rm <id>...` | Delete finished tasks' records. |
| `catchpen transcripts <name>` | Copy a running VM's agent sessions to the host, and list the copies. |

## Snapshots

| Command | Meaning |
|---|---|
| `catchpen snapshot <vm> [name] [--disk-only]` | Save a snapshot, with memory when running unless `--disk-only`. |
| `catchpen snapshots <vm>` | List snapshots. |
| `catchpen restore <vm> <snapshot> [--disk-only]` | Return a VM to a snapshot. |
| `catchpen rmsnap <vm> <snapshot>...` | Delete snapshots. |

See [snapshots.md](snapshots.md); `reset`, `rebase` and `gc` are under VM lifecycle.

## Daemon and pool

| Command | Meaning |
|---|---|
| `catchpen daemon` | Run the background jobs in the foreground, logging to stderr. |
| `catchpen daemon install` | Install and start the systemd user service (`journalctl --user -u catchpen`). |
| `catchpen daemon uninstall` | Stop and remove the service. |
| `catchpen daemon status` | Show whether the daemon runs. |
| `catchpen pool ls` | List pool VMs (`filling`, `ready`, or `canary hit`: kept for inspection). |
| `catchpen pool fill` | Replace stale pool VMs and create missing ones. |
| `catchpen pool drain` | Destroy all pool VMs. |

## Package caches

| Command | Meaning |
|---|---|
| `catchpen cache` | Show what each cache holds. |
| `catchpen cache clear [apt\|go\|npm\|pypi\|docker ...]` | Empty all caches, or the ones named. |

## Audit, recordings and system

| Command | Meaning |
|---|---|
| `catchpen audit [<name>] [-f]` | Show the host audit log. |
| `catchpen recordings <name>` | List a VM's session recordings. |
| `catchpen recordings <name> play <id> [--speed 2] [--idle 2s]` | Replay one, with title and clipboard sequences dropped; `--idle` caps pauses. |
| `catchpen doctor` | Check host requirements. |
| `catchpen init` | Write a commented `config.yaml`, `secrets.env` (mode 0600) and every template category file (`templates/distros.yaml`, `runtimes.yaml`, ...) under `~/.config/catchpen`. Files that exist are kept. |
| `catchpen version` | Print the version, commit, commit date and Go version. |
| `catchpen completion bash\|zsh\|fish\|powershell` | Print a shell completion script. |

## Internal commands

QEMU, ssh and `catchpen task run` call these back; they are hidden from `--help` and not meant to be run by
hand: `ssh-proxy`, `egress-stdio`, `cache-stdio`, `notify-stdio`, `task-supervise`.
Because of them, catchpen must be installed at a stable path; `go run` won't work.

## Environment variables

| Variable | Use |
|---|---|
| `CATCHPEN_HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME` | Where catchpen keeps its files. See [configuration.md](configuration.md#directories). |
| `CLAUDE_CONFIG_DIR` | Host Claude Code directory for `sync.claude_config` (default `~/.claude`). |
| `SSH_AUTH_SOCK` | Your agent, forwarded when `ssh.agent: host`. |
| `DISPLAY`, `WAYLAND_DISPLAY`, `SSH_ASKPASS` | Confirmation prompts for the restricted agent. |
| `TMUX` | Detects a host tmux for `catchpen tmux`. |
| `HERDR_ENV` | Set inside herdr panes; `catchpen herdr` then focuses the workspace instead of opening herdr. |
| any `NAME` | A `secret:NAME` falls back to the environment variable `NAME`. |
