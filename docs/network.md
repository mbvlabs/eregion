# Network: Cloudflare DNS and private access

**Status:** M4 implementation notes · 2026-10-20  
**Scope:** Cloudflare DNS reconciliation and private-access device grants. Both
use the generic platform action path: one durable `eregion_operations` row and
one River job per accepted mutation, with the same event cursor, cancellation,
and idempotency rules as deployments.

## Cloudflare DNS

### API token and configuration

Create a Cloudflare API token with the minimum scope for one zone:

- **Permissions:** `Zone → Zone → Read` and `Zone → DNS → Edit`.
- **Zone resources:** include only the specific zone (or the smallest set of
  zones you reconcile).
- Do not use a Global API Key. Do not grant account-wide DNS edit unless the
  deployment genuinely manages multiple zones.

Store the token in an absolute root-owned `0600` regular file, for example
`/run/secrets/cloudflare-token`. The file must not be a symlink, must not be
group/world-readable, and must be 20–512 bytes after trimming. The token value is
never accepted on a command line, in action input, in operation rows, in River
arguments, or in logs; only its **path** travels in `platform.dns.reconcile`
input.

Zone identity is explicit:

```text
--zone-id   32 lowercase hexadecimal characters
--zone-name example.com (lowercase, no trailing dot)
```

### Serve configuration

The API/worker process needs an explicit zone allowlist before `dns list` or
`dns reconcile` can run:

| Environment | Meaning |
| --- | --- |
| `EREGION_CLOUDFLARE_ZONES` | comma-separated `zoneID=zoneName` pairs (either side may be the 32-character ID) |
| `EREGION_CLOUDFLARE_TOKEN_FILE` | absolute root-owned `0600` API-token file used by the read-only inventory |
| `EREGION_DNS_ADMINS` | comma-separated actor subjects allowed to reconcile DNS |
| `EREGION_ACCESS_ADMINS` | private-access administrators; also the DNS-admin fallback when `EREGION_DNS_ADMINS` is empty |

Example:

```text
EREGION_CLOUDFLARE_ZONES=0123456789abcdef0123456789abcdef=example.com
EREGION_CLOUDFLARE_TOKEN_FILE=/run/secrets/cloudflare-token
EREGION_DNS_ADMINS=ops@example.com
```

A zone name or ID alone is refused: the service does not resolve or guess the
missing half from the network. The configured pairs become the DNS inventory and
the allowlist for `platform.dns.reconcile`; a reconcile naming another zone fails
with `precondition_failed` before the token is read or Cloudflare is contacted.
If no zones are configured, both `dns list` and `dns reconcile` fail closed with
an `input_required` configuration error rather than returning an empty or
arbitrary zone.

DNS authorization is independent of private-access administration:
`EREGION_DNS_ADMINS` controls only DNS reconciliation, while
`EREGION_ACCESS_ADMINS` controls grants and the private proxy. When
`EREGION_DNS_ADMINS` is unset, the access list is reused for compatibility.
Both paths use the durable PostgreSQL audit sink.

### Desired records

Describe desired records as JSON objects. The CLI accepts one `--record` per
record or one `--records-file` containing a JSON array:

```json
{"name":"app.example.com","type":"A","content":"203.0.113.10","ttl":300,"proxied":true,"comment":"api"}
```

Supported types are `A`, `AAAA`, `CNAME`, and `TXT`, at most 200 records per
reconciliation. Names must equal the zone or be inside it. `@` means the zone
apex. `ttl` is `1` (automatic) or 60–86400. `TXT` cannot be proxied. Duplicate
name/type pairs are rejected. There is no wildcard and no unmanaged-record
adoption.

### Commands

```text
eregion dns plan --zone-id <id> --zone-name example.com \
  --token-file /run/secrets/cloudflare-token \
  --record '{"name":"app.example.com","type":"A","content":"203.0.113.10"}' --json

eregion dns reconcile --zone-id <id> --zone-name example.com \
  --token-file /run/secrets/cloudflare-token \
  --record '{"name":"app.example.com","type":"A","content":"203.0.113.10"}' \
  --idempotency-key ci:dns-2026-10-20.1 --json

eregion dns list --zone example.com --json
```

- `dns plan` is the API dry run. It normalizes and validates the request without
  inserting an operation or job. The plan does **not** call Cloudflare or claim
  to show the current diff; use `dns list` for observed routes.
- `dns reconcile` is the real submission. The same authenticated actor, kind,
  target, and idempotency key returns the original operation; the same key with
  different input is a conflict.
- `dns list` reads the read-only `dns` inventory collection. The API process must
  be configured with `integrations.RuntimeConfig.DNSZones` (zone ID, name, and
  token-file reference); the CLI does not read the token locally.

A timed-out submission is ambiguous. Reuse the same key; reconciliation re-reads
Cloudflare and converges, but never blindly create a second operation.

### Ownership and safety

- Only records whose comment starts with `eregion:managed` are created, updated,
  or deleted. The operation identity is recorded in the comment as
  `eregion:managed op=op_...`, making a retried create/update identifiable.
- An unmanaged record on a desired name/type is a `precondition_failed`; eregion
  never overwrites it.
