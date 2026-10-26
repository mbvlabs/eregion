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

What remains is the read side used by `logs search`/`logs tail` and the
workspace graph:

- `internal/telemetry/datasource.go` defines the bounded query/result types and
  the honest `ok`/`no_data`/`stale`/`absent`/`error` states.
- `internal/telemetry/datasource_http.go` reads an already-configured private
  metrics/logs backend over HTTP. It has no install, render or Compose path.
- `internal/cli/logs.go` is the only operator command that reads logs. A later
  node_exporter/Grafana design can reuse these adapters; `metrics …` is not an
  operator command in the alpha.

Reads currently access configured backend URLs, not PostgreSQL and not a
finished private node-command abstraction.

URL configuration: `EREGION_TELEMETRY_URL`, `_LOGS_URL`, `_METRICS_URL` (with
matching CLI flags). Protected read credentials: `EREGION_TELEMETRY_LOGS_TOKEN_FILE`
and `_METRICS_TOKEN_FILE`; read tokens differ from ingest credentials. Environment
token values also remain in the current adapter. Align access through the chosen
private operator channel rather than making private backend ports public.

Search queries return JSON envelopes; tail supports NDJSON log records and a
final result (`--json` also selects its streaming form). Results identify source,
unit/window/timestamp and distinguish absent, no-data, stale and unavailable.
None may be rendered as fabricated zero/healthy data. Inspect current help for
exact filters and output behavior.

Source/unit tests do not qualify a real telemetry VM, live ingestion, Grafana
alert evaluation, external vendor TLS/auth or retention. Record such runs in
[MANUAL-TESTING](MANUAL-TESTING.md), never infer them from registration.
