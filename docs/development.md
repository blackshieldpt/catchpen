# Development

```
go build -o ~/.local/bin/curral ./cmd/curral
go vet ./...
go test -race ./...                                           # unit tests
go test -tags integration -timeout 30m ./internal/vm/         # boots real VMs (KVM and network)
go test -tags integration -timeout 30m -run '^TestCanaries$' ./internal/vm/   # one integration test
```

The integration tests need everything a real host does (see
[installation.md](installation.md)), network access to download cloud images, and time:
some boot several VMs. Some run a Gitea server to test forges.

## Layout

| Path | Contents |
|---|---|
| `cmd/curral/` | The CLI (spf13/cobra), one file per area. |
| `internal/vm/` | The VM manager: lifecycle, sessions, exports, tasks, snapshots, pool, idle stop, gc, canary handling. |
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

[PLAN.md](../PLAN.md) has the design and its history.

## Conventions

- Treat everything from the guest as hostile: output, files, git objects, notifications.
  New host-side handling of guest data needs limits and a test with hostile input.
- Tests run against real git, real files and `httptest` servers rather than mocks.
- Document user-visible changes in the [changelog](../CHANGELOG.md) and the relevant page
  under `docs/`.
