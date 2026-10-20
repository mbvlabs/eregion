# Signed upgrades and release readiness

**Status:** M7 building blocks implemented in `internal/upgrade` and the
`eregion upgrade` command group. This is not a claim that the M7 roadmap gate or
high availability is complete. The upgrade path replaces one already-installed
binary on one host, records a journal and backups, and can roll that replacement
back. It does not restart services, drain traffic, coordinate a live
control-plane rollout, or prove failover.

## Trust model

A release is an Ed25519-signed JSON manifest. The signature covers the canonical
manifest bytes with the `signature` field empty. Canonical means Go
`encoding/json` marshaling of the `Manifest` struct: fixed struct-field order, no
insignificant whitespace, `omitempty` rules applied, and Go's default HTML
escaping. `Manifest.Sign` in `internal/upgrade` is the reference signer. The
manifest names every artifact, its exact size, and its SHA256. The CLI:

1. loads a trusted public key from `EREGION_UPGRADE_PUBLIC_KEY_FILE` (preferred)
   or `EREGION_UPGRADE_PUBLIC_KEY`;
2. verifies the detached signature before trusting any artifact URL or digest;
3. downloads or copies the artifact and verifies the exact signed size and
   SHA256;
4. checks platform, contract, node protocol, version transition, and schema
   direction;
5. only then stages and replaces the binary.

No private key is accepted by the operator CLI. The release signer reads the
private key from the `EREGION_RELEASE_SIGNING_KEY` GitHub secret or a 0600 key
file, and never writes or prints it. Public keys are not secrets but are still
not logged by the upgrade path.

### Manifest format

`eregion/release/v1` is forward-only. Unknown fields are ignored by
`encoding/json`; a future incompatible format must use a new schema name.

```json
{
  "schema": "eregion/release/v1",
  "version": "1.1.0",
  "channel": "stable",
  "published_at": "2026-10-20T00:00:00Z",
  "contract": "eregion/v1",
  "node_protocol": 1,
  "schema_version": 3,
  "upgrade_from": ["1.0.0"],
  "artifacts": [
    {
      "name": "eregion_1.1.0_linux_amd64.tar.gz",
      "target": "operator",
      "os": "linux",
      "arch": "amd64",
      "entry": "eregion",
      "url": "https://objects.example.com/eregion/eregion_1.1.0_linux_amd64.tar.gz",
      "size": 12345678,
      "sha256": "64-lowercase-hex-characters"
    }
  ],
  "signature": "base64-ed25519-signature"
}
```

Rules enforced by `Manifest.Validate` and `Manifest.Verify`:

- `schema` must be `eregion/release/v1`; `version` must be semantic;
  `published_at` must be RFC3339.
- `contract` must be `eregion/vN` and must match the running result contract.
- `node_protocol` must match the running node protocol.
- `schema_version` is the forward-only database schema the release requires.
- each artifact has a unique target/OS/arch and a unique safe file name.
- `entry` is a relative path inside the archive, with no traversal.
- `sha256` is 64 lowercase hexadecimal characters; `size` is positive and
  bounded.
- artifact URLs are absolute HTTPS URLs without credentials, fragments, opaque
  forms, loopback, or private/special IP literals.
- node artifacts are Linux-only; operator artifacts may be Linux, macOS, or
  Windows. Supported architectures are `amd64` and `arm64`.

## Download safety

`upgrade.NewDownloader` is deliberately strict:

- HTTPS only (the only exception is an unexported test switch for loopback
  fixtures); plaintext HTTP is refused.
- redirects are never followed. A 3xx response is an explicit refusal.
- proxy environment variables are ignored; the downloader dials directly.
- DNS answers are resolved before dialing and connections to loopback,
  link-local, private, carrier-grade NAT, benchmarking, documentation, and
  reserved addresses are refused.
- a host allowlist can be configured with
  `EREGION_UPGRADE_ALLOWED_HOSTS=host1,host2`; when set, no other host is
  contacted.
- manifests are limited to 1 MiB; artifacts and extracted binaries to 1 GiB.
- the signed size and SHA256 are checked while streaming to a temporary file;
  a mismatch removes the temporary file and leaves the installed binary alone.

GitHub release-asset download URLs redirect to a separate object host. Because
redirects are refused, a manifest that uses such a URL will fail closed. Publish
direct HTTPS object URLs, or use `--artifact-dir` with an operator-controlled
offline mirror. Mirror files are still checked for regular-file type, exact
size, and SHA256; symlinks and paths outside the mirror are refused.

## Compatibility checks

`upgrade.CheckCompatibility` reports each decision, not a single boolean:

| Check | Rule |
| --- | --- |
| platform | target supports the running OS; node is Linux-only |
| artifact | one signed artifact exists for target/OS/arch |
| contract | manifest contract equals the running `eregion/v1` contract |
| node protocol | manifest protocol equals the running node protocol |
| database schema | current schema may migrate forward to the manifest schema; a current schema newer than the manifest is incompatible |
| version transition | manifest version is not older than the running version; when `upgrade_from` is non-empty, the running semantic version must be listed |

