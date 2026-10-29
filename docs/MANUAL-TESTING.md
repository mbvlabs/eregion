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
Date / build / commit / channel: 2026-10-29 / edge-22f52c676669 / 22f52c6 / edge
CI for 22f52c6: pass (check with race, integration with race, macOS CLI);
  Publish edge: pass
Operator OS/arch: linux/amd64 (CLI upgraded edge-ed6e4f35b78c -> edge-22f52c676669: pass)
Server: existing test server, context `main`, ubuntu 26.04 amd64
Node upgrade dry run: pass; node_version_mismatch reported, node unchanged
Node upgrade (`apply --auto-upgrade-nodes`): binary install PASS (node now
  edge-22f52c676669, commit 22f52c676669); apply FAIL, exit 6, at
  host-database-v1 "control-plane database is unavailable"
Post-apply: eregion-api/eregion-worker crash-loop with exit 6; doctor 13 pass,
  1 warn, 4 fail (drift.pending, version.schema, both services); schema 0/0
  reported because the database cannot be read
Cause: not eregion. The control database is reached through the operator
  machine's Tailscale Funnel (TLS-terminated TCP); a network outage there at
  08:35 UTC, before the upgrade, left the public Funnel ingress dropping TLS
  handshakes. Re-adding the Funnel did not help; restarting tailscaled did
  (all six ingress addresses then completed TLS from the server).
Diagnosability gap (open): the fault cause is not surfaced by apply, plan,
  doctor or server status, and admin cannot read /etc/eregion or the eregion
  service journal, so the network error was not observable from the operator
  path.
Retry `apply --auto-upgrade-nodes`: pass, exit 0; applied host-database-v1
  (schema 13 -> 16), api/worker restart and readiness. Second `apply`: no-op
  (0 of 17). doctor: 18 pass, 0 warn, 0 fail. CLI/node edge-22f52c676669,
  schema 16/16, River 7/7, all services active.
Version lockstep / signed node upgrade from edge-8a7179d6d95d: pass.
App: githello/production, private git https://github.com/mbvlabs/eregion-hello
  (fine-grained read token), web + worker, release command
  `eregion-hello release`, config APP_VERSION=v1, secret HELLO_SECRET, domain
  git-test1.mbvlabs.com (Cloudflare proxied). Flag-only create (not the guided
  form): accepted, op_49bbb5e2e870fac4005801dec7351c17; first deploy
  op_14cdbdbbe86aaa79823ef246167c0e66.
Private git clone + Buildpacks build: pass (commit ea1b225, pack 82s, release
  rel_ef2f8f4e60684cb0722e616382a1d4af).
First deploy: FAIL, exit 6, unavailable after 4m56s. The release command ran as
  `docker run <image> eregion-hello release` without the CNB launcher, so the
  image's default web process started and never exited; at the 120s timeout
  the node killed only the docker client, the container kept running, the
  error was retryable and every retry hit a container-name conflict.
  Previous release unchanged (none). Leaked container
  eregion-githello-production-release-7e5cbbcbabf7 still runs on main (no
  published port); admin has no Docker access to remove it.
Fix (working tree, not yet published): the release command is its own node
  step (platform-command-01-v1, ReleaseCommandPayload) that runs through the
  launcher for a Buildpacks image like a process command; nonzero exit,
  start failure and timeout are terminal, and a timeout removes the container.
  The release container now carries the app/environment ownership labels, so
  app/env delete, status and logs find it; the leaked container above predates
  this and has none, so it needs a one-time manual `docker rm -f`.
Published e344781 (edge-e344781ced4b; CI pass, edge pass); CLI and main
  upgraded, apply no-op, doctor 18/0/0.
Second first-deploy attempt op_ec5db74078f168c2391b868a047f73c9: release
  command pass (1.5s, launcher), web + worker candidates started, web health
  pass, Caddy switch pass; public check FAIL after 30s, "tls: internal error"
  (no certificate yet for git-test1). Route restored, candidates removed,
  exit 5. Initial hypothesis: the 30s public-check window was too short for
  first certificate issuance. That cause was NOT established; seeing an HTTP
  redirect through Cloudflare did not prove successful ACME validation.
Diagnostic retry on the same published build, using the existing release:
  op_44c553d6e9d34a1740b79b870f3fd217. Release command correctly skipped;
  candidate health passed. Read-only Caddy admin polling saw git-test1 appear
  at 11:04:14 and disappear by 11:04:29, before the public probe failed at
  11:04:46. Confirmed routing race: periodic reconciliation renders the old
  serving pointer, undoing the candidate switch during public verification.
