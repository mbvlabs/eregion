# Node-local telemetry mechanism

The product decision is MASTER D5: every node (currently the main node) runs
node_exporter from setup on, bound to **loopback only**. Telemetry is
**node-local**: one request-scoped snapshot for the operator, and authenticated
public HTTPS metrics/discovery paths for an external scraper, backed by the
existing main hostname and public ingress. There is no central telemetry
server, Prometheus/Grafana/Loki deployment, metric or log database, external
backend, global PromQL query or retained metric history. Operation history is
PostgreSQL data and never depends on telemetry.

## Operator read path

```text
operator CLI
  -> saved workspace access: SSH in embedded userspace WireGuard
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
the public HTTPS discovery path through local Caddy with the real hostname:
401 is healthy, 200 is an unauthenticated exposure and fails, a missing public
hostname or gateway credential warns until configured, and WireGuard state does
not gate the public HTTPS observation. Neither check stores a sample, produces
history or replaces `metrics show`. The total number of doctor rows is not a
fixed contract: compare the reported summary and the named rows, not an assumed
count.

## Authenticated endpoint and discovery

Eregion adapts vaire's nginx metrics endpoint to the **existing public ingress
Caddy** plus a **dedicated loopback gateway**, separate from application route
ownership. `host-telemetry-v2` ensures
`/etc/eregion-telemetry/targets.json` (`root:caddy 0640`, no secret) and the
`eregion-telemetry.service` unit on `127.0.0.1:9101`. The gateway Caddyfile is
`/etc/eregion-telemetry/Caddyfile`, written by `metrics rotate` and by an apply
that preserves its existing hash. The dedicated instance has its own `admin
off`, Caddyfile, autosave/data directories and `StateDirectory`; public
`caddy.service` and `/etc/caddy/eregion.d/*.caddy` application routes are
separate, and telemetry never rewrites the worker's app fragment. A missing
gateway credential, a stopped gateway or an absent `wg0` therefore cannot stop
public ingress or application activation: the exact telemetry paths fail with a
bounded gateway error while every other public route keeps serving.

The root `/etc/caddy/Caddyfile` is owned by the bootstrap step
`host-ingress-v3` and imports the worker-owned application fragment. It owns
only the existing API allowlist plus two exact telemetry paths. It does not use
a `/metrics/*` catch-all:

- `/metrics/node` rewrites to the gateway's fixed `/node` path;
- `/metrics/targets.json` rewrites to the gateway's fixed `/targets.json` path.

Both proxy to the loopback gateway with an explicit upstream `Host:
127.0.0.1:9101` (Caddy would otherwise preserve the public Host) and forward the
scraper's `Authorization` header to the gateway. The gateway requires basic
authentication and strips `Authorization`/`Cookie` before calling the raw
exporter. The raw exporter remains `127.0.0.1:9100` and is never public.

The gateway unit runs the same pinned Caddy binary as `User=caddy`, waits only
for `network-online.target`, requires the gateway Caddyfile before starting and
is `WantedBy=multi-user.target`. It has no `BindsTo`, `Wants`/`After` or
`WantedBy` relationship with `wg-quick@wg0.service`, and it does not bind the
WireGuard address. `host-telemetry-v2` removes only the exact legacy
`ufw allow in on wg0 to 10.78.0.1 port 9101 proto tcp` rule when present; it
never creates that rule and never deletes a foreign rule. A removal failure is
surfaced rather than swallowed. On a hostname-less control plane the step
verifies the old gateway is both disabled and inactive, not merely stopped. If
ufw is unavailable the check reports it as unobserved rather than failing the
step. The gateway serves two basic-auth paths:

- `/node` reverse-proxies `http://127.0.0.1:9100/metrics`, rewrites the path to
  `/metrics`, and strips the scraper's `Authorization`/`Cookie` headers before
  the loopback request;
- `/targets.json` serves the HTTP service-discovery document. One entry names
  the main public hostname on port 443 with `__metrics_path__: /metrics/node`,
  `__scheme__: https`, `eregion_node`, `eregion_component: node`,
  `eregion_access: public_https` and `eregion_public: true`.

The gateway Caddyfile (`root:caddy 0640`) carries only the basic-auth username
and a bcrypt hash, never the password. TLS ends at the existing public ingress;
the gateway is HTTP on loopback. A normal apply takes the telemetry lock before
any managed write, snapshots the gateway Caddyfile, discovery document and unit
plus the gateway service's enabled/active state, validates the complete
supported managed layout and extracts its single `metrics` bcrypt hash. It then
writes the full loopback migration, validates and restarts only the gateway. A
failure restores every snapshotted file and the prior service state. Recovery
uses a bounded context detached from the cancelled request so rollback is not
aborted with the activation, while the original cancelled/deadline
classification is preserved. A malformed existing file fails closed before any
mutation and never donates a hash; a genuinely absent file remains unconfigured
until `metrics rotate` or setup configures it. The hash cost is 8, matching
vaire; the generated password has 256 bits of entropy, so cost is not the
protection.

`metrics endpoint` reads the non-secret endpoint description over node RPC:
state, public hostname, desired public HTTPS URLs, loopback gateway address,
access, basic-auth username, whether the gateway Caddyfile exists, and whether
this machine holds a local credential. A missing hostname is reported as
`unconfigured`; a managed credential whose file is not the current loopback
render is `needs_apply`; no target is reconstructed from the listener, a
request host, the WireGuard address or a parameter. The description says the
URLs are the desired ingress paths, not a claim that DNS, certificate issuance,
route activation or reachability has been observed. `metrics config` generates a
Prometheus `http_sd_configs` snippet and the equivalent Alloy `discovery.http` +
`prometheus.scrape` snippet; both use HTTPS and authenticate both discovery and
scrape, and TLS verification remains at the system trust-store default. Ordinary
output, including `--json`, always uses `<metrics-password>`. `--format
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
listener. A hostname-less control plane still installs the pinned binary for the
loopback gateway, but its public telemetry endpoint stays unconfigured and there
is no WireGuard fallback listener.

`metrics rotate --confirm <node>/metrics` follows vaire's rotate path rather
than a generation-fenced transaction. The operator generates a fresh 256-bit
password, saves it owner-only in the resolved workspace credential directory *before* the node
request, and sends only its bcrypt hash. The node takes the same node-local
telemetry lock used by apply, renders the loopback gateway Caddyfile, validates
it with `caddy validate`, restarts only `eregion-telemetry.service`, and
restores the previous Caddyfile if validation or restart fails. The worker's
PostgreSQL desired-route lock and public Caddy reload are not involved. If the
RPC itself fails, the CLI
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
one, and performs the same rotate before its doctor gate; the protected handoff
includes the public metrics/discovery URLs, username and password, and `login`
restores the local credential. `secrets export` includes the local credential
when present and reports an explicit omission when it is missing. An apply on a
node upgraded from the older WireGuard layout reads the existing managed gateway
hash and rewrites only the gateway to loopback, so the credential is preserved;
a genuinely unconfigured node has no gateway Caddyfile until `metrics rotate` is
run or a fresh handoff is imported, and `doctor` reports that as a warning, not
a pass.

## Local collector foundations

`host-node-exporter-v3` is the versioned host step for the exporter primitives.
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

The managed `eregion_node_info` gauge is produced from the configured node and
the workspace identity read from the control database. Its labels are
`eregion_node`, `eregion_workspace` and `eregion_workspace_id`; before assignment,
only the known node label is emitted. It is available to direct public scrapers
and HTTP-discovered scrapers alike. It does not rename the OS host or alter
`node_uname_info`. Database errors do not replace a previously written identity
file with guessed values. The doctor check separately requires
`node_textfile_scrape_error` to be present exactly once and zero, because
`node_scrape_collector_success` can be 1 even when a `.prom` file is malformed
or unreadable.

The systemd collector reads only the named unit set over D-Bus; it does not
enumerate every host unit. `node-exporter.service` is loopback-only and has no
`wg-quick@wg0.service` ordering or requirement, so it starts independently at
boot. The dedicated telemetry gateway is also independent of WireGuard: it
listens only on `127.0.0.1:9101`, public Caddy proxies two exact HTTPS paths to
it, and the access setup neither opens 9101 on `wg0` nor restarts the gateway.
Public Caddy never loads the gateway Caddyfile itself.

## Read-only external scrape

The generated snippets are the supported scrape path. An external Prometheus or
Grafana Alloy configures the main hostname's
`https://<hostname>/metrics/targets.json` discovery URL and basic auth, then
scrapes `https://<hostname>/metrics/node` with the same credential. Both
discovery and scrape require the credential; the raw exporter and gateway stay
private on loopback, and TLS verification uses the system trust store by
default. Public reachability, certificate issuance and authentication are
separate observations, not implied by a reload. Prometheus/Grafana Alloy are not
installed or supervised by eregion, and the node stores no metrics.

### Grafana Cloud and logical node display

Grafana Cloud's Metrics Endpoint connection can scrape the public metrics URL
directly with basic authentication. Select the stack's existing Prometheus data
source, not a new data source pointing at the exporter. A connection test is not
ingestion proof: save the job, then verify `node_uname_info` in Explore.

Node Exporter Full (dashboard 1860, revision 46) normally displays the OS
`nodename`. After the identity metric is present, a copy can display
`thelab / main` without changing the underlying hostname filter:

1. Edit the existing `nodename` variable, displayed as **Nodename**. Keep its
   name and single-value selection, and select the same Prometheus data source.
2. In the classic query editor, use:

   ```promql
   query_result(node_uname_info{job="$job"} * on(job, instance) group_left(eregion_display) label_join(eregion_node_info{job="$job"}, "eregion_display", " / ", "eregion_workspace", "eregion_node"))
   ```

   If using the **Query result** query type instead, enter the inner PromQL
   expression without the `query_result(...)` wrapper.
3. Set the variable regex to:

   ```text
   /eregion_display="(?<text>[^"]+)|nodename="(?<value>[^"]+)/g
   ```

4. Verify the preview shows the logical workspace/node as text and the original
   OS nodename as value. Leave the dependent `node` (**Instance**) variable and
   panel filters unchanged, then verify instance selection and panels.

Both metrics must share `job` and `instance`, with one current identity series
per pair. Missing identity samples or duplicate series make this join unsuitable;
restore the original variable rather than inventing labels. This customization
follows documented named text/value captures; it is not a claim that the stock
dashboard or a particular live Grafana instance was automatically changed.

A future separate telemetry node or Grafana is **not prohibited long-term**.
Adding one needs its own WireGuard topology, retention and access decisions; it
is not in beta/v1 scope.

## Not full vaire parity

These primitives borrow vaire's authenticated endpoint, `targets.json`, textfile
and narrow systemd collector shape, but this is not vaire exporter parity.
Missing today:

- postgres/pgbouncer exporters and an exporter slice/memory budget;
- vaire's nginx fail2ban jail, optional source-CIDR allowlist and per-IP rate
  limit with a separate rate-limit zone. Standard Caddy basic authentication
  protects the public HTTPS paths, but this track does not add a custom proxy, a
  mandatory allowlist or rate limiting; do not claim full vaire nginx parity;
- a central Prometheus/Grafana deployment, alerting, metric/log storage or
  retained history;
- cross-node aggregation; there is only the main node today.

The endpoint is no longer WireGuard-only: `https://<hostname>/metrics/node` and
`/metrics/targets.json` are public HTTPS paths behind basic authentication. They
expose no public administration API, and the raw exporter remains private.
None of these omissions is hidden by the `metrics show` snapshot or the doctor
rows. The public HTTPS gateway integration replaces the earlier private-only
endpoint; the service-boundary handoff is recorded separately from manual host
qualification. A real `.prom` fixture and a separately deployed Prometheus/Alloy
scraper remain manual acceptance.

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

Endpoint tests cover credential generation/hash verification, public HTTPS
discovery that never derives a target from the loopback listener or WireGuard
address, HTTPS discovery and scrape basic auth in both Prometheus and Alloy,
TLS verification defaults, the password placeholder, owner-only `--out` writes,
refusal to overwrite an existing output file, no plaintext password in ordinary
or rotation output, local save-before-send, restoration of the previous local
credential on a failed node request, and safe endpoint text/JSON. Host tests
cover the exact public ingress paths and explicit gateway Host, the loopback
gateway unit with no WireGuard dependency, the Caddyfile's
basic-auth/loopback-rewrite/header-strip/admin-off shape, removal of only the
exact legacy firewall rule, preservation of an existing hash on the loopback
render, fail-closed malformed config and root-only rollback. Doctor tests cover
public HTTPS 401 pass, 200 exposure fail, unconfigured hostname warning,
unreachable fail and malformed config fail; WireGuard state no longer gates the
public endpoint. They do not qualify a real server, exporter installation,
public DNS/TLS, systemd D-Bus collection, a real `.prom` file, Caddy
validation/restart, credential rotation on a live node or a real scrape. Record
those runs in [MANUAL-TESTING](MANUAL-TESTING.md); never infer them from unit
tests.
