# eregion: Initial Research

**Date:** 2026-10-16
**Status:** Planning — research findings with agreed product direction
**Purpose:** Capture the analysis behind rebuilding deploycrate-ce as an agent-first
platform using eru's CLI, transport, and safety patterns, plus PostgreSQL and River
for durable control-plane state and execution.

Sections 1–3 describe the source projects as researched, not requirements for
eregion. Sections 4–7 define the revised target architecture and roadmap. Where they
differ, the target design takes precedence over a literal port of either source.

### Agreed direction at a glance

Execution planning now lives in [the implementation roadmap](roadmap.md), with
[terminal design direction](tui-design.md) and [interactive TUI sketches](design/tui-sketches.html).

- **Agent first, human second.** Complete, discoverable, noninteractive operation is
  the primary contract; rich human terminal experiences layer on top.
- Support **Debian and Ubuntu**; exact supported releases remain to be selected.
- Provide an **early server-side API** for CI/CD deployment triggers and automation;
  a web UI is deferred.
- Require an **external PostgreSQL database** for control-plane state and use
  **River** for background jobs, following deploycrate-ce. No SQLite or NATS
  JetStream in the initial design.
- Design for multiple control-plane instances and role-focused **control-plane,
  application, and build servers**. Shared PostgreSQL is necessary but not
  sufficient for HA.
- Expose telemetry primitives and easy integrations with external observability
  stacks. **Do not bundle ClickHouse for v1.**
- Explore the CLI's full potential with **Charm libraries**, live views, and graphs
  without compromising the agent contract.

Sources examined:

- `/home/mbv/Work/deploycrate-ce` (source of functionality)
- `/home/mbv/Work/eru` (source of architecture/patterns)
- Target repo: `/home/mbv/Work/eregion` (this repo, currently empty)

---

## 1. Project statements

### deploycrate-ce — what it is today

A self-hosted PaaS / deployment platform. Rails-like Go web app scaffolded with
Andurel (`github.com/mbvlabs/andurel`), fx dependency injection, Echo v5 + gonertia
(Inertia v2) + Svelte 5/Tailwind 4 frontend, templ for error views, Bun/pgx against
PostgreSQL 17, River background jobs, ClickHouse for logs/metrics rollups, OpenTelemetry
throughout. Releases are keyless Sigstore/cosign signed, published to Cloudflare R2
(`ce-stable.deploycrate.com`, `ce-edge.deploycrate.com`), amd64+arm64.

It bootstraps a fresh Debian 13 VPS into a single-server control plane, supports adding
worker Node servers (user-owned VPSs it does *not* provision), builds Go apps via Cloud
Native Buildpacks (`pack`), ships them as Docker images to a managed OCI registry, and
runs them across environments behind Caddy with blue/green self-updates.

### eru — what it is

A single-binary cobra operator CLI for running a hardened PostgreSQL cluster across
nodes, built CLI/agent-first. Its architecture is the target model for eregion:

- Declarative command registry (`internal/cli/tree.go`, ~1400 lines) with per-command
  `CommandMeta` (audience, mutating, destructive(+flags), confirm tokens, dry-run,
  prompts, platforms, needs_restart, error_codes, next, when_to_use, examples).
- `meta.go` statically validates the whole tree at init; `eru commands` fails the
  listing on any metadata violation (build-gate pattern).
- Output envelope `eru/v1` with human|json|agent|markdown modes + `--jq`; failures are
  `{error:{code,message,hint,details}}`; per-command `Next` steps auto-appended via
  `output.CommandNext`.
- Userspace WireGuard (wireguard-go + gVisor netstack — no TUN, no root) → SSH with
  pinned host keys → `eru node rpc`: single `eru-rpc/v1` request line on stdin, NDJSON
  streamed events (step/log/progress/warning/result) + one result event.
- Engine: Check/Apply steps, journal at `/var/lib/eru/journal.json`, inputs hashing,
  unified-diff plan preview, one aggregated restart per apply behind a confirm token.
- Secrets: age passphrase bundle (secretsdoc), operator-side generation, client-side
  SCRAM verifier construction, longest-first redaction registry at all output paths.
- Config: TOML desired state on the node, CAS-protected writes (`base_revision`),
  immutable keys, plan preview in the same request.
- Upgrades: channel resolution via signed manifests, operator pushes node binaries.

---

## 2. deploycrate-ce — detailed findings

### 2.1 Architecture

Monolithic Go module `deploycrate-ce` (Go 1.26.5). Layout:

| Dir | Role |
| --- | --- |
| `cmd/` | `app` (web + job runner), `bootstrap` (installer CLI + `host-resource-access` privileged dispatcher), `seeds` |
| `config/` | Env-backed configuration structs via `caarlos0/env`, fx module |
| `clients/` | buildpacks, caddy, clickhouse, cloudflare, container, email, firewall, github, objectstorage, postgresql, prometheus, registry, resourceaccess, resourcehealth, ssh, wireguard |
| `controllers/` | HTTP handlers per resource + `api/` subpackage |
| `services/` | Business logic (~130 files) |
| `models/` | Bun entities + domain logic/validation + factories |
| `queue/` | River processor, periodic jobs, `jobs/`, `workers/` |
| `router/` | Echo router, typed `routes/`, cookies, middleware |
| `database/` | Connection setup + embedded SQL migrations (00001–00089) + clickhouse migrations |
| `internal/` | setup, resourceaccess, hostcommand, nodeinstall, releaseauth, secretcrypto, telemetry, storage, sudo, wireguard, validation, hypermedia, inertia, routing, server, buildpacks |
| `scripts/` | install.sh, build-release.sh, node install scripts |

