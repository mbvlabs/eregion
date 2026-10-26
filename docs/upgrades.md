# Signed releases and binary replacement

Reference for the current code. The target is vaire's model (MASTER D14–D15):
edge on every push to `master`, the `1.x` line with `v1.0.0-alpha.N` tags, cosign
keyless signatures with a pinned workflow identity, `install.sh` and channel
manifests, and `upgrade` (CLI only) followed by `apply --auto-upgrade-nodes`.
The Ed25519 scheme below is replaced by work list row 10.

## Trust and distribution

`internal/upgrade` uses an **Ed25519-signed** `eregion/release/v1` manifest, not
vaire/deploycrate's Sigstore scheme. It names version/channel/time, contract,
node protocol, required DB schema, supported upgrade sources and artifact
target/OS/arch/entry/URL/size/SHA-256. `Manifest.Sign` is the canonical signer:
Go JSON struct serialization with the signature field empty. Strict loading
refuses unknown fields/trailing JSON. Never hand-edit canonical signed content.

Pin a trusted public key out of band using `EREGION_UPGRADE_PUBLIC_KEY_FILE`
(or the supported public-key value configuration). A key downloaded beside an
artifact is not independently trusted. `SHA256SUMS` alone is not authentication.
Verify the manifest, then signed artifact size/hash, before executing a binary.
A first install needs a separately trusted verifier/key; there is no documented
trustworthy shortcut that starts by executing the unverified downloaded CLI.

Downloads use direct HTTPS without redirects/proxy environment, with response
bounds and DNS/address safety checks. GitHub asset URLs redirect and are refused;
use direct signed object URLs or `--artifact-dir` offline mirror. Mirror files
still require regular-file/containment/size/hash checks. An optional
`EREGION_UPGRADE_ALLOWED_HOSTS` allowlist restricts destinations.

## Existing commands

| Command | Current behavior |
| --- | --- |
| `upgrade verify` | Signature/structure only; no install or compatibility claim. |
| `upgrade check` | Verify manifest, platform/contract/protocol/schema/version compatibility and current binary facts. |
| `upgrade plan` | Document migrate -> workers -> API -> nodes order; optional supplied inventory checks, not live rollout. |
| `upgrade apply --dry-run` | Replacement plan and intent-bound confirmation token. |
| `upgrade apply --confirm …` | Lock, reconcile journal, verify/stage artifact, save previous binary, replace atomically and health-probe. |
| `upgrade rollback --dry-run/--confirm …` | Plan/restore a recorded hash-verified backup, never an arbitrary path. |
| `upgrade status` | Binary/process/journal version facts, backups and interrupted state. |
| `guide`, `upgrade guide` | Guide generated from current command metadata, not a complete-feature guarantee. |

Common inputs include target (`operator`/`node`), binary/state directory,
manifest/public key, artifact mirror, current/schema version and health policy.
Inspect current help; do not infer vaire's `upgrade --yes` or automatic node
upgrade flags exist here.

Compatibility checks enforce target OS/arch, envelope contract, node protocol,
schema direction and version transition. A dev version is not evidence of a
published release. Schema is currently embedded through 010, not the example
version 3 from older manifests. Live database inspection must supply the actual
current version rather than assuming it.

## Replacement/recovery guarantees and limits

State defaults under the OS state directory (`eregion/upgrade`), overridden by
`EREGION_UPGRADE_STATE_DIR`. Production paths must be deliberate and protected:
directories 0700, journal/metadata 0600, installed/staged binary 0755. Refuse
symlink/nonregular/group-world-writable targets. Per-target journals/backups record
hashes; staging/rename and directory sync are atomic on the destination filesystem.
The exclusive upgrade lock prevents concurrent replacement.

After install, the new binary's version must match the signed manifest. When
configured with a DB URL, the probe also checks read-only migration readiness
against the manifest schema. A failed probe restores the recorded previous
binary. `--health-check=false` is an explicit opt-out, not normal qualification.

Interrupted installing/verifying/rollback journals are reconciled under the lock
on the **next** apply/rollback; unknown/corrupt state fails closed. Unconfirmed
verification is not promoted to a successful install. Recompute dry-run approval
after recovery changes target state. No background process watches it afterward.

This replaces **one binary on one host**. It executes the new binary for its
probe, but does not restart/drain the running services, confirm their application
health, drive a fleet or guarantee failover. Restoring a binary does not roll back
DB migrations. Migrations are forward-only; an old binary incompatible with the
current DB cannot be made safe by calling binary rollback.

## Release workflow

`.github/workflows/release.yml` builds on `v*` tags or explicit existing-tag
dispatch. Current artifact matrix includes Linux/macOS/Windows amd64/arm64;
Linux archives also supply node targets. This build matrix is broader than the
decided Linux/macOS operator product and does not qualify macOS `server init`.

The workflow builds versioned archives, runs source checks, generates/signs the
manifest, uploads to configured S3-compatible storage and publishes convenience
GitHub assets. Required configuration: signing secret
`EREGION_RELEASE_SIGNING_KEY`, variables `EREGION_RELEASE_BASE_URL` and
`EREGION_RELEASE_S3_URI`; optional endpoint/region, OIDC role or AWS credential
secrets use the `EREGION_RELEASE_*` names in the workflow. It refuses missing
signing/destination configuration. Inspect upload ordering before claiming
atomic manifest-last publication; the current file loop is not that guarantee.

Manual upgrade qualification includes tampered signature/artifact refusal,
incompatible schema/protocol, unsafe file permissions, interrupted replacement/
rollback, lock contention, failed health-probe restoration and explicit rollback.
Record running-service results separately from binary replacement results.