Working-tree correction: every route writer selects the saved candidate
  instances while its operation/deployment is running and the switch step
  is running/completed, unless public verification failed or restoration
  began. Otherwise it selects serving instances. Regression covers first
  deploy, switch-before-ack, verification, and restoration after failure.
  The public-check window also becomes 2 minutes, matching deploycrate, to
  allow certificate issuance; this alone does not fix the routing race.
Local verification of the route correction: just check (format/vet/unit),
  just build, darwin/arm64 build: pass. Full PostgreSQL integration suite:
  pass (189.620s) against a new disposable PostgreSQL 17 on loopback port
  55439, removed afterwards. Race detector not run locally (no C compiler).
Route correction published as 5329590 (edge-5329590a3f48), CI (including
  race integration) and edge publication pass. CLI/main upgraded; schema
  16/16, River 7/7, doctor 18 pass; second apply no-op.
Published-build retry op_85ed4ffb0f50d20de1b9be5835de8d46, existing release
  rel_63507d362852c4fd97048936ad1f7e7b: release command correctly skipped,
  candidate web/worker started, web health passed. Caddy polling confirmed
  git-test1 stayed installed from 11:24:23 through 11:26:19 despite periodic
  reconciliation. Public verification failed with TLS internal error after
  the full 2-minute window (11:24:21–11:26:22); only then was the route removed
  and both candidate containers cleaned up. Follow exit 5. No serving release.
  Route-race correction: pass on host; certificate problem remains separate.
Read-only diagnosis: ACME directory reachable from main over IPv4 and IPv6
  (HTTP 200). Caddy sends logs to the system journal; no log files exist in
  /var/log/caddy. Admin cannot read Caddy's journal or use sudo without human
  authentication. Need privileged Caddy logs for the verification interval
  before diagnosing ACME failure or retrying. No certificate bypass applied.
Operator-supplied privileged Caddy logs establish the certificate failure:
  HTTP-01 validation followed an HTTPS URL through Cloudflare and received
  525 (origin TLS handshake failure). TLS-ALPN-01 also failed because the
  Cloudflare edge did not negotiate acme-tls/1. Production Let's Encrypt
  reported failed-authorization rate limiting with retry-after 2026-10-29
  11:28:18 UTC. ZeroSSL fallback separately failed obtaining EAB credentials
  (caddy_legacy_user_removed). Increasing the timeout cannot solve this.
The proposed DNS-only workaround was rejected: proxied Cloudflare is a
  supported product requirement, and deploycrate works without that step.
Operator authorized and installed official Caddy 2.11.4 amd64 .deb on main,
  verified with the upstream SHA-512 checksum, replacing Ubuntu's 2.6.2-14.
  Configuration validated; Caddy restarted. Existing application hostnames
  still return HTTPS 200/303, and doctor remains 18 pass. DNS stays proxied.
Retry on Caddy 2.11.4: op_8587e038f577d90d756d0d174554bf76, same release.
  Release command skipped, candidate health passed; TLS still failed for the
  full 11:39:10–11:41:11 verification window. Follow exit 5; route restored
  and both candidate containers removed. Upgrading Caddy alone did not fix it.
