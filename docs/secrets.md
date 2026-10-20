# Revisioned encrypted secrets

Status: M3 building block. The vault package, authenticated HTTP adapter, and CLI
commands are implemented. The control plane still has to apply the provided DDL
as its next explicit migration and mount the handler. Revisioned secrets are not
part of `docs/platform-contract.md` action input; values never enter an operation
input, River argument, event, or node payload.

## Guarantees

- PostgreSQL stores only AES-256-GCM ciphertext and secret-free metadata.
- The 256-bit master key lives in a protected regular file outside PostgreSQL,
  never in a database row, environment variable, operation row, or log.
- Each ciphertext carries an envelope version and key ID. The authenticated
  header lets a key rotation preserve old revisions.
- The authenticated associated data binds the ciphertext to the exact
  `app/environment/name` namespace and immutable revision. Moving ciphertext
  between secrets or revisions fails authentication.
- Every revision is immutable. Rotation appends `latest+1` under a compare-and-
  swap guard; archival keeps all revisions.
- Resolution returns plaintext only in memory. It is designed for transient
  `node.Request.Secrets` injection and is explicitly excluded from JSON.
- API and CLI output contain metadata only. `secret export` writes one explicit
  protected destination file and never prints a value.

## Storage schema

`internal/vault/schema.sql` is the complete DDL. `vault.SchemaSQL` embeds it for
the migration owner. The package never runs DDL automatically.

```sql
eregion_secrets(app, environment, name, latest_revision, state, ...)
eregion_secret_revisions(app, environment, name, revision,
                         key_id, ciphertext, ciphertext_sha256,
                         revision_hash, ...)
```

The control plane should add it as migration `005_revisioned_secrets.sql` after
the existing platform migration and add the normal migration checksum. Suggested
hook in `internal/controlplane/database.go`:

```go
//go:embed migrations/005_revisioned_secrets.sql
var migration005 string

var migrations = []migration{
    // existing migrations...
    {version: 5, name: "005_revisioned_secrets.sql", sql: migration005},
}
```

Copy `internal/vault/schema.sql` into the migration file; do not edit an already
applied migration.

## Master key file

Format `eregion/vault-keyring/v1`:

```json
{
  "format": "eregion/vault-keyring/v1",
  "current": "k_main",
  "keys": [
    {
      "id": "k_main",
      "key": "<base64 of exactly 32 random bytes>",
      "created_at": "2026-10-20T00:00:00Z"
    }
  ]
}
```

Rules enforced by `vault.LoadKeyRing`:

- absolute path, regular file, no symlinks;
- mode `0600` or stricter (`0400` is accepted, group/other bits are rejected);
- at most 64 KiB and at most 16 keys;
- unique key IDs matching `^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$`;
- the `current` key must exist in `keys`.

Provisioning example (run as the control-plane service owner):

```sh
umask 077
install -d -m 0700 /etc/eregion
key="$(openssl rand -base64 32)"
created="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '{\n  "format": "eregion/vault-keyring/v1",\n  "current": "k_main",\n  "keys": [{"id":"k_main","key":"%s","created_at":"%s"}]\n}\n' \
  "$key" "$created" > /etc/eregion/master-key.json
chmod 0600 /etc/eregion/master-key.json
unset key
```

`vault.GenerateKeyRing` and `vault.WriteKeyRing` are the programmatic equivalents
used by tests and setup tools. The service loads the ring once at startup; a key
change requires a restart.

## Key rotation policy

1. Append a new key entry to the protected file and make it `current`.
2. Keep every old key until no stored revision references it. Ciphertext stores
   the key ID used to seal it, so old revisions remain readable.
3. Restart the control plane. New and rotated revisions seal with the new key.
4. Archival does not re-encrypt. If a key must be removed, first rotate each
   value to a new revision under the new key (or run a future explicit rewrap
   tool), verify resolution, then remove the old key.
5. `KeyRing.CheckRotation(now, 90*24*time.Hour)` reports a stale current key. The
   recommended policy is rotation at least every 90 days and immediately after
   suspected exposure.

The key file must be included in the operator's protected backup/restore plan.
Losing a key makes its revisions unrecoverable; exposing the key plus a database
backup exposes those revisions.

## HTTP API

