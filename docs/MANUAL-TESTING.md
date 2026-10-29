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

## Resume testing — saved session state, 2026-10-29

Start a fresh session by reading [MASTER](MASTER.md), then
[IMPLEMENTATION](IMPLEMENTATION.md), then this section and the run records below.
MASTER row 16 remains in progress; this is a resumption aid for its existing
acceptance checklist, not a second work list. **Alpha.1 is not qualified.**
Facts below are the last recorded observations, not a fresh host check.

### Target and build snapshot

| Item | Last recorded state |
| --- | --- |
| Repository | `/home/mbv/work/eregion`, branch `master`; saved evidence committed through `b5a1a56` (documentation-only). Uncommitted after it: this resume section, the 2026-10-29 resumed run record, and the unpublished `host-ingress-sysctl-v1` source fix (`internal/host/step_ingress_sysctl*.go`, `runner.go`). |
| Tested CLI and main | `edge-ef91d70f6215`, source `ef91d70f6215180c64f73ba5b60bb3f3a2655d70`; CI and signed edge publication passed. Do not confuse a newer edge publication with an installed or qualified build. |
| Existing server | Context/node `main`, host `exploration`, Ubuntu 26.04 amd64; public IP `46.224.129.96`; operator `admin@10.78.0.1:22` through embedded WireGuard, endpoint `46.224.129.96:51820`. |
| Health | Doctor 18 pass / 0 warn / 0 fail; second apply 0 applied / 17 skipped; schema 16/16, River 7/7; Caddy 2.11.4; `net.ipv4.tcp_migrate_req=0` (diagnostic change reverted). A build with the sysctl step will apply one new step. |
| External database | PostgreSQL 17 on the operator machine, container `eregion-cp-pg`, host port 5433. Main reaches it through TLS-terminated Tailscale Funnel `thelab.tailbbed89.ts.net:10000`. No DSN belongs in this document. |
| Test app | `githello/production`, `APP_VERSION=v3`; web ×2 (`eregion-hello`, port 8080) and worker ×1 (`eregion-hello worker`, no port); release command `eregion-hello release`; health `/healthz`. |
| Serving release | `rel_07d1c0fb27cdc0d68c7e5815a56e9328`, healthy monorepo source `b895cb2f54daeaedafac44397072db893cd8ef1c`; deployment `dep_4ca1b01d314414a4fdaef2730cd1760b`, revision 8, release command `eregion-hello release` (last checked 17:55 UTC). |
| Source desired state | Private `https://github.com/mbvlabs/eregion-hello.git`, ref `main`, context `apps/web`, builder `paketobuildpacks/builder-jammy-base`, credential name `GIT_READ_TOKEN`. |
| Primary | `https://git-test1.mbvlabs.com/`, last recorded DNS-only, returns `eregion sample: v3`. |
| Alias | `https://git-test2.mbvlabs.com/users/sign-in`, Cloudflare-proxied, returns `alias ready`; binding check path `/users/sign-in`. Both bindings last observed route=applied, certificate=valid, public=ok. |
| Secret metadata | Current `HELLO_SECRET` revision 2; original v1 rollback used its pinned revision 1. `GIT_READ_TOKEN` revision 1. Values must never be printed. |

Fixture checkout: `/home/mbv/.cache/eregion-hello`. Useful source commits:

- `ea1b2254217fd440837fd01a1b7a75e3926f9c63`: original v1 root app.
- `6dc04688b87d8b7d24087d4fe6ec74fcfd027fd7`: healthy root app with source-v2
  header and alias route.
- `034eb826f82fc134ecb38c6de523f32a4751be35`: intentionally unhealthy root
  app unless `FAIL_HEALTH=0`; has **no** `apps/web` directory.
- `b895cb2f54daeaedafac44397072db893cd8ef1c`: healthy `apps/web`; fixture
  branch `main` last pointed here. Root source still has the deliberate failure.

Do not deploy old root commits with the current `apps/web` context and mistake
missing-path rejection for a health test. Read desired state before changing it.

### Safe first checks

These are inspection commands, not an instruction to upgrade or deploy:

```sh
git status --short
git log -1 --oneline
eregion version
eregion context show --name main --json
eregion --context main server status --json
eregion --context main doctor
eregion --context main env status githello production --json
eregion --context main domain list --app githello --json
eregion --context main build list githello production --limit 10 --json
curl --fail --show-error --silent https://git-test1.mbvlabs.com/
curl --fail --show-error --silent https://git-test2.mbvlabs.com/users/sign-in
```

If CLI/node versions differ, first record both. A new `upgrade` changes the local
CLI only; `apply --auto-upgrade-nodes` mutates the server and possibly its
configured database. Obtain applicable target authorization before changing
builds, and rerun affected checks on the chosen published candidate. Never
silently upgrade just to make an inspection command work.

