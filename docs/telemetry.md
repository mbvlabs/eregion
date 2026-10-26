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

`controlplane.RegisterExtensions` calls
`telemetry.ConfigureProductionWithResolver` with the shared per-node transport
resolver. Install/agent work uses durable platform actions and bounded
`telemetry-*` node steps for files/directories and narrow Docker Compose commands.
There is no second queue or arbitrary shell protocol.

Placement reads node roles/address. The telemetry storage role cannot coexist
with control-plane. Agent actions target application/builder nodes; setup does
not install a collector everywhere, nor does a telemetry-role bootstrap install
the stack. Docker/Compose, enrollment and protected credential references must
already be usable. The placement adapter reads roles and address; the
transport resolver owns SSH identity verification. The existing per-node agent
is an OpenTelemetry Collector deployed with Compose; D5 and D10 replace it with
node_exporter as a host step.

| Action | Purpose |
| --- | --- |
| `platform.telemetry.credential` | Generate/rotate a per-node ingest token in a protected credentials file; result is metadata/digest only. |
| `platform.telemetry.install` | Render/install the dedicated stack with credentials/password file references, retention and optional additive upstream export. |
| `platform.telemetry.agent` | Install node collector with local telemetry destination, optional upstreams, or explicit `external_only`. |

Inspect `internal/telemetry/actions.go` and command discovery/generic submission
before using raw inputs. Token/password/CA files are explicit protected file
references, never secrets in operation input, events or process arguments.

Default storage/rendering: OpenTelemetry Collector Contrib, VictoriaMetrics,
VictoriaLogs, read-only vmauth gateways, unmodified Grafana; optional Tempo.
Defaults: 15-day metrics, 14-day logs, 7-day traces; validation caps 90/90/30.
Collector/Victoria components are Apache 2.0; Grafana/Tempo are AGPL. No forked
Grafana or ClickHouse is implied. Provisioned alerts have no notification contact
point and are not evidence of live alert delivery.

Additive export keeps local storage. `external_only` requires upstream destinations
and refuses simultaneous local-address/token/CA fields. Upstreams support OTLP
HTTP/gRPC with HTTPS, or private/loopback HTTP, and file-backed auth/CA references.
Rendering/mounting a queue does not prove live delivery, retention under load,
backpressure isolation or endpoint failover.

## CLI reads

Current reads: `logs search`, `logs tail`, `metrics query`; workspace graphs adapt
the same `telemetry.DataSource`. Reads currently access configured backend URLs,
not PostgreSQL and not a finished private node-command abstraction.

URL configuration: `EREGION_TELEMETRY_URL`, `_LOGS_URL`, `_METRICS_URL` (with
matching CLI flags). Protected read credentials: `EREGION_TELEMETRY_LOGS_TOKEN_FILE`
and `_METRICS_TOKEN_FILE`; read tokens differ from ingest credentials. Environment
token values also remain in the current adapter. Align access through the chosen
private operator channel rather than making private backend ports public.

Search/query return JSON envelopes; tail supports NDJSON log records and a final
result (`--json` also selects its streaming form). Results/graphs identify source,
unit/window/timestamp and distinguish absent, no-data, stale and unavailable.
None may be rendered as fabricated zero/healthy data. Inspect current help for
exact filters and output behavior.

Source/render/unit tests do not qualify a real telemetry VM, live OTLP ingestion,
Grafana alert evaluation, external vendor TLS/auth or retention. Record such runs
in [MANUAL-TESTING](MANUAL-TESTING.md), never infer them from registration.