A development build (`version: dev`) is not ordered against releases; the
manifest is still checked structurally, cryptographically, and for platform,
contract, node protocol, and schema direction. When the installed version is
unknown and the manifest publishes `upgrade_from`, the check fails; pass
`--current-version` with the installed target's version. The node target is
"unknown" by default because the operator process is not the node binary.

`upgrade.CheckControlPlaneCompatibility` applies the shared checks to an
operator-supplied inventory of control-plane instances. It refuses an empty
inventory and reports per-instance failures for mixed schema versions, mixed
node protocols, unsupported contract versions, and versions not listed in
`upgrade_from`. This checks the inventory, not live processes. It is not proof
of a coordinated rollout, quorum, or high availability.

### Forward-only database policy

Migrations remain explicit and append-only. An older binary never runs against
a newer schema: `system migrate` and `Ready` already fail closed when the
database is newer, migration history has a gap, or an applied checksum changed.
An upgrade may require a newer schema and apply those migrations with
`eregion system migrate`. There is no automatic down-migration. Rollback of the
binary is only safe when the restored binary supports the schema that is still
present; the manifest's `schema_version` and the compatibility report make that
visible instead of guessing. Restoring an older schema requires an explicit
database restore outside this tool.

## Commands

| Command | Behavior |
| --- | --- |
| `upgrade check` | Verify the manifest signature, compatibility, current binary hash, and print the plan diff. No download or replacement. |
| `upgrade apply --dry-run` | Same plan plus a plan-bound confirmation token. |
| `upgrade apply --confirm TOKEN` | Verify the token against a freshly computed plan, acquire the lock, recover an interrupted journal, verify the artifact, back up the current binary, write the journal, and atomically rename the staged binary into place. |
| `upgrade rollback --dry-run` | Select the journal's last backup (or `--backup`) and print the verified restore plan and token. |
| `upgrade rollback --confirm TOKEN` | Safety-copy the current binary, verify the recorded backup, write the rollback journal, and atomically restore the backup. |
| `upgrade status` | Read the binary hash, journal-confirmed installed version, process version, interrupted state, and retained backups. Never mutates. |
| `upgrade guide` | Generate the versioned agent guide (`eregion/guide/v1`) from the implemented command registry, in Markdown or JSON. |

Common flags: `--target operator|node`, `--binary`, `--state-dir`,
`--manifest`, `--artifact-dir`, `--public-key-file`, `--schema-version`,
`--current-version`, `--dry-run`, `--confirm`, `--backup`, `--format`.

`--schema-version` may be omitted when `EREGION_DATABASE_URL` is set; the CLI
then inspects pending migrations with `system migrate`'s read-only plan and
never applies DDL. `upgrade apply` always requires a plan-bound token; a token
is invalid if the binary, artifact, or state changed between the dry run and
apply.

## Files, permissions, and replacement

Default state directory:

- `EREGION_UPGRADE_STATE_DIR`, else
- `$XDG_STATE_HOME/eregion/upgrade`, else
- `~/.local/state/eregion/upgrade`.

Production services should set an explicit root-owned path. Layout per target:

```text
<state>/upgrade.lock                       exclusive replacement lock (flock on Unix)
<state>/<target>/journal.json              durable replacement journal (0600)
<state>/<target>/downloads/<sha>.artifact verified content-addressed archive (0600)
<state>/<target>/backups/<label>/<binary>  verified previous binary (0700)
<state>/<target>/backups/<label>/<binary>.json  backup metadata (0600)
```

- the state directory is forced to 0700; backups directories are 0700.
- journals and backup metadata are 0600.
- staged and installed binaries are 0755.
- the target must be an existing regular file, not a symlink or device, and must
  not be group- or world-writable. A writable baseline is refused.
- staged files are created beside the destination (same filesystem), fsynced,
  chmodded, then atomically renamed. The destination directory is fsynced.
- the previous binary is copied to the backup path and its SHA256 is compared
  before the journal is written.

`upgrade` never downloads and executes a binary, edits services, or restarts a
process. After a successful replacement, the operator or service manager must
restart the process. Rollback is explicit; it is not automatic on a crash.

## Lock, journal, and interruption recovery

The upgrade lock is an exclusive `flock` on `<state>/upgrade.lock` on Unix; the
kernel releases it when a process dies. Non-Unix builds use an exclusive lock
file and document the stale-file limitation. A second apply or rollback fails
with `conflict` instead of racing.

The journal is written **before** the atomic rename and updated after it. If a
process is interrupted, the next `upgrade apply` or `upgrade rollback`
reconciles under the lock:

