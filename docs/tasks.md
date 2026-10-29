# Headless tasks

A task runs an agent on a prompt in a fresh VM, with no one watching, and brings the
work out through the host.

```
catchpen task run --repo github:acme/api --prompt "fix issue #42"
catchpen task run --repo github:acme/api --prompt-file task.md --agent opencode --out bundle --into ~/src/api
catchpen task run --agent cmd --cmd 'make fix' --prompt "..."   # any command, prompt on stdin
catchpen task run -t go-1.27,codex-api-key --agent codex --repo github:acme/api --prompt "..."
catchpen task run --file tasks.yaml --parallel 2
catchpen task ls
catchpen task show <id>
catchpen task logs <id> [-f] [--raw]
catchpen task cancel <id>
catchpen task rm <id>
```

## What a task does

1. It waits for the budget in config.yaml (all running catchpen VMs count):

   ```yaml
   tasks: { max_vms: 2, max_memory: 24G }
   ```

2. It gets a new VM named `task-<id>`, with the template `-t` gives (as on
   `catchpen up`), from the warm pool when one matches, with read-only deploy keys unless
   `--rw`.
3. The agent works on branch `catchpen/<id>` without permission prompts: claude
   `-p --output-format stream-json`, `opencode run --format json --auto`,
   `pi -p --mode json`, `codex exec --json --yolo`. omp also runs headless. Codex
   tasks need `-t codex-api-key` (the default `codex` section has no credentials, and
   `codex-chatgpt` has no sign-in in a new VM, so both are refused), and Codex
   reports no cost. `--agent cmd` runs your command with the prompt on stdin
   and its path in `$CATCHPEN_PROMPT_FILE`. `--timeout` (default 2h, from 1m to 48h)
   stops it; `--max-budget-usd` caps Claude's spend (claude only). Prompts are limited
   to 100 KiB.
4. Whatever the agent leaves uncommitted is committed.
5. The work is exported from the host:

   | `--out` | Result |
   |---|---|
   | `pr` (default with a repo) | Pushes `catchpen/task-<id>/catchpen/<id>` through a remote of `--into` (default: the current directory) with your credentials, and opens a PR with the prompt and result. |
   | `bundle` | Fetches the branch into `refs/catchpen/...` of `--into`. |
   | `patch` | Writes an mbox in the task directory, made on the host from the scanned commits. |
   | `none` | Nothing is exported. |

6. The VM is destroyed, unless `--keep`, the export failed, or a canary was hit. A VM
   kept after a failed export holds the commits, and the task's error says how to pull
   them.

A task moves through `queued`, `starting` (it holds a budget slot while its VM is
created), `running` and `exporting`, and ends `done`, `failed` or `cancelled`. A task
whose supervisor died shows as `lost`; `cancel` or `rm` destroys its VM.

## Output

Everything is on the host in `~/.local/share/catchpen/tasks/<id>/`:

| File | Contents |
|---|---|
| `task.json` | Spec, state, turns, tokens and cost. |
| `prompt.md` | The prompt. |
| `output.jsonl` | The agent's event stream. |
| `result.md` | Its final message. |
| `supervisor.log` | The supervisor's own log. |
| `stderr.log` | The agent's stderr. |
| `pr-body.md` | The PR description, with `--out pr`. |
| `<id>.mbox` | The patches, with `--out patch`. |

The agents' transcripts are copied to `transcripts/task-<id>/` when the task ends.
`task logs` renders the output; `--raw` prints the event stream with OSC, DCS and APC
sequences (window title, clipboard) dropped. `task run -f` and `task logs -f` follow it,
and exit 1 if the task didn't end `done`.

## Batch files

```yaml
defaults: { repo: "github:acme/api", timeout: 1h, out: pr, into: ../api }
tasks:
  - prompt: "fix issue #42"
  - prompt_file: tasks/refactor.md
    agent: opencode
    title: Refactor the parser
matrix: { agent: [claude, opencode] }
```

Each task has `prompt` or `prompt_file`, and any of `agent`, `repo`, `template` (a `-t`
spec), `timeout`, `out`, `into`, `title`, `rw`, `keep`, `cmd` and `max_budget_usd`. The optional
`matrix: { agent: [...], repo: [...] }` runs each task once per combination, except
for the keys a task sets itself: in the example, the second task runs once, with
opencode. A batch holds at most 100 tasks.
A relative `into` or `prompt_file` is relative to the batch file's directory (`~` isn't
expanded); without `into`, `pr` and `bundle` use the current directory. `--parallel`
limits how many of the batch run at once. `--file` can't be combined with
other task flags except `--parallel`, and `--follow` only for a batch of one task.

## Notes

- Task VMs ignore the agents' own notification hooks; the task's result is notified
  instead (`task.done`, `task.failed`).
- A task whose VM a canary hit paused fails at once, and its VM is kept for inspection.
  It holds its share of the budget until you destroy it.
- Restore, reset and rebase refuse while a VM runs a task.
