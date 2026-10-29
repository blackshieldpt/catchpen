# Agents and credentials

catchpen installs coding agents into template bases and gives each VM only the
credentials its template asks for.

## Agents

| Agent | Installed from | Needs |
|---|---|---|
| `claude` (Claude Code) | npm | `runtimes.node` |
| `opencode` | npm | `runtimes.node` |
| `pi` | npm | `runtimes.node` |
| `omp` (oh-my-pi) | its release binary, checked against the release's SHA256SUMS | nothing |
| `codex` (OpenAI Codex CLI) | npm (`@openai/codex`) | `runtimes.node` |

Agents are sections of `agents.yaml`. The default installs `claude`, `opencode`, `pi` and
`codex` (with `auth: none`). The other built-in sections are `claude-vanilla`,
`claude-api-key`, `claude-yolo`, `omp`, `codex-api-key` and `codex-chatgpt`; picks add to the default, and
`-name` drops one:

```
catchpen up dev -t omp,-pi                   # the default agents plus omp, without pi
catchpen up dev -t codex-api-key             # Codex with OPENAI_API_KEY
catchpen up dev --claude-auth api-key        # Claude's auth for this VM only
```

Settings of your own go in a section of `~/.config/catchpen/templates/agents.yaml`. It
merges with the default's agents field by field:

```yaml
# ~/.config/catchpen/templates/agents.yaml
my-agents:
  agents:
    opencode: { keys: [ANTHROPIC_API_KEY, OPENROUTER_API_KEY] }
    pi: { version: "1.3.0", keys: [ANTHROPIC_API_KEY] }
    omp: { version: "18.12.5", keys: [OPENROUTER_API_KEY] }
    codex: { auth: api-key }             # or chatgpt, or none
  runtimes: { node: "24" }
```

```
catchpen up dev -t my-agents
```

| Field | Meaning |
|---|---|
| `version` | `latest` (default) or a version such as `1.3.0`. omp needs `latest` or a full `X.Y.Z`. |
| `auth` | Claude: `oauth-token` (the default) or `api-key`. Codex: `api-key` (the default), `chatgpt` or `none` (installed, with no credentials or API hosts; the built-in `codex` section's setting). |
| `keys` | Provider API keys for opencode, pi and omp (not Claude or Codex). |
| `yolo` | `true` starts claude (`--dangerously-skip-permissions`), opencode (`--auto`) or codex (`--yolo`) in `catchpen run` without permission prompts. Not for pi or omp. |

Agents are baked into the base and don't update themselves. To get new versions, rebuild
the base (`catchpen template rebuild -t <picks>` or `--all`, then `catchpen reset --latest`
or a new VM).
The omp checksum catches a broken download, not a tampered release; pin `version:`.

## Running an agent

```
catchpen run dev claude              # attach to (or start) the claude tmux session
catchpen run dev opencode -- --model x
catchpen run dev codex
```

The session starts in the VM's repo when it has exactly one, `/workspace` with several,
or home with none. Arguments and `yolo` apply only when the session starts; `run`
reattaches to a running one. For headless runs, see [tasks.md](tasks.md).

## Credentials

Credentials are resolved on the host and pushed over SSH after every start. They never
enter bases or seed images. Push again to running VMs after changing them:

```
catchpen creds push dev
catchpen creds push --all
```

- **Claude Code**: `oauth-token` uses `CLAUDE_CODE_OAUTH_TOKEN` (create it once with
  `claude setup-token`); `api-key` uses `ANTHROPIC_API_KEY`. The variable is written to
  `~/.config/catchpen/claude.env`, which only the guest's `claude` command loads: other
  agents and SSH sessions don't get it in their environment. It is not a secret from
  the VM, though: anything running as the agent user can read the file, and programs
  Claude Code starts inherit its environment. VMs on a base older than this wrapper
  still get it in every session's environment, and catchpen warns at each start until
  `catchpen reset --latest` moves them to a current base. With `oauth-token`, setting
  `ANTHROPIC_API_KEY` in `env:` is refused, since it would override the token.
- **Codex**: with `api-key`, catchpen logs Codex in with `OPENAI_API_KEY` at every push
  (`codex login --with-api-key`, the key on stdin), which stores it in
  `~/.codex/auth.json`. With `chatgpt`, catchpen pushes nothing: sign in once inside each
  VM with `codex login --device-auth`, and the login stays in that VM's `auth.json`.
  catchpen never copies your host's Codex login, because its refresh tokens rotate:
  copies on several machines invalidate each other. Tasks need `api-key`, since a new
  VM has no sign-in. With `none`, as in the default `codex` section, Codex is installed
  but gets no credentials and no API hosts; pick `codex-api-key` or `codex-chatgpt` to
  use it. `chatgpt` and `none` both drop an API key login an earlier
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
| Claude Code's token or key, and its plugin folders | `~/.config/catchpen/claude.env` |
| Claude Code's settings and MCP servers | `~/.claude.json`, `~/.claude/settings.json` (catchpen's hooks, the claude.ai sync keys) |
| opencode | `~/.local/share/opencode/auth.json`, `~/.config/opencode/opencode.json` |
| pi | `~/.pi/agent/auth.json`, `~/.pi/agent/mcp.json`, `~/.pi/agent/settings.json` (plugins), `~/.local/share/catchpen/pi-bundled.json` (the `bundled` plugins catchpen added) |
| omp | `~/.omp/agent/.env`, `~/.omp/agent/mcp.json` |
| Codex | `~/.codex/auth.json` (written by `codex login`), `~/.codex/config.toml` |

