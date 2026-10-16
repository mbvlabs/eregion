# eregion

An **agent-first** self-hosted application platform for user-owned Debian and Ubuntu
servers. Human terminal experiences come second, with a rich Charm-based interface
planned on top of the same machine-readable contracts. A web UI can come later.

**Current state: M0.1 — CLI foundation.** Discovery, help, version information, and
shared operation/output contracts are implemented. There is no running control
plane, database connection, job worker, bootstrap, or deployment command yet.

## Build and explore

Requires Go 1.27.1 or newer and [just](https://just.systems/). No database or
credentials are needed for this milestone.

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

Linux CI runs all checks; macOS and Windows CI test the portable operator CLI.
Server support will be a separate Debian/Ubuntu VM matrix once host operations exist.

## Design and contracts

- [Implementation roadmap](docs/roadmap.md)
- [Agent/wire contracts and exit codes](docs/contracts.md)
- [Research and architecture](docs/initial_research.md)
- [TUI design](docs/tui-design.md) and [interactive sketches](docs/design/tui-sketches.html)
- [Versioned JSON Schema](schema/eregion-v1.json)

The intended control plane uses external PostgreSQL and River, role-focused
servers, an early automation API, and external telemetry integrations. M0.2 adds
the first durable operation; the current operation service is an interface only.