### Evidence already available

Detailed pass/fail records and operation IDs are below. Logs and sanitized JSON
are under `/home/mbv/.cache/eregion-run/` (operator-local, not Git artifacts):

| Evidence | Files |
| --- | --- |
| Pinned setup upgrade/convergence | `24-setup-apply.log` |
| Second deploy and failed-health isolation | `27-*` through `32-*` |
| Rollback secret metadata, scale and restart | `33-*` through `37-*` |
| Zero-failure curl continuity on primary and proxied alias | `38-continuous-v3.json`, `39-*`, `40-*` |
| Missing context, moved-branch retry, successful monorepo deploy | `41-*` through `46-*` |
| JSON/refusal/SSH contracts and container repair | `47-*` through `60-*` |
| Create replay and original build-stop failure | `61-*` through `69-*` |
| Build-stop regression checks, published upgrade and passing host retest | `70-*` through `78-*` |
| Alias removal, injected route-write failure and automatic recovery | `79-*` through `87-*`, `92-domain-recovery-final.json` |
| Latest inventory, NDJSON logs and unsigned-upload rejection | `88-*` through `91-*` |
| Release-command change/failure/refusal, cleanup obstruction, Caddy reload resets and `tcp_migrate_req`, rejected reload and recovery | `93-*` through `123-*` |

The Python alias continuity monitor returned 403 and is **not** passing evidence;
use the later curl measurements. Saved binding/operation acceptance is not
completion. The route-write test failed its operation as expected; periodic
reconciliation subsequently recovered the routes without rewriting that failure.
It does **not** qualify rejection of a Caddy reload.

### Remaining acceptance to select next

The complete baseline tables above remain authoritative for acceptance details;
do not treat a passed subset as completion of an entire row.

