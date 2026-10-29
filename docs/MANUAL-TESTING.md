# Manual testing

Acceptance checks for the v1 alpha and the record of real-server runs.
[MASTER](MASTER.md) owns scope, decisions (D-numbers below) and order; this file
says what must work on a real server and what has been observed.

## Evidence rules

- Record the build/commit, date, operator OS/arch, server provider, OS/arch and
  topology, and the outcome. A run from a checkout is not a published-binary run.
- Use **pass**, **fail**, **blocked** (missing implementation) or **not run**.
  A skipped or absent check is never a pass.
- A submission is acceptance only. Keep the operation ID, wait for its terminal
  state, inspect serving and route state, and check the external response.
- Never store tokens, DSNs, passwords, handoff plaintext or passphrases, or
  secret values in evidence. IDs, versions, sanitized errors and response
  markers suffice.
- An existing server is not disposable. Get authorization before mutating a
  host, DNS, a database or a release.

## Developer checks

```sh
just fmt-check
go build ./...
go vet ./...
GOOS=darwin GOARCH=arm64 go build ./...
go -C examples/hello vet ./...
go -C examples/hello test ./...
```

Add isolated unit/regression tests for changed logic. Inspect tests before a
broad run: `just check` runs `go test ./...`, and some existing tests start
Docker fixtures. PostgreSQL integration tests need an explicit
`EREGION_TEST_DATABASE_URL` with CREATEDB and create/drop databases; never point
them at `EREGION_DATABASE_URL` or a shared database. Record them as not run unless
explicitly selected. The `postgres`, `ssh-fixture`, `transport` and `wireguard`
recipes are legacy fixtures, not the qualification policy.

## Alpha baseline — one main node

Run on an authorized fresh server (Debian 13 or Ubuntu 26.04) with a Go test app
that prints its version, a second commit of it, and a commit whose health check
fails. Install the CLI from the published edge or alpha channel.

### Server and operator

| Check | Pass means |
| --- | --- |
| Install | `install.sh` installs a signed CLI on Linux and on macOS; `upgrade` moves between two edge builds. |
| Preflight | Unsupported OS/arch, a missing credential or an unreachable host is refused before any change, with the fix. |
| Init plan | The plan names every change; declining starts nothing; `--json` without `--confirm <server>` refuses instead of prompting. |
| Init apply | Fresh server ends hardened, SSH on WireGuard only, Caddy with a valid certificate for the hostname, node_exporter reachable only over WireGuard, services ready, and the final doctor gate reports `doctor: N ok` before the handoff. A forced failing check stops before the handoff with `doctor_failed` and the fix commands; `--resume` after the fix completes without repeating earlier phases, and `--skip-doctor` ends with a warning. |
| Lockdown revert | Breaking confirmation during the SSH switchover reopens public SSH by itself; `server init --resume` completes. |
| Interrupted init | Interrupting before and after the database step and before the handoff resumes with `--resume` without lockout or duplicate identity; `--discard` drops a pending init. |
| Handoff | Plain secrets plus bundle and passphrase; init waits for "stored"; no API token in it; `--secrets-out` writes a file only when asked. |
| Login | `login` with the bundle on a second machine reaches the same server; a wrong host key fails closed; `context remove` deletes the context. |
| Atomic login | An unreachable import leaves existing context, identity and known_hosts byte-identical; an existing name requires exact `--replace NAME`; successful replacement proves readiness before swapping identity. |
| Context selection | With current context a, `--context b` and separately `EREGION_CONTEXT=b` target b for RPCs, domain/app-create DNS addresses and protected handoff identity fields; unknown names fail with the list hint. Init ready/doctor stay pinned to the initialized context even when the variable names a different server. |
| Version lockstep | A CLI newer than the node refuses with `node_version_mismatch` before any change; `apply --auto-upgrade-nodes` upgrades the node; a second `apply` is a no-op; `doctor` stays clean. |
| Dry-run version mismatch | `--dry-run --auto-upgrade-nodes` returns `node_version_mismatch` without upload/install/restart, including from macOS or a cross-architecture dev CLI; the node build stays unchanged. |
| Signed node upgrade | A published node refuses an unsigned/mismatching upload and a dev CLI; a matching signed release artifact installs. Failed manifest retrieval never falls back to checksum-only trust. |
| SSH command | `ssh --command '…'` preserves remote stdout and exit status without a PTY when piped; sudo's password refusal shows `input_required` and its fix in plain stderr or the JSON failure envelope. |
| Break-glass | `node unlock-ssh` from the provider console reopens public SSH for its window. |
| Operator tools | `server status` and `doctor` report the server's state; `eregion ssh` opens a shell over WireGuard. |
| Secrets export | `secrets export` and `secret export` need the human factor; `--yes`, `--json` and stdin cannot bypass it. |