Mount the exported handler after authentication, for example outside the generic
API router:

```go
vaultService, err := vault.NewService(pool, keyRing)
if err != nil {
    return err
}
vaultHandler := api.NewVaultHandler(vaultService, api.AuthVaultAuthorizer(auth))
baseHandler := api.NewHandler(auth, service, ready)

handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path == "/v1/secrets" || strings.HasPrefix(r.URL.Path, "/v1/secrets/") {
        vaultHandler.ServeHTTP(w, r)
        return
    }
    baseHandler.ServeHTTP(w, r)
})
```

`api.AuthVaultAuthorizer` requires `operations:read` for reads and
`operations:submit` for mutations, then enforces an explicit action grant for the
exact secret kind and environment target:

| Capability | Action kind | Target |
| --- | --- | --- |
| create/rotate | `platform.secret.set` | `env/<app>/<environment>` |
| list/get metadata | `platform.secret.list` | `env/<app>/<environment>` |
| archive | `platform.secret.archive` | `env/<app>/<environment>` |
| export plaintext | `platform.secret.export` | `env/<app>/<environment>` |

A grant covers every secret name in that environment. `env/<app>/*` wildcards
the environment segment under the existing grant grammar. Operator and system
principals with no `action_grants` retain administrative access; CI and webhook
principals without grants cannot read or mutate secrets even when they hold
`operations:submit` and `operations:read`. The service performs the grant check
only after the namespace is validated, so the target is never taken from an
unauthenticated payload.

If an external authenticator supplies the actor, use `api.NewVaultAccess(actor,
authorize)`; the callback must enforce the same kind/target grants. Do not pass
an always-allow callback merely because the request already passed scope
authentication. Identity always comes from authentication. A request body
containing `actor` or any other unknown field is rejected.

Every vault route requires TLS or a literal loopback client address. Plaintext
HTTP to a non-loopback peer is refused with `precondition_failed`.

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/secrets` | Create (`expected_revision: 0`) or rotate one revision |
| `POST` | `/v1/secrets/archive` | Archive under a revision CAS guard |
| `GET` | `/v1/secrets?app=&environment=&limit=` | Secret-free metadata page |
| `GET` | `/v1/secrets/{app}/{environment}/{name}` | Secret-free metadata |
| `GET` | `/v1/secrets/{app}/{environment}/{name}/export` | Plaintext for a protected destination |

Create/rotate body:

```json
{
  "app": "api",
  "environment": "production",
  "name": "DATABASE_URL",
  "value_base64": "cG9zdGdyZXM6Ly8...",
  "expected_revision": 0
}
```

The server decodes, validates, encrypts, and commits in one short transaction
before returning metadata. The plaintext byte slice is zeroed after the call.
No operation is submitted, so there is no Submit input containing plaintext.
This is a deliberate separate credential endpoint.

If secret changes must become durable operation events later, the core should
add a built-in action whose normalizer receives only namespace plus expected
revision, and whose worker obtains the plaintext from the already-encrypted row.
Do not add a plaintext field to `platform.ActionInput`. The exact integration
hook is `vault.Store.Set(ctx, tx, namespace, plaintext, expectedRevision, actor)`:
the core can call it inside its own transaction and record the non-secret
`vault.RevisionRef.Hash` in its domain event. Until then, mount the handler above.

## CLI

The CLI never opens PostgreSQL and never has the master key. It uses the
authenticated API client in `internal/api/vault.go`.

```sh
# Plaintext is read from an absolute 0600 regular file, never a flag.
eregion secret set --app api --environment production --name DATABASE_URL \
  --file /run/secrets/database-url --expected-revision 0 --json

# Rotate from the current revision.
eregion secret set --app api --environment production --name DATABASE_URL \
  --file /run/secrets/database-url --expected-revision 3 --json

# Metadata only; values are never listed.
eregion secret list --app api --environment production --json

# Archival is a CAS-guarded transition; values remain in immutable revisions.
eregion secret archive --app api --environment production --name DATABASE_URL \
  --expected-revision 4 --json

# Writes a new 0600 file and never prints the value.
eregion secret export --app api --environment production --name DATABASE_URL \
  --dest /run/secrets/database-url --json
