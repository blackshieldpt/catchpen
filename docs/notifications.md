# Notifications

```yaml
# config.yaml
notify:
  sinks:
    - desktop                                   # notify-send
    - { ntfy: { url: https://ntfy.sh, topic: my-curral, token: secret:NTFY_TOKEN } }
    - { webhook: { url: https://hooks.example/curral, headers: { Authorization: secret:HOOK_AUTH } } }
  events: [task.done, task.failed, egress.prompt, agent.waiting, vm.idle_stop, canary.hit]   # default: all
```

## Sinks

| Sink | Settings |
|---|---|
| `desktop` | `notify-send` on the host. |
| `ntfy` | `url` (default `https://ntfy.sh`), `topic` (required), `token` (a literal or `secret:NAME`). |
| `webhook` | `url`, and `headers` with literal or `secret:NAME` values. |

URLs must be `http` or `https`.

## Events

| Event | When |
|---|---|
| `task.done`, `task.failed` | A headless task ends. |
| `egress.prompt` | A blocked request waits for your answer under `on_deny: prompt`, while `curral egress watch` or the daemon is running. |
| `agent.waiting` | An agent in an interactive VM is waiting for you. |
| `vm.idle_stop` | Idle stop shut a VM down. |
| `canary.hit` | A decoy credential was used; its text is withheld. |

## How `agent.waiting` works

Claude's `Notification` and `Stop` hooks, an opencode plugin, a pi extension and an omp
hook (`agent_end`), written at each start, POST to `10.0.2.101:80` in the guest. That reaches
`curral notify-stdio` on the host, which sends the notification. The guest holds no sink
credentials. Its text is stripped of control characters, cut to 300 characters and
limited to a burst of 5 per VM, then one per 30 s. Task VMs' hooks are ignored.

## With the daemon

The systemd user service doesn't see your shell's environment, so tokens it needs belong
in `secrets.env` or a `command:` secret source. See
[deployment.md](deployment.md#the-daemon).
