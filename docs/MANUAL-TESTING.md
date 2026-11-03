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
| Init apply | Fresh server ends hardened, SSH on WireGuard only, Caddy with a valid certificate for the hostname, node_exporter reachable only over WireGuard, services ready, and the final doctor gate reports `doctor: 20 ok` before the handoff. A forced failing check stops before the handoff with `doctor_failed` and the fix commands; `--resume` after the fix completes without repeating earlier phases, and `--skip-doctor` ends with a warning. |
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

### Disk retention (D30, alpha.1)

Run the mutating steps only on the authorized disposable test app and database;
backdate fixture rows only there. Never prune the production app to satisfy these
checks.

| Check | Pass means |
| --- | --- |
| Retention policy | `eregion retention show --json` returns `scope: server` with `keep_releases 10`, `keep_newer_than_days 14`, `artifact_grace_days 14`, `buildpack_cache_max_bytes 10737418240` and `buildpack_cache_max_percent 80`, every source `server`. `eregion retention set --keep-releases 5 --keep-days 10 --artifact-grace-days 10 --cache-cap-bytes 1073741824 --cache-cap-percent 75 --json` stores that server default and `show` returns it. `eregion retention set --app <app> --env <env> --keep-releases 2 --keep-days 3 --artifact-grace-days 3 --json` commits a new environment revision (accepted operation ID printed); `retention show --app <app> --env <env> --json` marks only the three release fields `environment`, both cache fields stay `server`, and `env status` names the new revision. `--clear-override` restores inheritance; a flagless `set` with `--json` or no TTY refuses with `input_required`. Host status 2026-11-03: pass on main (236–252, 274). |
| Prune dry run | `eregion prune --dry-run --json` returns `dry_run: true`, one `items` entry per planned kind with `reference` and `bytes` where known, and `planned_bytes`; warnings name unreadable observations. No operation is accepted (`operations list` gains none), and `release list`, Docker images/containers and registry content are unchanged (compare counts and `docker system df` before and after). A second dry run prints the same plan. Host status 2026-11-03: pass on main (253–267); only the dangling images carried bytes, release rows and image items did not. |
| Pruned rollback | After a real `prune --confirm <server>` removes an old release, `release list` shows the row with `pruned_at` and its ID still present. `rollback <app> <env> --to <pruned-id> --dry-run --json` and `rollback … --confirm <app>/<env>` both refuse with `precondition_failed` naming the release and prune time; `deploy --release <pruned-id>` refuses the same way. Rollback to the kept target still succeeds and serves. Host status 2026-11-03: not run; the real prune was blocked by defect 5. |
| Registry GC | Record `du -sb /var/lib/eregion/registry` and the registry service state before a prune that removes at least one `registry_artifact`. The operation events show the manifest removal, then `Registry garbage collection completed`; `du` falls by at least the removed manifests/layers, `systemctl is-active eregion-registry` is active and `curl -fsS 127.0.0.1:5000/v2/` returns 200. A stale `eregion-registry-gc` container or a running `garbage-collect` process makes the step fail closed with the registry left stopped and its recovery instruction; a collection or restart failure is never suppressed, and `--delete-untagged` is never used. A deploy after GC builds, verifies publicly and serves. Host status 2026-11-03: not run; the real prune was blocked by defect 5 and the dry-run plan had no registry artifacts. |
| 30-release retention | On the disposable app run `eregion retention set --app <app> --env <env> --keep-releases 5 --keep-days 1 --json`, deploy 30 distinct releases, then use an authorized SQL update on the disposable control database to backdate all 30 `created_at` values to older than one day and run `prune --confirm <server>`. Exactly the newest five releases remain unpruned (serving release #30 and rollback target #29 are in that set); 25 release items mark rows with `pruned_at` while all 30 release rows remain, and separate `image`/registry items remove their images and manifests. A following `prune --dry-run` lists no release items. Host status 2026-11-03: not run; needs a freshly initialized disposable server. |
| host-registry-v2 upgrade | On a node whose registry predates `host-registry-v2`, `plan` lists `host-registry-v2` once. `apply` writes `storage.delete.enabled: true` in `/etc/eregion/registry/config.yml`, restarts `eregion-registry.service` with `REGISTRY_STORAGE_DELETE_ENABLED=true`, and a second `plan`/`apply` reports the step skipped. A manifest delete no longer returns 405, and a deploy after the upgrade serves. Host status 2026-11-03: pass on main (230–233, 270–273); the manifest-delete/deploy-after-upgrade variants were not separately run. |
| Doctor retention | `eregion doctor --json` includes `host.retention`. With no run yet on a fresh install it is `pass` with "no retention run yet"; the startup catch-up or next daily run records one. After a run it names the finish time, status, removed items and reclaimed/planned bytes. A failed last run, a running run older than two days, or a last success older than two days with the worker up is `warn` with a fix. The main-node summary is 20 pass / 0 warn / 0 fail. Host status 2026-11-03: warn (19/1) because of defect 5; the 20/0/0 state is not reached. |
| Doctor Buildpacks cache | `host.build_cache` reads the latest durable cache measurement from `retention_runs`; it never walks Docker volumes or the filesystem in `doctor`. With no measurement, or one older than 48 hours, it is `pass` with unknown (a fresh host stays healthy), never a zero-byte pass. After a retention run records a measurement under the cap it is `pass` and names used and cap bytes. Set the server cap below the measured cache within the allowed range (`retention set --cache-cap-bytes 1073741824 --json`) or build until the cache exceeds it; `doctor` reports `warn` naming the cap. After `prune --confirm <server>` removes least-recently-used entries the next retention run refreshes the measurement and the row returns to `pass`. Cache volumes are named `eregion-build-cache-<hex app>_<hex env>`, `eregion-launch-cache-<hex app>_<hex env>` or legacy `pack-cache-*.<build\|launch>`. Host status 2026-11-03: pass with unknown on main (275). |

Host status on main (2026-11-03 evening, `edge-1e541a7fad3e`): **policy**
show/set and the environment revision pass; **prune dry run** passes (items,
reasons and known bytes; nothing changed); **host-registry-v2** applied once;
**doctor Buildpacks cache** passes with unknown because no run measured it.
**Pruned rollback**, **registry GC**, the deploy-after-GC check, disk-freed
verification and **30-release retention** are not run: the worker's daily
retention run fails before it can plan because it cannot read the root-only
registry storage (defect 5), so `host.retention` warns and a real prune cannot
execute. The 30-release row and the full baseline remain for a freshly
initialized server. Evidence in the 2026-11-03 (evening) run record.

## Resume testing — saved session state, updated 2026-11-03

Start a fresh session by reading [MASTER](MASTER.md), then
[IMPLEMENTATION](IMPLEMENTATION.md), then this section and the run records below.
MASTER row 16 remains in progress; this is a resumption aid for its existing
acceptance checklist, not a second work list. **Alpha.1 is not qualified.**
Facts below are the last recorded observations, not a fresh host check.

### Target and build snapshot

| Item | Last recorded state |
| --- | --- |
| Repository | `/home/mbv/work/eregion`, branch `master`; tested revisions `a77d3bf` (worker rescue), `13fdb25` (control-DB failure classification plus `host-journal-access-v1`) and `1e541a7` (retention CLI, migrations 017–019 and `host-registry-v2`), all with green CI and signed edge publication. `42a0541` and earlier builds are context, not qualification. |
| Tested CLI and main | `edge-1e541a7fad3e`, source `1e541a7`; the 2026-11-03 evening run upgraded `edge-13fdb2518757` -> `edge-1e541a7fad3e`. Earlier passes ran on `edge-13fdb2518757`, `edge-a77d3bfa4fe6`, `edge-42a0541c09d4` and `edge-ef91d70f6215`. Do not confuse a newer edge publication with an installed or qualified build. |
| Existing server | Context/node `main`, host `exploration`, Ubuntu 26.04 amd64; public IP `46.224.129.96`; main hostname `db2.mbvlabs.com`; operator `admin@10.78.0.1:22` through embedded WireGuard, endpoint `46.224.129.96:51820`. |
| Health | Doctor 19 pass / 1 warn / 0 fail at `edge-1e541a7fad3e`: `host.retention` warns because the automatic daily run cannot read the registry storage (defect 5); the healthy target remains 20 pass / 0 warn / 0 fail; second apply 0 applied / 19 skipped; schema 19/19, River 7/7; Caddy 2.11.4 (not restarted by any 2026-11-03 upgrade, apply or `host-registry-v2`); `net.ipv4.tcp_migrate_req=1` from `host-ingress-sysctl-v1`; admin reads the eregion journal without sudo. |
| Known open defect | **Defect 5:** the worker-run retention inventory cannot read `/var/lib/eregion/registry`, which `host-directories-v1` creates as `root:root 0700`, while `eregion-worker.service` runs as `eregion`; the automatic daily run fails `unavailable` "the node's registry storage is unreadable" and `doctor` warns on `host.retention`. Defects 3 and 4 are fixed and host-verified on `a77d3bf`/`1e541a7` (rescue in about 9s; the rescued fresh build then succeeded, evidence 240–246). |
| External database | PostgreSQL 17 on the operator machine, container `eregion-cp-pg`, host port 5433. Main reaches it through TLS-terminated Tailscale Funnel `thelab.tailbbed89.ts.net:10000`. No DSN belongs in this document. |
| Test app | `githello/production`, `APP_VERSION=v3`; web ×2 (`eregion-hello`, port 8080) and worker ×1 (`eregion-hello worker`, no port); release command `eregion-hello release`; health `/healthz`. |
| Serving release | `rel_b7099da06361a0bfaa8ee6866e178152`, source commit `3461a87`, deployment `dep_51d61163648e39d6adf4be5cdef005d3`, built and deployed by op_181c9da0f94c37ff6784c08201d366b1 on 2026-11-03 (the defect-4 rescue retest). Rollback target: `rel_b38312bd825f8c7e66ae3fe906148795` (image `5e277f663727`), deployed by op_5e277f6637276de54c3011fc1fc52a0f. Earlier: `rel_0170b1be56a64a747d522a2831cb02d8`, redeployed by op_4dc3431876c9f4f0e5caae4cd22d695f and op_6f4a7968f5823f71cd1003ec33a81293. |
| Source desired state | Private `https://github.com/mbvlabs/eregion-hello.git`, ref `main`, context `apps/web`, builder `paketobuildpacks/builder-jammy-base`, credential name `GIT_READ_TOKEN`. |
| Primary | `https://git-test1.mbvlabs.com/`, last recorded DNS-only, returns `eregion sample: v3 src3`. |
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
| TCP migration dense continuity | `124-*` through `127-*` |
| Release-changing dense switches, symlink escape, replay, failed-command rollback, failed-candidate cleanup, domain churn with worker kills, dev dry-run, detach/replay and cancel | `128-*` through `154-*` |
| CLI upgrade between two edge builds, signed node upgrades, CI endpoint path, worker-kill rescue, failed-manifest refusal, journal access | `155-*` through `207-*` (monitors `170`, `180`, `195`) |
| CLI upgrade to `1e541a7`, `host-registry-v2` and migrations 017–019, defect-4 rescue retest, retention policy/dry run and the retention-inventory defect | `208-*` through `277-*` (monitors `228`, `240`; summary `277`) |

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
| Guided create | **Not run:** guided `app create`/`env create` success and failure, ending at a serving URL or a failed operation; guided vs flag-only revision equivalence. Flag-only create and app-create replay passed. Needs DNS A records git-test3/git-test4 → 46.224.129.96 and an agent in a PTY. |
| CI endpoint | **Not run:** authenticated CI deploy; expired, revoked and wrong-environment tokens denied; nothing else of `/v1` reachable publicly. Needs defect 1 published and applied, plus a token created at a human terminal. |
| Push to deploy | **Not run:** signed push deploys; a redelivery does not deploy twice; another branch or a bad signature does nothing; after rotation the old secret fails. Needs `env update --auto-deploy` at a human terminal and a GitHub webhook on `mbvlabs/eregion-hello`. |
| Factor-gated ops | **Not run:** positive `token create`, `secrets export`, `secret export`, `app delete`, `env delete` (including deleting the last environment); cleanup and history verified. JSON/no-TTY refusals and deletion preview passed. Needs the human authenticator. |
| Login and contexts | **Not run:** successful second-machine login from the bundle, atomic failed replacement, wrong host key, context selection and `context remove`. Needs the bundle from `secrets export`, then an agent. Preserve the working main context and identity. |
| Upgrades | **Partial pass:** signed upgrade on main, unsigned/dev rejection and the dev-CLI dry-run version mismatch passed (`152-*`). `upgrade` between two edge builds passed on 2026-11-03 (`42a0541` -> `a77d3bf` -> `13fdb25`, then evening `13fdb25` -> `1e541a7`; previous-binary copy kept); a failed manifest download during a node upgrade refused with `unavailable` and no checksum-only fallback (`193-*`). Still **not run:** `install.sh` acceptance on Linux/macOS for these builds. |
| macOS | **Not run (no Mac available, R10):** `install.sh` on macOS and an installed CLI against the server. CI's macOS CLI job and the darwin/arm64 cross-build are not host acceptance. Required before v1.0.0 final; not an alpha.1 gate. |
| Disk retention (alpha.1 gate, R9) | **Partial pass (2026-11-03 evening, `edge-1e541a7fad3e`):** policy show/set and environment revision, `host-registry-v2` upgrade, prune dry run (plain and JSON; items with reasons and known bytes; no operation, release rows or Docker/registry state changed; second run identical), serving and rollback protection for githello and andurel, and the no-value refusal passed. **Blocked before the real prune by defect 5:** the worker's daily retention run fails `unavailable` "the node's registry storage is unreadable" (`/var/lib/eregion/registry` is `root:root 0700`, worker is `eregion`), so `doctor` is 19/1 not 20/0/0. Pruned-rollback refusal (dry run and real), registry GC, deploy after GC, disk-freed verification and 30-release retention remain **not run**. The 30-release row and the full baseline need a freshly initialized disposable server. |
| Fresh host | **Not run:** preflight and plan variants, init apply with the pinned Caddy, doctor gate / `--resume` / `--skip-doctor`, interrupted init and `--discard`, lockdown revert, handoff and break-glass. Needs an authorized fresh Debian 13 or Ubuntu 26.04 server and provider console access. |
| Release-command recovery | **Not run:** lost or unresolved response recovery variants. Once-per-release skipping, a changed command running again and restart/scale refusal after a failed run passed. Needs a scoped fault; stays an alpha.1 gate (R4). |
| Repair variants | **Not run:** competing or stale repair; a repair must never restore an old release when a deploy races it. Basic serving-container repair passed. Needs a scoped fault. |
| Final pass | **Not run:** the complete baseline (every row, including retention) on the candidate build, starting from a freshly initialized server (R3, R11). Earlier builds' passes are context, not qualification; the user authorizes `1.x` and the tag after the recorded pass. |

Defect 3 is fixed and host-verified on `a77d3bf`: a worker SIGKILL now rescues
the operation on worker start (9.0s measured, evidence 183; the earlier
observation was 30m21s, evidence 149/150). **Defect 4 is fixed and host-verified
on `1e541a7`:** the same rescue test now finds the git read credential on attempt
2, clones the private repository again and succeeds as
op_181c9da0f94c37ff6784c08201d366b1 (rel_b7099da06361a0bfaa8ee6866e178152;
evidence 240–246). The CI endpoint path (defect 1) is host-retested and passes;
the cleanup-summary fix is host-retested in a successful redeploy only, not with
a forced cleanup failure. The Caddy-reload reset fix is host-verified.

**Defect 5 (new, open):** a worker-run retention inventory cannot read
`/var/lib/eregion/registry`. `host-directories-v1` creates that directory
`root:root 0700`, while `eregion-worker.service` runs as `eregion`, so the first
daily run fails `unavailable` "retention did not complete: the node's registry
storage is unreadable" (op_c90b2c9a9a1e6f0430dd13aec337ab40, attempt 8 at
2026-11-03T21:18:27Z) and `doctor` reports `host.retention` warn. The operator
`prune --dry-run` succeeds only because its node RPC path runs as root, so it
masks the worker path. A real `prune` cannot execute until this is fixed; the
real prune was therefore not submitted. Missing prerequisites for the remaining
alpha.1 rows: that fix, an authorized fresh server, and human participation for
factor-positive operations. macOS is deferred (R10). Other unqualified variants
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
`edge-1e541a7fad3e` is the latest installed and tested build; earlier passes are
not automatically a full baseline pass for it. The 2026-10-29/30 runs closed
release-changing dense switches, symlink-escape rejection, same-key replay after
the branch was deleted, rollback refusal for a release whose command never
succeeded, failed-candidate leak recording, interleaved domain changes with
worker kills, the dev-CLI dry-run mismatch and terminal detach/replay/cancel.
They found defect 3 (worker SIGKILL left an operation running for about
30 minutes); the earlier 2026-11-03 run verified its rescue fix, retested the CI
endpoint path on the host, passed the between-two-edge-builds upgrade and the
failed-manifest fail-closed check, applied `host-journal-access-v1`, and found
defect 4 (a rescued deployment attempt loses the git read credential). The
2026-11-03 evening run upgraded to `1e541a7`, applied `host-registry-v2` and
migrations 017–019, host-verified defect 4's fix, and passed the retention
policy and prune-dry-run checks; it found defect 5 (the worker cannot read the
root-only registry storage), so the real prune and the rest of the retention
row remain unqualified. Source fixes have isolated regressions, build/vet and
darwin/arm64 cross-build evidence; published CI also ran race checks. These are
distinct from manual acceptance.

