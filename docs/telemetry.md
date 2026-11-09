# Node-local telemetry mechanism

The product decision is MASTER D5: every node (currently the main node) runs
node_exporter from setup on, bound to **loopback only**. Telemetry is
**node-local**: one request-scoped snapshot for the operator, and one
WireGuard-only authenticated endpoint for an external scraper. There is no
central telemetry server, Prometheus/Grafana/Loki deployment, metric or log
database, external backend, global PromQL query, public endpoint or retained
metric history. Operation history is PostgreSQL data and never depends on
telemetry.

## Operator read path

```text
operator CLI
  -> saved context: SSH in embedded userspace WireGuard
  -> `eregion node rpc` on the node
  -> metrics.show: node_exporter on 127.0.0.1:9100
  -> bounded current NodeSnapshot (discarded when the command exits)
```

`eregion metrics show [--json]` is read-only. It has no prompt, dry-run,
confirmation or mutation path, and JSON/no-TTY never asks anything. The node
RPC runs as root but does **not** open the control database, so a metrics read
does not depend on PostgreSQL, River or the API.

The exporter listens only on loopback. There is no direct WireGuard or public
exporter listener, token or scraper account. The snapshot's `scrape` object
states `access: loopback`, `public: false`, `authentication: none`, the
operator-RPC collection mode and the loopback endpoint. The operator command is
the scraper for this snapshot; nothing runs on the node to store scrapes.

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
loopback exporter/access boundary, sample time/freshness, CPU, memory, load,
uptime and filesystem free space. Unavailable fields print their reason. The
command ends with the explicit statement that no samples are stored.

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
`node-exporter.service` and `caddy.service` on the main role. The dedicated
telemetry instance is checked by `telemetry.endpoint` rather than the shared
service list. `telemetry.local` checks the local exporter health: it reads one
bounded response from the
loopback endpoint, requires `/var/lib/node_exporter/textfile` to exist with
root:root 0755 and no symlink, and requires node_exporter's `textfile` and
`systemd` collector success families plus exactly one unlabelled
`node_textfile_scrape_error` sample whose value is zero. Collector success alone
does not prove the textfile was readable. A missing directory,
missing/ambiguous scrape-error sample or a non-zero/ambiguous one fails;
duplicate, timestamped or malformed collector success samples are also
ambiguous and fail closed. A down WireGuard interface does not hide the
loopback exporter or its strict directory state. `telemetry.endpoint` probes
`/targets.json` without a credential: 401 is healthy, 200 is an unauthenticated
exposure and fails, a node without the telemetry Caddyfile warns until
rotation, and WireGuard down leaves that external observation unobserved as
warn. Neither check stores a sample, produces history or replaces `metrics
show`. The total number of doctor rows is not a fixed contract: compare the
reported summary and the named rows, not an assumed count.

## Authenticated endpoint and discovery

Eregion adapts vaire's nginx metrics endpoint to a **dedicated Caddy instance**,
separate from the public ingress Caddy. `host-telemetry-v1` ensures
`/etc/eregion-telemetry/targets.json` (`root:caddy 0640`, no secret), the
`eregion-telemetry.service` unit and the WireGuard-only firewall rule. The
Caddyfile is `/etc/eregion-telemetry/Caddyfile`, written by `metrics rotate`,
not by `apply`, so an apply never replaces a newer credential with an older
one. The dedicated instance has its own `admin off`, Caddyfile, autosave/data
directories (isolated from public Caddy) and `StateDirectory`; public
`caddy.service`, `/etc/caddy/Caddyfile` and `/etc/caddy/eregion.d/*.caddy` are
never touched by telemetry. A missing `wg0`, an invalid telemetry Caddyfile or
a failed telemetry restart therefore cannot stop public ingress.

