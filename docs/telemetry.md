# Telemetry node, CLI reads, and external export

**Status:** implementation note · 2026-10-20  
**Scope:** M5 telemetry increment. Operation history remains in PostgreSQL and
works with no telemetry node. Bulk telemetry is opt-in and runs only on a
dedicated `telemetry` node.

## Placement and action path

Telemetry never runs on the main/control-plane host. `platform.telemetry.install`
and `platform.telemetry.agent` are ordinary platform actions: one durable
`eregion_operations` row, one River job, the shared event cursor, cancellation,
and idempotency contract. The control plane does not open its own queue.

The production adapters are registered by `controlplane.RegisterExtensions` from
both `serve` and `worker`. The process builds one PostgreSQL-backed
`controlplane.NodeTransports` resolver and passes it to telemetry through
`ExtensionConfig.NodeTransports`; the same resolver instance is also used by the
worker, so there is one transport cache and one WireGuard key lock per node:

```go
telemetry.ConfigureProductionWithResolver(registry, pool, nodes)
```

`ConfigureProduction(registry, pool, transport)` remains for local and
single-node workers; it adapts one already-selected transport to the same
resolver interface. A resolver may return only the transport that authenticates
as the requested node, and the pinned host identity recorded in placement must
match before any step runs.

- **Placement** reads `eregion_platform_nodes` (`roles`, `address`). A node with
  `telemetry` plus `control-plane` is refused. The address must be a literal
  private `IP:port`. The dispatch identity is derived from that recorded address;
  the table has no host-key column, so pinned SSH host keys remain the
  transport's responsibility (`EREGION_NODE_SSH_KNOWN_HOSTS`).
- **Dispatch** sends bounded, versioned `telemetry-*` steps through the resolved
  `node.Transport` for the target node. The node protocol has no arbitrary shell
  or filesystem request, so `internal/telemetry/remote` adds only three step shapes:
  `mkdir` beneath a declared root, atomic `file` writes beneath that root, and a
  narrow `docker compose` argv (`pull`, `up --detach --remove-orphans --wait`,
  or `ps --format json`). Every step is journaled under the operation's stable
  effect key; file content is represented by a hash in step inputs, so the node
  journal never stores credential plaintext.
- **Status** reports only observed facts. No telemetry node is `absent`;
  placement with no probe is `unknown`; a TCP connection to the private read
  endpoints is `reachable` or `unreachable`. Reachability is not asserted as
  service health.

`ConfigureProduction` adds no migration and does not bootstrap Docker/Compose;
the node must already be enrolled and have a container runtime.

Example submission (the plan is validated first):

```sh
eregion platform submit \
  --kind platform.telemetry.install \
  --target node/telemetry-1 \
  --input-file telemetry-install.json \
  --idempotency-key ci:telemetry-install-1 --json
```

```json
{
  "credentials_file": "/etc/eregion/telemetry/ingest-tokens.json",
  "grafana_password_file": "/etc/eregion/telemetry/grafana-password",
  "retention": {"metrics_days": 15, "logs_days": 14, "traces_days": 7},
  "enable_traces": false
}
```

Both files are root-owned `0600` references. Their contents never appear in
action input, operation events, River arguments, or command lines. Per-node OTLP
ingest tokens are generated per application/builder node and written only to a
`0600` token file on the telemetry node.

### Issuing per-node ingest credentials

`platform.telemetry.credential` issues one token and appends it to the protected
credential array consumed by `platform.telemetry.install`. The operation input
names only the target node and credential file; the result contains the node,
the file path, and the token's SHA-256 digest. The token itself never appears in
action input, operation events, River arguments, command lines, or the result.

```sh
eregion platform submit \
  --kind platform.telemetry.credential \
  --target node/app-1 \
  --input-file telemetry-credential.json \
  --idempotency-key ci:telemetry-credential-app-1 --json
```

```json
{"credentials_file": "/etc/eregion/telemetry/ingest-tokens.json"}
```