### 2.2 Entry points

1. **`cmd/app`** — web server; production runs an *independent* River job runner
   (`deploycrate-ce jobs`) so slot stops can't cancel in-flight work. Other
   subcommands: `version`, `migrate` (PG + ClickHouse), `backups activate`,
   `host-resource-access`.
2. **`cmd/bootstrap`** — transient installer: `install [--dry-run]`, `resume`,
   `logs`, `ssh-ca recover`, `node install --manifest-stdin`, `version`,
   `host-resource-access`. Deleted after successful install.
3. **`cmd/seeds`** — dev seed runner.

### 2.3 The installer (control-plane setup steps)

From `internal/setup/steps.go` (`DefaultSteps`), each step = check+apply script from
`internal/setup/scripts/`:

1. `host-packages` — baseline host packages
2. `server-users` — admin + service account, SSH keys
3. `backup-tools-0-18-1` — pinned Restic + manifests
4. `ssh-ca-recovery-v1` — SSH CAs + age-encrypted recovery bundle
5. `host-safety` — journald, fail2ban, swap
6. `wireguard` — WG mesh interface
7. `node-exporter-1-11-1-netlink-v1` — bound to WG address
8. `docker-29-6-2-telemetry-log-routing-v1` — pinned docker/containerd/buildx/compose, journald log driver
9. `cadvisor-0.57.0-docker-attribution-v4`
10. `clickhouse-25-8-28-1-observability-v2`
11. `clickhouse-migrations-v1`
12. `otel-collector-0.157.0-isolate-environment-logs-v1` (issues telemetry identity/JWKS)
13. `prometheus-3-13-1-service-metrics-v4` (24h raw retention)
14. `buildpacks-0-40-6-cache-v1` — pack CLI + warmed builder/run image
15. `database-resource-ownership-v2` — local PG or external PG validation
16. `application-binary` — release binary into blue slot
17. `application-config-backups-v1` — protected app env file
18. `database-migrations` — embedded pg migrations
19. `application-admin` — create/update admin user
20. `application-service-caddy-2-11-4-system-slice-v2` — blue/green systemd slots + Caddy
21. `health-check` — poll `http://127.0.0.1:8080/api/health` 30×2s
22. `control-plane-topology-capabilities-v1` — commit bootstrap topology (DB rows) + Caddy route
23. `ssh-hardening` — SSH port/firewall
24. `service-health` — verify services/listeners/active slot

Bootstrap DB topology (`services/bootstrap.go`): system application + Production/
Development envs, system-managed Resources (control-plane PostgreSQL w/ endpoint +
credential + admin credential reconciliation, "DeployCrate Telemetry" opentelemetry
Resource with local+WG OTLP endpoints, "DeployCrate CE Registry" distribution image,
buildpack secret), system private network, control-plane WireGuard peer, Caddy route,
encrypted S3 backup credential payload, capability/version records.

### 2.4 Node install

`internal/nodeinstall/scripts/node-install.sh` — single idempotent script: Debian 13
gate → apt base → persistent journald + tuned docker daemon.json → `admin` + `deploycrate`
users → SSH user-CA trust → wg0 conf + peers from manifest → pinned node_exporter,
cAdvisor, otelcol-contrib (inline generated config with OIDC auth + node token) → UFW
rules → systemd enable → capability dirs → pack CLI if `capability_build` → health
verification → deletes installer binary, prints single-line JSON result.

Manifest (v4) schema/validation: `internal/nodeinstall/manifest.go` — server id, node
name, private address (CIDR pool from `internal/wireguard`), listen/ssh ports,
control-plane WG key/endpoint, node peers, SSH user CA pubkey, OTLP endpoint, telemetry
issuer/JWKS/node token, capabilities.

### 2.5 App deployment flow (end-to-end)

- Create app: `services/application_setup.go` (`Create`, `CreateApplication`).
- Create env + wizard: `CreateEnvironment`, `DeleteIncompleteEnvironment`.
- Source: `EnvironmentSource` kinds — `github`+`git`, `registry`+`image`.
- Buildpack config: runtimes `go|rails|laravel|django`, buildpack scripts/versions,
  process formation, health path (`EnvironmentSetup.Complete`).
- Trigger build: `EnvironmentSetup.StartBuild/StopBuild/RetryBuild`; GitHub push →
  `services/github_webhook.go` (signature verify → Change kind `github_push`/`build`)
  → River job `build_source`.
- Build execution (`services/build_execution.go`): `loading_source` →
  `preparing_workspace` → `downloading_source` → `loading_registry` → `building_image`
  (pack on node over SSH) → `resolving_artifact` → creates `Release` + state revision.
- Release orchestration (`services/release_deployment.go`): release-command jobs first
  (one-off `release` container), then per-target `deploy_release` jobs.
- Deploy execution (`services/deployment_execution.go`): steps recorded to
  `deployment_events` — `preflight` → `resolving_secrets` → network reconcile +
  resource-container connections → `docker_candidate` (pull image) →
  `service_candidates` → `health_check` → `worker_candidates` → `traffic_switch`
  (blue/green Caddy swap) → post-switch health → `markSucceeded` → `cleanup`.
- Deploy controls: queue manual/retry/stop/restart, source deploys, release deploys,
  promote to production.
- Secrets: revision-based (`environment_state_revisions` + `environment_target_states`
  desired/applied), bulk create/rotate/archive, export to production.