Removing a secret on the host removes it from the environment on the next push.
opencode's and pi's provider keys, and Codex's login, stay in their auth files until
replaced; omp's `.env` is rewritten on every push.

Codex reaches `api.openai.com` with an API key, or `chatgpt.com` and `auth.openai.com`
with `chatgpt`; catchpen allowlists those. With an API key it also tries `chatgpt.com`,
`github.com` and `api.github.com` while it runs. `chatgpt.com` stays blocked (GitHub's
hosts too, unless the VM has the `github` preset, as the network default does), and Codex
works without them, logging errors about refreshing its model list. Codex's own command
sandbox (bubblewrap) can't set up in the guest, so catchpen sets
`sandbox_mode = "danger-full-access"` unless you set another mode: the VM is the
sandbox.

catchpen merges its settings into Codex's `config.toml` at every push and keeps the rest:
update checks, analytics, feedback and metrics off, credentials in `auth.json` rather
than a keyring, the notification hook (replacing a `notify` of your own, with a
warning), the template's MCP servers, remote plugins off (`features.remote_plugin =
false`) and a `[plugins."<name>@catchpen"]` entry per plugin. Comments in the file aren't
kept. catchpen parses
the file on the host, so one that isn't valid TOML, is over 256 KiB or nests more than 64
levels is saved as `config.toml.catchpen-bak` and replaced, with a warning saying which.
catchpen doesn't read back any agent config file over 4 MiB: the push fails until you move
it aside in the VM.

## Environment

```yaml
# ~/.config/catchpen/templates/env.yaml
gh-token: { env: { GH_TOKEN: "secret:GH_TOKEN" } }
lisbon: { env: { TZ: Europe/Lisbon } }
```

```
catchpen up dev -t gh-token,lisbon
```

`env:` sets variables for every ssh session, as literal values or `secret:NAME`. `HOME`,
`USER`, `PATH`, `SHELL` and `LOGNAME` can't be set. `env.yaml` has no default. A VM takes
`env:` from its `-t` when it is created, and again at each `catchpen creds push`, which
re-reads the sections; sessions opened after the push see the change. The [GitHub tutorial](github.md#part-2-token-for-gh) uses this for `gh`.

## MCP servers

```yaml
# ~/.config/catchpen/templates/mcp.yaml
docs-mcp:
  mcp:
    - { name: docs, type: http, url: "https://mcp.example.com/mcp", headers: { Authorization: "secret:DOCS_AUTH" } }
fs-mcp:
  mcp:
    - { name: fs, type: stdio, command: [npx, -y, "@modelcontextprotocol/server-filesystem", /workspace] }
```

```
catchpen up dev -t docs-mcp,fs-mcp
```

`mcp.yaml` has no default. Servers are written to every agent's config; pi and Codex skip SSE servers. `type` is `stdio`
(with `command` and optional `env`), `http` or `sse` (with an `http(s)` `url` and
optional `headers`).
Remote MCP hosts are added to the VM's allowlist.

## Syncing host settings

The `sync:` settings, with their defaults:

```yaml
sync:
  git_identity: true       # host git user.name and user.email (default)
  claude_config: false     # host ~/.claude settings, CLAUDE.md, agents, skills, commands
  claude_ai: false         # Claude's skill and plugin sync from claude.ai
  opencode_claude_skills: false  # opencode also reads ~/.claude/skills
```

`sync.yaml` has no default section. Its built-in sections change one setting each:

```
catchpen up dev -t claude-config            # claude_config: true
catchpen up dev -t no-git-identity          # git_identity: false
catchpen up dev -t claude-ai-sync           # claude_ai: true
catchpen up dev -t opencode-claude-skills   # opencode_claude_skills: true
```

With `claude_config`, the guest's `~/.claude/settings.json` is replaced on every push,
minus `env`, hooks, credential helpers, the status line and plugins; its
`syncClaudeAiSkills` and `syncClaudeAiPlugins` are set to `false` unless `claude_ai` is
on.

## Skills and plugins

catchpen copies skills and plugins into the agents at every start. They are set in
`agents.yaml` sections, next to the agents. The built-in `claude` section gives Claude Code
three of Anthropic's skills: `skill-creator`, `mcp-builder` and `frontend-design`;
`claude-vanilla` is Claude Code without them. Codex, opencode, pi and omp get none, and
no plugins are built in: the `agents.yaml` that `catchpen init` writes has examples to
uncomment, among them Anthropic's plugins for Claude Code, OpenAI's superpowers for
Codex, pi's bundled extensions and Blackshield's `improve` skill.

```
catchpen up dev -t -claude,claude-vanilla   # Claude Code without skills
catchpen up dev -t team-review              # a section of your own, below
```

```yaml
# ~/.config/catchpen/templates/agents.yaml
team-review:
  description: "Our review checklist"
  skills:
    - { git: "https://github.com/acme/agent-skills", ref: "0123456789abcdef0123456789abcdef01234567", path: review }
