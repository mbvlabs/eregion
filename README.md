# eregion

An **agent-first** self-hosted application platform for user-owned Debian and Ubuntu
servers. Human terminal experiences come second, with a rich Charm-based interface
planned on top of the same machine-readable contracts. A web UI can come later.

**Current state: M0.2 — durable diagnostic operations.** The authenticated API,
external PostgreSQL state, explicit migrations, and River workers are implemented.
The diagnostic operation changes only database records/events. Host bootstrap,
deployments, remote node execution, and the Charm viewer are not implemented yet.

## Build and explore

Requires Go 1.27.1 or newer and [just](https://just.systems/). No database or
credentials are needed for local discovery and help.

```sh
just build
bin/eregion commands --json
bin/eregion commands --path version --json
bin/eregion help commands --json
bin/eregion version
```

Agents should use `--json` or `--output json`. Every successful invocation, help
request, or command/flag failure produces one `eregion/v1` JSON envelope on stdout.
Plain output is append-only, with errors on stderr. Neither mode prompts or probes
the terminal. Only implemented commands appear in discovery.

```sh
just fmt      # format Go sources
just check    # formatting, go vet, unit/contract/schema/executable tests
just race     # race-detector tests
```

For the first live operation, see [control-plane setup](docs/control-plane.md).
`just integration` runs PostgreSQL/River/API/process-crash tests when
`EREGION_TEST_DATABASE_URL` points to a disposable PostgreSQL server.

Linux CI runs unit/race checks and a separate PostgreSQL 17 integration job;
macOS and Windows CI test the portable operator CLI.
Server support will be a separate Debian/Ubuntu VM matrix once host operations exist.

## Design and contracts

- [Implementation roadmap](docs/roadmap.md)
- [Agent/wire contracts and exit codes](docs/contracts.md)
- [Control-plane setup and API](docs/control-plane.md)
- [Research and architecture](docs/initial_research.md)
- [TUI design](docs/tui-design.md) and [interactive sketches](docs/design/tui-sketches.html)
- [Versioned JSON Schema](schema/eregion-v1.json)

The intended control plane uses external PostgreSQL and River, role-focused
servers, an early automation API, and external telemetry integrations. M0.2 proves
durable execution with database-only diagnostics; it is not a production PaaS yet.