- More than one managed record for one name/type is a precondition failure.
- Deleting a managed record requires it to be absent from the desired set. An
  empty desired set is refused rather than interpreted as "delete everything".
- Every planned mutation is authorized and audited before the first HTTP
  mutation. Audit intent, success, failure, and denial entries carry the
  authenticated actor subject and operation ID.
- If the audit sink fails, the action fails closed before the mutation. If a
  mutation succeeded but its post-audit failed, the operation fails and says not
  to assume the remaining changes were applied.
- `dns list` is a read. It performs no mutation and writes no audit entry.

## Private access

### Model

A grant is an exact, auditable permission for one authenticated subject and one
private device to reach a set of exact routes:

- Routes are lowercase host names, host/path values, or CIDR blocks. They cannot
  contain a scheme, backslash, wildcard, control character, or leading/trailing
  whitespace.
- Each route is matched exactly; there is no wildcard or prefix match.
- A grant expires (default 24 hours, bounded by the configured maximum, default
  30 days) and may carry a short note.
- The grant ID is derived from the durable operation ID, so a retry after a lost
  acknowledgement returns the original grant instead of creating a second one.
- Revocation records the revoking actor and operation. Repeating the same revoke
  is a no-op; a different revoker does not rewrite history.
- The store can be the PostgreSQL implementation (`NewPostgresGrantStore`) for a
  shared control plane, or the file implementation for a single-process
  deployment. The in-memory store is test-only.

### Authorization and audit

Grant and revoke require an `AccessController` with an explicit admin allowlist,
a persistent `GrantStore`, and a durable `AuditSink`. A missing controller,
store, or audit sink fails closed with `unavailable`. The actor is resolved by
the API from the bearer credential, never from the request body.

The enforcement middleware (`AccessController.Middleware`) is mounted in front of
a private handler after authentication. It accepts only an identity from a
verified resolver and an exact route from a server-side resolver. Missing
identity is `401`, an unresolvable route is `400`, a missing/expired/revoked
grant is `403`, and an unavailable store or audit sink is `503`. The audit
decision is part of the decision: if it cannot be recorded, access is denied.

### Commands

```text
eregion access grants grant --subject alice --device laptop-1 \
  --route app.internal/api --route 10.0.0.0/8 \
  --expires-at 2026-11-20T12:00:00Z --note "incident drill" \
  --idempotency-key ci:access-alice-1 --json

eregion access grants list --subject alice --json
eregion access devices --json
eregion access grants revoke --grant-id grant_... \
  --idempotency-key ci:access-revoke-1 --json
```

All three commands accept `--dry-run` where they submit an action. `grant` and
`revoke` are mutations with dry-run support; `list` and `devices` are reads of the
`access` inventory and perform no work.

`access devices` groups observed grants by device and shows active routes. It is
an inventory view, not a WireGuard enrollment or a live tunnel status.

## What is tested, and what is not

### Tested with a fake Cloudflare API

The integration tests start a loopback `httptest` Cloudflare-compatible endpoint
and exercise the real `Cloudflare` client:

- list, create, update, and delete requests for managed records;
- no-op convergence when the desired state already matches;
- unmanaged-record conflict refusal with zero mutations;
- authorization denial before mutation;
- audit failure before mutation and audit failure after a mutation;
- the token never appearing in canonical action input or the result;
- DNS inventory reads from configured zones (including the token file path
  handling) without a live Cloudflare account;
- a reconcile for an unconfigured zone is refused before the token is read or
  the fake Cloudflare endpoint is contacted, and an unconfigured control plane
  fails `dns list`/`dns reconcile` closed;
- `EREGION_CLOUDFLARE_ZONES` parsing (both `=` and `:` separators, either side
  as the ID), duplicate/invalid entries, and the required token-file reference.

The fake also exercises the same `platform.Registry` normalization and executor
path used by the API and worker, including the operation identity in ownership
comments.

### Not tested against real Cloudflare

This worktree does **not** hold live Cloudflare credentials, so the following are
honest gaps:

- real token-scope validation and Cloudflare's exact error responses;
- real rate-limit behavior, network timeouts, and retry timing;
- live zone pagination beyond the fake's `result_info` shape;
- DNS propagation, proxied-record behavior, and Cloudflare's own validation of
  proxied CNAME targets;
- real account-level token restrictions and IP allowlists.

The request shape, redirect refusal, response-size bound, authorization/audit
ordering, ownership rule, and idempotent re-read are tested; upstream
compatibility is not claimed beyond those contracts.

### Tested for private access

- admin allowlist denial, exact-route authorization, expiry, and revocation;
- grant and revoke replay from the same operation ID;
- audit failure denying a grant and denying an authorization decision;
- grant listing (including revoked records) and the inventory shape;
- PostgreSQL grant and audit stores are exercised by the opt-in control-plane
  integration suite when `EREGION_TEST_DATABASE_URL` is set. File-store behavior
  is covered by its unit tests.

### Not tested here

- real WireGuard device enrollment, address allocation, or tunnel enforcement;
  the middleware and grant model are implemented, but multi-node private
  connectivity remains a later increment;
- a live access proxy or Caddy route integration;
- Cloudflare Access, Zero Trust, or account-wide policy interaction.
