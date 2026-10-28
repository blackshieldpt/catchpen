# herdr

[herdr](https://herdr.dev) is a terminal workspace manager for coding agents: it shows
which agents are working, blocked on a question or idle, and brings them back after a
restart. `catchpen herdr` gives each running VM a herdr workspace whose agent herdr
tracks like a local one, while the agent stays inside its VM.

## Setup

Install herdr on the host (tested with herdr 0.9.5), then:

```sh
catchpen herdr                  # a workspace for every running VM, then open herdr
catchpen herdr dev              # just dev, and focus it
catchpen herdr dev --agent pi   # run pi instead of the default agent
catchpen herdr --sync           # add and close workspaces to match running VMs, without opening herdr
```

`catchpen herdr` starts a herdr server if none is running. Inside herdr it focuses the
workspace instead of opening a second client.

## What it does

- Each VM gets one workspace, labeled `catchpen:<vm>`, whose pane runs
  `env HERDR_AGENT=<agent> catchpen run <vm> <agent>`. `HERDR_AGENT` tells herdr which
  agent runs behind catchpen, so it reads the pane as that agent and shows its state.
- The agent is Claude Code when the VM has it, else the VM's first agent in the order
  `claude`, `opencode`, `pi`, `omp`, `codex`; a VM with none gets a shell
  (`catchpen attach`). `--agent` picks another for new workspaces; existing ones are kept.
- catchpen registers that command with herdr as the pane's resume command, so after a
  herdr restart the pane runs it again and reattaches to the agent, which kept running
  in the VM's tmux.
- While herdr is running and has catchpen workspaces, `catchpen start` adds one for the
  started VM and `catchpen stop` closes the stopped VM's. Workspaces you made yourself
  are left alone.
- Headless task VMs get no workspace: their agent runs on its own, and
  `catchpen task logs` shows it.

To run another agent in the same VM, split the pane in herdr and run, for example,
`HERDR_AGENT=codex catchpen run dev codex`. herdr tracks it, but won't resume it after a
restart.

## Without catchpen herdr

The prefix is all herdr needs. In any herdr pane:

```sh
HERDR_AGENT=claude catchpen run dev claude
```

For a catchpen host you reach over SSH, install herdr there too, run `catchpen herdr --sync`
on it, and add it from your machine with `herdr machine add <host>`: its workspaces
appear next to your local ones.

## Security

- herdr runs on the host only. Don't install it in a VM or forward its socket into
  one: the socket controls every pane, so a guest that reached it could type into
  your terminals.
- catchpen gives herdr only the VM name, the agent's name and the `catchpen run` command.
  Nothing from the guest is passed to herdr except the terminal output it reads,
  which is what you'd see in the pane anyway.
- `herdr integration install` sets up hooks for agents installed on the host; it doesn't
  reach agents in catchpen VMs, and catchpen doesn't need it.

## Limits

- herdr tells an agent's state from what's on screen, as it does for Claude Code and
  Codex on the host. For opencode, pi and omp, which herdr normally hears from through
  plugins, that's less precise.
- Pane IDs can change across herdr restarts; catchpen finds its workspaces by their
  label.
