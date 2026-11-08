# Node-local telemetry mechanism

The product decision is MASTER D5: every node runs node_exporter from setup on,
bound to its WireGuard address only. Telemetry is **node-local and
request-scoped**. There is no telemetry server, Prometheus/Grafana/Loki, metric
or log database, external backend, global PromQL query or retained metric
history. Operation history is PostgreSQL data and never depends on telemetry.

## Operator read path

```text
operator CLI
  -> saved context: SSH in embedded userspace WireGuard
  -> `eregion node rpc` on the node
  -> metrics.show: node_exporter on 10.78.0.1:9100 (wg0 only)
  -> bounded current NodeSnapshot (discarded when the command exits)
```

`eregion metrics show [--json]` is read-only. It has no prompt, dry-run,
confirmation or mutation path, and JSON/no-TTY never asks anything. The node
RPC runs as root but does **not** open the control database, so a metrics read
does not depend on PostgreSQL, River or the API.

The exporter listen address is the node's WireGuard address
(`10.78.0.1:9100`); ufw only opens 9100 on `wg0`. There is no public endpoint,
token, basic-auth credential or independent unattended scraper identity. The
snapshot's `scrape` object states `access: wireguard`, `public: false`,
`authentication: none`, the operator-RPC collection mode and the endpoint. The
operator command is the scraper; nothing runs on the node to store scrapes.

## Snapshot

Schema: `eregion/node-metrics/v1`.

- identity: logical node name (`node.json` when readable), exporter `nodename`,
  OS/kernel/machine, exporter build version;
- timing: `fetched_at` is recorded after the second response completes;
  exporter `observed_at` comes only from the second scrape's
  `node_time_seconds`, with freshness (`fresh`, `stale` or `unknown`) and its
  threshold. A missing second timestamp stays unknown instead of borrowing the
  first scrape's time; a materially future exporter timestamp is unknown, not a
  clamped fresh sample;
- `scrape`: endpoint, listen address, access, public, authentication,
  collection and a note;
- `cpu`: utilization percent, logical CPUs and counter window. The node-side
  collector reads node_exporter twice one second apart and computes
  `(1 - idle_delta / total_delta) * 100` across the reported CPU/mode counters.
  node_exporter v1.11.1 emits the eight modes `user`, `nice`, `system`, `idle`,
  `iowait`, `irq`, `softirq` and `steal` on `node_cpu_seconds_total`; guest
  time is exported separately as `node_cpu_guest_seconds_total` and is ignored
  here. Legacy `guest`/`guest_nice` series in `node_cpu_seconds_total` are
  tolerated but excluded from the denominator because Linux counts that time
  inside user/nice. A missing counter family, changed counter set, reset,
  missing any expected Linux mode for any observed CPU, missing idle delta,
  non-positive or inconsistent movement yields `cpu: null` plus an issue; never
  a fabricated zero;
- `memory`: total, available, used and used percent from
  `node_memory_MemTotal_bytes`/`node_memory_MemAvailable_bytes`;
- `filesystems`: real mounts paired from `node_filesystem_size_bytes` and
  `node_filesystem_avail_bytes`. tmpfs, overlay, Docker and pseudo mounts are
  omitted;
- `load`: `node_load1`, `node_load5`, `node_load15`;
- `uptime_seconds`: `node_time_seconds - node_boot_time_seconds`;
- `issues`: explicit field/reason entries for every observation that could not
  be produced. A null value is never rendered as zero or healthy.

The parser is a bounded Prometheus text reader: 4 MiB body, 1 MiB line, only
the needed families, strict label grammar and escapes, valid label whitespace
and one trailing comma, unknown lines ignored. A sample for a consumed family
that is malformed, duplicated, or carries a timestamp is a hard `unavailable`
refusal, not a silent omission or an old sample presented as current.
Exporter-provided label values are stripped of terminal control and format
characters before they enter the snapshot. The HTTP client ignores ambient
proxies, does not follow redirects, has a five-second request timeout and a
ten-second collection budget.

## Human output and workspace

`metrics show` prints the same snapshot as JSON: node/exporter identity, the
WireGuard endpoint and access boundary, sample time/freshness, CPU, memory,
load, uptime and filesystem free space. Unavailable fields print their reason.
The command ends with the explicit statement that no samples are stored.

The workspace Fleet detail reads the same `metrics.show` node RPC and shows one
current CPU sample plus current memory/filesystem/load/uptime facts. It does not
draw a history sparkline from a single value, does not query PromQL and does not
pretend to have request metrics. It shows the source's explicit `fresh`,
`stale` or `unknown` freshness independently of whether CPU is available;
unknown exporter time is never replaced by collection time. It carries the
node-computed age and threshold and adds monotonic local elapsed time after
receipt, so operator/node wall-clock skew does not fabricate freshness. A
refresh that reloads a focused detail also re-queries metrics. A query for a
different node than the snapshot identifies is refused rather than relabeled. A
per-query generation fence discards an older response after a newer request, so
it cannot overwrite the fresh view. Envs has no graph because no per-request
history exists.

`server status` and the existing `services.running` doctor check include
`node-exporter.service` on the main role. There is no separate metric doctor
check; the existing `services.running` row covers it alongside the other
required services. The total number of doctor rows is not a fixed contract:
compare the reported summary and the named service row, not an assumed count.

## Verification

Isolated tests cover text-fixture parsing, missing values, the pinned
node_exporter v1.11.1 eight-mode CPU set, legacy guest-mode exclusion, counter
resets, second-scrape-only timestamps, post-response
completion time, malformed, timestamped and duplicate relevant samples, valid
label whitespace/trailing comma plus invalid grammar, label-value sanitization,
materially future exporter timestamps, two-read collection, response bounds,
node-RPC method selection, CLI plain/JSON shape and the workspace adapter
including explicit freshness, node-computed age/threshold, refresh re-query,
node mismatch and the metrics-response generation fence. They do not qualify a
real server, exporter installation, WireGuard reachability, or a live upgrade.
Record those runs in [MANUAL-TESTING](MANUAL-TESTING.md); never infer them from
unit tests.
