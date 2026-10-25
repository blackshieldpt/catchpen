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
| `codex` (OpenAI Codex CLI) | npm (`@openai/codex`) | `runtimes.node` |

```yaml
agents:
  claude: { auth: oauth-token }          # or api-key; per VM: curral up --claude-auth api-key
  opencode: { keys: [ANTHROPIC_API_KEY, OPENROUTER_API_KEY] }
  pi: { version: "1.3.0", keys: [ANTHROPIC_API_KEY] }
  omp: { version: "18.12.5", keys: [OPENROUTER_API_KEY] }
  codex: { auth: api-key }               # or chatgpt, or none
```

| Field | Meaning |
|---|---|
| `version` | `latest` (default) or a version such as `1.3.0`. omp needs `latest` or a full `X.Y.Z`. |
| `auth` | Claude: `oauth-token` (the default) or `api-key`. Codex: `api-key` (the default), `chatgpt` or `none` (installed, with no credentials or API hosts; the `default` template's setting). |
| `keys` | Provider API keys for opencode, pi and omp (not Claude or Codex). |
| `yolo` | `true` starts claude (`--dangerously-skip-permissions`), opencode (`--auto`) or codex (`--yolo`) in `curral run` without permission prompts. Not for pi or omp. |

Agents are baked into the base and don't update themselves. To get new versions, rebuild
the base (`curral template rebuild <template>`, then `curral reset --latest` or a new VM).
The omp checksum catches a broken download, not a tampered release; pin `version:`.

## Running an agent

```
curral run dev claude              # attach to (or start) the claude tmux session
curral run dev opencode -- --model x
curral run dev codex
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
  `~/.config/curral/claude.env`, which only the guest's `claude` command loads: other
  agents and SSH sessions don't get it in their environment. It is not a secret from
  the VM, though: anything running as the agent user can read the file, and programs
  Claude Code starts inherit its environment. VMs on a base older than this wrapper
  still get it in every session's environment, and curral warns at each start until
  `curral reset --latest` moves them to a current base. With `oauth-token`, setting
  `ANTHROPIC_API_KEY` in `env:` is refused, since it would override the token.
- **Codex**: with `api-key`, curral logs Codex in with `OPENAI_API_KEY` at every push
  (`codex login --with-api-key`, the key on stdin), which stores it in
  `~/.codex/auth.json`. With `chatgpt`, curral pushes nothing: sign in once inside each
  VM with `codex login --device-auth`, and the login stays in that VM's `auth.json`.
  curral never copies your host's Codex login, because its refresh tokens rotate:
  copies on several machines invalidate each other. Tasks need `api-key`, since a new
  VM has no sign-in. With `none`, as in the `default` template, Codex is installed but
  gets no credentials and no API hosts; set `agents.codex.auth` in a template extending
  `default` to use it. `chatgpt` and `none` both drop an API key login an earlier
  `api-key` setting left.
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
| `env:` | `~/.ssh/environment` |
| Claude Code's token or key | `~/.config/curral/claude.env` |
| Claude Code's settings and MCP servers | `~/.claude.json` |
| opencode | `~/.local/share/opencode/auth.json`, `~/.config/opencode/opencode.json` |
| pi | `~/.pi/agent/auth.json`, `~/.pi/agent/mcp.json` |
| omp | `~/.omp/agent/.env`, `~/.omp/agent/mcp.json` |
| Codex | `~/.codex/auth.json` (written by `codex login`), `~/.codex/config.toml` |

Removing a secret on the host removes it from the environment on the next push.
opencode's and pi's provider keys, and Codex's login, stay in their auth files until
replaced; omp's `.env` is rewritten on every push.

Codex reaches `api.openai.com` with an API key, or `chatgpt.com` and `auth.openai.com`
with `chatgpt`; curral allowlists those. With an API key it also tries `chatgpt.com`,
`github.com` and `api.github.com` while it runs. `chatgpt.com` stays blocked (GitHub's
hosts too, unless the template has the `github` preset, as `default` does), and Codex
works without them, logging errors about refreshing its model list. Codex's own command
sandbox (bubblewrap) can't set up in the guest, so curral sets
`sandbox_mode = "danger-full-access"` unless you set another mode: the VM is the
sandbox.

curral merges its settings into Codex's `config.toml` at every push and keeps the rest:
update checks, analytics, feedback and metrics off, credentials in `auth.json` rather
than a keyring, the notification hook (replacing a `notify` of your own, with a
warning), and the template's MCP servers. Comments in the file aren't kept. curral parses
the file on the host, so one that isn't valid TOML, is over 256 KiB or nests more than 64
levels is saved as `config.toml.curral-bak` and replaced, with a warning saying which.
curral doesn't read back any agent config file over 4 MiB: the push fails until you move
it aside in the VM.

## Environment

```yaml
env: { GH_TOKEN: "secret:GH_TOKEN", TZ: Europe/Lisbon }
```

`env:` sets variables for every ssh session, as literal values or `secret:NAME`. `HOME`,
`USER`, `PATH`, `SHELL` and `LOGNAME` can't be set. A VM takes `env:` from its template
when it is created, and again at each `curral creds push`; sessions opened after the push
see the change. The [GitHub tutorial](github.md#part-2-token-for-gh) uses this for `gh`.

## MCP servers

```yaml
mcp:
  - { name: docs, type: http, url: "https://mcp.example.com/mcp", headers: { Authorization: "secret:DOCS_AUTH" } }
  - { name: fs, type: stdio, command: [npx, -y, "@modelcontextprotocol/server-filesystem", /workspace] }
```

Servers are written to every agent's config; pi and Codex skip SSE servers. `type` is `stdio`
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
`~/.pi/agent/sessions`, omp `~/.omp/agent/sessions`, Codex `~/.codex/sessions`; not their
credentials) are copied
to the host under `transcripts/<vm>/<time>/`:

- when a task ends;
- before `destroy`, `restore` and `reset`, when the VM is running;
- on `curral transcripts <vm>`, which also lists earlier copies.

They come from the guest, so only regular files and directories are unpacked: no
symlinks, nothing outside the directory, at most 1 GiB and 200,000 entries.
