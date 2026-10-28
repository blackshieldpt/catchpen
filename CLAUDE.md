# catchpen

A Go CLI that runs coding agents in QEMU/KVM VMs. The guest is hostile: treat
everything from it (output, files, git objects, notifications) as attacker input.
[docs/development.md](docs/development.md) has the layout, conventions and how to add an
agent; read it before changing code.

## Commands

```
go build -o ~/.local/bin/catchpen ./cmd/catchpen   # install at a stable path: QEMU and ssh call it back
go vet ./... && go vet -tags integration ./...
go test -race ./...                            # unit tests
go test -p 1 -tags integration -timeout 60m ./internal/vm/ ./internal/runner/   # boots real VMs
```

Keep `-p 1`: `vm` and `runner` both boot VMs. Docs site: `mkdocs build --strict`
(mkdocs 1.6.1, mkdocs-material 9.7.7, as in `.github/workflows/docs.yml`).

## Rules that tests don't fully catch

- Reach a guest only through `DialGuest` / `waitGuest`; `TestGuestConnectionsGuarded`
  checks `vm` and `runner`.
- Guest output is `sshx.GuestOutput`: `Raw()` to parse, `%s` to show.
- A VM's files, state and lock go through `m.vm(name)` (`internal/vm/vmdir.go`). Lock
  order: pool fill lock, pool lock, then a VM's lock; a skill or plugin fetch holds its
  repo's cache lock (internal/gitsrc), a leaf taken before the push. Locks are flocks and not
  re-entrant: code holding a VM's lock calls `stopLocked` / `takeResume`, never `Stop`,
  `Destroy`, `Restore` or `CanaryHit`.
- `runner` imports `vm`, so `vm`'s tests can't import `runner`. Shared test helpers
  live in `internal/vmtest`, which must not import `vm`.
- Tests use real git, real files and `httptest`, not mocks.
- User-visible changes go in `CHANGELOG.md` and the matching page under `docs/`. Doc
  claims must match the code; check flags, defaults and error messages before writing
  them.

## Commits

Short imperative subject, matching `git log`. No `Co-Authored-By` or other
attribution trailers.