Repeating the action for the same node rotates that node's token in the file
(keeping every other node's entry) and records the new digest. The file must be
absent or a regular `0600` file; symlinks and group/world-readable files are
refused. The install action reads the same file from the control-plane host,
renders it into the telemetry node's `0600` token file, and keeps credential
values out of every event and command.

## Storage and license decision

The default stack is deliberately small and self-hosted:

| Component | License | Role |
| --- | --- | --- |
| OpenTelemetry Collector Contrib | Apache 2.0 | OTLP/gRPC + OTLP/HTTP ingest, batching, queueing |
| VictoriaMetrics | Apache 2.0 | bounded-retention metrics storage |
| VictoriaLogs | Apache 2.0 | bounded-retention log storage |
| Grafana (unmodified upstream image) | AGPL 3.0 | dashboards and alerting defaults |
| Tempo (optional) | AGPL 3.0 | bounded-retention traces when `enable_traces` is set |

VictoriaMetrics + VictoriaLogs were chosen over Loki/Tempo/Mimir because they
are Apache 2.0, have a single small binary per store, and expose simple retention
flags. The default footprint is one telemetry node with Docker volumes and
memory limits in the collector (512 MiB with a 128 MiB spike allowance).
Retention defaults are 15 days metrics, 14 days logs, and 7 days traces, capped
by validation at 90/90/30 days. Grafana is shipped unmodified; eregion supplies
provisioning, dashboards, and alerting defaults. There is no bundled ClickHouse.

Alerting defaults are rendered into
`grafana/provisioning/alerting/eregion-alerts.yaml` and mounted through the same
`grafana/provisioning` volume as the datasource and dashboard provider. The
provisioned rules watch VictoriaMetrics self-monitoring data that the rendered
stack actually produces: low free disk space and absent self-monitoring. They do
not assume an application metric exists; absent data stays `NoData` rather than
being treated as healthy.

Metrics and logs read paths go through `vmauth` gateways on the private address.
Ingest tokens are not reused for reads; read tokens are derived from the Grafana
admin secret and written to `0600` files on the telemetry node.

## External export

There are two export modes:

- **additive** (the default) keeps the local telemetry node and optionally
  copies telemetry to one or more upstream OTLP endpoints;
- **external-only** (`platform.telemetry.agent` with `external_only: true`) ships
  directly to upstream endpoints and does not install or require a telemetry
  node.

### Local stack export (additive)

`platform.telemetry.install` accepts an optional upstream OTLP endpoint in
addition to the telemetry node:

```json
{
  "credentials_file": "/etc/eregion/telemetry/ingest-tokens.json",
  "grafana_password_file": "/etc/eregion/telemetry/grafana-password",
  "external_export": {
    "endpoint": "https://otlp.example.com:4318",
    "protocol": "otlp_http",
    "token_file": "/etc/eregion/telemetry/external-token",
    "tls_ca_file": "/etc/eregion/telemetry/external-ca.crt"
  }
}
```

- `protocol` is `otlp_http` (default) or `otlp_grpc`.
- Plain HTTP is accepted only for private/loopback addresses; public endpoints
  require HTTPS.
- `token_file` and `tls_ca_file` are references on the telemetry node. The
  collector mounts them read-only and authenticates with a file-backed bearer
  token. No credential value is rendered into the collector config, compose
  file, events, or command arguments.
- Metrics, logs, and (when enabled) traces are copied to the upstream endpoint.
  The local stack is still installed; this form is additive only.

### Node agent: additive or external-only

`platform.telemetry.agent` installs the collector on an application or builder
node. In additive mode it requires the local telemetry endpoint and its per-node
credential and may also list `external_exports`:

```json
{
  "telemetry_address": "10.77.0.10:4317",
  "token_file": "/etc/eregion/otel/ingest-token",
  "tls_ca_file": "/etc/eregion/otel/ca.crt",
  "external_exports": [
    {
      "endpoint": "https://otlp.example.com:4318",
      "protocol": "otlp_http",
      "token_file": "/etc/eregion/otel/external-token",
      "tls_ca_file": "/etc/eregion/otel/external-ca.crt"
    }
  ]
}
```

External-only mode replaces the local telemetry node. The same node still
exposes OTLP receivers for applications, scrapes host metrics, and reads
container/application logs; only the destinations change:

```json
{
  "external_only": true,
  "external_exports": [
    {
      "endpoint": "https://otlp.example.com:4318",
      "token_file": "/etc/eregion/otel/external-token",
      "tls_ca_file": "/etc/eregion/otel/external-ca.crt"
    },
    {
      "endpoint": "https://otlp-eu.example.com:4317",
      "protocol": "otlp_grpc"
    }
  ],
  "root_dir": "/var/lib/eregion/otel-agent",
  "log_paths": ["/var/log/containers/*.log"]
}
```

Validation is fail-closed:

- `external_only` requires at least one `external_exports` entry and refuses a
  `telemetry_address`, `token_file`, or `tls_ca_file` in the same input. Those
  local fields are not silently ignored.
- Additive mode keeps the previous requirement for a private
  `telemetry_address`, a `0600` token-file reference, and a CA-file reference.
- Every external endpoint follows `ExternalExport` validation: HTTPS, or plain
  HTTP only for a private/loopback address; `otlp_http` (default) or `otlp_grpc`;
  no embedded URL credentials; `token_file` and `tls_ca_file` are absolute file
  references validated for safety. At most eight distinct
  endpoint/protocol pairs are accepted.
- The external token and CA files are mounted read-only at indexed paths
  (`/etc/otelcol/external-<n>-token` and `external-<n>-ca.crt`). Their contents
  are never read into action input, events, River arguments, commands, or
  results.

Rendering matches the mode. Additive mode writes the local ingest-token file and
TLS CA and includes the `otlp/telemetry` exporter. External-only mode writes
neither the local token file nor the CA, omits their compose mounts, and emits
only the external exporters in the metrics, logs, and traces pipelines. The
action result in external-only mode contains the node, `external_only: true`,
the non-secret external endpoints, and file/command counts; it never contains a
token value or a fabricated local endpoint.

Existing agent inputs without `external_exports` or `external_only` behave
exactly as before.

## CLI reads

`eregion logs search`, `eregion logs tail`, and `eregion metrics query` query the
private read gateways through `telemetry.DataSource`; they never open PostgreSQL
or SSH, and they never submit work. Output is plain text by default. Search and
query return one JSON envelope with `--json`; `tail` emits NDJSON records (one
`log` record per entry and one terminal `result`) with `--output ndjson`,
`--ndjson`, or `--json`.

```sh
eregion logs search --source '{host="app-1"}' --since 1h --limit 100 --json
eregion logs tail --source '*' --interval 1s --output ndjson
eregion metrics query --source app-1 --expression 'up' --unit count --graph
```

Source endpoint and credential configuration:

| Flag | Environment | Meaning |
| --- | --- | --- |
| `--telemetry-url` | `EREGION_TELEMETRY_URL` | node origin; ports 9427 (logs) and 8427 (metrics) are derived |
| `--logs-url` | `EREGION_TELEMETRY_LOGS_URL` | explicit logs read-gateway origin |
| `--metrics-url` | `EREGION_TELEMETRY_METRICS_URL` | explicit metrics read-gateway origin |
| `--logs-token-file` | `EREGION_TELEMETRY_LOGS_TOKEN_FILE` | `0600` bearer-token file |
| `--metrics-token-file` | `EREGION_TELEMETRY_METRICS_TOKEN_FILE` | `0600` bearer-token file |
| — | `EREGION_TELEMETRY_LOGS_TOKEN` | in-process token when no file is used |
| — | `EREGION_TELEMETRY_METRICS_TOKEN` | in-process token when no file is used |

Time windows use `--since`/`--end` or an absolute `--start`/`--end`; missing
configuration is the explicit `absent` state, not zero samples. `tail` stops on
interrupt (exit 130) and supports `--once` for a bounded read.

`eregion ui` accepts the same `--telemetry-url`, `--logs-url`,
`--metrics-url`, and token-file flags (and the same `EREGION_TELEMETRY_*`
environment) and adapts `telemetry.DataSource` to the workspace graph boundary.
With no telemetry configuration the fleet/env detail view says no telemetry node
is configured rather than drawing an empty graph. A configured source still
distinguishes no samples, stale data, and an unavailable backend. The workspace
never queries PostgreSQL or the telemetry node to invent missing values.

## Verified here vs not verified

Verified by unit and integration tests in this increment:

- placement parsing/refusal against a fake and (when
  `EREGION_TEST_DATABASE_URL` is set) a real migrated PostgreSQL database;
- registration and dispatch through a fake per-node resolver, including refusal
  of a resolved transport whose pinned identity names another node;
- file/mkdir steps dispatched through the real node protocol, including
  convergent retry and changed-input refusal;
- credential issuance writes one 0600 array, rotates/replaces one node entry,
  reports only a digest, and rejects a token in action input;
- external export rendering, file-reference mounts, and credential-free output,
  for both the additive local stack and the additive/external-only node agent;
- external-only agent validation: refusal of a local address/token/CA, refusal of
  an empty export list, duplicate/over-limit exports, and unsafe HTTP endpoints;
- external-only agent rendering and action dispatch: no local ingest token or CA
  mount, external-only collector pipelines, and a result with no token value;
- Grafana alerting provisioning is rendered with a datasource UID that matches
  the provisioned datasource, and the provisioning directory is mounted;
- CLI search/tail/query envelopes, NDJSON tail, plain output, explicit
  absent/no-data/stale states, terminal-escape sanitization, and unavailable
  backends;
- workspace graph adapter mapping from `telemetry.DataSource`, including absent,
  stale, and window handling, and the CLI's no-configuration nil source.

Not verified in this increment, and not claimed:

- a real telemetry VM receiving production OTLP data or serving Grafana;
- Grafana actually evaluating the provisioned alert rules, or delivering
  notifications (no contact point is configured);
- `docker compose` actually converging on a fresh host, package installation, or
  node enrollment/bootstrap;
- true service health, HA, backup/restore, or retention enforcement under load;
- a real external observability endpoint accepting the rendered OTLP traffic,
  including its authentication/TLS behaviour; external-only mode is verified by
  validation, render, and dispatched-action tests, not against a live vendor;
- external endpoint failover, per-export queue isolation under backpressure, or
  delivery guarantees beyond the collector's configured retry/queue settings;
- SSH/WireGuard transport identity beyond the pinned key configuration; the
  node table records an address, not a host key.

Operation diagnosis, `operations get/follow/events`, and durable change history
remain available with no telemetry node.
