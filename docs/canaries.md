# Canaries

Canaries are decoy credentials planted in a VM. Nothing in the VM uses them, so any use
of one means something went looking for credentials and tried to send them out.

## Turning them on

List the kinds in a template (the `default` template plants all three):

```yaml
canaries: [aws, env, git]
```

`canaries: []` turns them off for a template that inherits them. Each VM gets its own
values, pointing at hosts that exist nowhere:

| Kind | Planted in the agent user's home |
|---|---|
| `aws` | `~/.aws/credentials`, and an `endpoint_url` in `~/.aws/config`. Left out when the template's `env:` sets any `AWS_*` variable. |
| `env` | `~/.env.production`, with a database URL, an API URL and token, and a Stripe-style key. |
| `git` | `~/.git-credentials`. |

The host keeps each VM's values in `vms/<name>/canaries.json`.

## What counts as a hit

- a connection to a canary host through the egress proxy, allowlisted or not;
- a canary value in a plain-HTTP request's line or headers (not its body), or in a
  package cache request;
- a canary value or host (hosts in any letter case) in what leaves the VM through
  curral:
  - the commits that `pull`, `pr` and tasks bring out, including headers, author and
    committer, messages (as stored and as re-encoded to UTF-8), root commits, merges
    and binary files;
  - branch names;
  - patches, which are made on the host from the scanned commits;
  - a PR's title and body;
  - a task's result and an agent's notification text.

## What happens on a hit

1. The request or export is refused. A notification is sent with the offending text
   withheld.
2. `canary.hit` is written to the audit log and sent to notifications, each at most a
   few times per minute.
3. The VM is dealt with according to `canary_action` in `config.yaml`:

   ```yaml
   canary_action: pause    # default; or stop, or log
   ```

   - `pause` freezes the VM as it was, for inspection. `curral ls` shows it `paused`,
     and `ssh`, `run`, `attach` and the like refuse it until you run
     `curral resume <name>`, which also sets the guest clock.
   - `stop` powers it off at once.
   - `log` only records the hit.

Tasks and the pool react on top of that:

- A task whose VM is paused fails at once.
- A task VM with any canary hit is kept for inspection, whatever `canary_action` did.
  While it runs (paused included), it counts against the task budget
  (`tasks.max_vms`, `tasks.max_memory`).
- A pool VM with a hit is never handed out.

If a VM's `canaries.json` can't be read, its proxy, cache and exports refuse everything,
and its notifications and task results are withheld.

## Responding to a hit

1. Look at what triggered it: `curral audit <name>` shows the hit, and
   `curral egress log <name>` shows the surrounding requests.
2. Inspect the paused VM before resuming it. `curral snapshot <name>` keeps its memory
   and disk for later analysis, and recordings are in `curral recordings <name>`.
   `curral transcripts` refuses a paused VM; it lists copies made earlier.
3. Then `curral resume <name>` to continue, or `curral destroy <name>`.

## What goes unseen

- Values sent encoded, or over HTTPS to an allowed host.
- Clients that don't use the proxy (database drivers for the `DATABASE_URL`, Node's
  `fetch`). Their connections just fail.
- Network use in VMs with `egress: open` or `egress: none`. Their exports,
  notifications and task results are still scanned, and so are cache requests in
  `open` VMs.
- `curral cp` and `curral forward`.
- Commits the guest pushed to a forge itself (with a deploy key or token). The host has
  them once it fetches them.
- A careful attacker can tell the decoys from real credentials: the hosts don't
  resolve, the GitHub token's checksum is wrong, and the seed's user-data lists them.
- If an agent prints the decoys, they end up in transcripts and recordings, where secret
  scanners may flag them.