| Journal status | Condition | Recovery |
| --- | --- | --- |
| `installing` | destination already matches the staged binary hash | mark `installed` |
| `installing` | staged binary still matches the recorded hash | finish the atomic rename, mark `installed` |
| `installing` | staged binary is gone and the recorded backup matches the original hash | mark `rolled_back`; the original binary remains or is restored |
| `rolling_back` | destination already matches the backup hash | mark `rolled_back` |
| `rolling_back` | the recorded source backup matches its hash | stage and finish the restore, mark `rolled_back` |
| either | nothing matches | mark `failed`, refuse to guess, and require explicit operator recovery |

`upgrade status` reports `interrupted: true` for a nonterminal journal. A
corrupt journal fails closed as `precondition_failed`; it is never reset
silently. Interrupted reconciliation can change the current binary identity, so
an apply confirmation token computed before recovery may be refused. Re-run the
dry run and apply the new plan.

## Release workflow

`.github/workflows/release.yml` runs on `v*` tags and manual dispatches. It:

1. builds `eregion` and `eregion-node` with `-trimpath` and version metadata for
   Linux, macOS, and Windows `amd64`/`arm64` (node archives are Linux-only);
2. packages one binary per `.tar.gz` with a stable entry name;
3. writes `SHA256SUMS` for integrity listing;
4. runs `internal/upgrade/releasegen`, which recomputes artifact sizes and
   SHA256, builds the manifest, signs it with
   `secrets.EREGION_RELEASE_SIGNING_KEY`, self-verifies the signature, and writes
   the public key for distribution;
5. publishes the archives, checksum listing, manifest, and public key.

The workflow fails if the signing secret is absent. It also fails unless the
repository variable `EREGION_RELEASE_BASE_URL` names a direct HTTPS object-store
base URL: GitHub release-asset URLs redirect, and the upgrade downloader refuses
redirects, so a manifest containing those URLs would not be installable. `SHA256SUMS` is **not** a
signature and must never be described as one: it is an unauthenticated
convenience listing. Authenticity comes only from the Ed25519 manifest
signature verified against the published public key.

Operators should pin the public key out of band (for example in configuration
management) and set `EREGION_UPGRADE_PUBLIC_KEY_FILE`. Rotating the key requires
publishing the new key through the same trusted channel; the CLI has no
key-discovery or key-transparency protocol.

## Release criteria

A release is publishable only when all of these hold:

- the manifest and every artifact are built from the tagged commit;
- the Ed25519 signature verifies with the published key, and an unsigned
  manifest is refused;
- every advertised artifact has a recorded size and SHA256 and is covered by
  the signature;
- supported OS/arch matrix is complete for the advertised targets;
- `upgrade check` succeeds against a database at the previous supported schema,
  and the forward-only schema plan has been reviewed;
- the contract and node protocol versions match the release;
- `upgrade_from` is set when old versions cannot upgrade directly;
- the release notes state the tested failover scope and do not claim HA;
- recovery drills below have been run against disposable test installations.

## Recovery drills

Run these on disposable hosts and test roots. Do not replace a real user binary
as part of the drill.

1. **Tampered signature:** change one manifest byte after signing. Check,
   dry-run, and apply must fail before any download; the binary must be
   unchanged.
2. **Tampered artifact:** publish a correctly signed manifest whose artifact
   bytes differ in size or SHA256. Apply must fail, leave no journal entry, and
   leave the binary unchanged.
3. **Unsupported compatibility:** use a mismatched contract, node protocol,
   OS/arch, or a newer database schema. Prepare must report the exact failed
   checks and exit `precondition_failed`.
4. **Interrupted before rename:** write the journal, stop the process, remove
   the staged file, and keep the original binary. The next apply/rollback must
   reconcile to `rolled_back` without replacing anything unexpected.
5. **Interrupted after rename:** write the journal and complete the rename but
   not the final journal update. The next apply/rollback must mark `installed`
   and keep the new binary.
6. **Interrupted rollback:** write the rollback journal, then kill the process.
   Recovery must finish from the recorded, hash-verified source backup.
7. **Lock contention:** start an apply and attempt a second apply. The second
   must exit `conflict` and change nothing.
8. **Permissions:** create a group/world-writable target binary, a symlinked
   target, or a writable trust anchor. Each must be refused.
9. **Explicit rollback:** after a successful apply, dry-run rollback, verify the
   token and diff, restore, and confirm the previous binary hash and mode.

## Limitations

- This is one-host, one-binary replacement. It does not restart, drain, or
  health-check a service.
- `CheckControlPlaneCompatibility` validates a supplied inventory. It does not
  observe live processes, hold a cluster-wide lock, or prove HA.
- Multiple workers and a shared database are not proof of HA; the roadmap gate
  remains open until the agreed failover and outage drills are actually run.
- There is no automatic rollback on process crash or failed health check.
- There is no database down-migration. Forward-only schema policy is deliberate.
- Redirecting downloads are refused, so a release mirror must serve objects
  directly or the operator must use `--artifact-dir`.
- Non-Unix lock files can outlive a crashed process; remove one only after
  confirming no upgrade is running.
- The agent guide is generated from implemented metadata. It does not advertise
  unimplemented deployment or HA capabilities.