Failed deploy follows returned exit 5 on main, successful follows returned 0,
and the build-stop retest returned 130 with terminal cancellation and the prior
serving release preserved. Terminal Ctrl-C detach/replay and explicit cancel
passed (153/154); guided failure and the remaining output-mode variants remain
not run. Complete and record the remaining baseline for the actual release
candidate, from a freshly initialized server (R11), before asking the user to
authorize `1.x` or `v1.0.0-alpha.1`.

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
Date / build / commit / channel: 2026-10-29 / edge-42a0541c09d4 / 42a0541 / edge
CI (race, integration, macOS CLI) and edge publication: pass
CLI upgrade edge-ef91d70f6215 -> edge-42a0541c09d4: pass
apply --auto-upgrade-nodes: pass; applied host-ingress-sysctl-v1 plus the
  api/worker restarts and readiness for the new binary (4 of 18); Caddy not
  restarted (active since 11:38 UTC). Second apply 0 of 18. Doctor 18/0/0;
  schema 16/16, River 7/7. Kernel value 1; boot file present.
Dense continuity retest (redeploy rel_07d1, six fresh-TLS loops on the
  direct primary plus one loop on the proxied alias):
  op_e63ac7b9a3dccba70827ac4e93ff1315 1200/1200 + 280/280,
  op_5409d44112c85916e41f0cab6fc9d08e 1366/1366 + 338/338,
  op_3757d4167c88fa5a17c703ee525f4286 1063/1063 + 269/269,
  op_d5f8122c1dee93272e3afc0360afb339 1389/1389 + 357/357; each run switched
  Caddy once. Zero failures (previous build: 2-3 resets per switch at four
  loops). Caddy reload reset fix: PASS on host.
