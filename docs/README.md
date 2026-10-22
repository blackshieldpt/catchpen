# curral documentation

## Getting started

- [Installation](installation.md): requirements, install, first VM, upgrading
- [CLI reference](cli-reference.md): every command and flag

## Using curral

- [Templates](templates.md): what a VM contains, `extends`, bases
- [Configuration](configuration.md): `config.yaml`, secrets, directories, distros
- [Agents and credentials](agents.md): Claude Code, opencode, pi, omp, Codex, MCP, transcripts
- [Forges, repos and SSH](forges.md): GitHub and Gitea, `pull` and `pr`, SSH agents,
  port forwards
- [Sessions, recordings and audit](sessions.md)
- [Headless tasks](tasks.md)
- [Notifications](notifications.md)
- [Snapshots, reset and base updates](snapshots.md)

## Security

- [Security model](security-model.md): what curral protects and its limits
- [Egress](egress.md): the allowlist proxy and prompts
- [Package caches](package-caches.md)
- [Canaries](canaries.md): decoy credentials and what happens on a hit
- [Reporting vulnerabilities](../SECURITY.md)

## Operating curral

- [Deployment](deployment.md): host sizing, the daemon, warm pool, idle stop, scheduled
  rebuilds, headless hosts, backups
- [Troubleshooting](troubleshooting.md): logs, error messages and recovery

## Contributing

- [Development](development.md): building, tests, code layout
- [Changelog](../CHANGELOG.md)