- HTTP access: `public|basic_auth|private_network` access modes.
- Domains/DNS: domain aliases, env DNS mode `manual|cloudflare`, Throught CaddyRouteService.

### 2.6 Change / task / attempt model

- `changes` — durable, per-environment sequence-numbered domain mutation record:
  `sequence`, `kind`, `trigger_type` (user/webhook/system/manual/installer/api), `actor`,
  `correlation_id`, status machine (`pending/committed/running/completed/failed/
  cancelling/cancelled`), `corrects_change_id` for corrective flows.
- `change_items` — per-subject intent (`action`, `subject_type/id`, previous/requested
  JSONB values).
- `change_tasks` — durable queue-of-record work units tied to a change (`kind`,
  subject, `idempotency_key`, `input`, `attempt_count`, `available_at`, `parent_task_id`,
  `server_id`, `environment_target_id`).
- `change_task_attempts`, `change_logs` — schema exists, **dormant** (unwired).
- `change_releases` — change↔release association.
- Observed change kinds: `environment_setup, deploy, manual_deploy, build, redeploy,
  environment_update, github_push, deployment_retry, resource_projection,
  deployment_recovery, system_update, backup, backup_execute, database_restore,
  resource_restore, bootstrap`.

### 2.7 Node management

- Routes: `nodes.index/new/create/show/confirm/retry/capabilities.update`.
- Enrollment (`services/node_enrollment.go`): SSH host-key probe, WG node-address
  allocation, server row (`kind=worker`), encrypted SSH private key + passphrase,
  Confirm (host-key fingerprint), Retry, Execute (River job): remote install (manifest),
  verify identity, persist OS/dist/arch, create `WireGuardPeer`, apply peer on control
  plane, `node-telemetry-target`, transition `ssh_ca_verification` → SSH user-CA
  verification (sudo + metrics reachable over WG), harden sshd, `reconcileNodeMesh`
  (push peer config to all nodes), ensure `ServerNetwork`, `CompleteTrustTransition`.
- Enrollment state machine: `awaiting_confirmation → queued → installing → verifying →
  ready | failed`.
- Capabilities: `build | runtime | resource | database | repository | telemetry` (+ default
  Go buildpacks runtimes). Host provisioning: `services/server_management.go`
  `ProvisionCapabilities`.
- Host ops: containers control/logs, image remove, prune, reboot, OS updates
  check/apply via `internal/resourceaccess/host.go` sub-operations (`sudo -n` re-exec).

### 2.8 Resources (managed databases/services)

- Engines: `postgresql, mysql, redis, clickhouse, registry, opentelemetry, http, tcp`;
  types `database|cache|service` (flows heavily specialized to postgresql and clickhouse).
- CRUD + deploy: `services/resource_management.go` (List/Options/Details/Create/Update/
  Archive/Deploy).
- Logical databases (PG): create/destroy + credential reconciliation.
- Credentials: create/rotate/archive/reveal (encrypted payloads, `secretcrypto`).
- Endpoints: roles public/local/wireguard; managed primary endpoint auto-created on
  first install; expose-traffic toggle = `ResourcePrivateAccess`.
- Installations: one active per resource; placement validated vs capability; default
  health check; container run + observe; stop/restart/logs/archive with guards (active
  backup policy/restore).
- Volumes/mounts CRUD.
- Resource DNS: validate/configure/reconcile/remove/status.
- Health checks: CRUD + sweep (5s periodic), statuses.
- Registry resource: list/find/inventory/reveal credentials/delete image/external.

### 2.9 Backups

- Destinations: S3/R2; probe bucket; generated restic password + age identity;
  encrypted payloads; recovery-material page.
- Policies: 5-field cron, target `server` (strategy `filesystem`, restic) or `resource`
  (strategy `logical`, pg_dump, `tar.age`), retention + verification JSON, `next_run_at`.
- Scheduling: Change kind `backup` + ChangeTask `backup_execute` + River `backup_schedule`
  w/ unique `scheduled_at`; periodic seeder.
- Execution: `backup_execute` River job → server path (restic filesystem w/ sources+
  excludes, ClickHouse metric-rollup JSONL export, recovery manifest) or DB path
  (pg_dump + globals, tar.age). States: `pending → running → uploaded → verified |
  verification_failed | failed | pruned`.
- Verification: `backup_verify` job.
- Retention: `backup_retention` job per policy.
- Restore: `resource_restores`; verified backup only, name-confirmation required,
  requires active policy → safety backup ChangeTask → `database_restore_prepare` →
  `database_restore_apply` (restore/verify/drop/rename cutover, rollback support).

### 2.10 DNS

- Cloudflare connection: token verify + zone list/sync, token rotate; encrypted token.
- Environment DNS: mode `manual|cloudflare`, records, ownership-marker TXT, public-IP
  resolution per server, River-driven reconciliation (retry 8×, unique binding+generation),
  adopt/retry/refresh endpoints.
- Resource DNS: same machinery scoped to resources.

### 2.11 Networking

- Private networks: system network (bootstrap), per-env docker networks reconciled on
  target, `server_networks` (node join, driver `wireguard`).
- Private access (VPN to resources): enable/disable (WG private endpoint), device
  enrollment (keygen, address allocation, peer apply), revoke, per-resource grants,
  client config.
- Caddy routes: managed env routes (+ backends) with state machine
  `pending/applied/failed/removal_pending/removed`; blue/green traffic switch buffers;
  resource routes (`infrastructure/caddy/resource-routes/:id`); custom routes; 30s
  in-process reconciler ticker.
