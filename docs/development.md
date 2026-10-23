# Development

```
go build -o ~/.local/bin/curral ./cmd/curral
go vet ./...
go test -race ./...                                           # unit tests
go test -p 1 -tags integration -timeout 60m ./internal/vm/ ./internal/runner/   # boots real VMs (KVM and network)
go test -tags integration -timeout 30m -run '^TestCanaries$' ./internal/vm/   # one integration test
```

The integration tests need everything a real host does (see
[installation.md](installation.md)), network access to download cloud images, and time:
some boot several VMs. Some run a Gitea server in Docker to test forges (skipped without
Docker).

## Layout

| Path | Contents |
|---|---|
| `cmd/curral/` | The CLI (spf13/cobra), one file per area. |
| `internal/vm/` | The VM manager: lifecycle, sessions, exports, snapshots, pool, idle stop, gc, canary handling. A VM's files, state and lock go through `vmDir` (`vmdir.go`, which also gives the lock order). |
| `internal/runner/` | Headless tasks: submit, schedule on the VM budget, supervise, export. |
| `internal/vmtest/` | What the tests of `vm` and `runner` share: fake QEMU and supervisor processes, a curral home, a throwaway Gitea. |
| `internal/template/` | Template loading, `extends` merging and validation; built-in templates in `builtin/`. |
| `internal/cloudinit/` | Cloud-init and base build script generation, and the seed ISO. |
| `internal/qemu/` | QEMU command lines, QMP and `qemu-img`. |
| `internal/egress/` | The allowlist proxy, presets, lists and prompts. |
| `internal/cache/` | The package caches. |
| `internal/canary/` | Decoy generation and scanning. |
| `internal/forge/` | GitHub and Gitea APIs, host keys, repo resolution. |
| `internal/agents/`, `internal/creds/` | Agent config and credentials pushed into guests. |
| `internal/sshx/`, `internal/sshagent/` | SSH clients, ssh_config and restricted agents. |
| `internal/task/` | Task specs, batch files and storage. |
| `internal/daemon/` | The daemon's jobs and systemd unit. |
| `internal/notify/`, `internal/audit/`, `internal/record/` | Notifications, the audit log and recordings. |

## CI and releases

`.github/workflows/ci.yml` runs on every push to `master` and every pull request:
gofmt, vet and staticcheck (with and without the `integration` tag), govulncheck, the
unit tests with `-race`, a GoReleaser snapshot build and the docs build.
`integration.yml` runs the integration tests nightly and on demand, on a runner with
KVM. `docs.yml` publishes the docs site to GitHub Pages.

To release:

1. Rename `## [Unreleased]` in `CHANGELOG.md` to `## [X.Y.Z] - YYYY-MM-DD` and start a
   new empty `## [Unreleased]` above it. That section becomes the release notes; the
   release fails without one. A pre-release needs its own section, such as
   `## [0.2.0-rc.1]`.
2. Commit, then `git tag vX.Y.Z && git push origin vX.Y.Z`.

`release.yml` reruns CI, then GoReleaser builds the binary (static, `-trimpath`,
reproducible), uploads it and a `.tar.gz` with `SHA256SUMS` to a draft release, signs
each with cosign keyless (a `.sigstore.json` bundle whose identity is the release
workflow at that tag), attests build provenance, and only then publishes the release.
A tag with a hyphen (`v0.2.0-rc.1`) makes a pre-release.
Check the release locally with `goreleaser release --snapshot --clean --skip=publish,sign`.

## Adding an agent

1. Add its field to `Agents` (`internal/template/agents.go`) and its entry to `AgentDefs`
   (`internal/template/registry.go`): npm package and environment, whether it needs Node,
   its auth modes (or provider keys), its yolo flag and its transcript paths. Merging,
   validation, the install, `curral run`, tasks' agent list, transcripts and the CLI
   help follow from it.
2. Write its push in `internal/agents` (settings, MCP servers, credentials, the
   notification hook) and add it to `pushers`.
3. Add its task command and event-stream parsing to `internal/task/agent.go`, and the
   API hosts it needs to `agentHosts` in `internal/vm/create.go` if it doesn't take
   provider keys.
4. Document it in `docs/agents.md`.

Tests fail while the registry, `Agents`, `pushers`, the hook or the task command are
out of step.

## Conventions

- Treat everything from the guest as hostile: output, files, git objects, notifications.
  New host-side handling of guest data needs limits and a test with hostile input.
- Guest command output (`sshx.Run`, `sshx.Stream`'s stderr, `agents.Remote`, host git
  over guest commits) comes as `sshx.GuestOutput`: `Raw()` to parse it, `%s` to show it
  (trimmed, control characters dropped). It can't be converted to a string by accident.
- Connect to a guest with `DialGuest` or `waitGuest` only: their connections close when
  a canary hit pauses the VM.
- Tests run against real git, real files and `httptest` servers rather than mocks.
- Document user-visible changes in the [changelog](https://github.com/blackshieldpt/curral/blob/master/CHANGELOG.md) and the relevant page
  under `docs/`.
