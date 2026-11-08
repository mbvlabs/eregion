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
`node-exporter.service` on the main role. `telemetry.local` additionally checks
the local exporter health: it reads one bounded response from the WireGuard
endpoint, requires `/var/lib/node_exporter/textfile` to exist with root:root
0755 and no symlink, and requires node_exporter's `textfile` and `systemd`
collector success families plus exactly one unlabelled `node_textfile_scrape_error`
sample whose value is zero. Collector success alone does not prove the textfile
was readable. A missing directory, missing/ambiguous scrape-error sample or a
non-zero/ambiguous one fails; duplicate, timestamped or malformed collector
success samples are also ambiguous and fail closed. A down WireGuard interface
reports the collector health as unobserved (**warn**, not pass), while an
independent directory configuration error still fails. The check does not store
a sample, produce history or replace `metrics show`. The total number of doctor
rows is not a fixed contract: compare the reported summary and the named rows,
not an assumed count.

## Local collector foundations

`host-node-exporter-v2` is the versioned host step for the local primitives. It
keeps node_exporter 1.11.1 bound only to the node's WireGuard address and adds:

- the root-owned `0755` textfile directory
  `/var/lib/node_exporter/textfile`, and its root-owned `0755` parent
  `/var/lib/node_exporter`, both refused as symlinks or non-directories by the
  apply/check; apply also refuses a non-root-owned or group/other-writable
  existing parent before any mutation instead of repairing it. Setuid, setgid
  and sticky bits are preserved in the observation and rejected by doctor. The
  safe repair uses `chmod 0755` and then explicitly clears special bits with
  `chmod u-s,g-s,o-t`, because numeric chmod can preserve setgid on a
  directory; apply verifies the resulting mode is exactly `0755` and fails
  closed otherwise;
- `--collector.textfile.directory` with that directory;
- `--collector.systemd` with a narrow unit include:
  `(eregion-.+|caddy|docker|node-exporter|wg-quick@wg0)[.](service|timer)`.

The textfile directory is a foundation, not a metrics source. No
`eregion_*.prom` file is written today: a self-metric family must come from a
real producer, such as a future retention/backup/doctor job that actually
observes the value. Emitting a synthetic constant would be worse than exposing
nothing. Future owners write one atomic `0644` file per owner; node_exporter
reads them as its own user. The doctor check separately requires
`node_textfile_scrape_error` to be present exactly once and zero, because
`node_scrape_collector_success` can be 1 even when a `.prom` file is malformed
or unreadable.

The node_exporter unit remains WireGuard-only and keeps the existing systemd
hardening. The systemd collector reads only the named unit set over D-Bus; it
does not enumerate every host unit. The unit still starts with `wg0`, so a node
with WireGuard down has no reachable exporter by design.

## Read-only external scrape example

The following examples are documentation only; eregion does not install,
supervise or authenticate Prometheus or Grafana Alloy. They work only from a
host with a WireGuard route to the node and firewall permission for 9100. They
do not add a public endpoint, TLS terminator, basic-auth credential, service
discovery or rotation.

Prometheus:

```yaml
scrape_configs:
  - job_name: eregion-node
    metrics_path: /metrics
    static_configs:
      - targets: ["10.78.0.1:9100"]
```

Grafana Alloy:

```hcl
prometheus.scrape "eregion_node" {
  targets    = [{"__address__" = "10.78.0.1:9100"}]
  forward_to = [prometheus.remote_write.default.receiver]
}
```

The Alloy fragment assumes a `prometheus.remote_write.default` receiver. The
example intentionally shows no credential because the exporter has none, and it
does not add a public endpoint, TLS terminator, service discovery or rotation. A
future separate telemetry node or Grafana is **not prohibited long-term**.
Adding one needs an explicit WireGuard peer/route, a firewall decision, an
authenticated endpoint or a scraper-local trust boundary, discovery and
credential rotation; none is in beta/v1 scope.

## Not full vaire parity

These local foundations borrow vaire's textfile and narrow systemd collector
shape, but this is not vaire exporter parity. Missing today:

- the authenticated external `https://<host>/metrics/...` endpoint, basic-auth
  password file and fail2ban protection;
- `targets.json`/`http_sd` (Prometheus) and `discovery.http` (Alloy) service
  discovery;
- credential rotation and a protected metrics credential handoff;
- the exporter systemd slice/memory budget and postgres/pgbouncer exporters;
- a central telemetry service, Prometheus/Grafana deployment, alerting,
  metric/log storage or retained history;
- eregion self-metric producers. The textfile directory is ready, but no
  meaningful family is emitted until a real producer exists.

None of these omissions is hidden by the `metrics show` snapshot or the doctor
row. Host qualification of `host-node-exporter-v2`, the systemd collector over
D-Bus, the textfile collector reading a real file, a real exporter restart and
the scrape examples remains manual acceptance.

## Verification

Isolated tests cover text-fixture parsing, missing values, the pinned
node_exporter v1.11.1 eight-mode CPU set, legacy guest-mode exclusion, counter
resets, second-scrape-only timestamps, post-response
completion time, malformed, timestamped and duplicate relevant samples, valid
label whitespace/trailing comma plus invalid grammar, label-value sanitization,
materially future exporter timestamps, two-read collection, response bounds,
node-RPC method selection, CLI plain/JSON shape and the workspace adapter
including explicit freshness, node-computed age/threshold, refresh re-query,
node mismatch and the metrics-response generation fence. Local-health tests
cover missing, duplicate, malformed, labelled, timestamped, non-zero and
out-of-range `node_textfile_scrape_error`, strict root:root 0755 no-symlink
directory observation, apply refusal before mutation, WireGuard-down warning and
independent directory-error failure. They do not qualify a real server, exporter
installation, WireGuard reachability, systemd D-Bus collection, textfile
reading, exporter restart, a live upgrade or a real scrape. Record those runs in
[MANUAL-TESTING](MANUAL-TESTING.md); never infer them from unit tests.