- WireGuard devices: status, device address allocation, resource grants.
- Network access rules tables exist; rules applied only through private-access grants.

### 2.12 Observability

- Ingest: node otelcol (journald docker logs + container OTLP) → control-plane otelcol
  over WG (JWT telemetry identity, per-node tokens) → ClickHouse.
- ClickHouse schema: `metric_rollups`, `otel_logs`, `otel_traces`, `otel_metrics_*`,
  `request_observations`.
- Metric rollups: periodic 1 min; fleet/host/system/environment read APIs.
- Request analytics: periodic 1 min, Caddy access logs → `request_observations` → PG
  (dashboard/request pages; geo enrichment).
- Log/trace read APIs: env logs, system logs, slow queries, recent traces, full trace.
- Raw 24h service metrics in Prometheus; durable logs/traces/rollups in ClickHouse.

### 2.13 Self-update & jobs

- Self-update: channel manifest fetch + signature verify (`internal/releaseauth`):
  CheckForUpdates / Start (Change kind `system_update`) / Execute (install release dir,
  blue/green slot switch, DB traffic flip via Caddy handles, health waits) / Reconcile
  (resume/complete/recover/rollback) / healSteadyState / pruneReleases. Jobs:
  manual `self_update` + periodic `self_update_reconcile` every 30s.
- Full River job inventory: `send_transactional_email, send_marketing_email,
  metric_rollup, request_observation, resource_health_sweep, backup_schedule,
  backup_execute, backup_verify, backup_retention, database_restore_prepare,
  database_restore_apply, build_source, deploy_release, release_command,
  node_enrollment, dns_reconciliation, self_update, self_update_reconcile`.
- Periodic cadences: resource_health_sweep 5s, self_update_reconcile 30s, metric_rollup
  1m, request_observation 1m; caddy reconciler ticker 30s in-process; workload
  reconciliation on app startup (requeues unresolved deployments, recovers missing
  serving instances, cleans stale backends).
- Job sandbox UI: `/system/tasks` run/retry/cancel/delete.

### 2.14 Web/API surface & auth

- JSON API today = exactly 2 endpoints: `GET /api/health` and
  `POST /api/environments/:environmentID/deployments` (Bearer token, body `{"reference"}`).
- Everything else is Inertia-style web routes (session cookie + CSRF + rate limiter).
  Full route tables in `router/routes/*.go`.
- Machine→machine credentials that already exist: env deploy token (`dcenv_` prefix,
  per-environment), telemetry node OIDC tokens, SSH CA, encrypted `Credentials` payloads
  (registry/cloudflare/backup) — all revealed/rotated only via the web UI.
- `users`: argon2 + pepper, `is_admin`; no signup route; email confirmation +
  password reset exist; `tokens` table is for email/reset codes only, not API tokens.

### 2.15 Configuration

- Env-var driven (`caarlos0/env`), requiredif-no-default panics at startup; global
  package vars + `Config` struct (App, DB, ClickHouse, Telemetry, Email, AwsSes, Auth,
  SSHCA, Metrics).
- Installed hosts: `/etc/deploycrate-ce/app.env` + `slots/blue.env`/`slots/green.env`
  (ports 8080/8081); `installer.json` (non-secret) + `installer-secrets.json`
  (transient, removed at final confirm).
- Secrets encrypted at rest: AES-256-GCM versioned format, hex 32-byte key, optional
  per-purpose AAD (`internal/secretcrypto`).

---

## 3. eru — detailed findings (runtime, not command catalog)

### 3.1 Distribution model

One Go binary, two roles: operator CLI and (hidden subtree) `eru node *` — invoked
per-command, never a daemon. `eru node rpc` reads exactly ONE `eru-rpc/v1` request
line from stdin, runs it, streams NDJSON events + one result event. `eru node
version` / `node selfcheck` for handshake and pre-apply binary checks; `eru node run
<job>` for systemd-timer oneshots. Operator commands execute node methods via one SSH
exec per call: `sudo -n /usr/local/bin/eru node rpc` (plain when connecting as root).
`eru.prev` kept for rollback. ~30 of ~65 commands implemented in eru today; the rest
are `notImplemented` stubs.

### 3.2 Session & transport

`openSession` (every remote operator command):

1. Context resolution: `~/.config/eru/contexts/<name>/` — `context.toml`, `wg.key`,
   `id_ed25519`, `known_hosts`, `credentials.json`.
2. `upTunnel`: userspace WireGuard (no TUN, no root), MTU 1420, keepalive 25s, lazy
   handshake, AllowedIPs = node subnet only.
3. SSH dial to WG address with pinned-key callback; retry each 2s until `--timeout`;
   host-key mismatch or auth failure aborts instantly with transport-failure hint.
4. Handshake: `eru node version`; version+channel+commit must match (unless
   `--auto-upgrade-nodes`); then optional lockdown confirm (`ssh.confirm`).

RPC protocol `eru-rpc/v1` (frozen schema; 1MiB request cap; default lock timeout 30s):

- Request: `{schema, request_id, method, params, secrets, dry_run, confirm, operator,
  lock_timeout_s, client_version/commit/channel}`.
- Node `Serve` (dispatch): decode → register ALL secrets into redaction registry
  before anything prints → version check (only `upgrade.install` exempt) → method
  lookup → load `/etc/eru/cluster.toml` when cluster-scoped → confirm-token check
  computed server-side → signal context → `engine.Acquire` flock → optional per-request
  runlog → handler under `recover()` → mutex'd eventWriter (each event flushed+fsynced,
  mirrored to runlog) → single `result` event. CLI exit code = last event's exit code.
