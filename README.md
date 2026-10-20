# eregion

An **agent-first** self-hosted application platform for user-owned Debian and Ubuntu
servers. Every capability is a durable, machine-readable operation first; human
terminal experiences come second, on top of the same contracts. A web UI can come
later.

**Current state: M0–M7 building blocks implemented.** The authenticated API,
external PostgreSQL state, versioned migrations, River workers, the Check/Apply
step engine, node dispatch, pinned SSH and userspace WireGuard transports, the
Charm operation viewer, host bootstrap, node enrollment, Buildpacks releases,
deployments and rollback, PostgreSQL resources and backups, telemetry/log reads,
Cloudflare DNS and private-access grants, GitHub webhooks, signed upgrades, and
guided terminal workspaces are in this tree.

This is an **experimental v0.1 line**, not a production or HA claim. The
fresh-VM matrix, a real Cloudflare zone, a real telemetry-node install, and
multi-host failover have not been exercised here; see
[release criteria](docs/release-criteria.md) for the exact gate status.

## Build and explore

Requires Go 1.27.1 or newer and [just](https://just.systems/). No database or
credentials are needed for local discovery and help.

```sh
just build
bin/eregion commands --json
bin/eregion commands --path platform --json
bin/eregion help platform deployments deploy --json
bin/eregion guide --json
```

Agents should use `--json` or `--output json`. Every successful invocation, help
request, or command/flag failure produces one `eregion/v1` JSON envelope on
stdout. Plain output is append-only, with errors on stderr. Neither mode prompts
or probes the terminal. Only implemented commands appear in discovery. Mutating
commands support dry-run/preview semantics and stable idempotency keys.

## Run it locally

The development launcher creates a disposable loopback PostgreSQL, API, and
worker. It never touches host services, and it refuses to stop containers it does
not own.

```sh
just dev-up          # print the loopback URL and how to load credentials
source .local/dev/env
bin/eregion ui --interactive
just dev-status
just dev-down        # discard only this checkout's disposable database
```

End-to-end drills against the running development stack:

```sh
just smoke           # real image build, deploy, failed candidate, rollback
just buildpacks-smoke  # directory-source Buildpacks release, deploy, served body
just terminal-smoke  # real PTY viewer matrix, detach, replay
just recovery-drill  # disposable PostgreSQL backup/verify/restore drill
just upgrade-drill   # signed release, apply, rollback, health restore
```

`just release-check` runs the full local release gate. Container prerequisites
skip explicitly and never silently pass.

## Host bootstrap and nodes

`eregion bootstrap` converges one fresh Debian or Ubuntu host (local or over
pinned SSH) with a resumable journal, explicit role placement, database
preflight, migration, API/worker services, readiness, context login, scoped
automation credentials, and commit-confirm for access-breaking changes. See
[bootstrap](docs/bootstrap.md).

Additional application, builder, registry, edge, and telemetry nodes enroll over
pinned SSH with a node-generated identity, probe-derived capabilities, role
constraints, and WireGuard peer reconciliation. A hostname is never
authorization. See [nodes](docs/nodes.md).

## Deploy, resources, telemetry, network

- Buildpacks releases, durable deployments, health-checked traffic switch,
  rollback, reconciliation, and per-target terminal views:
  [platform contract](docs/platform-contract.md).
- PostgreSQL application resources, verified backups, retention, confirmed
  restore, and a separate control-plane database recovery procedure:
  [resources](docs/resources.md).
- Dedicated telemetry node, OTLP ingest, external export, CLI log/metrics reads,
  and honest absent/stale/no-data states: [telemetry](docs/telemetry.md).
- River-backed Cloudflare DNS reconciliation and audited private-access grants:
  [network](docs/network.md).
- Signed releases, compatibility checks, operator/node upgrade and rollback,
  drills, and release criteria: [upgrades](docs/upgrades.md),
  [release criteria](docs/release-criteria.md).

## Development

```sh
just fmt       # format Go sources
just check     # formatting, vet, unit/contract/schema/executable tests
just race      # race-detector tests
just integration-norace   # PostgreSQL/River/API/process tests
just transport            # SSH/WireGuard transports, disposable fixtures
```

`just integration` runs the PostgreSQL/River/API/process suite when
`EREGION_TEST_DATABASE_URL` points at a disposable PostgreSQL server. Transport
tests use disposable Docker fixtures and skip cleanly when Docker is unavailable.
`EREGION_TEST_DOCKER=1` adds real Docker deploy/rollback coverage.

## Design and contracts

- [Implementation roadmap](docs/roadmap.md)
- [Agent/wire contracts and exit codes](docs/contracts.md)
- [Control-plane setup and API](docs/control-plane.md)
- [Platform API and extension contract](docs/platform-contract.md)
- [Automation security](docs/automation-security.md)
- [Research and architecture](docs/initial_research.md)
- [TUI design](docs/tui-design.md) and [interactive sketches](docs/design/tui-sketches.html)
- [Versioned JSON Schema](schema/eregion-v1.json)

The control plane uses external PostgreSQL and River. Operation history lives in
PostgreSQL and works without any telemetry node. The main node runs the control
plane, application runtime, Buildpacks, and registry by default; additional nodes
take explicit roles. Telemetry never runs on the main node. Remote effects are
at-least-once with idempotency and reconciliation, not a distributed lock.