### Application workflow

| Check | Pass means |
| --- | --- |
| App create | Guided `app create` with a private git repo, a `web` and a `worker` process, config, a secret and a domain follows the create and its first deploy to a serving card with the public URL (or a failed operation with its error); Ctrl-C detaches without cancelling and names `eregion operations follow <id>`. The flag-only form produces the same revision, and JSON returns both operation IDs without waiting. No SQL, raw JSON or manual Caddy edits. |
| Deployability | An environment missing source, domain or DNS reports exactly what is missing and refuses to deploy. |
| Domain and DNS | `domain add` shows the exact record; wrong DNS stays pending, never ready; a Cloudflare-proxied record becomes ready once the origin serves it; the certificate verifies without `-k`; a second alias and the primary both serve. |
| Alias check path | An alias added with `domain add … --check-path /users/sign-in` reaches `public=ok` while `/api/health` returns 404 on that host; the deploy public check names the host, path and status in its events. |
| Second deploy | A new commit, a changed config value and a rotated secret reach a new release; continuous requests during the switch see no failures (D17); old containers stop afterwards. |
| Failed candidate | The failing commit leaves the previous release serving and its operation failed, not rewritten. |
| Rollback | `rollback --confirm` restores the previous release with the secret revisions it used (D18); the public response matches. |
| Environment changes | `env update` and `config set` commit a new revision without deploying; the next deploy applies it; `restart` restarts the serving containers. |
| Monorepo build | Git source `context_path=apps/web` builds from that directory; missing paths and symlink escapes fail with the configured path named, never fall back to root. |
| Moved-branch retry | Submit a branch build, record its exact commit, move the branch, then retry the failed build: only the recorded commit is rebuilt. Same-key accepted replay needs no remote git access. |
| Build lifecycle | `build logs` streams a running build; `build stop` stops it; `build retry` starts a new build of the same commit. |
| Processes | `process scale web 2` runs two replicas behind Caddy; the worker runs without a port; logs show both. |
| Release command scope | A release command runs once on deploy; `process scale` and `rollback` skip the succeeded command and run no command container; changing the command runs it again on the next deploy; restart, scale and rollback refuse when its run has not succeeded. |
| Release command recovery | After a lost command response leaves the run unresolved, making the original operation terminal and deploying again recovers the completed command as succeeded without running it a second time; running, absent or unreachable original effects keep the original owner and defer. Missing/mismatched inputs hashes and status-query failures also preserve ownership and dispatch no command. |
| Leaked-container retry | A forced previous/candidate-container removal failure creates a named cleanup row visible in `env status` without failing a verified traffic switch; removing the obstruction lets maintenance remove the container and row. |
| Container repair | Removing a serving web container and restarting the worker queues one `platform.restart`, the container returns, and a second worker start with nothing missing queues no operation. A competing deploy or changed serving-deployment ID causes stale repair to skip/refuse, never restore an old release. |
| Create replay | Retrying `app create` and `env create` with the original idempotency key returns the same create and first-deploy IDs without another vault revision; without a key an existing name conflicts. |
| JSON contracts | `domain add --dry-run --json` returns one object with preview and DNS records under `data`; `logs --json` is one envelope, `--output ndjson` is the explicit streaming choice, and `--json --follow` refuses with that fix. |
| Detach and retry | Ctrl-C detaches without cancelling; `operations follow` replays; the same submission key returns the original operation; `cancel` is explicit. |
| Restart | Restarting services, the worker and Caddy keeps serving state and routes; reconciliation completes without duplicate effects. |
| Route failure | A forced Caddy reload failure shows desired, applied and failed state separately and recovers on retry; concurrent domain changes keep unrelated routes. |
| Status and logs | `app status`/`env status` show the serving release, processes, domains with DNS/route/TLS/public state and recent operations; `logs` works with no telemetry node. |
| CI | The per-environment endpoint deploys a ref with a deploy token; expired, revoked and wrong-environment tokens fail; nothing else of the API is reachable publicly. |
| Push to deploy | With auto-deploy on, a push to the bound branch deploys that commit; GitHub's redeliver creates no second deployment; a push to another branch and a wrongly signed request do nothing; after `--rotate-webhook-secret` the old secret fails. |
| Token factor | `token create` needs the TOTP code; `--yes`, `--json` and stdin cannot mint one; the secret is shown once and `token list` shows metadata only. |
| Factor-gated removal | Real `app delete`/`env delete` need exact-target confirmation and a TOTP code at a human terminal; JSON/yes/pipes cannot perform them. Direct node submission without a valid same-request factor and all real HTTP delete submissions refuse; dry-run preview stays agent-usable. |
| Removal | `domain remove`, `env delete --confirm` and `app delete --confirm` remove containers, routes and secrets per D13; other apps keep serving; operation history remains. |

