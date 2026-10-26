# Routes, DNS and private access

Mechanism reference; [MASTER](MASTER.md) owns scope and status. Three different
things must not be conflated: **domain binding**, **Caddy route**, **DNS record**.

## Environment domains

```sh
eregion domain add hello production hello.example.com --dry-run --json
eregion domain add hello production hello.example.com --primary --json
eregion domain list --app hello --environment production --json
eregion domain remove hello production hello.example.com --dry-run --json
eregion domain remove hello production hello.example.com --confirm <token> --json
```

App and env must already exist, migrations through 013 must be applied, and Caddy
must have been installed by setup with a public main hostname. Only local/main-node
environments can receive these domain bindings. `domain add` is idempotent for the
same binding; `--primary` promotes it and demotes the current primary. Every
environment has at most one primary (partial unique index). Removing the primary is
refused: promote another hostname first.

The binding, route, DNS record, certificate and public response are separate:

| Fact | Where it lives | Meaning |
| --- | --- | --- |
| Binding | `eregion_platform_domains` row | Desired hostname, environment and primary flag. Accepted intent only. |
| Route | `desired_revision`/`applied_revision`, `route_state`, `route_error` | Caddy accepted and loaded a routes file containing that domain. A pending/failed row does not delete the binding. |
| DNS | `expected_addresses`, `dns_state` (`pending`/`ok`/`wrong`/`proxied`), `dns_addresses`, `dns_error` | 1.1.1.1 and 8.8.8.8 answers over UDP compared with the main node's public address. Proxied or wrong never becomes ready. |
| Certificate | `certificate_state` (`pending`/`valid`/`failed`), `certificate_not_after`, `certificate_error` | TLS handshake to the hostname through local Caddy with SNI, chain and name verified against system roots. |
| Public response | `public_state` (`pending`/`ok`/`failed`), `public_status`, `public_error` | HTTP response through local Caddy with the real hostname, using the environment revision's health path. |

`domain add` prints the exact A/AAAA record from the current context's WireGuard
endpoint (the main node's public address) and records it as `expected_addresses`.
The node does not infer a public address from its interfaces. `domain list` shows
route, DNS, certificate, public and overall readiness separately.

Route path:

1. The submission transaction inserts/deletes `eregion_platform_domains` and
   records the operation/job, together.
2. The worker takes one PostgreSQL advisory lock for the routes file, joins all
   desired domain rows to each environment's serving deployment web replicas, and
   renders `/etc/caddy/eregion.d/routes.caddy` (`hostname { reverse_proxy … }`).
   A deployment switch uses the same lock and writer with candidate upstreams.
3. The new fragment is adapted with Caddy's `/adapt` endpoint before it replaces
   the file. The file is written to a sibling temp file, fsynced and renamed; the
   importing `/etc/caddy/Caddyfile` is then loaded through loopback admin port
   2019.
4. A rejected load restores the prior routes file and reloads it. The database
   binding is never reverted; the route revision and last error record the failed
   attempt, so `domain list` distinguishes desired from applied.
5. A maintenance job re-renders on worker start and every 30 seconds. A failed
   reload therefore retries without another domain command. Concurrent domain
   changes keep every unrelated route because each write renders the complete
   current table under the lock.

A second periodic job observes DNS, certificate and public response separately
(every five minutes and on worker start). Observations never change the binding or
the route; a later run overwrites them. The command's success message proves an
accepted operation, not DNS, TLS or a healthy app; only `domain list` readiness
aggregates all four observations.

The main hostname's Caddy site exposes **only** `/readyz` and
`/v1/webhooks/github`, forwarding to loopback. The per-environment CI deploy
endpoint and per-environment webhook path (MASTER section 6.7, D28) are not
wired yet. Never expose the whole admin API
as a workaround. Caddy's admin endpoint must stay local; access to it is
administrative authority.

This is MASTER section 6.5/D3/D17's route path: a primary hostname plus aliases,
desired/applied/observed state, serialized atomic writes, startup and retry
reconciliation, and Caddy backends switched between candidate and serving
containers on dynamic loopback ports.

## Cloudflare and private access (off the surface)

Cloudflare DNS automation comes after v1 (MASTER D26); the alpha uses manual DNS
with resolver checks. The `dns plan/reconcile/list` commands and the
`resources`/`backups` inventory are not registered. `internal/integrations`
keeps the DNS action and its explicit `zoneID=zoneName` configuration for the
after-v1 phase; it never installs a Caddy route or binds an environment domain.

Private-network access modes come after v1 (MASTER D22); the `access …` commands
are not registered. The internal grant controller remains for the optional
`/private/` proxy. Missing store/audit/controller fails closed; the proxy uses a
verified identity and server-resolved exact route, and audit failure denies.
These HTTP grants are not vaire-style operator authentication, WireGuard device
provisioning, deploycrate resource-network grants, or Cloudflare Zero Trust.