Evidence: ~/.cache/eregion-run/124-* through 127-*.
Remaining: as in the previous record, minus the TCP migration retest.
```

```text
Date / build / commit / channel: 2026-10-30/31 / edge-42a0541c09d4 / 42a0541 / edge
Operator OS/arch: linux/amd64; CLI and main edge-42a0541c09d4
Server: existing main, Ubuntu 26.04 amd64; schema 16/16, River 7/7; doctor
  18/0/0 and second apply 0/18 before the run; no upgrade during it
App: githello/production; git-test1 direct, git-test2 Cloudflare-proxied
Release-changing dense switches (128-131; six fresh-TLS primary loops plus one
  alias loop): fresh build op_4da4ef81457eb47fc41e19bad2a70755 (commit 3461a87,
  release rel_0170b1be56a64a747d522a2831cb02d8) 4,597 primary + 967 alias
  responses (5,564 including the pre-switch phase); release redeploy
  op_4dc3431876c9f4f0e5caae4cd22d695f 1,150 + 245; rollback to
  rel_07d1c0fb27cdc0d68c7e5815a56e9328
  op_51c9ca8c40a26b61d559383bf6183866 1,049 + 227. 7,673/7,673 responses
  during the three switch windows (8,235/8,235 including the pre-switch
  phase), no resets; bodies changed v3 <-> v3 src3 at the switches. A preceding
  rollback op_ce09a4dbb762c46375677f4a19c8b6c9 also succeeded (24.4s, 61
  events). PASS.
