# Environment secrets

Current mechanism reference. The target model is MASTER D18 and D27: an
environment revision names its secrets, each deploy resolves their current
values and records the exact revisions, and `secret export` is the only
plaintext read, behind the human factor.

## Storage and service wiring

`internal/cli/extensions.go` builds the vault when `EREGION_VAULT_KEY_FILE` is set;
`internal/api/mux.go` mounts its authenticated `/v1/secrets` handler. `server
init` generates the keyring at `/etc/eregion/vault-keyring.json`, wires
`EREGION_VAULT_KEY_FILE` into `control-plane.env`, and includes the ring in the
secrets handoff. Root creates it and hands it to the unprivileged control-plane
account (mode 0600), because the API and worker must read it at startup; an
existing ring is verified, never overwritten.

PostgreSQL holds AES-256-GCM ciphertext, key ID, namespace/revision bindings and
metadata. The master key stays outside PostgreSQL in an absolute, owner-only,
regular non-symlink file. Format `eregion/vault-keyring/v1`:

```json
{"format":"eregion/vault-keyring/v1","current":"k_main","keys":[{"id":"k_main","key":"BASE64_OF_32_RANDOM_BYTES","created_at":"2026-10-26T00:00:00Z"}]}
```

This is a shape example, not a usable key. Generate securely with the vault
helpers or a secret manager; never commit keys. File limits: 64 KiB, up to 16
unique keys, 0600/0400. Services load it at startup; restart after changing it.
New current keys seal new revisions; retain all old keys needed by historical
revisions. Archival does not re-encrypt data. Rotation of a current value does
not make its old revisions dispensable. Back up the keyring separately and
securely: DB restore without it cannot decrypt secrets.

## Commands and semantics

```sh
eregion secret set --app api --environment production --name DATABASE_URL --file /run/secrets/database-url --expected-revision 0 --json
eregion secret list --app api --environment production --json
eregion secret archive --app api --environment production --name DATABASE_URL --expected-revision 1 --json
eregion secret export --app api --environment production --name DATABASE_URL --dest /run/secrets/exported-url --json
```

Set reads an absolute protected file, creates/rotates under compare-and-swap and
returns metadata, not a value. Export creates a new explicit 0600 file with
`O_EXCL`; it never overwrites or prints plaintext. Values are 1–32768 bytes, no
NUL; trailing newlines are not trimmed. Secret names match
`^[A-Z][A-Z0-9_]{0,127}$`. Revisions are immutable; archival retains them.

Vault writes are direct credential transactions, not ordinary queued actions
containing plaintext. Each route requires appropriate transport scope and
`platform.secret.set/list/archive/export` authorization for
`env/<app>/<environment>`. A scope alone does not grant CI secret access. Plaintext
transport requires TLS or a literal loopback peer; export must remain explicit.

## Deployment injection is separate from storage

Today an environment holds pinned `secret_refs`, for example
`{"DATABASE_URL":"vault:api/production/DATABASE_URL@1"}`, settable only through
the raw action API at creation. D18 replaces them: the revision names secrets,
the deploy resolves the current revisions and records them on the deployment.

`vault.EnvironmentResolver` resolves exact revisions at deployment time.
Transient values travel in `node.Request.Secrets` and to the workload environment;
persist only reference/revision hashes. They must not enter operation input,
River arguments, node payload/journal, logs or CLI output. Missing configured
resolver/revision must fail, never silently deploy without the secret.

Setting or rotating a value does not redeploy, as in deploycrate; the next deploy
picks it up. Namespace rows are not platform app/env FKs; app and env deletion
(D13) must remove them deliberately.
Docker/root administrators can inspect running container environment; this is
not a multi-tenant isolation boundary.