- Streaming: stdout-only NDJSON; stderr tail (8KiB) kept for error details, shown
  with `--verbose`.
- Data-plane bypass: pgx v5 dialled straight through the WG dialer with verify-full
  TLS; local psql with `-v` indicator-validated vars and per-call caps.

### 3.3 Engine

- Steps with `Check`/`Apply`; `Inputs()` canonical bytes (secrets hashed); journal at
  `/var/lib/eru/journal.json` per-step `running/completed/failed` + `inputs_hash` +
  error. Re-apply reasons: no entry / inputs changed / Check incomplete / previous
  failure / explicit `--step`. Plan = reasons + unified file diffs.
- `shell.go`: embedded bash → `/bin/bash -s`, secrets via env only, per-line redacted
  events, SIGTERM + 10s grace → SIGKILL, script error keeps 20-line tail. Files via
  tmp+fsync+chown+chmod+rename+dirsync atomic writer.
- Restart aggregation: one restart per apply, requires confirm token; stop PgBouncer →
  cluster restart → poll readiness → start PgBouncer. Unconfirmed pending restart
  persists in journal and drives the `next` hint (`eru apply --confirm <token>`).

### 3.4 State & config

- Source of truth on the node: `/etc/eru/cluster.toml` (FileStore Load/Save/Revision
  over sha256), `/etc/eru/node.toml` (membership), `/var/lib/eru/{journal.json,
  state.json}`, `/run/lock/eru.lock` (+ info file), `/var/log/eru/`, runtime service
  secrets in `/etc/eru/runtime/`.
- Operator holds only credentials (context dir above) plus `pending/<name>/`
  progress+answers during init.
- Config write path: `config.get` → bytes+revision → edit → `config.set {toml|set,
  base_revision}`; node rejects immutable keys; CAS compare (mismatch ⇒
  `precondition_failed` + `config_changed`); saves; previews engine plan in the same
  request; dry-run skips save.

### 3.5 Concurrency / long-running

- No daemons, no reconcile loops; all work = operator commands or systemd timers.
- Global node mutex `/run/lock/eru.lock` (flock, Shared for reads, Exclusive for
  mutations), 200ms poll, `.info` holder file. Recurring timers take a shared-lock
  wait — busy ⇒ job exits 0 skipped.
- Timers: e.g. `eru-certs.timer` hourly with randomized delay → `eru node run
  certs-renew`.
- sshd lockdown commit-confirm: snapshot → transient revert/unbreak timers → confirm
  over WG.

### 3.6 Secrets

- age ASCII-armored passphrase bundle (`secretsdoc`, scrypt factor 18); installed
  locally via `eru login`; `eru secrets export` re-exports (optionally rotated).
- All secrets generated operator-side; SCRAM-SHA-256 verifiers built client-side so
  plaintext never reaches psql/logs/SSH streams; SSR secret matrices live in steps.
- Redaction registry: `secrets.Default`, min length 6, longest-first, applied at all
  output paths; nodes register request secrets before anything prints.

### 3.7 Upgrades

- `eru upgrade`: refuse Homebrew/Nix paths; channel select (stable/edge); signed
  manifest resolve (https-only, size caps, same-host-only redirects); decide; preflight
  (run cached binary to confirm version/channel); swap target→`target.prev`; verified
  artifact download; handshake auto-upgrades nodes unless `--skip-nodes`.
- Node upgrades arrive via the operator: push artifact (release cache or env override
  for dev), then `upgrade.install` RPC (version-exempt; staging `.new`/`.prev`, frozen
  params, sha256).

### 3.8 cluster init

Phase machine (`internal/cli/clusterinit.go`): `secrets_generated → first_contact →
binary_installed → host_bootstrapped → transport_switched → locked_down → dns_confirmed
→ database_bootstrapped → tls_checked → secrets_written → secrets_confirmed`;
`--resume` restarts after the last completed phase; `--local` runs the identical flow
in-process (operator must be root).

- First contact = direct dialer with provider root creds, `~/.ssh/config`-resolved
  attributes; push artifact; `cluster.bootstrap phase=host` returns WG pubkey + SSH
  host keys (node preflight min RAM, fixed env probe).
- DNS step: computes A/AAAA, prompts "Type 'confirmed'" (skip via `--dns-ready`/no-TTY),
  then checks public resolvers; proxied/mismatch are warnings only, never blocking.
- Database phase: `cluster.bootstrap {phase:database, cluster_toml, node_name, local}`
  — node parses+validates TOML, requires node membership + host phase complete, applies
  database steps, aggregates restart, returns result.
- TLS wait: poll tls.status every 5s up to 120s; placeholder/self-signed ⇒ `tls_pending`
  warning, live retries hourly via timer.
- Final handoff: secrets doc written + confirmed into node state; `eru login` consumes
  the exported bundle on other machines.

---

## 4. Target architecture

### 4.1 Product model

eregion is an **agent-first, self-hosted application platform**, not a CLI wrapper
around deploycrate-ce's web application. deploycrate-ce supplies the operational
capabilities and durable job model; eru supplies the CLI contract, secure node
transport, and host execution patterns.

Users bring their own Debian or Ubuntu servers. eregion configures and operates
them; it does not provision VPSs through cloud providers.

