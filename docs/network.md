# Routes, DNS and private access

Mechanism reference; [MASTER](MASTER.md) owns scope and status. Three different
things must not be conflated: **domain binding**, **Caddy route**, **DNS record**.

## Current environment domains

```sh
eregion domain add hello production hello.example.com --dry-run --json
eregion domain add hello production hello.example.com --idempotency-key ops:hello-domain-1 --json
eregion domain list --app hello --environment production --json
eregion domain remove hello production hello.example.com --idempotency-key ops:hello-domain-remove-1 --json
```

These are current command examples, not proof a full setup works. App and env
must already exist, migrations through 010 must be applied, and Caddy must have
been installed by setup with a public main hostname. Only local/main-node
environments can receive these domain bindings.

Current path:

1. Submission transaction inserts/deletes `eregion_platform_domains`, one unique
   hostname per environment binding, along with operation/job acceptance.
2. Worker `internal/controlplane/routes.go` joins all desired domain rows to env
   stable ports and renders `internal/platform/routes.go` site blocks:
   `hostname { reverse_proxy 127.0.0.1:<host_port> }`.
3. Worker writes `/etc/caddy/eregion.d/routes.caddy`; Caddy loads the importing
   `/etc/caddy/Caddyfile` through loopback admin port 2019.
4. On reload rejection the prior file is restored. The DB intent is not reverted.
   On acceptance the operation completes; the CLI may show/check the A record.

**Limits that upcoming implementation must address:** the table stores no applied
revision, last error, TLS state or observation time. List reads desired bindings,
not observed Caddy state. The worker's file write is not atomic or globally
serialized; domain work is not a journaled node step and has no general startup/
periodic route reconciliation. A reload failure can leave DB/file/proxy disagreeing.
The domain command's success message does not prove DNS, TLS or a healthy public
app. Dry-run validation is not a provider/backend probe.

The main hostname's Caddy site exposes **only** `/readyz` and
`/v1/webhooks/github`, forwarding to loopback. The per-environment CI deploy
endpoint and per-environment webhook path (MASTER section 6.7, D28) are not
wired yet. Never expose the whole admin API
as a workaround. Caddy's admin endpoint must stay local; access to it is
administrative authority.

The target (MASTER section 6.5, D17): a primary hostname plus aliases per
environment; desired, applied and observed route state with DNS, certificate
and public response reported separately; serialized atomic writes;
reconciliation on start and retry; Caddy backends switched between candidate and
serving containers on dynamic loopback ports.

## Cloudflare subsystem

Off the v1 surface: Cloudflare DNS automation comes after v1 (MASTER D26); the
alpha uses manual DNS with resolver checks.

`dns plan/reconcile/list` are separate actions/inventory. They do not install a
Caddy route or bind an environment domain. Configure workers/API with:

| Variable | Purpose |
| --- | --- |
| `EREGION_CLOUDFLARE_ZONES` | Comma-separated explicit `zoneID=zoneName` pairs; no guessing. |
| `EREGION_CLOUDFLARE_TOKEN_FILE` | Absolute root-owned 0600 regular non-symlink token file. |
| `EREGION_DNS_ADMINS` | Actor-subject allowlist for reconciliation; falls back to access admins if empty. |

Use a token restricted to the intended zone with Zone Read and DNS Edit, never a
Global API Key. Actions carry a file reference, not the secret value. Desired
records support A/AAAA/CNAME/TXT (max 200), zone-contained names, TTL 1 or
60–86400, and no proxied TXT. No wildcard/unmanaged adoption. Only ownership
comments starting `eregion:managed` may be mutated. Conflicting unmanaged or
duplicate managed records fail closed; empty desired sets cannot delete all.

Authorization/audit precede mutation; audit failure stops it. A post-mutation
audit error does not imply nothing happened. Reconcile re-reads provider state
on retry. Plan normalizes input; it does **not** fetch a live Cloudflare diff.
Fake-provider tests are not real-account/DNS-propagation evidence.

## Private-access subsystem

Off the v1 surface: private-network access modes come after v1 (MASTER D22).

`access devices` groups observed grants; it is not device/VPN enrollment.
`access grants grant/list/revoke` manages exact subject/device/route grants with
expiry and durable audit in PostgreSQL. Route matching is exact, not prefix or
wildcard. Creation is keyed to operation identity; revoke is idempotent.

`EREGION_ACCESS_ADMINS` enables the explicit administrator allowlist. Missing
store/audit/controller fails closed. The optional `/private/` handler uses a
verified identity and server-resolved route; missing/expired/revoked grant denies
access, and audit failure also denies. Proxy rewriting strips bearer credentials,
cookies, proxy auth and caller-supplied forwarding headers.

These HTTP grants are not vaire-style operator authentication, WireGuard device
provisioning, deploycrate resource-network grants, or Cloudflare Zero Trust.
Do not claim any of those from this grant model.