Symlink escape (132-133): relative symlink op_e70ba096d8386648f75e73fb63025c49
  and absolute symlink op_30fe551ab69119ebc83d3eecb1226118 both failed
  precondition_failed naming context path "apps/web" absent/outside; previous
  release kept serving. PASS.
Same-key replay after branch deletion (134-136): key qual-replay,
  op_f82e084105cafb638dfd52f6b7106445 succeeded (release
  rel_ab664af01d2828346c363defe223e01e); replaying the same key returned the
  same succeeded operation without another deploy (135); a new key was refused
  because the branch was gone (136, precondition_failed). PASS.
Failed release-command rollback (137-140): env update to command `false`
  op_d6e7c1e706c0f9d51699e5caf753b888; deploy
  op_af19655248dabe3766a06b28b52c4a89 failed at platform-command-01-v1 (exit
  status 1) and rel_ab664af01d2828346c363defe223e01e kept serving; rollback to
  rel_73e26b4973dd4d1deef36aed0e8ab334 was refused at submission
  (precondition_failed, "release command has not succeeded for this release");
  command restored op_d595b0296338811caf53a97477b323a3. PASS.
Failed candidate with removal failure (141-146): a temporary docker wrapper
  failed only the candidate web-1 removal (30-minute guard); FAIL_HEALTH config
  op_0cccf6cb8ab9b041e0287871a0f2c385; deploy
  op_308fae9fa06fe611113e1bb90381fbf7 failed health 503 and the previous
  release kept serving; its cleanup step failed with the injected error and
  144 showed pending_cleanup ["githello-production-web-1-037149ee58d2"].
  Fault removed 09:04:26; FAIL_HEALTH unset
  op_c34b332d6bfab5d8880bb2114d4ac91f. Candidate-cleanup warning was correct,
  but the final summary still said "candidate cleanup completed" (defect 2).
  No post-removal env status was saved, so the later maintenance removal was
  not captured in 141-146. PASS for isolation and leak recording; removal not
  evidenced.