The CLI must expose the complete platform to agents. Human-facing terminal features
are a secondary presentation layer, not a separate implementation of operations.
A future web UI should reuse the same operation model.

### 4.2 Control plane, API, and execution

The control plane requires a server-side API from the early implementation stages
so CI/CD systems can trigger deployments. This must not wait for GitHub webhooks or
a web UI. A supervised control-plane service (working command name:
`eregion serve`) hosts the API and River workers; whether API and worker processes
are deployed together or separately is an implementation decision.

The common operation flow is:

1. An agent, CLI, CI system, or webhook submits an authenticated operation.
2. The control plane validates authorization, inputs, and required confirmations.
3. Domain changes and associated River jobs commit in the same PostgreSQL
   transaction.
4. Workers execute durable tasks, invoking node operations where needed.
5. Clients inspect or stream persisted operation events and terminal status.

Disconnecting a submitting client must not cancel its build or deployment. Agents
must be able to reconnect using a stable operation identifier. Explicit cancellation
is a separate operation with defined safe boundaries.

CLI and API entry points must use the same application services, validation,
authorization, and enqueueing paths. The exact CLI-to-control-plane transport is
still open; SSH-based node RPC remains the host execution mechanism, not a reason
to make deployments depend on an SSH session's lifetime.

### 4.3 PostgreSQL and River

Use a required **external PostgreSQL database** for authoritative control-plane
state, domain history, and River job tables. This replaces the earlier SQLite
proposal. The intended boundary is a database independent of any one control-plane
process; hosting requirements remain to be defined.

Use River as deploycrate-ce does, rather than implementing a custom SQL queue or
introducing NATS JetStream. Reuse its transactional insertion, retries, scheduling,
and multi-worker execution patterns where applicable.

- **Domain records** describe changes, releases, deployments, desired/applied state,
  and operation events. These are the agent-facing history and status contract.
- **River records** describe execution bookkeeping. Queue status alone is not a
  sufficient domain status model.
- Handlers must tolerate **at-least-once execution** using idempotency and
  reconciliation. No exactly-once guarantee is assumed for external side effects.
- Do not hold database transactions open while builds, deployments, or remote
  commands run.
- Persist progress/events needed for reconnect and diagnosis. If `LISTEN/NOTIFY` is
  used to wake readers, notifications are hints, not the durable event store.

River-backed scheduling and reconciliation replace the earlier proposal to move
all recurring platform work to systemd timers. Systemd still supervises services
and can run genuinely host-local maintenance tasks. Cadences and overlap behavior
must be reviewed rather than copied blindly.

Revisit a separate broker only for demonstrated requirements such as independent
replayable consumers, substantial fan-out, or intermittently connected messaging.
Do not build a speculative broker abstraction for v1.

### 4.4 HA and server roles

Design the state and execution model to permit multiple control-plane instances.
An external PostgreSQL database enables shared state but does **not** by itself
provide HA. Correctness also requires coordination of conflicting operations,
worker-loss recovery, idempotent side effects, and safe schema migrations. API
routing, database availability, shared artifact access, and credential availability
also need explicit deployment designs.

Model server types explicitly:

- **Control-plane servers:** API, orchestration, and background workers.
- **Application servers:** application containers, runtime networking, and health.
- **Build servers:** source preparation, Buildpacks, build caches, and image pushes.

Roles should drive installed dependencies, placement, permissions, and resource
isolation. Whether roles can be combined on one server, and how resource-hosting
servers fit this model, remain open. Do not silently assume a dedicated machine is
required for every role.

Node-local file locks remain useful for host mutations, but cannot coordinate
multiple control-plane instances. Platform-level coordination must be database-
backed or otherwise distributed; exact scopes and mechanisms require design.

### 4.5 CLI and agent contract

Adopt eru's declarative command tree and metadata validation, structured output,
stable error codes, next-step hints, and no-TTY discipline. Every prompt must have
a noninteractive equivalent; missing inputs identify the required flags. Destructive
operations require explicit, server-validated confirmations.

Agents must be able to discover commands, preview changes, submit operations,
inspect status/events, retry safely, and request cancellation without scraping
human prose. Metadata and machine-readable behavior are foundational acceptance
criteria, not final-phase polish.

Use Charm libraries for human interaction where useful: guided workflows, live
deployment views, dashboards, and terminal graphs. These views consume the same
structured state/events. Interactive rendering must not leak into machine output
or become necessary to access a feature. The web UI stays deferred while the CLI's
potential is explored.

### 4.6 Telemetry without a bundled analytics database

Do **not** port deploycrate-ce's bundled ClickHouse storage or its ClickHouse-
dependent rollup and analytics pipeline into v1. Instead expose telemetry primitives
that make integration with Grafana-compatible and other existing stacks easy:

- Application and platform logs with useful identity and correlation fields.
- Metrics endpoints and/or export paths suitable for external collection.
- OpenTelemetry-compatible traces and export configuration.
- Documented setup examples for external collectors and backends.

Grafana is a visualization layer, not the telemetry store; integration examples
must identify the necessary collector/backend. The exact collector packaging,
local buffering, and default retention are open. Do not assume users must run a
bundled Prometheus or any other storage backend.

Durable **operation history in PostgreSQL** is distinct from bulk telemetry. Agents
must still diagnose failed builds/deployments without requiring an external
observability service. CLI graph views depend on available data sources and should
handle unconfigured external telemetry explicitly.

### 4.7 Source reuse and boundaries

