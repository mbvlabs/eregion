# eregion

**deploycrate-ce functionality, delivered like vaire: one glorious CLI.**

One Go binary sets up, secures, deploys and operates applications on servers
you own. Agent-first JSON commands and a polished human terminal experience; no
web UI.

**Status: beta.1 fresh-test prerelease; fresh-install qualification is pending.**
Setup, the private operator channel, durable operations, builds, deployments,
domain routes, extensions, staging-to-production promotion and upgrades have
code. Authenticated public HTTPS telemetry and external collection passed on the
recorded signed edge build. Cloudflare automation has separate qualification
gates. See MANUAL-TESTING for evidence and Linear for actionable work.

## Start here

1. [MASTER](docs/MASTER.md): goal, decisions, the application model, the target
   command tree and the v1 alpha/beta scope. Linear tracks execution status.
2. [IMPLEMENTATION](docs/IMPLEMENTATION.md): code map, current commands, database
   schema and execution boundaries.
3. [MANUAL-TESTING](docs/MANUAL-TESTING.md): the alpha/beta acceptance checks and
   the record of real-server runs.

[AGENTS.md](AGENTS.md) explains how to work in this repository.

## Build and discover

Requires Go 1.27.1 or newer; `just` is optional.

```sh
just build
bin/eregion commands --json
bin/eregion help server init --json
```

Discovery and help need no database or credentials. The operator surface is
MASTER section 5: `server init`, `plan`, `apply`, `login`, `workspace`, `secrets
export`, `app`, `env`, `process`, `config`, `domain`, `deploy`, `rollback`,
`restart`, `build`, `release list`, `logs`,
`metrics show|endpoint|config|rotate`, `operations`,
`token`, `protect`, `ssh`, `upgrade`, `doctor`, `ui`, `commands`, `guide`,
`skill` and `version`.
Node-mode entry points (`eregion node …`) are hidden server plumbing, not part of
`commands --json`.

Servers run Debian 13 or Ubuntu 26.04 (amd64/arm64) with Docker, Buildpacks, a
local registry and Caddy, and keep their state in an external PostgreSQL 17+
with River. The operator CLI targets Linux and macOS; today's `server init`
needs a Linux operator binary matching the server architecture.

## Focused references

- [Machine and operation contracts](docs/contracts.md)
- [Secrets](docs/secrets.md), [routes and DNS](docs/network.md)
- [Telemetry](docs/telemetry.md), [TUI design](docs/tui-design.md),
  [terminal presentation](docs/workspace.md)
- [Upgrades](docs/upgrades.md), [security notes](docs/security-review.md)