Interleaved domain changes and worker kills (147-150): add git-test3
  op_adab749ca783474b144b1c08b8414ead, add git-test4
  op_79bb8ebc76a8ba5c57e1705e40b165b7, remove git-test3
  op_53147c6f3d81cd1b7d2d2d96a917d081, remove git-test4
  op_20732130eeeb39f4262243381bd62da8 (follow not saved), re-add git-test3
  op_f0d20fed6c26eb6bd138c84bd211e170. Monitor 147: 7,074/7,074 HTTP 200
  (3,537 primary + 3,537 alias). SIGKILLs at 09:06:50 and 09:07:21 left the
  re-add running until 09:37:39 (elapsed 30m21s), then it succeeded. DEFECT 3.
Dev-CLI dry-run mismatch (151-152): `apply --dry-run --auto-upgrade-nodes`
  refused node_version_mismatch and `server status` refused with the same code;
  node binary SHA-256, size and service start timestamps were unchanged
  before/after (152-node-*). 151 config set
  op_cf611159edfef3dc7880f943aadb1455 succeeded; no lock left. PASS.
Detach and cancel (153-154): real-terminal Ctrl-C detached from
  op_6f4a7968f5823f71cd1003ec33a81293 after 39 events, exit 130, with the
  follow hint; `operations follow` replayed it to success in 26.8s with
  rel_0170b1be56a64a747d522a2831cb02d8 serving. Explicit `operations cancel`
  op_39546ea45e1125630c1d8011a093f39e reached cancelling at sequence 14 and
  cancelled at a step boundary (19.6s); rel_0170b1be56a64a747d522a2831cb02d8
  still served. PASS.
Live `build logs --follow`: streamed in a real terminal; no numbered evidence
  file or operation ID was saved. PASS observed; evidence gap.