| Source capability | eregion direction |
| --- | --- |
| deploycrate installer | Resumable region init and Check/Apply steps; adapt for Debian and Ubuntu |
| deploycrate domain model + River | PostgreSQL-backed services, durable jobs, operation history |
| Web controllers/forms | Agent-first commands and early automation API |
| Build/deploy workers | Durable orchestration with streamed/replayable progress |
| Host privileged dispatcher | eru-style node RPC over pinned SSH/WireGuard |
| Node capabilities | Explicit server roles plus placement/capability validation |
| Web sessions and deploy token | Operator contexts plus scoped API credentials; auth design required |
| ClickHouse analytics pipeline | External telemetry integrations; no bundled ClickHouse in v1 |
| Web dashboard | Rich terminal views first; web UI later |

Reuse eru's transport, node RPC event protocol, redaction, atomic file writer,
Check/Apply journal, configuration revision checks, init phase machine, and verified
upgrade patterns. Adapt rather than copy assumptions about local authoritative
state, global locks, PostgreSQL-specific restart ordering, or daemonless execution.

TOML can describe bootstrap and node-local configuration, with revision-protected
writes. Platform entities and shared desired state belong in PostgreSQL; exact
ownership boundaries must prevent competing sources of truth.

Reuse deploycrate's build/deploy/resource/backup semantics and River patterns while
removing dependencies on browser sessions, Inertia responses, and bundled analytics.

---

## 5. Decisions and open questions

### Confirmed product and architecture direction

1. Agent-first operation; human terminal UX second; web UI deferred.
2. Debian and Ubuntu hosts, supplied by the user.
3. Early server-side API for CI/CD and automation, independent of web UI timing.
4. External PostgreSQL for platform state; River for durable background jobs and
   recurring platform work. No SQLite, custom queue, or JetStream for v1.
5. Design for multiple control-plane instances and explicit application/build roles.
6. No bundled ClickHouse for v1; expose telemetry for external observability stacks.
7. Rich CLI exploration with Charm libraries, without weakening machine contracts.

### Working implementation proposals

- Keep Cloud Native Buildpacks (`pack`) as the source build engine, with live build
  output and durable operation status.
- Adopt eru's secure transport and safety mechanisms, with commit-confirm for
  changes that could cut access.
- Use command topics such as `region`, `nodes`, `apps`, `build`, `deploy`,
  `resources`/`db`, `backups`, `secrets`, `dns`, `net`, `telemetry`, `registry`,
  `system`, `upgrade`, and `login`/`context`. Reserve `node *` for node mode.
- Adapt signed-release distribution and operator-driven node upgrades from the
  source projects rather than retaining web-app slot updates unchanged.

### Open design questions

- Which Debian/Ubuntu releases and CPU architectures are supported initially?
- What does the required external PostgreSQL deployment permit: managed service,
  user-operated database hosts, or a separately bootstrapped database role? Define
  supported PostgreSQL versions, TLS, pooling, backups, and outage behavior.
- What HA guarantees ship in v1 versus later? Define API routing, worker failover,
  conflicting-operation coordination, migrations, and artifact/secret availability.
- Can server roles be combined? Is resource hosting a separate role or an
  additional capability, and what isolation is required for build servers?
- How does the CLI reach the API? Which CI/CD endpoints are public versus private,
  and how are scoped credentials issued, rotated, revoked, and audited?
- What are the reconnect/event retention and cancellation guarantees? How are
  submission idempotency and duplicate CI requests handled?
- Which recurring jobs need River scheduling versus host-local mechanisms, and
  what concurrency/overlap guarantees do they require?
- Which telemetry primitives ship by default? Define collector packaging, log
  streaming/buffering, external backends, and the data sources for terminal graphs.
- Should public SSH remain available, or should nodes eventually use full lockdown?
- Where is the boundary between TOML bootstrap/node config and shared desired state?
- Reuse deploycrate's R2 + Sigstore pipeline, or adapt eru's release site?

---

## 6. Roadmap (revised phases)

The dependency order below is a planning proposal. Agent contracts, API/job
durability, Debian/Ubuntu compatibility, and HA-safe state ownership are foundations,
not features to bolt on after the manual deployment path.

### Phase 0 — Skeleton port

Set up the repo and adapt eru's tree/metadata/output, context/transport, node RPC,
engine, secrets, and configuration revision checks. Define versioned command/API
contracts and the bootstrap-config versus shared-state boundary. Establish external
PostgreSQL migrations, River integration, and a minimal domain-operation/event model.
Introduce an API/worker skeleton with shared application services.

**Done when:** the command catalog validates; a test operation submitted through
CLI/API commits domain state and a River job atomically, executes through node mode,
and exposes structured status/events. A disconnected client can reconnect. Local
execution and remote transport have separate explicit tests.

### Phase 1 — Region bootstrap and control-plane service

Implement `region init` with resumable bootstrap phases and role-aware host steps
on supported Debian and Ubuntu releases. Validate the external PostgreSQL connection
and migrations; install the supervised API/worker service, private transport, and
necessary ingress. Add scoped automation credentials, login/secrets handoff,
plan/apply, and access-safety checks. Install only dependencies required by the
selected roles; no ClickHouse or unconditional build stack on control-plane hosts.

**Done when:** a fresh host on each supported distribution becomes a healthy
control plane; re-apply is idempotent; an authenticated automation client can submit
and inspect a durable test operation. Document database and API exposure requirements.

### Phase 2 — Role-focused node enrollment