Operator then disabled proxy for git-test1. At 11:43 UTC both 1.1.1.1 and
  8.8.8.8 resolved its A record to 46.225.150.17, not main's 46.224.129.96
  (confirmed by saved context and main's eth0 address). The other IP also
  answers as Caddy and redirects HTTP to HTTPS, which can explain challenge
  requests missing the Caddy instance holding the challenge. The previous
  proxied origin value was not observable and must not be assumed verified.
  No deploy was retried after discovering the DNS mismatch.
Operator corrected both origin targets. git-test1 remains DNS-only,
  git-test2 remains proxied. The first submission after the correction refused
  before operation creation because the stored DNS observation was still wrong;
  after the periodic observer refreshed to dns=ok, retry was accepted.
First successful deploy: op_5a0dee15a50da94ace13bae428eeb1b1, 18.6s, exit 0,
  edge-5329590a3f48 with manually upgraded Caddy 2.11.4, release
  rel_63507d362852c4fd97048936ad1f7e7b (full deployment ID not captured).
  Primary public check transitioned from TLS error at 11:49:43 to HTTP 200 at
  11:49:46. External HTTPS (normal certificate verification) returns
  `eregion sample: v1`; worker logs show `worker heartbeat version=v1`.
Proxied alias: added git-test2 with /healthz, operation
  op_eb772442b05bff4cbe21092e77f50b42. Within 12s external HTTPS through
  Cloudflare (172.67.142.154) returned HTTP 200 and `eregion sample: v1`.
  Local TLS verification against Caddy with git-test2 SNI/hostname verified
  the origin certificate successfully. Fresh certificate issuance works with
  the proxy ON and the correct origin; disabling proxy is not required.
Remaining qualification: second-commit/zero-downtime deploy, failed candidate,
  rollback, scale/restart and the rest of the baseline. The Caddy upgrade alone
  was not a fix for the wrong DNS origin; Caddy 2.6.2 was not re-tested after
  correcting DNS, so no version-specific ACME defect has been established.
```

```text
Date / build / commit / channel: 2026-10-29 / edge-6d0e6bcdf65a / 6d0e6bc / edge
Server: existing main, Ubuntu 26.04 amd64; schema 16/16, River 7/7
Setup pin: official Caddy 2.11.4 packages for amd64/arm64, pinned SHA-256,
  host-ingress-v2. Both upstream package hashes independently checked.
  Local check/build/vet/darwin build pass. Initial 654469b edge signing failed
  with Rekor conflict and macOS shell test failed on checksum tooling;
  7d70ff6 signed successfully but macOS shell test still failed. Final 6d0e6bc
  scopes Linux installer execution test to Linux; artifact/version tests stay
  cross-platform. All CI jobs and edge publication pass for 6d0e6bc.
CLI/main upgrade: pass, doctor 18/0/0, second apply 0/17. Caddy 2.11.4 already
  installed manually, so automatic fresh installation remains NOT RUN.
App: githello/production; git-test1 direct, git-test2 Cloudflare-proxied
Second source commit: 6dc0468; APP_VERSION v2 and HELLO_SECRET revision 2.
  Config/secret changes did not deploy (v1 kept serving until explicit deploy).
  op_feb09d5b91fae33c1b9b71acbfb20aad succeeded, release
  rel_6587e1fc95ab6a382279ed68b07eb076. Primary continuity: 107 v1 + 281 v2
  responses, zero failures. Python alias monitor got 541 HTTP 403 responses
  despite curl returning 200; not counted as alias continuity pass.
Failed candidate: source 034eb82, op_f3c8d98c922de1ccf77254b62fdefff9,
  release rel_b07293e8cb84db75c2d4cf64ad8a6718, HTTP health 503, exit 5.
  Candidate web/worker removed. Prior v2 kept serving: curl monitor 342 primary
  + 412 proxied-alias responses, all 200/v2.
Rollback: op_007901e79d9e111d91e7013c6343b147 succeeded, v1 public response,
  release rel_63507d362852c4fd97048936ad1f7e7b. Read-only node inventory
  confirms deployment dep_17829516b0867de58811cc5ad36566da pinned HELLO_SECRET
  revision 1, original deployment also 1, v2 deployment 2. No secret exported.
Scale/restart: scale op_a76e2a47dc63ab399307664ca27b76a9 queued restart
  op_ca824ea04f46caa801945e4617ad1295; explicit restart
  op_b500b918c472987bb16b2aae28d41b3d. Both succeeded, two running web
  replicas on distinct loopback ports plus worker without published port;
  release command skipped in rollback, scale and restart. Restarts used the
  current HELLO_SECRET revision 2, distinct from rollback's pinned revision 1.
Repeated continuity with curl: op_62aed6ec69fbb7ac82beb77427e01ffd,
  source 6dc0468, config v3, release rel_8353d51796535fdf752f2c3450052af8.
  Primary 129 v2 + 136 v3; proxied alias 153 v2 + 163 v3; zero failures.
  Previous containers removed after verification.
Monorepo/moved-branch retry: revision context apps/web at source 034eb82
  failed with named missing path, op_74435aa7a0c4fea621a9331f458d250a,
  build bld_b921cb6d6db5d2e0365010a352d983bd. Branch then advanced to b895cb2
  adding healthy apps/web. Retry op_038e1d3a1c7c6eb6e0207d2736a08742 still
  checked out 034eb82 and failed on the original missing path (exit 5), proving
  no fallback to moved branch/root. New deploy op_5e45b93d5ae086627a6ebfa63ab36304
  built b895cb2 from apps/web and succeeded, release
  rel_07d1c0fb27cdc0d68c7e5815a56e9328.
Alias-specific path: git-test2 bound to /users/sign-in, public 200; /api/health
  returns 404. Proxied alias serves v3; certificate verification remains on.
Repair fault injection: using saved sudo credential over SSH (never printed),
  removed only githello web replica githello-production-web-1-11ba8f12ce95.
  Worker restart queued one system/maintenance restart
  op_86d5c64bb534a6ae4df042651c0b4671; succeeded, both replicas restored,
  same serving release, release command skipped. A second worker start queued
  no additional repair. Doctor 18 pass, both test hosts serve v3.
Contracts: logs --json single envelope; --json --follow refused exit 2;
  domain dry-run JSON object; ssh command remote exit 7 preserved. App delete
  dry-run allowed, real env delete/secret export/token create in JSON/no-TTY
  refused exit 2. No factor bypass, export or deletion performed.
Create replay: original githello-create-1 returned original create
  op_49bbb5e2e870fac4005801dec7351c17 and first deploy
  op_14cdbdbbe86aaa79823ef246167c0e66; secret metadata remains revisions 1
  (GIT_READ_TOKEN) / 2 (HELLO_SECRET), no rotation from replay.
Build stop: build bld_ea19d783ac55541189e81f29dcbce1d3, original operation
  op_0dd481a2c083a6bf7901ee0af2775363. Stop op_e6734f665ded938f4bf7acf19e861b86
  accepted at 12:36:41, dispatched at 12:36:43, but its node Apply only ran
  at 12:36:57 when pack had finished. Deploy cancelled at boundary, exit 130;
  previous release preserved. Retry op_6937cbe93056c26a5108757865dbeb49
  built the same b895cb2 successfully. Stop-immediately check FAIL: global
  host lock prevented cancellation marker reaching the in-flight build.
Fix in source: cancellation-marker step is host-lock-independent, retaining
  its own effect lock and normal journal. All other steps remain serialized.
  Regression holds host lock while running stop and checks marker is written;
  another regression checks ordinary steps still block and signal replay is
  idempotent. Local check/build/vet/darwin build pass.
Build-stop retest: published ef91d70; CI and signed edge publication green.
  CLI/main upgraded to edge-ef91d70f6215, second apply 0/17, doctor 18/0/0.
  Build bld_d21b35126cd6010b2ec48ba11da6f328 was running go build when stop
  op_3d2383275011db0f4d11b8c3dd61ca1d wrote the marker immediately (node step
  2.0 seconds, entire stop 5.7 seconds). Pack reported interrupted rather than
  completing. Original deploy op_5244c4b7bb966618bca76e6a69d9d072 cancelled,
  exit 130; previous v3 release remained serving. PASS on this build.
Alias removal: op_8f136156eb93ba45ca9d6a0c60ae87c5 submitted, then repeated
  removal op recorded in 81-alias-remove-submit.json was followed to success.
  Inventory omitted alias; primary kept serving. Alias restored afterward.
Route-write failure/recovery: temporarily chmod 0555 the generated-routes
  directory /etc/caddy/eregion.d, with an EXIT restoration trap. Alias add
  op_09f89c35fa4f52ed12df2d76eae0d688 failed exit 5, both bindings visibly
  route_state=failed with could-not-write error; primary still served v3.
  Restored original 0755/uid999/gid983. Periodic reconciliation reapplied
  desired routes without re-submission, cleared errors; both domains now
  route=applied, certificate=valid, public=ok. Failed operation stays failed.
  This qualifies write failure/recovery, NOT a rejected Caddy reload.
Release upload rejection: RPC on published main given a private dummy upload
  with correct SHA-256. Missing declaration rejected; dev declaration rejected;
  naming the current signed edge release with unsigned payload rejected as
  not the signed linux/amd64 artifact. No binary replacement or service restart;
  temp upload removed; version still edge-ef91d70f6215. PASS.
Historical unlabelled release container from pre-fix attempt:
  eregion-githello-production-release-7e5cbbcbabf7 was still running with no
  published ports; removed explicitly after confirming identity. No other app
  containers targeted. NDJSON log snapshot parsed line-by-line successfully.
Evidence: ~/.cache/eregion-run/71-* through 92-*; latest doctor 18/0/0.
Remaining: guided terminal/detach, init on a fresh authorized target,
  factor-positive CI/deletion/export, atomic login, cleanup obstruction,
  rejected-reload injection and remaining baseline variants are not yet
  qualified. Alpha is not complete.
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