```

A skill without `agent` goes to every agent the VM has; a plugin names its agent.
Entries for agents the VM doesn't have are dropped. Claude's skills come with the `claude`
section: with `-claude` and another Claude section (`-t -claude,claude-api-key`), or with
your own `claude` section, Claude has only the skills those sections list.

**Where they come from.** catchpen fetches each one on the host from an `https` git repo at
a full 40-character commit, keeps the repos in `~/.cache/catchpen/sources/`, and copies the
files in at every start. When a fetch fails, the start warns and the VM keeps the copy it
has. A VM keeps the list it was created with: `catchpen creds push` takes it from the
template as it is now, which is also how an existing VM gets a newer pin. pi's `bundled`
plugins are files of the pi package installed in the VM; one that isn't there is skipped
with a warning.

**Trust.** A pinned commit fixes the bytes, not who wrote them. On GitHub, a commit from a
fork can be fetched through the parent repo's URL, so pin commits you have read.

**Where each agent reads them**, under the guest's home:

| Agent | Skills folder | Plugin folder | How it is turned on |
|---|---|---|---|
| claude | `.claude/skills/<name>/` | `.local/share/catchpen/plugins/claude/<name>/` | `CLAUDE_CODE_PLUGIN_DIRS` in Claude Code's environment, as `--plugin-dir` does |
| codex | `.codex/skills/<name>/` | `.codex/plugins/cache/catchpen/<name>/local/` | `[plugins."<name>@catchpen"]` with `enabled = true` in `.codex/config.toml` |
| opencode | `.config/opencode/skills/<name>/` | `.local/share/catchpen/plugins/opencode/<name>/` (needs a `package.json`) | its path in `plugin` in `.config/opencode/opencode.json` |
| pi | `.pi/agent/skills/<name>/` | `.local/share/catchpen/plugins/pi/<name>/` | its path in `packages` in `.pi/agent/settings.json`; a `bundled` one in `extensions` |
| omp | `.omp/agent/skills/<name>/` | `.omp/agent/extensions/<name>/` (needs an `index.ts`, `index.js` or `package.json`) | none: omp loads the folder |

**Your own folders.** catchpen marks the folders it copies with a `.catchpen` file holding the
folder's name, and only replaces or removes marked folders. A folder of the same name
without the mark is yours: catchpen leaves it, doesn't turn it on as a plugin, and warns.
When catchpen can't delete part of its old copy (files made with `sudo`, say), it leaves
it in a `.catchpen-new.*` folder next to it and warns you to remove that with `sudo rm -rf`. A skill of the same name in
your host `~/.claude/skills`, synced with `claude_config` (`-t claude-config`), is used
instead of catchpen's, if the sync brings it: one it leaves out (a link out of
`~/.claude/skills`, say) doesn't replace catchpen's copy, and neither does any skill when
the sync fails. A host copy an earlier sync already put in the VM stays, though: catchpen
never replaces a folder without its mark.

**Isolation.** Each agent gets its own skills. opencode is told not to read
`~/.claude/skills` (`OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1`, set at each start) unless
`sync: { opencode_claude_skills: true }` (section `opencode-claude-skills`).

**Off in VMs**: Claude's skill and plugin sync from claude.ai (keep it with
`sync: { claude_ai: true }`, section `claude-ai-sync`), Claude's clone of its official
plugin marketplace, and Codex's remote plugins.

**Installing your own.** Skills and plugins you install inside the VM stay: catchpen only
replaces folders and config entries it marked as its own. Installs that download need the
host in the egress allowlist (`-t github` for GitHub).

## Transcripts

The agents' session stores (claude `~/.claude/projects`, opencode's `opencode.db`, pi
`~/.pi/agent/sessions`, omp `~/.omp/agent/sessions`, Codex `~/.codex/sessions`; not their
credentials) are copied
to the host under `transcripts/<vm>/<time>/`:

- when a task ends;
- before `destroy`, `restore` and `reset`, when the VM is running;
- on `catchpen transcripts <vm>`, which also lists earlier copies.

They come from the guest, so only regular files and directories are unpacked: no
symlinks, nothing outside the directory, at most 1 GiB and 200,000 entries.
