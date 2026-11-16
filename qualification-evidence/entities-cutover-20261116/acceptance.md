# Entities centralization acceptance — 2026-11-16

## Verdict

Accepted for local master integration of the entities persistence cutover.
This is not RC1 readiness, release publication, or live-node qualification.
The checkpoint includes the existing beta.8 functional repairs on which this
cutover was built. Unrelated operator documentation/tooling changes are not
part of the commit.

```text
CLI / application services / workers / vault
  -> entities.Store / StoreTx
  -> private generated queries
  -> the existing PostgreSQL pool
```

Application-owned catalog, host observation and migration-inspection SQL is
also centralized. Goose and River retain their own library SQL. Goose catalog
reads use a private database/sql generation target on the existing
repeatable-read transaction, not another persistence store or pool.
All three private query targets are covered by `just sqlc-generate` and
`just sqlc-diff`. No application migration or Go dependency was changed.

## Review and regressions

Independent infrastructure and domain reviews found no remaining blockers.
The review identified three transaction escapes, all repaired:

- Artifact preview now uses its environment snapshot's image repository.
- Build stop/retry admission uses transaction-bound build reads.
- Managed-domain admission uses transaction-bound provider metadata.

The preview regression failed before the fix and passed afterward.
Real PostgreSQL single-connection tests cover all three paths. Additional
production-caller tests cover extension composition, domain/job/event atomicity,
replay/conflict behavior, and webhook-secret rollback. Application-boundary
resource validation and deployability evaluation retain explicit DNS and
artifact facts.

## Frozen-source verification

571 Go/SQL files; before/after manifests are identical:

`42db565456af06c5bc9041e4e98207c4f1f05390945c16a72b5b7f7adbe3aaec`

The manifest is SHA-256 of file hashes ordered by `LC_ALL=C` path sorting.
The primary independently checked every recorded hash and reproduced it.

- Normal: **88 top-level tests + 61 subtests passed**, no failures/skips.
- Race: **88 top-level tests + 61 subtests passed**, no failures/skips/races.
- Build, vet, all-package test compilation, formatting, all three sqlc diff
  checks, and staged/unstaged whitespace checks passed.
- Independent tester used disposable containerized PostgreSQL 17.11 on
  loopback port 36002. Leftover test databases/connections: **0 / 0**.

The exact test regex is in both final logs. It excludes simulated-server,
scripted-node and other fixtures outside the current integration boundary.
This is targeted cutover verification, not a broad `just test` pass.

## Remaining qualification limits

`just lint` still exits 1 under the operator-owned configuration. The global
lint backlog has not been cleared. This completion did not change lint
configuration or add suppressions. Do not claim a green lint or `just check`
gate.

No live installation, provider mutation, push, tag or release was performed.
RC1 host acceptance and broader qualification remain separate.
