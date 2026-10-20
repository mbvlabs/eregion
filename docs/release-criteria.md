# Release criteria — v0.1 (experimental)

**Version:** v0.1  
**Status:** experimental. These are gates for a tagged v0.1 release, not a
claim that eregion is production-ready or highly available.  
**Rule:** publish only guarantees that were actually exercised. An unchecked or
failed gate blocks the release; it is never waived by prose.

## Gate summary

| Gate | Required evidence | Status in this tree |
| --- | --- | --- |
| G1 Formatting and static checks | `just fmt-check`, `go vet ./...` | Runnable locally; must be recorded for the tag |
| G2 Unit/contract/schema tests | `go test ./...`, examples tests, script tests | Runnable locally; must be recorded for the tag |
| G3 PostgreSQL/River integration | isolated test DB, `go test -count=1 ./internal/controlplane -run TestIntegration` | Requires a disposable database; not run in a plain checkout |
| G4 Signed release artifacts | `upgrade verify` with the published key; manifest/artifact hashes | Requires the release workflow and signing secret |
| G5 Upgrade and rollback drills | recovery drills in [upgrades.md](upgrades.md#recovery-drills) | Must be run on disposable hosts; not proven by unit tests |
| G6 Terminal compatibility | `just terminal-smoke` under the TERM matrix | Requires `just dev-up`; see below |
| G7 Security review | [security-review.md](security-review.md), findings triaged | Review performed; S1–S4 remain open |
| G8 Fresh-VM install | documented matrix on clean Debian/Ubuntu VMs | **Unproven here** |
| G9 Real Cloudflare | real zone/token, DNS and private-access path | **Unproven here** |
| G10 Multi-host HA/failover | drilled loss/outage scenarios | **Unproven here** |

## G1 — Formatting and static checks

```sh
just fmt-check
go vet ./...
go -C examples/hello vet ./...
```

Pass criteria: no `gofmt -l cmd internal schema examples` output; vet exits 0.
Record the commit SHA and toolchain (`go version`) with the evidence.

## G2 — Unit, contract, schema, and script tests

```sh
go test ./...
go -C examples/hello test ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
```

Pass criteria: all packages pass with no skipped release-blocking test. The
upgrade suite must include the compatibility, health-probe restore, and
interruption-recovery tests:

```sh
go test ./internal/upgrade ./internal/cli -run 'Upgrade|Rollout|Health|Guide' -count=1
```

## G3 — PostgreSQL, River, and process integration

Use a disposable server with a role that can `CREATEDB`:

```sh
export EREGION_TEST_DATABASE_URL='postgres://test:test@127.0.0.1:55432/test?sslmode=disable'
go test -count=1 ./internal/controlplane -run TestIntegration -timeout 600s
```

Pass criteria: the suite creates isolated databases and passes. Do not run this
against a shared application database. If CGO is unavailable, omit `-race`; the
release record must say which variant ran.

## G4 — Signed release artifacts

The release workflow must have:

1. built every advertised artifact from the tagged commit with `-trimpath` and
   version metadata;
2. produced a manifest whose artifacts have exact sizes and SHA256 values;
3. signed the manifest with the release Ed25519 key and self-verified it;
4. published the manifest, public key, archives, and (unauthenticated)
   `SHA256SUMS`.

Verify from a trusted install:

```sh
EREGION_UPGRADE_PUBLIC_KEY_FILE=/etc/eregion/release.pub \
  eregion upgrade verify --manifest manifest.json --json
```

Then run `upgrade check`/`apply --dry-run` against a database at the previous
supported schema:

```sh
eregion upgrade check --manifest manifest.json \
  --target operator --schema-version N --json
eregion upgrade apply --manifest manifest.json \
  --target operator --schema-version N --dry-run --json
```

Pass criteria: signature verifies; every artifact is covered; contract, node
protocol, and forward-only schema direction pass; an unsigned or tampered
manifest is refused before any download.

A GitHub release-asset URL redirects. Because the downloader refuses redirects,
the manifest must use direct HTTPS object URLs or the operator must use a
verified offline mirror (`--artifact-dir`).

## G5 — Upgrade and recovery drills

Run every drill in [upgrades.md](upgrades.md#recovery-drills) on disposable
hosts and test roots:

1. tampered signature;
2. tampered artifact (size and SHA256);
3. unsupported contract, node protocol, platform, or newer database schema;
4. interruption before the rename;
5. interruption after the rename;
6. interruption during rollback;
7. lock contention;
8. unsafe target/backup permissions or symlinks;
9. explicit rollback;
10. failed health probe after apply (the new binary must be restored
    automatically and the journal must record the failure).

Pass criteria: each drill leaves the expected binary, journal state, and
backup; no drill silently replaces a binary or guesses an outcome.

## G6 — Terminal compatibility

Requires a running development environment:

```sh
just dev-up
just dev-status          # must report ready
just terminal-smoke      # runs the TERM matrix
```

The matrix is `TERM=dumb`, `xterm-256color`, `screen-256color`, `NO_COLOR`,
ASCII, reduced motion, CJK/emoji labels, and 20×6/300×80 sizes. Golden and
program tests run without a dev environment:

```sh
go test ./internal/tui ./internal/viewer -count=1
```

Pass criteria: detach exits 130 without cancelling the operation; the operation
completes and replays; no terminal escape renders as a control sequence; ASCII
and no-color modes stay readable; wide labels fit the declared width.

**If `just dev-status` is not ready, record the gate as not run.** A skipped
terminal drill is not a pass.

## G7 — Security review

Review [security-review.md](security-review.md). Before tagging:

- every Medium/High finding is fixed or has a written, scoped acceptance with an
  owner and a release-blocking decision;
- every open Low finding has a documented mitigation or is accepted for the
  experimental line;
- no plaintext secret appears in an event, River argument, log, or node journal;
- host-key pinning, webhook HMAC, vault route authorization, and command
  construction are re-checked against the tagged commit.

At the time of this document S1 (Medium, git build SSRF) is open, and S2–S4
(Low) are open. v0.1 may ship only if those are explicitly accepted in the
release record; they must not be described as fixed.

## G8 — Fresh-VM install matrix (explicitly unproven)

The install document describes the intended flow. This tree does **not** contain
evidence of a full install on a clean VM matrix. Before claiming support,
exercise at least:

| Host | Role | Evidence to record |
| --- | --- | --- |
| Debian 12 `amd64` | `main` | bootstrap plan/diff/apply, `version`, `/readyz`, first deploy and rollback |
| Ubuntu 24.04 `amd64` | `main` | same as above |
| Debian 12 `arm64` | `application` | node install, SSH pin, node check, operation on the node |
| Ubuntu 24.04 `arm64` | `builder` | node install, Docker/Buildpacks availability, a directory/git build |

A fresh-VM run must start without a developer checkout, without `just`, and
without ambient environment variables. Until that exists, do not claim
"supported on Debian/Ubuntu" beyond the development drills in this repository.

## G9 — Real Cloudflare (explicitly unproven)

The repository tests the integration policy and fake upstream behavior. It does
**not** prove:

- a real zone, token scope, DNS record, or proxied hostname;
- the private-access path through a real Cloudflare Tunnel/access policy;
- WAF, rate-limit, or TLS configuration;
- token rotation and revocation against the real API.

Do not publish "Cloudflare supported" as a tested guarantee. The honest
statement is: the client and policy checks exist; a real-account drill remains
open.

## G10 — Multi-host HA and failover (explicitly unproven)

Multiple workers sharing PostgreSQL, or a second control-plane process, are not
by themselves high availability. The following are **unproven** in this tree:

- control-plane process loss with a load balancer and in-flight submissions;
- worker loss while a node effect is running, including remote acknowledgement
  loss;
- PostgreSQL primary outage, failover, reconnect, and advisory-lock behavior;
- competing operations on different control-plane processes;
- node loss and recovery during a deploy;
- schema migration under a live mixed-version control plane;
- backup/restore of the database plus vault key.

The upgrade plan's `migrate → workers → API → nodes` order is a documented
procedure, not evidence that a live rollout was coordinated. Inventory checks
read operator-supplied facts, not live processes.

## HA statement (required release-note text)

Use exactly this scope, filling in only what was drilled:

> v0.1 is an experimental single-control-plane release. Tested: durable
> operations and River jobs in PostgreSQL, local and pinned-SSH node execution,
> process-crash reconciliation on one host, explicit upgrade/rollback with
> journal recovery, and the terminal/API behavior covered by the gates above.
> Not tested: multi-host failover, PostgreSQL primary failover, real Cloudflare,
> and a fresh-VM install matrix. Multiple workers are not a high-availability
> guarantee.

If a failover drill is later completed, add one sentence per tested scenario
with the date, topology, and observed recovery time. Never generalize from a
single-host drill to a multi-host HA claim.

## Release record template

```text
Version: v0.1.0
Commit: <sha>
Toolchain: <go version>
Date: <UTC>
G1: pass|fail  evidence: <command output reference>
G2: pass|fail  evidence:
G3: pass|fail|not run  evidence:
G4: pass|fail|not run  evidence:
G5: pass|fail|not run  evidence:
G6: pass|fail|not run  evidence:
G7: pass|scoped acceptance  findings: S1..Sn
G8: not run (unproven)
G9: not run (unproven)
G10: not run (unproven)
HA statement: <the paragraph above, unedited except for tested items>
Known limitations: <links to upgrades.md#limitations and security-review.md>
```
