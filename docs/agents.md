# Agents and credentials

curral installs coding agents into template bases and gives each VM only the
credentials its template asks for.

## Agents

| Agent | Installed from | Needs |
|---|---|---|
| `claude` (Claude Code) | npm | `runtimes.node` |
| `opencode` | npm | `runtimes.node` |
| `pi` | npm | `runtimes.node` |
| `omp` (oh-my-pi) | its release binary, checked against the release's SHA256SUMS | nothing |

```yaml
agents:
  claude: { auth: oauth-token }          # or api-key; per VM: curral up --claude-auth api-key
  opencode: { keys: [ANTHROPIC_API_KEY, OPENROUTER_API_KEY] }
  pi: { version: "1.3.0", keys: [ANTHROPIC_API_KEY] }
  omp: { version: "18.12.5", keys: [OPENROUTER_API_KEY] }
```

| Field | Meaning |
|---|---|
| `version` | `latest` (default) or a version such as `1.3.0`. omp needs `latest` or a full `X.Y.Z`. |
| `auth` | Claude only: `oauth-token` (the default) or `api-key`. |
| `keys` | Provider API keys for opencode, pi and omp (not Claude). |
| `yolo` | `true` starts claude (`--dangerously-skip-permissions`) or opencode (`--auto`) in `curral run` without permission prompts. Not for pi or omp. |

Agents are baked into the base and don't update themselves. To get new versions, rebuild
the base (`curral template rebuild <template>`, then `curral reset --latest` or a new VM).
The omp checksum catches a broken download, not a tampered release; pin `version:`.

## Running an agent

```
curral run dev claude              # attach to (or start) the claude tmux session
curral run dev opencode -- --model x
```

The session starts in the VM's repo when it has exactly one, `/workspace` with several,
or home with none. Arguments and `yolo` apply only when the session starts; `run`
reattaches to a running one. For headless runs, see [tasks.md](tasks.md).

## Credentials

Credentials are resolved on the host and pushed over SSH after every start. They never
enter bases or seed images. Push again to running VMs after changing them:

```
curral creds push dev
curral creds push --all
```

- **Claude Code**: `oauth-token` uses `CLAUDE_CODE_OAUTH_TOKEN` (create it once with
  `claude setup-token`); `api-key` uses `ANTHROPIC_API_KEY`. The variable is written to
  the VM's session environment (`~/.ssh/environment`), so every SSH session and every
  agent `curral run` starts can read it, not just Claude Code. With `oauth-token`,
  setting `ANTHROPIC_API_KEY` in `env:` is refused, since it would override the token.
- **opencode, pi and omp** get the provider keys listed in `keys:`, in their own files, so
  an Anthropic API key for them doesn't override Claude's subscription token. Known
  keys: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `GEMINI_API_KEY`,
  `GROQ_API_KEY`, `MISTRAL_API_KEY`, `XAI_API_KEY`, `DEEPSEEK_API_KEY`. Their API hosts
  are added to the VM's allowlist.

Secret values come from config.yaml `secrets:`, the environment, or `secrets.env`; see
[configuration.md](configuration.md#secrets).

### Where they land in the guest

All of these files are mode 0600:

| What | File |
|---|---|
| `env:`, and Claude Code's token or key | `~/.ssh/environment` |
| Claude Code's settings and MCP servers | `~/.claude.json` |
| opencode | `~/.local/share/opencode/auth.json`, `~/.config/opencode/opencode.json` |
| pi | `~/.pi/agent/auth.json`, `~/.pi/agent/mcp.json` |
| omp | `~/.omp/agent/.env`, `~/.omp/agent/mcp.json` |

Removing a secret on the host removes it from the environment on the next push.
opencode's and pi's provider keys already in their auth files stay until replaced; omp's
`.env` is rewritten on every push.

## Environment

```yaml
env: { GH_TOKEN: "secret:GH_TOKEN", TZ: Europe/Lisbon }
```

`env:` sets variables for every ssh session, as literal values or `secret:NAME`. `HOME`,
`USER`, `PATH`, `SHELL` and `LOGNAME` can't be set.

## MCP servers

```yaml
mcp:
  - { name: docs, type: http, url: "https://mcp.example.com/mcp", headers: { Authorization: "secret:DOCS_AUTH" } }
  - { name: fs, type: stdio, command: [npx, -y, "@modelcontextprotocol/server-filesystem", /workspace] }
```

Servers are written to every agent's config; pi skips SSE servers. `type` is `stdio`
(with `command` and optional `env`), `http` or `sse` (with an `http(s)` `url` and
optional `headers`).
Remote MCP hosts are added to the VM's allowlist.

## Syncing host settings

```yaml
sync:
  git_identity: true       # host git user.name and user.email (default)
  claude_config: false     # host ~/.claude settings, CLAUDE.md, agents, skills, commands
```

With `claude_config`, the guest's `~/.claude/settings.json` is replaced on every push,
minus `env`, hooks, credential helpers, the status line and plugins.

## Transcripts

The agents' session stores (claude `~/.claude/projects`, opencode's `opencode.db`, pi
`~/.pi/agent/sessions`, omp `~/.omp/agent/sessions`; not their credentials) are copied
to the host under `transcripts/<vm>/<time>/`:

- when a task ends;
- before `destroy`, `restore` and `reset`, when the VM is running;
- on `curral transcripts <vm>`, which also lists earlier copies.

They come from the guest, so only regular files and directories are unpacked: no
symlinks, nothing outside the directory, at most 1 GiB and 200,000 entries.