## Next run — publish edge and upgrade the existing test server

Candidate implementation commit: `ed6e4f3`. Use the test server already set up
for this work; do **not** rerun `server init`, reset its database, or discard
existing applications. Run fresh-init checks separately only on an explicitly
authorized fresh target. No step below is recorded as passed yet.

### 1. Publish the candidate

1. Complete the repository-publication secret review before changing visibility.
   Visibility changes and any credential rotation are operator actions, not
   implied by this procedure.
2. Verify remote `master` contains the candidate; push only if needed and with
   explicit authorization. The 2026-10-28 read-only check found remote `master`
   already at `ed6e4f3`, with its edge workflow failed and no successful candidate
   publication verified. After visibility/billing is resolved, explicitly rerun
   that failed edge workflow (or authorize a new publish). Wait for success and
   verify that the signed edge manifest identifies the intended commit. An older
   edge build does not qualify these changes.
3. Record the actual published version and commit in the run record below.
   Neither this source commit nor a locally built `dev` binary is published-build
   evidence. Do not create an alpha tag yet.

### 2. Upgrade the CLI and test server

Replace `<test-context>` with the existing saved test-server context. Confirm
that it addresses the intended server before running the mutating apply.

```sh
eregion context show --name <test-context> --json
eregion upgrade --channel edge --yes
eregion version --json
eregion --context <test-context> apply --auto-upgrade-nodes
eregion --context <test-context> doctor
eregion --context <test-context> server status --json
```

`upgrade` replaces the local CLI only. `apply --auto-upgrade-nodes` installs the
matching verified server binary and converges host steps, including pending
control-database migrations. This mutates the test server and its configured
external database; confirm both targets are authorized. Stop on an apply or
doctor failure rather than running app tests on a partially upgraded host.
Require matching candidate CLI/node builds, schema **16/16**, current River
migrations and ready services with no unexplained doctor failures. Preserve
sanitized output and errors, never credentials.

### 3. Exercise the disposable application

Use a dedicated test app, test repository and authorized domain; preserve any
existing application that is not explicitly disposable. Use the application
baseline table above as the acceptance checklist, in this order:

1. Guided `app create`: web plus worker, config, secret and domain; follow through
   the first build/deploy to a working HTTPS URL. Record both operation IDs.
2. Deploy a second commit while continuously requesting the public URL; verify
   the new version and no failed requests during the switch.
3. Deploy a deliberately failing-health commit; require nonzero CLI exit and
   failed operation state while the previous release keeps serving.
4. Roll back with exact-target confirmation; verify the previous version and its
   recorded secret revisions at the public URL.
5. Scale web to two replicas and restart; verify both backends and that the
   already-succeeded release command is not executed again.

### 4. Qualify the remaining fixes

- Builds: monorepo context path and retry of the original commit after a branch
  has moved; accepted same-key replay must not depend on the git remote.
- Domains: alias-specific check path, primary public-check evidence and separate
  desired/applied/DNS/TLS/public states.
