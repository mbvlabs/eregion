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