```

`secret set` rejects symlinks, non-regular files, group/other-readable files, an
empty value, NUL bytes, and values over 32768 bytes. It does not trim a trailing
newline. `secret export` refuses to overwrite any existing path, creates the
destination with `O_EXCL` and mode `0600`, syncs it, and removes it on failure.
The destination path is not a secret, but its contents are.

The CLI owner merges `a.secretSpecs()` into the main command tree. The methods live
in `internal/cli/secret.go`; `EREGION_API_URL` and `EREGION_API_TOKEN` select the
authenticated API.

## Resolution and node injection

`vault.Store.Resolve` (and the package-level `vault.Resolve`) return a
`vault.Resolution`:

```go
resolution, err := store.Resolve(ctx, actor, "api", "production", vault.RevisionMap{
    "DATABASE_URL": 3,
    "STRIPE_KEY":   1,
})
if err != nil {
    return err
}
registry := &secrets.Registry{}
resolution.Register(registry) // before any log/event/error

request.Secrets = resolution.Env() // transient; node.Request only
// Persist resolution.Revisions and resolution.Digest, never Values.
```

Semantics:

- `nil` revision map: resolve every active latest revision for the environment
  (bootstrap/discovery only).
- empty non-nil map: resolve nothing (explicitly secret-free).
- explicit map `name -> revision`: resolve exactly those immutable revisions and
  fail if any name/revision is missing or archived. Use this for deployments so a
  later rotation cannot change an in-flight rollback.

Limits: 128 secrets per environment, 32768 bytes per value, 256 KiB total
plaintext per resolution. A resolution includes:

```go
type Resolution struct {
    Values    map[string]string      `json:"-"`  // plaintext; never serialize
    Revisions map[string]RevisionRef `json:"revisions"`
    Digest    string                 `json:"digest"`
}
```

`RevisionRef.Hash` is a SHA-256 binding of namespace, revision, key ID, and
ciphertext (never plaintext). It is safe in deployment rows, operation events,
and logs. `Resolution.Digest` binds the sorted name/revision set and is safe to
record.

Injection rule: set `node.Request.Secrets` immediately before transport. Never
put values in `node.Request.Payload`, an operation JSON input, a River argument,
an event, a node journal, or a process argument list. The node transport already
registers request secrets with a per-request redaction registry before events or
errors, and the node task keeps them out of its journal. The platform container
step maps the injected values to `docker run -e KEY` (value read from the child
process environment), so values do not appear in the container command line.

## Validation and threat controls

- App and environment: `^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`.
- Secret name: `^[A-Z][A-Z0-9_]{0,127}$`; it becomes an environment key.
- Value: 1–32768 bytes, no NUL, empty rejected.
- Revision: positive `bigint`; CAS conflicts return `conflict` with expected and
  current revisions, never a value.
- Nonce: fresh 96-bit random nonce for every seal; AES-GCM authentication tag is
  128 bits.
- Ciphertext: stored with SHA-256 and a namespace/revision binding hash. Reads
  verify both before opening the AEAD, then verify the key ID.
- Redaction: values are registered with the per-request `secrets.Registry` before
  any event/error. `Resolution` marshals with `Values` omitted.
- Files: key and input files require owner-only permissions and are opened after
  an `Lstat`/`Fstat` identity check. Export destinations are create-only `0600`.
- API: bounded 64 KiB JSON body, strict fields, no caching, no redirects, TLS or
  loopback only for plaintext-bearing routes, and fixed non-reflective errors.
- Database: only ciphertext, key ID, hashes, namespace, revision, actor, and
  timestamps. No plaintext digest or searchable value index.

## Tests

`internal/vault/vault_test.go` covers AEAD round trip and every-byte tamper,
namespace/revision AAD isolation, nonce uniqueness, key rotation decryption,
key-file permissions/symlinks, revision-hash binding, CAS revision decisions,
redaction, size/name validation, and the exported schema.

`internal/api/vault_test.go` covers authentication, plaintext-transport refusal,
body-identity rejection, metadata-only list/set responses, export, CAS conflict,
and client helpers. `internal/cli/secret_test.go` covers protected input files,
no-value stdout/JSON, export destination mode and no-overwrite behavior, and
metadata-only list/archive. No test requires a live PostgreSQL server; the SQL
path is exercised by the control-plane integration suite once migration 005 is
applied.
