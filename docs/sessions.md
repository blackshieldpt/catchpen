# Sessions, recordings and audit

## Interactive sessions

```
catchpen ssh dev                    # a shell
catchpen ssh dev -- docker ps       # a command (no tty, not recorded)
catchpen run dev claude             # an agent in a guest tmux session; reattaches later
catchpen attach dev                 # the guest's main tmux session
catchpen cp ./notes.md dev:/tmp/    # scp, either direction (-r for directories)
```

`catchpen run <vm> claude|opencode|pi|omp|codex [-- args]` starts the agent in a guest tmux
session named after it and reattaches to it later. New arguments and `yolo` apply only
when the session starts. `catchpen attach` does the same for a `main` shell session. The
guest tmux prefix is `C-a`, so it doesn't clash with a host tmux. VMs created before
`catchpen run` existed need recreating for it.

`ssh -- command` joins the command's words with spaces without quoting them; quote for
the remote shell yourself.

With `catchpen setup-ssh`, `ssh catchpen-<vm>`, `scp` and editors' remote SSH (VS Code
Remote-SSH, for example) work directly. Those connections aren't recorded; `catchpen ssh`,
`run` and `attach` are.

## Host tmux

`catchpen tmux [<vm>...]` opens a host tmux session `catchpen` with a window per running VM,
each running `catchpen attach`. `start` and `stop` add and close windows while the session
exists, and `catchpen tmux --sync` reconciles it. Windows you open there yourself are left
alone. Headless task VMs get no window: their agent runs unattended, and an attached
session would forward the VM's SSH agent (`ssh.agent: host` or `restricted`) for the whole
run.

## herdr

`catchpen herdr` does the same with [herdr](herdr.md), which also shows each agent's state
and resumes it after a restart.

## Ephemeral VMs

```
catchpen up scratch --ephemeral
catchpen up scratch --ephemeral -- make test
```

`--ephemeral` creates a new VM, attaches (or runs the command, with each argument
shell-quoted) and destroys the VM, with its deploy keys and tokens, when the session ends
or catchpen gets SIGINT, SIGTERM or SIGHUP. `catchpen ls` marks ephemeral VMs, so one left
behind by a crash is easy to spot. Idle stop leaves them alone.

## Recordings

Interactive `ssh`, `run` and `attach` sessions run the system ssh on a host pty and
record its output (not your keystrokes) as asciicast v2 in
`recordings/<vm>/<id>.cast`: mode 0600, kept after `destroy`. The guest can't alter
them, but it can end one early: a recording stops at 256 MiB as stored, and output is
stored JSON-escaped, where a control character, `<`, `>` or `&` takes 6 bytes (about
43 MiB of invisible NUL bytes fill it). The session goes on unrecorded.
`catchpen recordings` marks such a recording `(truncated)`, and `play` ends with a
line saying whether the recording is complete; the guest can't fake either.

```
catchpen recordings dev                         # list
catchpen recordings dev play <id> [--speed 2]   # replay, title and clipboard sequences dropped
```

`asciinema play` also reads them, but replays guest escape sequences (window title,
clipboard writes) unfiltered and doesn't say when a recording was cut short.
`record: false` in config.yaml turns recording off.

## Audit log

```
catchpen audit [<vm>] [-f]
```

`audit.jsonl` (rotated at 32 MiB) records:

- VM lifecycle;
- credentials pushed (secret names only);
- deploy keys and tokens added and removed, and restricted agents;
- allowlist edits and answered egress prompts (timeouts are only in the VM's egress log);
- forwards, `cp`, `pull` and `pr`;
- the start and end of every `ssh`, `run` and `attach` session, with its recording id;
- canary hits.

Like recordings, the audit log is never removed by `gc`.