| Baseline area | Recorded coverage and remaining qualification |
| --- | --- |
| Guided/operator flow | **Not run:** guided app/env create success and failure, Ctrl-C detach and reattach, flag/guided revision equivalence. Flag-only create and app-create replay passed. |
| Login and contexts | **Not run:** atomic failed replacement, successful second-machine login, wrong-key refusal, context selection variants and removal. Preserve the working main context and identity. |
| Zero-downtime switch | **Fail on edge-ef91d70f6215 under concurrent clients:** each Caddy `/load` resets a few in-flight TLS handshakes (Caddy's per-load SO_REUSEPORT listener). Source fix `host-ingress-sysctl-v1` (`net.ipv4.tcp_migrate_req=1`) is unpublished; after publishing, apply on main and repeat the dense four-to-six-loop primary test across several switches. |
| Cleanup and route failure | Previous-container removal failure, visible cleanup row and later maintenance removal passed; rejected Caddy reload with restore and periodic recovery passed; write-failure recovery and alias removal passed. **Not run:** failed-candidate removal failure and concurrent/interrupted route changes. Use a scoped reversible fault and restoration guard; do not disturb other apps. |
| Release-command recovery | Once-per-release skipping, a changed command running again, and restart/scale refusal after a failed run passed. **Not run:** rollback refusal (needs a release whose recorded command never succeeded) and unresolved/lost-response recovery variants. |
| Build/repair variants | Monorepo, missing-path rejection, moved-branch retry, stop/retry and basic serving-container repair passed. **Not run:** symlink-escape rejection, replay with unavailable git, competing/stale repair variants and live build-log follow acceptance. |
| CI/webhooks | **Not run:** authenticated CI deploy, expired/revoked/wrong-target token denial, public API restriction, signed push deployment, redelivery deduplication, other-branch/bad-signature rejection and webhook-secret rotation. Token creation requires the human factor; a webhook secret must never enter ordinary evidence. |
| Protected operations/removal | JSON/no-TTY refusals and deletion preview passed. **Not run:** factor-positive token creation, both exports, app/env deletion including last environment, cleanup/history verification and direct same-request-factor rejection variants. Human enters the authenticator code; never derive it from node files. |
| Fresh-host acceptance | **Not run:** fresh pinned Caddy installation, preflight/plan variants, init doctor/resume/skip-doctor, interrupted init/discard, lockdown revert, handoff and break-glass. Requires an explicitly authorized fresh target; main is not that target. |
| Release/platform variants | Signed upgrades on main and unsigned/dev rejection passed. **Not run or partial:** macOS installed-CLI acceptance, remaining dry-run mismatch and failed-manifest-retrieval variants. Cross-compilation and CI are not a substitute for host acceptance. |

No infrastructure outage or injected fault was left at the stopping point. The
known open defect is the Caddy-reload connection reset above (fix in source,
not yet published). Missing prerequisites: an authorized fresh server and
human participation for factor-positive operations. Other unqualified variants
remain **not run**, not implementation failures or passes.

### Safety and local resources

- Do not rerun `server init` on main, reset its database, edit applied migrations,
  erase journals or remove existing apps, especially `andurel`. Restrict test
  mutations to explicitly authorized fixtures and targets.
- Keep Cloudflare proxy support and normal TLS verification. Do not use `-k` or
  disable the proxy as a product fix. The earlier certificate problem was an
  incorrect origin IP, not an established Caddy-version defect.
- Saved operator credentials are under
  `/home/mbv/.config/eregion/servers/main/`. The protected `sudo-password` file
  can authenticate authorized OS fault-injection commands over SSH without
  displaying it. Plain `sudo -n` is insufficient for Docker/systemctl commands;
  do not log credentials or bypass the application's factor gates through root.
- Private fixture read credential file:
  `/home/mbv/.config/eregion-test-git-token`. Protected test secret files:
  `/home/mbv/.cache/eregion-hello-secrets/HELLO_SECRET` and `HELLO_SECRET_V2` in
  that directory. Reference files only; never print or copy values into docs.
- Historical unlabelled release container
  `eregion-githello-production-release-7e5cbbcbabf7` was removed. The injected
  routes-directory permissions were restored to 0755, uid 999/gid 983. No
  deliberate fault was left active.
- The control database depends on the operator machine's Tailscale/Funnel. The
  earlier outage recovered after the operator restarted Tailscale; diagnose
  connectivity without exposing the DSN rather than resetting state.
- Inspect selected tests before running them: `just check` includes tests that
  can start Docker. Do not use an ambient production DSN as a test target or
  touch `eregion-platform-test-pg` on port 55432. Prior disposable integration
  run used loopback 55439 and removed its container afterward; any new run needs
  its own authorized isolated target and explicit `EREGION_TEST_DATABASE_URL`.
- For local Go builds, use `GOTMPDIR` and `TMPDIR` set to
  `/home/mbv/.cache/eregion-gotmp` to avoid the previously full `/tmp`. Local race
  testing lacks a C compiler; race checks were performed in CI.
- This saved document does not authorize a new commit, push, branch, tag,
  release, database or host mutation. Confirm applicable authorization first.

## Current qualification boundary

Real-host evidence spans several published edge builds, recorded below.
`edge-ef91d70f6215` is the latest installed and tested build; earlier passes are
not automatically a full baseline pass for it. Source fixes have isolated
regressions, build/vet and darwin/arm64 cross-build evidence; published CI also
ran race checks. These are distinct from manual acceptance.

Failed deploy follows returned exit 5 on main, successful follows returned 0,
and the build-stop retest returned 130 with terminal cancellation and the prior
serving release preserved. Guided failure and viewer-detach acceptance remain
not run. Complete and record the remaining baseline for the actual release
candidate before asking the user to authorize `1.x` or `v1.0.0-alpha.1`.

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
Date / build / commit / channel: 2026-10-29 / edge-ef91d70f6215 / ef91d70 / edge
Operator OS/arch: linux/amd64; CLI and main edge-ef91d70f6215 (resumed snapshot)
Server: existing main, Ubuntu 26.04 amd64, kernel 7.0.0-30; schema 16/16,
  River 7/7; doctor 18/0/0 before and after
App: githello/production; git-test1 direct, git-test2 Cloudflare-proxied
Release command scope (changed command): env update to
  `eregion-hello release changed`, op_9fb563245865bcaad59d77b502710111 (no
  deploy). deploy --release rel_07d1c0fb27cdc0d68c7e5815a56e9328,
  op_2e03c1f3b7d922dc49609a3b048e4ca2: command ran again for the same release
  ("release command ran version=v3"), succeeded, 52+52 monitor requests 200.
  PASS.
Release command failed run: command `false`, op_16a4eefef2536158a493a810627b8af0;
  deploy op_abfb7a138e27de0188d60aa28ee96b77 failed at platform-command-01-v1
  (exit status 1), exit 5 precondition_failed; rel_07d1 kept serving. Then
  restart and `process scale web 3` refused at submission, exit 5, "release
  command has not succeeded for this release"; no operation and no revision
  (replicas stayed 2). PASS. Rollback --to rel_8353d51796535fdf752f2c3450052af8
  (op_b55430000c5515b87c929a0d8c53622d) succeeded and skipped the command: it
  pins the release's recorded revision, whose command succeeded for that
  release, so the current failed command does not apply. Correct by design;
  a rollback refusal needs a release with no succeeded recorded command and
  was not constructed. Restored `eregion-hello release`
  (op_990bdfc52243088f97142a453396dd11); deploy
  op_831954d2473e0f9b201f8747d3787c4b skipped the succeeded command, rel_07d1
  serving again.
Leaked-container retry: fault = temporary root /usr/local/bin/docker wrapper
  failing only `rm -f githello-production-web-1-b99bb0a5d5a1` (everything else
  exec /usr/bin/docker), 30-min systemd-run removal guard. Deploy
  op_438306f739281d1a8b0a1586fcfa5859 SUCCEEDED; its cleanup step failed with
  the simulated error; "Previous release cleanup is pending" event; env status
  `pending_cleanup` named the container ("1 leaked containers pending
  cleanup"). Maintenance at 17:40:09 retried and failed (fault active). Wrapper
  and timer removed 17:41:28; the 17:45 maintenance run removed the container
  and the row. PASS. Observations: the cleanup event says "an internal error
  occurred" instead of the step error, and the final summary still says
  "candidate cleanup completed" while previous-release cleanup is pending.
Zero-downtime switch: FAIL under concurrent clients. During op_438306f7 one
  primary request (DNS-only, direct to Caddy) failed with curl 35 "Recv
  failure: Connection reset by peer" at 17:38:47.73, overlapping Caddy's
  /load at 17:38:47.67. Dense reproduction (4 parallel fresh-TLS loops on the
  primary, redeploying rel_07d1): op_a9e23a8a95ff5d75eed8933e0f530cd4 3 resets
  / 596, op_50c38192df1cf847784e547268fcaf6c 2 / 579, all within ~10 ms of the
  routes step. Earlier zero-failure runs used one sequential low-rate client.
  Cause: Caddy 2.11.4 (listen_unix.go) binds a new SO_REUSEPORT listener per
  config load and closes the old one; Linux resets connections still queued
  on the closed socket (reset after ClientHello, before ServerHello; same
  pattern as caddy.community topic 33952, no upstream fix). deploycrate
  changes routes through the admin API, which reloads the same way.
  Diagnostic: runtime-only `sysctl -w net.ipv4.tcp_migrate_req=1` on main;
  op_dc411627c7accd271fcdcc34dbb6791a 0/584,
  op_9ba884b586d44e50d2d79950612f756a 0/581 (4 loops),
  op_99521edfc690f22bc9e2e7b2fd1f1f79 0/900,
  op_94926b929ade66e8e30ecbb1a91179f3 0/851 (6 loops). Reverted to 0
  afterwards. Source fix (working tree, unpublished): new host step
  host-ingress-sysctl-v1 persists and applies the setting before Caddy.
  Requires publish, upgrade/apply and a dense continuity retest.
Rejected Caddy reload: domain remove git-test2
  (op_160c91170612fcf59d842ad0dd5bf149) succeeded, alias 525. Fault = root
  file /etc/caddy/eregion.d/zz-qualification-fault.caddy with an invalid
  directive (fragment /adapt passes, full /load rejects), 15-min removal
  guard. domain add git-test2 --check-path /users/sign-in,
  op_e33190b41329d608ab4dbde45f1081c5: failed exit 5 "Caddy rejected the
  routes (400)"; routes file restored without git-test2; git-test2 desired 1 /
  applied 0 route_state=failed, git-test1 failed state recorded but still
  serving v3. Deploy op_1542fa11ffa1d56408f9e157dbe57e33 under the fault
  failed at platform-routes-01-v1, exit 5, candidates cleaned up, rel_07d1
  still serving, monitor 131/131 200. Fault and timer removed 17:54:59;
  within 45 s periodic reconciliation applied both routes without
  resubmission: git-test1 24/24, git-test2 2/2 applied, certificate valid,
  public ok, alias `alias ready` 200 through Cloudflare. Failed operations
  stay failed. PASS (rejected reload and recovery). Concurrent/interrupted
  route changes remain NOT RUN.
Contract observation: `deploy --output ndjson` refuses with
  invalid_output_mode although deploy follows and its help says follow
  commands accept ndjson; `operations follow --output ndjson` works.
Final state: rel_07d1 serving, deployment dep_4ca1b01d314414a4fdaef2730cd1760b,
  revision 8, release command `eregion-hello release`, no pending cleanup,
  no wrapper, fault file, guard timer or sysctl change left; doctor 18/0/0.
Evidence: ~/.cache/eregion-run/93-* through 123-* (97/98/104/106/120 monitors,
  112-* and 114-* dense runs).
Remaining: publish and retest the TCP migration fix; guided create/detach,
  login/context variants, release-command recovery variants, symlink escape,
  CI/webhooks, factor-positive operations, fresh-host init and macOS install.
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