The unit runs the same pinned Caddy binary as `User=caddy`, waits for
`network-online.target` and `wg-quick@wg0.service`, and requires the telemetry
Caddyfile before starting. It is `BindsTo=wg-quick@wg0.service` and
`WantedBy=multi-user.target wg-quick@wg0.service`, so every wg start/restart
also starts the telemetry instance; a missing wg0 is not a permanent skip. It
restarts on failure. The site uses an explicit `bind 10.78.0.1`; the site
address alone is only a Host matcher. It listens only on `10.78.0.1:9101`; ufw
opens 9101 only on `wg0`, a failing ufw rule fails host apply rather than being
swallowed, and `host-telemetry-v1` Check compares `ufw show added` against the
exact rule so drift is repaired on the next apply. If ufw is unavailable the
check reports it as unobserved rather than failing the step, matching Apply's
existing skip. It serves two basic-auth paths:

- `/node` reverse-proxies `http://127.0.0.1:9100/metrics` and strips the
  scraper's `Authorization`/`Cookie` headers before the loopback request;
- `/targets.json` serves the HTTP service-discovery document. One entry names
  `10.78.0.1:9101` with `__metrics_path__: /node`, `__scheme__: http`,
  `eregion_node`, `eregion_component: node`, `eregion_access: wireguard` and
  `eregion_public: false`.

The telemetry Caddyfile (`root:caddy 0640`) carries only the basic-auth
username and a bcrypt hash, never the password. There is no TLS terminator
because WireGuard encrypts the transport; there is no public listener. Caddy's
own request handling and timeouts apply. The hash cost is 8, matching vaire; the
generated password has 256 bits of entropy, so cost is not the protection.

`metrics endpoint` reads the non-secret endpoint description over node RPC:
listen address, URLs, access, basic-auth username, whether the telemetry
Caddyfile exists, and whether this machine holds a local credential. `metrics
config` generates a Prometheus `http_sd_configs` snippet and the equivalent
Alloy `discovery.http` + `prometheus.scrape` snippet. Ordinary output, including
`--json`, always uses `<metrics-password>`. `--format
prometheus|alloy|targets --out FILE` writes one new owner-only file (`0600`)
with the real credential when the format needs one; it fails without a local
credential. The `targets` file contains no secret and is also a valid
Prometheus `file_sd` document if an operator prefers a local file over HTTP
discovery.

The control-plane host step installs the pinned Caddy package even when no
public hostname is configured. Dependencies are checked at their real paths:
`/usr/bin/caddy` must report the pinned version and the `caddy:caddy` account
must exist; a matching binary elsewhere on `PATH` is not accepted. Before
installing it inspects `/etc/caddy/Caddyfile` and
`caddy.service`/`caddy-api.service`; any existing config or enabled/active
Caddy service is treated as unmanaged and the step refuses rather than
overwriting or stopping it. On a fresh host it installs the pinned package,
ensures the account, removes only the package-created default Caddyfile and
disables only a package-started service, leaving no public Caddy config or
listener.

`metrics rotate --confirm <node>/metrics` follows vaire's rotate path rather
than a generation-fenced transaction. The operator generates a fresh 256-bit
password, saves it owner-only in this machine's context *before* the node
request, and sends only its bcrypt hash. The node takes a node-local telemetry
lock, renders the telemetry Caddyfile, validates it with `caddy validate`,
restarts only `eregion-telemetry.service`, and restores the previous Caddyfile
if validation or restart fails. The public route writer's PostgreSQL advisory
lock and public Caddy reload are not involved. If the RPC itself fails, the CLI
restores the previous local credential (removing a first credential it just
generated). Rotation promises only that the node credential changed, not that
every scraper or handoff has the new password; output warns that scrapers must
be updated and that `secrets export` is stale. Neither the password nor the
bcrypt hash is written to ordinary output, logs, operation events or journals.

A lost RPC response after the node applied the new hash can leave the operator
machine on the restored old password while the node serves the new one. That is
vaire's simple rotate limit; the fix is another deliberate `metrics rotate` (or
importing a current protected handoff), not a local recovery journal. There is
no compare-and-swap, pending file, generation, rate limiter or custom endpoint
process.

`server init` reuses the local credential when it exists, otherwise generates
one, and performs the same rotate before its doctor gate; it includes only the
username and password in the protected handoff, and `login` restores them.
`secrets export` includes the local credential when present and reports an
explicit omission when it is missing. A node upgraded from the older layout has
no telemetry Caddyfile until `metrics rotate` is run or a fresh handoff is
imported; `doctor` reports that as a warning, not a pass.

