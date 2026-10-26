# eregion

**deploycrate-ce functionality, delivered like vaire: one glorious CLI.**

One Go binary sets up, secures, deploys and operates applications on servers
you own. Agent-first JSON commands and a polished human terminal experience; no
web UI.

**Status: preparing the first v1 alpha; nothing released.** Setup, the private
operator channel, durable operations, builds, deployments, domain routes and
upgrades have code. The alpha's operator channel, application workflow and
release pipeline are still being built; see the work list in MASTER.

## Start here

1. [MASTER](docs/MASTER.md): goal, decisions, the application model, the target
   command tree, the v1 alpha scope and the one work list with status.
2. [IMPLEMENTATION](docs/IMPLEMENTATION.md): code map, current commands, database
   schema and execution boundaries.
3. [MANUAL-TESTING](docs/MANUAL-TESTING.md): the alpha acceptance checks and the
   record of real-server runs.

[AGENTS.md](AGENTS.md) explains how to work in this repository.

## Build and discover

Requires Go 1.27.1 or newer; `just` is optional.

```sh
just build
bin/eregion commands --json
bin/eregion help server init --json
```

Discovery and help need no database or credentials. The application commands are
`app`, `env`, `process`, `config`, `domain`, `deploy`, `rollback`, `restart`,
`build` and `release list`; the target tree is in MASTER.

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