Implement `nodes add` first contact and adapt node installation into role-aware
engine steps. Add WireGuard/SSH trust, identity, placement/capability validation,
mesh reconciliation, and node status/check commands. Install Buildpacks and build
dependencies on build servers; runtime dependencies on application servers. Cover
both supported distributions and define how additional control planes join.

**Done when:** application and build servers enroll idempotently and pass identity
and connectivity checks. Additional control-plane worker/API participation is
tested against shared state without duplicate unsafe side effects.

### Phase 3 — Apps: durable builds, deploys, and CI/CD

Port apps, environments, sources/targets, build configs, releases, deployments,
instances, changes, revisions, and environment secrets into PostgreSQL-backed
services. Adapt deploycrate's River build/deploy/release-command workers. Implement
CLI/API submission, build-server placement, image/registry paths, application-server
deployment, health checks, Caddy traffic switch, rollback, and reconciliation.

Provide durable status/events, explicit retry/cancel semantics, dry-run plans, and
an authenticated CI/CD deployment trigger. Add a first Charm live-operation view on
the same contracts; automation must never require it.

**Done when:** a sample Go app builds and deploys through CLI and CI/API paths;
blue/green swap and rollback work; client disconnects do not stop execution; worker
loss is recoverable without unsafe duplicate effects.

### Phase 4 — Resources, credentials, backups

Port resource placement, endpoints, volumes/mounts, credentials, and health checks,
starting with PostgreSQL depth and a deliberately limited additional engine set.
Resource engines do not imply bundled platform telemetry storage; managed ClickHouse
support is deferred unless separately justified. Keep the required control-plane
database distinct from application resource lifecycle operations.

Implement River-backed backup scheduling, execution, verification, retention, and
restore with confirmation and safety backups. Define recovery responsibility for
the external control-plane database.

**Done when:** an app can use a PostgreSQL resource over the private network and a
backup → verify → restore drill succeeds. Resource deletion/restore cannot silently
operate on the control plane's required database.

### Phase 5 — External telemetry, DNS, and private access

Complete log access, metrics exposure, trace export, collector/backend configuration,
and external observability examples without ClickHouse-dependent rollups. Foundational
instrumentation and operation diagnostics are developed in earlier phases. Add
terminal graphs/views where data sources support them, with machine-readable access
to the same data.

Port Cloudflare/environment/resource DNS reconciliation through River, private
access devices/grants, and custom Caddy routes.

**Done when:** logs are accessible from the CLI, telemetry reaches a documented
external stack, DNS reconciles automatically, and private access grants are enforced.

### Phase 6 — Webhook automation and terminal workflows

Extend the existing control-plane service with GitHub webhook signature verification,
delivery deduplication, and the same durable build/deploy submission path. Expand
Charm-based fleet/application views, guided workflows, and graphs without creating
a separate business-logic path. CI/CD API triggers already exist from Phase 3.

**Done when:** GitHub push → build/deploy is observable through agent commands and
human terminal views; duplicate deliveries are safe; noninteractive parity is intact.

### Phase 7 — Lifecycle, HA validation, and operator readiness

Complete verified channel upgrades, node pushes/rollback, coordinated control-plane
upgrades and migrations, recovery/drill commands, skill/guide generation, and
documentation. Validate the agreed HA scope with multiple control planes, worker
loss, database outages, and competing operations. Metadata and structured output
must already be complete for commands as they are introduced, not added here.

**Done when:** fleet upgrades and recovery drills pass; documented failover
guarantees are tested; agents can discover and operate the full supported surface.

### Deferred (changed by the port)

- Web UI (potentially andurel-based), after exploring the CLI's full potential.
- Transactional/marketing email (deploycrate's own user flows semi-dormant).
- Bundled ClickHouse, internal telemetry analytics storage, and geo enrichment.
- NATS JetStream or another separate event broker absent demonstrated requirements.
- Database editor UI.

---

## 7. Recommendation

**Start with Phase 0, after resolving the foundation-level questions in Section 5.**
Prove one durable operation end-to-end using external PostgreSQL, River, the API,
agent CLI contracts, and node execution before porting the larger domain surface.

Adapt source code directly where appropriate rather than treating either project
as an architectural dependency. eru's generic machinery is reusable, but its
daemonless/local-state assumptions are not the target architecture. deploycrate's
PostgreSQL/River model is equally foundational; its web UI and ClickHouse pipeline
are not.

Do not treat the original copy-with-rename estimate as an implementation guarantee.
Multi-control-plane coordination, durable operations, distribution compatibility,
and API authorization need deliberate design and tests.

---

## 8. Appendix: coverage notes

All major areas of both codebases were examined via parallel subagent exploration
(six full passes total across the two repos). Deploycrate areas covered deeply:
installer steps, app deployment flow, change model, node enrollment, resources,
backups, DNS, networking, observability, jobs/self-update, API/auth surface.
eru areas covered: distribution/roles, session/transport/RPC (exhaustive),
engine/steps, state/config, concurrency/timers, secrets, upgrades, agent metadata
contract (verified line-precise), cluster init phase machine.

Files intentionally not read in detail at this stage (available for expansion):
`services/environment_setup.go` (144KB), `services/resource_management.go` (115KB),
`services/bootstrap.go` (47KB), build/deploy execution internals, `internal/
resourceaccess/container.go`, individual `clients/*`, full River job bodies,
`resources/js` frontend route tree, eru `internal/cli/{sql,console,logs,discovery,
progress,interview,prompts,handoff,flags,skill,guide}.go`, eru `schema/eru-v1.json`.
