# Entities cutover final targeted verification (2026-11-16)

## Scope and verdict

**PASS for the exact frozen-target scope.** Final normal and `-race` runs of the
73 prefix-selected integration tests plus the 15 anchored clean production
caller tests passed on the corrected frozen source. No FAIL, SKIP or DATA RACE
was observed. No source or test was edited by this verification.

This is targeted integration evidence, not `just test` / broad-suite
qualification. The root test boundary remains as recorded in
`entities-domain-cutover-prerequisites-20261116.md`.

## Target

| Fact | Value |
|---|---|
| Repository / workdir | `/home/mbv/work/eregion` |
| HEAD | `35cb3b4bfce8` |
| PostgreSQL | 17.11, existing disposable container at `127.0.0.1:36002` |
| DSN used explicitly | `postgres://postgres@127.0.0.1:36002/postgres?sslmode=disable` |
| Source manifest before | `42db565456af06c5bc9041e4e98207c4f1f05390945c16a72b5b7f7adbe3aaec` (571 files) |
| Source manifest after | same fingerprint, 571 files, byte-identical |
| Whitespace correction | verified restaged; `git diff --cached --check` exit 0 before execution |

## Selection

One anchored combined regex:

- 73 top-level tests matching `^TestIntegration(Entities|Cutover|Goose|Migration|RiverPartial|RiverGeneration|ReadinessRound)`.
- 15 anchored existing production tests: 5 workspace, 5 operation-follow, 3
  database-policy admission/retention, 1 retention repository, 1 extension
  grant retry.
- The 3 definition policy tests with the inert scripted transport were
  deliberately **not** selected.

Total: **88 top-level tests**.

New file `internal/controlplane/entities_cutover_integration_test.go` was
inspected before execution. All 6 tests are `TestIntegrationCutover*`:
no `httptest`, HTTP server, `node.Transport`, `os/exec`, Docker/Pack script or
node emulation. They exercise real `Service`/`Runtime`/PostgreSQL, use
`beforeSubmissionCommit` failure injection and maxConns=1 pools, and the
managed-domain admission path stores a real provider credential without any
external resolution call. This matches the worker report of 6 real
service/runtime/PG tests with no fake external resolution or transport.

## Final run results

| Run | Top-level | Subtests | PASS | FAIL | SKIP | DATA RACE | Exit |
|---|---:|---:|---:|---:|---:|---:|---:|
| Normal (`-count=1 -v`) | 88 | 61 | 88 + 61 | 0 | 0 | n/a | 0 |
| `-race -count=1 -v` | 88 | 61 | 88 + 61 | 0 | 0 | 0 | 0 |

Normal duration: 32.387s. Race duration: 37.975s. Both logs contain
`PASS` / `ok github.com/mbvlabs/eregion/internal/controlplane` and
`EXIT_STATUS=0`.

Logs:

- `entities-cutover-final-normal-20261116.log`
- `entities-cutover-final-race-20261116.log`

## Static and generated-code checks

| Check | Result | Evidence |
|---|---|---|
| `go build ./...` | exit 0 | `entities-cutover-final-build-20261116.log` |
| `go vet ./...` | exit 0 | `entities-cutover-final-vet-20261116.log` |
| `go test ./... -run '^$' -count=1` | exit 0 | `entities-cutover-final-test-compile-20261116.log` |
| `just sqlc-diff` (all 3 configs) | exit 0 | `entities-cutover-final-sqlc-diff-20261116.log` |
| `gofmt -l cmd internal schema examples` | empty output, exit 0 | `entities-cutover-final-gofmt-20261116.log` |
| `git diff --check` | exit 0 | `entities-cutover-final-diffcheck-20261116.log` |
| `git diff --cached --check` | exit 0 | same log |

## Cleanup / active state

- Test databases matching `eregion_test_%` on the explicit 36002 server: **0**.
- Test connections matching `eregion_test_%`: **0**.
- No `controlplane.test` process from this verification remained.
- The existing container was not created, stopped or modified.
- One unrelated broad `^TestIntegration` process remains visible on the host
  (PID 3453038, launched from `/tmp/opencode/eregion-check` against a different
  explicit DSN at 36001). It is not this verification and was left untouched.
- No unrelated database was inspected; cleanup queries were limited to
  `eregion_test_%` names and connections on the explicit 36002 target.