- Recovery: deliberately fail cleanup, remove the obstruction and observe retry;
  remove one disposable app's serving container and verify worker repair without
  reverting a competing deployment. Authorize each fault injection first.
- Operator contracts: failed atomic login replacement, selected-context routing,
  `ssh --command`, JSON/NDJSON shape, create replay and Ctrl-C detach.
- Safety: dry-run leaves the node binary unchanged; real exports/deletes require
  the same-request factor. Destructive checks affect only the disposable app.
- Init doctor/resume and fresh-host hardening remain separate qualification
  requirements; upgrading this existing server cannot establish them.

### 5. Record and resolve results

For every selected check, record pass/fail/not run, candidate build, operation,
release/deployment IDs and sanitized serving/TLS evidence. An accepted submission
is not a pass. Fix failures in source, review them, publish a new candidate with
explicit authorization and repeat affected checks. Run PostgreSQL integration
tests only against an explicitly authorized disposable test database, never the
server's ambient control DSN. Do not declare the alpha qualified or tag it until
the required baseline has a recorded pass for the build being shipped.

## Current qualification boundary

The integrated 2026-10-28 source changes have no real-server baseline record.
Every host check above remains **not run** for this worktree; earlier andurel-site
observations cited in MASTER concern an older build, not qualification here.
PostgreSQL integration tests (`just integration-norace`) **pass** for the
working-tree fixes below against a disposable local PostgreSQL 17 container
started for the run (bound to loopback, removed afterwards); the race build was
not run locally (no C compiler) and is left to CI. Selected isolated
tests/build/vet are developer evidence only.
During implementation, one Lane E CLI test invocation accidentally included the
excluded Docker SSH fixture; it is not authorized real-host or baseline evidence.
Final validation explicitly excludes that test.

Failed-follow acceptance: a failed deploy or failed guided first deploy returns
nonzero with the recorded error code (fallback exit 5), while retaining the
failed state/error and serving-state output. Successful follows exit 0; a remote
cancellation and Ctrl-C detach both exit 130 but have distinct messages. JSON and
NDJSON emit one failure envelope/result with operation ID and state. These checks
have isolated regressions but remain **not run** on the test server.

## Run record

Append one record per run. Do not backfill from history; runs before
2026-10-26 qualified older builds and earlier scope.

```text
Date / build / commit / channel: 2026-10-28 / edge-ed6e4f35b78c / ed6e4f3 / edge
Operator OS/arch: linux/amd64 (CLI upgraded edge-8a7179d6d95d -> edge-ed6e4f35b78c: pass)
Server: existing test server, context `main`, x86_64, node edge-8a7179d6d95d
Node upgrade dry run (`apply --dry-run --auto-upgrade-nodes`): pass; reported
  the upgrade, exit 5, node binary unchanged
Node upgrade (`apply --auto-upgrade-nodes`): FAIL, exit 2, validation_failed
  "invalid method params" / unknown field "version". The older node strictly
  decodes upgrade.install as {path, sha256}; ed6e4f3 added version/channel, so
  no existing node can be upgraded by it. `admin` has only the node-RPC sudo
  rule, so the RPC is the only upgrade path. Node left on edge-8a7179d6d95d.
CI for ed6e4f3: FAIL. macOS: unresolved checkout root for an empty git context
  path (/var vs /private/var). Integration: seven new tests registered nodes the
  test transport does not authenticate, three updated immutable revisions, and
  the scripted transport omitted the inputs hash a real node journals, so the
  release-command evidence rule correctly kept two deploys retrying.
Fixes (working tree, not yet published): upgrade.install params frozen at
  {path, sha256} with the release declaration uploaded as release.json beside
  the binary; empty context path resolves like "."; integration fixtures fixed.
Application baseline: not run (blocked on the node upgrade).
```

```text
Date / build / commit / channel:
Operator OS/arch:
Server provider, OS/arch, size:
External PostgreSQL version; schema current/required; River current/required:
App / environment / domains (non-secret):
Source commits; release, deployment and operation IDs:
Checks: pass / fail / blocked / not run, one line each
Public response markers and TLS result:
Failures and recovery observations:
Remaining blocker and next action:
```
