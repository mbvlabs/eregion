# Telemetry mechanism reference

The product decision is MASTER D5: every server runs node_exporter from setup
on, bound to its WireGuard address; the **telemetry node** is a separate
self-hosted Grafana with its metrics and log storage that scrapes servers over
WireGuard (after v1), or an external backend. No ClickHouse. In the alpha,
`logs` reads container logs over the operator channel (D25). Current bootstrap
installs none of this.

Operation history is PostgreSQL data, independent of telemetry availability. Do
not block operation diagnostics on a working log/metrics backend.

## Current wiring

The Docker Compose based telemetry stack install (collector, storage, read
gateways and its narrow `telemetry-*` node steps) is deleted: "never Compose"
(D5) and the telemetry node is after v1. No telemetry action, agent, credential
or inventory collection is registered, and no extension receives a node
transport for telemetry.

What remains is the read side used by the workspace metrics graph:

- `internal/telemetry/datasource.go` defines the bounded query/result types and
  the honest `ok`/`no_data`/`stale`/`absent`/`error` states.
- `internal/telemetry/datasource_http.go` reads an already-configured private
  metrics/logs backend over HTTP. It has no install, render or Compose path.
- `internal/cli/workspace.go` uses the adapter for workspace metrics. `logs`
  reads container logs over the operator channel and does not use this backend.

Reads currently access configured backend URLs for workspace metrics, not
PostgreSQL and not a finished private node-command abstraction.

URL configuration: `EREGION_TELEMETRY_URL`, `_LOGS_URL`, `_METRICS_URL` (with
matching CLI flags). Protected read credentials: `EREGION_TELEMETRY_LOGS_TOKEN_FILE`
and `_METRICS_TOKEN_FILE`; read tokens differ from ingest credentials. Environment
token values also remain in the current adapter. Align access through the chosen
private operator channel rather than making private backend ports public.

Metric queries return bounded series with source, unit/window/timestamp and
distinguish absent, no-data, stale and unavailable. None may be rendered as
fabricated zero/healthy data. Container logs are no longer a telemetry read:
`logs` streams the serving deployment's Docker containers over node RPC and
prefixes each line with process/replica.

Source/unit tests do not qualify a real telemetry VM, live ingestion, Grafana
alert evaluation, external vendor TLS/auth or retention. Record such runs in
[MANUAL-TESTING](MANUAL-TESTING.md), never infer them from registration.