## Local collector foundations

`host-node-exporter-v2` is the versioned host step for the exporter primitives.
It keeps node_exporter 1.11.1 on loopback and adds:

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

The textfile directory is a foundation, not a metrics source. No eregion
self-metric is written today; a family must come from a real producer rather
than a synthetic line. The doctor check separately requires
`node_textfile_scrape_error` to be present exactly once and zero, because
`node_scrape_collector_success` can be 1 even when a `.prom` file is malformed
or unreadable.

The systemd collector reads only the named unit set over D-Bus; it does not
enumerate every host unit. `node-exporter.service` is loopback-only and has no
`wg-quick@wg0.service` ordering or requirement, so it starts independently at
boot. The dedicated telemetry Caddy waits for wg0 and binds its WireGuard
address; the access setup opens 9101 on `wg0` and restarts only
`eregion-telemetry.service` after WireGuard comes up. Public Caddy never loads
the telemetry Caddyfile.

## Read-only external scrape

The generated snippets are the supported scrape path. A host with a WireGuard
route to the node configures Prometheus or Grafana Alloy with the
`/targets.json` discovery URL and the basic-auth credential. The endpoint
listens only on the node's WireGuard address; it is not a public listener and
has no TLS terminator. Prometheus/Grafana Alloy are not installed or
supervised by eregion, and the node stores no metrics.

A future separate telemetry node or Grafana is **not prohibited long-term**.
Adding one needs its own WireGuard topology, retention and access decisions; it
is not in beta/v1 scope.

## Not full vaire parity

These primitives borrow vaire's authenticated endpoint, `targets.json`, textfile
and narrow systemd collector shape, but this is not vaire exporter parity.
Missing today:

- postgres/pgbouncer exporters and an exporter slice/memory budget;
- vaire's nginx fail2ban jail, per-IP rate limit and separate rate-limit zone
  (Caddy serves the endpoint; WireGuard peer access and basic auth are the
  boundary);
- a central Prometheus/Grafana deployment, alerting, metric/log storage or
  retained history;
- a public `https://<host>/metrics/...` endpoint or TLS terminator (the
  endpoint is WireGuard-only and basic-auth protected);
- cross-node aggregation; there is only the main node today.

None of these omissions is hidden by the `metrics show` snapshot or the doctor
rows. Host qualification of `host-telemetry-v1`, the dedicated Caddy unit, the
real systemd collector over D-Bus, a real `.prom` file and a real external
scraper remains manual acceptance.

## Verification

Isolated tests cover text-fixture parsing, missing values, the pinned
node_exporter v1.11.1 eight-mode CPU set, legacy guest-mode exclusion, counter
resets, second-scrape-only timestamps, post-response completion time,
malformed, timestamped and duplicate relevant samples, valid label
whitespace/trailing comma plus invalid grammar, label-value sanitization,
materially future exporter timestamps, two-read collection, response bounds,
node-RPC method selection, CLI plain/JSON shape and the workspace adapter
including explicit freshness, node-computed age/threshold, refresh re-query,
node mismatch and the metrics-response generation fence.

Endpoint tests cover credential generation/hash verification, the
WireGuard-only HTTP discovery document, Prometheus/Alloy config generation, the
password placeholder, owner-only `--out` writes, refusal to overwrite an
existing output file, no plaintext password in ordinary or rotation output,
local save-before-send, restoration of the previous local credential on a
failed node request, and safe endpoint text/JSON. Host tests cover the
discovery document, the dedicated Caddy unit, the Caddyfile's
basic-auth/loopback-rewrite/header-strip/admin-off shape, the firewall rule and
preservation of an existing rotation-owned Caddyfile across apply. Doctor tests
cover 401 pass, 200 exposure fail, unconfigured warning, unreachable fail and
WireGuard-down warning. They do not qualify a real server, exporter
installation, WireGuard reachability, systemd D-Bus collection, a real `.prom`
file, Caddy validation/restart, credential rotation on a live node or a real
scrape. Record those runs in [MANUAL-TESTING](MANUAL-TESTING.md); never infer
them from unit tests.
