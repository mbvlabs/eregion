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
guided terminal workspaces are in this tree. Two cross-node build bullets remain
open: dedicated build credentials and registry artifact movement. A distinct
`build_node` is placement-checked but refused before `pack` until that path
exists, rather than pretending a local image moved to another host.

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

## Testing

There are no local test harnesses: no Docker launchers, drills, or scratch VMs.
Changes land with unit and integration tests (`just check`), then get built and
exercised by hand on a real test host:

```sh
just build build-node
bin/eregion bootstrap --help   # converge a fresh Debian/Ubuntu host over pinned SSH
```

See [bootstrap](docs/bootstrap.md) for the full first-contact flow.

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