Defects:
  1. Caddy's public path `/v1/environments/*/deployments` let `*` match one
     segment, so `/v1/environments/<app>/<env>/deployments` returned Caddy's
     404. Fixed in 2943e18 with `*/*` and a matcher regression test; not yet
     host-retested on main.
  2. A cleanup failure was summarised as "candidate cleanup completed".
     Observed in 143 (cleanup warning plus "completed" summary) and 144
     (pending_cleanup). Fixed in 4e827aa: cleanup failures are warning events
     naming the containers and failed state is retained; not host-retested.
  3. A worker SIGKILL leaves its platform operation running until River's
     rescue, about 30 minutes (30m21s observed on op_f0d2...). Fix in progress
     for alpha.1 (rescue this host's orphaned platform jobs on worker start).
Final state: rel_0170b1be56a64a747d522a2831cb02d8 serving; git-test3 re-added,
  git-test4 removal accepted without a saved follow; no post-session env status
  was captured. No injected fault, wrapper or guard timer was left; no upgrade
  ran during the session.
Evidence: ~/.cache/eregion-run/128-* through 154-*.
Remaining: the full baseline on the candidate build from a freshly initialized
  server (R3, R11), including guided create, CI, push to deploy, factor-positive
  operations, login/contexts, retention and the other open rows.
```

```text
Date / build / commit / channel: 2026-11-03 / edge-a77d3bfa4fe6 and
  edge-13fdb2518757 / a77d3bf and 13fdb25 / edge
CI and publication: a77d3bf CI and Publish edge pass (run 41691456825);
  13fdb25 CI and Publish edge pass (run 41692644196). Publication of the two
  later master commits was not used by this run.
Operator OS/arch: linux/amd64. CLI upgrade edge-42a0541c09d4 ->
  edge-a77d3bfa4fe6: pass, previous binary kept at ~/.local/bin/eregion.prev
  (reports edge-42a0541c09d4). CLI upgrade edge-a77d3bfa4fe6 ->
  edge-13fdb2518757 with `upgrade --version` for the second-build check: pass,
  previous binary reports edge-a77d3bfa4fe6.
Server: existing main, Ubuntu 26.04 amd64; before the run CLI and node
  edge-42a0541c09d4, schema 16/16, River 7/7, doctor 18/0/0.
Safe first checks (155-163): pass. Context main reaches admin@10.78.0.1 over
  WireGuard; server status role=main, all five services active, 57.4 GiB free;
  githello/production serving rel_0170b1be56a64a747d522a2831cb02d8 with web x2
  and worker x1; git-test1 200 "eregion sample: v3 src3", git-test2 200
  "alias ready". Only the leftover git-test3 alias remains from the 2026-10-30
  domain churn (dns=wrong, certificate/public pending); git-test4 is gone.
Node upgrade dry run (42a0541 -> a77dbf): pass, exit 5, node_version_mismatch
  "would upgrade node ..."; node binary sha256/size/mtime and API, worker and
  Caddy start timestamps identical before and after (169 vs 171).
Node upgrade (`apply --auto-upgrade-nodes`): pass, exit 0, 18:17:36-18:17:51,
  op_74a0c28c3ed7ca35e57c297fbf6a8419, 4 applied of 18: host-ingress-v2 (the
  CI path matcher fix; Caddyfile now /v1/environments/*/*/deployments), API and
  worker restarts, readiness. Node now edge-a77d3bfa4fe6. Caddy was reloaded
  by the ingress step, not restarted (caddy.service ActiveEnterTimestamp stayed
  2026-10-29 11:38:24 UTC). Monitor 170: 306/306 HTTP 200 (102 each on
  git-test1, git-test2 and andurel's eregion-test), zero failures.
Second apply: pass, op_5b4b1b9f317d0c2673349d350a796d5d, 0 applied of 18, no
  step executed. Doctor: 18 pass / 0 warn / 0 fail.
CI endpoint retest on the main hostname db2.mbvlabs.com (read from the managed
  Caddyfile, 177-179): GET
  /v1/environments/githello/production/deployments returns HTTP 405,
  allow: POST, with the API's {"error":"use POST ...","code":"usage_error"}
  (not Caddy's 404); POST without a token returns 401 unauthenticated "exactly
  one Bearer token is required"; /v1/apps returns Caddy's 404. Defect 1 host
  retest: PASS. No deploy was submitted on the CI path.
Defect 3 retest (180-183): deploy op_d0f76b56af719f63cb8141fa0c6f61c4 (fresh
  build of 3461a87) submitted 18:21:23; eregion-worker.service SIGKILLed at
  18:21:41 (systemd "on client request"); restart 18:21:46; the new worker
  logged "Recovered orphaned platform jobs before worker start" with
  requeued=1 at 18:21:48; operations follow showed the warning "Recovered
  orphaned platform job 167151 left running by an earlier worker process on
  this host" at 18:21:48.660; attempt 2 started at 18:21:50.543. Rescue latency
  about 9s, not 30 minutes. PASS for defect 3.
  The rescued attempt then FAILED at 18:21:54 with precondition_failed
  "git read credential is unavailable": bld_fc680448ac7db566b4d0dae3cd5a15f6
  and rel_53a22cfea2e1f3a2fa9dd523ac4c5510 failed; rel_0170b1be... kept
  serving; follow exit 5. This is defect 4 (below), not a recurrence of the
  30-minute stall. A fresh deploy of the same revision,
  op_5e277f6637276de54c3011fc1fc52a0f, succeeded 18:23:25-18:25:19 and built
  bld_48d3acf6d2555726880f191d12a5d1bc -> release
  rel_b38312bd825f8c7e66ae3fe906148795, deployment
  dep_7f256e534322e9bf0fc627047c5523d4. So the credential itself is intact;
  monitor 180: 1,773/1,773 HTTP 200, zero failures.
Failed-manifest check (a77dbf -> 13fdb25, 188-194): pass. The CLI was moved to
  the exact signed edge-13fdb2518757; `apply --dry-run --auto-upgrade-nodes`
  refused node_version_mismatch. Temporary scoped fault: /etc/hosts on main
  mapped eregion.mbvlabs.com to 127.0.0.1, with a copied backup and a
  10-minute systemd-run restoration guard. The node's resolver returned
  127.0.0.1 and its manifest fetch failed. `apply --auto-upgrade-nodes` then
  failed closed: exit 6, unavailable "release URL is unreachable", naming
  https://eregion.mbvlabs.com/releases/edge-13fdb2518757/manifest.json. The node
  binary sha256/size/mtime and API, worker and Caddy start timestamps were
  identical to before the attempt, and no /tmp/eregion-* upload was left; there
  was no checksum-only fallback. The fault was removed (original /etc/hosts
  restored, backup deleted, timer stopped, marker count 0) and the manifest
  fetch returned 200 again.
Completed upgrade (`apply --auto-upgrade-nodes`): pass, exit 0, 18:28:32-18:28:46,
  op_f9559862af54228b02167a95f2ff3615, 4 applied of 19: the new
  host-journal-access-v1 (admin added to systemd-journal), API and worker
  restarts, readiness. Node now edge-13fdb2518757, binary sha256
  aefec7b9f65ccbb94e5766e6e5cd44d2d2edb0f5d87226615421a7a2a540d7e9, exactly
  the signed manifest's linux/amd64 artifact. Caddy was not restarted (same
  ActiveEnterTimestamp). Monitor 195: 300/300 HTTP 200, zero failures.
  Second apply op_3a20c78b82b65cb6054c8ef72a1f0432: 0 applied of 19. Doctor:
  18 pass / 0 warn / 0 fail (the new step adds no doctor check). As admin,
  `journalctl -u eregion-worker.service` and `-u eregion-api.service` now
  return entries without sudo, exit 0 (199).
Defects:
  1. Fixed in 2943e18 and now host-retested: the API's 405/401 replace Caddy's
     404 on the CI deploy path; /v1/apps stays Caddy's 404.
  3. Fixed in a77d3bf and host-verified: SIGKILL rescue in about 9s with a
     warning event naming the orphaned job (was 30m21s).
  4. NEW, open: a rescued/retried deployment attempt that already recorded its
     deployment secret snapshot loses the git read credential and fails at the
     build step (op_d0f76b56af719f63cb8141fa0c6f61c4 attempt 2,
     "git read credential is unavailable") although GIT_READ_TOKEN revision 1
     is active and a fresh deploy of the same revision succeeded. The snapshot
     pins include the credential, but platform_deploy.go only resolves the
     credential into buildSecrets when len(dep.SecretRevisions)==0 and discards
     the resolver's credential value on the snapshot path, while the credential
     is not in the revision's secret_names. No code was changed.
Final state: rel_b38312bd825f8c7e66ae3fe906148795 serves githello/production
  (dep_7f256e534322e9bf0fc627047c5523d4, health healthy); git-test1 and
  git-test2 route=applied, certificate=valid, public=ok; git-test3 remains
  dns=wrong/pending; no pending cleanup; CLI, node and publish line
  edge-13fdb2518757; schema 16/16, River 7/7; doctor 18/0/0; no injected
  fault, hosts backup or guard timer left.
Evidence: ~/.cache/eregion-run/155-* through 207-* (monitors 170, 180, 195;
  rescue journal 183; blocked manifest 193; node before/after 190/193/198).
Remaining: defect 4; the full baseline on a freshly initialized server (R3,
  R11), including guided create, an authenticated CI deploy, push to deploy,
  factor-positive operations, login/contexts, retention and the other open
  rows. The Upgrades row now has the two-edge-build upgrade and failed-manifest
  checks; `install.sh` on Linux/macOS and macOS remain not run (R10).
```

```text
Date / build / commit / channel: 2026-11-03 (evening) / edge-1e541a7fad3e /
  1e541a7 / edge
CI and publication: 1e541a7 CI and Publish edge pass (runs 41718550479,
  41718550856).
Checks: safe first checks pass; CLI upgrade pass; apply dry run expected
  node_version_mismatch; apply/node upgrade pass; defect 4 retest pass;
  retention policy pass; prune dry run pass; real prune blocked (defect 5);
  second apply pass; doctor 19/1 (not the expected 20/0/0).
Operator OS/arch: linux/amd64. CLI upgrade edge-13fdb2518757 ->
  edge-1e541a7fad3e: pass; previous binary kept at ~/.local/bin/eregion.prev
  (reports edge-13fdb2518757). CLI upgrade happened before any node mutation.
Server: existing main, Ubuntu 26.04 amd64; before the run CLI and node
  edge-13fdb2518757, schema 16/16, River 7/7, doctor 18/0/0.
Safe first checks (208-219, 222-223): pass. Context main reaches
  admin@10.78.0.1 over WireGuard; server status role=main, all five services
  active, 56.8 GB free; githello/production serving
  rel_b38312bd825f8c7e66ae3fe906148795 with web x2 and worker x1;
  andurel/production serving rel_801151fb866e89e87e017fe9c5c0042c; git-test1 200
  "eregion sample: v3 src3", git-test2 200 "alias ready", andurel 200; TLS
  verified with the default CA store (no -k) before, during and after.
  Pre-state: 18 githello and 10 andurel releases, none pruned; registry
  du 312,003,409 bytes; 26 Docker images; Caddy start 2026-10-29 11:38:24.
Apply dry run (229): expected refusal, exit 5 node_version_mismatch "would
  upgrade node" - a dry run never installs a node binary.
Apply and node upgrade (230-233): pass, exit 5 confirmation_required after
  20.35s, op_502d5afadf4f948aaeca090418483413, because host-registry-v2 changes
  the registry unit/access. Confirmed on the node at 21:18:16.917 before the
  21:18:22.735 deadline (231). host-registry-v2 applied once: config
  delete.enabled true, unit REGISTRY_STORAGE_DELETE_ENABLED=true, registry
  restarted 21:16:55. host-database-v1 applied migrations 017-019 (schema
  19/19, River 7/7), API and worker restarted, readiness passed. Node now
  edge-1e541a7fad3e, binary sha256
  e2c3abb1fc5cebe3f879b8b8c300aee21e8bcaf1890648b8e1f342df85091711. Caddy was
  not restarted (ActiveEnterTimestamp stayed 2026-10-29 11:38:24 UTC).
  Monitor 228: 633/633 HTTP 200 across the dry run, apply, confirmation and
  second apply (211 per URL), zero failures; no public request depended on the
  registry restart.
Second apply (234): pass, no operation ID printed, 0 applied / 19 skipped, 5.78s.
  Doctor (230, 275): 19 pass / 1 warn / 0 fail. host.retention WARN: the
  startup daily run failed at 21:18:26 with 1 error. host.build_cache pass with
  unknown (no successful measurement). The no-run-yet host.retention pass was
  not observable: the startup catch-up submitted op_c90b2c9a… at 21:17:09.
  This is defect 5, not a 20/0/0 pass.
Defect 4 retest (240-246): pass. Deploy op_181c9da0f94c37ff6784c08201d366b1
  submitted 21:21:10; fresh git build started 21:21:19; worker SIGKILLed
  21:21:29; systemd restarted it 21:21:37; rescue logged at 21:21:39
  (requeued=1), warning event at 21:21:38.904 naming orphaned job 169781;
  attempt 2 started 21:21:40.980, retried platform-pack-01-v1, cloned the
  private repository again at 21:21:47.874 (git read credential available) and
  succeeded at 21:22:59.464 as rel_b7099da06361a0bfaa8ee6866e178152 /
  dep_51d61163648e39d6adf4be5cdef005d3. Monitor 240: 423/423 HTTP 200
  (141 each), zero failures. Defect 4 is fixed and host-verified.
Retention policy (236-252, 274): pass. Server default show is scope=server with
  keep_releases 10, keep_newer_than_days 14, artifact_grace_days 14, cache
  10 GiB/80%, every source server. githello override set keep-releases 3,
  keep-days 1, artifact-grace-days 1: accepted op_ba80a58d26ba832bf5325b022878cb1a,
  succeeded, new environment revision erv_b8f5163889b07167f955396f2f5da061
  number 15. Show marks exactly the three release fields environment and both
  cache fields server. Flagless set with --json refuses exit 2 input_required
  without prompting (248, 274).
Prune dry run (253-267): pass. Plain 258, JSON 259, repeat 265 identical.
  30 items: 16 release (all githello), 5 image, 6 image_skipped, 1
  registry_skipped, 2 dangling_image; planned_bytes 1,355,246,769 (only the
  dangling images carry bytes; release rows have no bytes and image items do
  not attribute shared-layer bytes). Kept: githello serving
  rel_b709 and rollback rel_b383 (image 5e277f663727), andurel serving rel_801
  and rollback rel_a68 (digest 56166f); no andurel items. The plan lists the
  older rel_0170 because it is no longer the rollback target after the new
  deploy. No operation accepted (108 before/after, 256/266); githello and
  andurel release rows and pruned_at unchanged; Docker 27 images / 9 containers
  unchanged; registry du 312,003,409 unchanged and /v2 200; /var/lib/eregion
  at 27% used before and after. The plan was safe to execute.
Real prune (5c): NOT RUN - blocked by defect 5. The automatic daily retention
  op_c90b2c9a9a1e6f0430dd13aec337ab40 failed its 8th attempt at
  21:18:27.076 with unavailable "retention did not complete: the node's
  registry storage is unreadable". /var/lib/eregion/registry is root:root 0700
  (host-directories-v1); eregion-worker runs as uid 999 eregion and
  `sudo -u eregion test -r` is false (233, 238). The operator dry run works
  because its node RPC path runs as root, masking the worker path
  (retention_data.go retentionInventory). A real prune cannot plan until the
  worker can read the registry, so pruned-rollback refusal, registry GC,
  deploy-after-GC, disk-freed verification and the 30-release row are not run.
Second apply after the deploy (270-273): pass, 0 applied / 19 skipped, 9.98s;
  schema 19/19; node and CLI edge-1e541a7fad3e; Caddy timestamp unchanged;
  registry config/unit persisted. Doctor (271) remains 19/1 because of defect 5.
Defects:
  4. Fixed in 1e541a7 and host-verified: a rescued deployment attempt now finds
     the pinned git read credential and finishes the private clone/build
     (op_181c9da0..., attempt 2; 240-246). No code was changed in this run.
  5. NEW, open: worker-run retention cannot read the root:root 0700 registry
     storage; the daily run fails unavailable and doctor warns. The real prune
     and all checks after it are blocked. The operator surface's root RPC path
     hides the mismatch. Needs a release fix (worker-readable registry storage
     or a root inventory path), not a host chmod.
Final state: rel_b7099da06361a0bfaa8ee6866e178152 serves githello/production
  (dep_51d61163648e39d6adf4be5cdef005d3, health healthy); git-test1 and
  git-test2 route=applied, certificate=valid, public=ok;
  andurel/production still serving rel_801151fb866e89e87e017fe9c5c0042c; no
  pending cleanup; no injected fault left; CLI/node edge-1e541a7fad3e; schema
  19/19, River 7/7; doctor 19/1.
Evidence: ~/.cache/eregion-run/208-* through 277-* (summary 277; monitors 228,
  240; apply 230/231/234/270; rescue 242-244; retention failure
  232-233/238/268/275).
Remaining: defect 5; the real prune, pruned-rollback refusal, registry GC,
  deploy-after-GC, disk-freed verification and 30-release retention; then the
  full baseline on a freshly initialized server (R3, R11), including guided
  create, an authenticated CI deploy, push to deploy, factor-positive
  operations and login/contexts. `install.sh` on Linux/macOS and macOS remain
  not run (R10).
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
