# Sessions, recordings and audit

## Interactive sessions

```
curral ssh dev                    # a shell
curral ssh dev -- docker ps       # a command (no tty, not recorded)
curral run dev claude             # an agent in a guest tmux session; reattaches later
curral attach dev                 # the guest's main tmux session
curral cp ./notes.md dev:/tmp/    # scp, either direction (-r for directories)
```

`curral run <vm> claude|opencode|pi|omp|codex [-- args]` starts the agent in a guest tmux
session named after it and reattaches to it later. New arguments and `yolo` apply only
when the session starts. `curral attach` does the same for a `main` shell session. The
guest tmux prefix is `C-a`, so it doesn't clash with a host tmux. VMs created before
`curral run` existed need recreating for it.

`ssh -- command` joins the command's words with spaces without quoting them; quote for
the remote shell yourself.

With `curral setup-ssh`, `ssh curral-<vm>`, `scp` and editors' remote SSH (VS Code
Remote-SSH, for example) work directly. Those connections aren't recorded; `curral ssh`,
`run` and `attach` are.

## Host tmux

`curral tmux [<vm>...]` opens a host tmux session `curral` with a window per running VM,
each running `curral attach`. `start` and `stop` add and close windows while the session
exists, and `curral tmux --sync` reconciles it. Windows you open there yourself are left
alone.

## Ephemeral VMs

```
curral up scratch --ephemeral
curral up scratch --ephemeral -- make test
```

`--ephemeral` creates a new VM, attaches (or runs the command, with each argument
shell-quoted) and destroys the VM, with its deploy keys and tokens, when the session ends
or curral gets SIGINT, SIGTERM or SIGHUP. `curral ls` marks ephemeral VMs, so one left
behind by a crash is easy to spot. Idle stop leaves them alone.

## Recordings

Interactive `ssh`, `run` and `attach` sessions run the system ssh on a host pty and
record its output (not your keystrokes) as asciicast v2 in
`recordings/<vm>/<id>.cast`: mode 0600, capped at 256 MiB per session, kept after
`destroy`. The guest can't alter them.

```
curral recordings dev                         # list
curral recordings dev play <id> [--speed 2]   # replay, title and clipboard sequences dropped
```

`asciinema play` also reads them, but replays guest escape sequences (window title,
clipboard writes) unfiltered. `record: false` in config.yaml turns recording off.

## Audit log

```
curral audit [<vm>] [-f]
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
