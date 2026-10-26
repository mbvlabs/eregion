# Implementation reference

Checked against source on 2026-10-26. This describes **current code**, not the
target command tree or a real-host certification. Product decisions and all
work status belong in [MASTER](MASTER.md); run evidence belongs in
[MANUAL-TESTING](MANUAL-TESTING.md).

## Execution and control boundaries

```text
operator CLI
  -> saved context: embedded userspace WireGuard -> pinned SSH -> sudo -n eregion node rpc
  -> NDJSON request/events/result -> shared application services (api.AppService)
  -> external PostgreSQL: domain intent + operation + River job, one transaction
  -> River worker -> local or pinned-SSH node transport -> Check/Apply effects
  -> durable events/state -> CLI plain/JSON/NDJSON/terminal presentations
```

Every operator command uses node-mode dispatch: the CLI writes one
`eregion-rpc/v1` request to `eregion node rpc` on stdin over SSH inside
WireGuard and reads NDJSON back. Contexts carry the WireGuard identity and SSH
key, not a bearer token. The HTTP API remains for the narrow CI/webhook path and
the public `/readyz` endpoint; the operator CLI does not configure or use it.
Before the first service call the CLI compares the node build with its own and
refuses with `node_version_mismatch` unless the node was auto-upgraded.
Read-only RPC methods `platform.workloads` and `platform.logs` read Docker
directly and do not need the control database; `operations.list` uses the shared
application service. The worker-to-node step transport is a separate protocol
boundary.

API and worker are supervised separately by `eregion-api.service` and
`eregion-worker.service`; `serve` can also run workers in-process. Domain intent
and queue insertion are atomic. Long remote effects do not hold a SQL
transaction open. Workers recheck placement and reconcile step acknowledgements.
An SSH disconnect never owns the lifetime of accepted work.

## Code map

| Location | Responsibility |
| --- | --- |
| `cmd/eregion` | One executable entry point, including `node-agent` and hidden `node rpc`. |
| `internal/cli/tree.go`, `meta.go`, `run.go` | Command catalog, safety metadata, output selection and dispatch. |
| `internal/cli/serverinit*.go`, `sshauth.go`, `handoff.go`, `login.go` | Guided setup (preflight, answers/pending, plan/confirm, apply, guarded handoff), first contact, DNS, private access and token-free age handoff/import. |
| `internal/cli/context.go`, `internal/cli/noderpc.go`, `internal/cli/hostnode.go`, `internal/cli/observe_rpc.go`, `internal/host/contextstore.go` | Context resolution, node RPC client/server, version lockstep, host RPC, Docker workload/log reads, SSH/WireGuard route. |
| `internal/cli/observe.go`, `internal/cli/logs.go`, `internal/cli/delete.go`, `internal/cli/operations.go` | Extended app/environment status, container logs over the operator channel, target-confirmed deletion and operation-history filters. |
| `internal/cli/host.go`, `internal/cli/serverstatus.go`, `internal/cli/nodeunlock.go` | `plan`, `apply`, `doctor`, `ssh`, `server status` and hidden `node unlock-ssh`. |
| `internal/rpc` | `eregion-rpc/v1` request/event/result wire types and bounded request decoding. |
| `internal/api` | Shared application services plus the HTTP authentication/grants, operation/inventory routes, vault, webhook and private proxy adapters. |
| `internal/controlplane` | pgx transactions, River, platform state/workers, deployment reconciliation, node resolution, extension wiring. |
| `internal/platform` | Validated action/input types, inventory model, placement rules, source/runtime steps, Caddy route renderer and the domain DNS/TLS/HTTP observation helpers. |
| `internal/engine`, `internal/node`, `internal/noded`, `internal/transport` | Check/Apply journal, effect identity, detached tasks, pinned SSH and userspace WireGuard. |
| `internal/host`, `internal/access`, `internal/doctor` | Bootstrap steps (incl. vault keyring and node_exporter), local pending-init state, recorded host intent, read-only observations, admin/firewall/WireGuard/SSH lockdown and break-glass scripts, and pure doctor checks. |
| `internal/guide` | Embedded agent operating guide printed by `guide agents` and installed as `SKILL.md` by `skill install`. |
| `internal/vault`, `internal/secrets` | Revisioned ciphertext/keyring and redaction. |
| `internal/integrations`, `internal/telemetry` | Internal DNS/private-access actions (no operator CLI before v1) and the telemetry read adapters used by the workspace metrics graph. |
| `internal/viewer`, `internal/tui`, `internal/workspaceui`, `internal/cli/ui` | Reducer/follow, operation view, full-screen workspace and guided inline components. |
| `internal/release`, `internal/upgrade`, `scripts/*.sh`, `.github/workflows/publish-*.yml` | Cosign-verified channel manifests and artifacts; local CLI replacement plus the verified linux/<node arch> binary used by `apply --auto-upgrade-nodes`. |

## Current command surface

Use the **built binary**, not a stale `bin/eregion`, to inspect exact arguments:

```sh
just build
bin/eregion commands --json
bin/eregion help app create --json
```

| Current commands | Meaning/limit |
| --- | --- |
| `server init` | Guided root first contact; Linux/macOS operator, arch-matching linux node binary, preflight, answer flags/TOML, plan/confirm, vault keyring and node_exporter steps, pending-init resume/discard, guarded handoff. |
| `plan`, `apply [--dry-run]`, `doctor`, `ssh [node]`, `server status` | Node-mode host convergence and inspection. `apply` records the host intent, streams step progress on stderr and skips completed steps; `doctor` exits non-zero on a failing finding. |
| `node unlock-ssh [--minutes N]` | Hidden node-only break-glass command, run as root from the provider console; reopens public SSH for a bounded window and schedules the re-lock. |
| `login --file … --passphrase-file …` | Import an age transfer bundle into a token-free context (SSH and WireGuard keys, route). |
| `context list/show/use/remove`, `context credential add` | Saved contexts, target-confirmed removal with its key files, and local server-auth-file editing. `use` takes `--name`; `remove <name>` takes `--confirm <name>`. |
| `app create/list/status/delete` | Guided or flag-only first environment: source, Buildpacks settings, processes, plain config, vault secret names and an optional domain. `create` accepts `platform.app.create`, queues its first deploy unless `--no-deploy`, then adds the domain as a separate `platform.domain.add` action. List/status read inventory. No delete yet. `status` shows the serving release, processes, per-domain state and recent operations; `delete` takes `--confirm <app>` (D13). |
| `env create/update/list/status/delete` | `create` accepts an environment with its first revision and queues its first deploy unless `--no-deploy`; `update` merges source, placement, runtime and process changes into a new immutable revision. In a terminal with no change flags, `update` walks the current revision prefilled, shows the diff and confirms; with flags or JSON/no-TTY it is flag-only. List/status include revision, processes and deployability. No delete yet. `status` shows serving release, per-process replicas, domains and recent operations; `delete` takes `--confirm <app>/<env>` (D13). |
| `process list/scale` | Read the desired formation; scale one process's replicas in a new revision. No deploy is queued. |
| `config set/unset/list` | Plain variables as a new revision or read from the current revision. No deploy is queued. |
| `platform releases create/list` | Exactly one of `--image`, `--directory`, `--git-url`. Directory is on the server. Transitional until the deploy row replaces it. |
| `platform deployments plan/deploy/list/retry/rollback` | Durable deployment actions; rollback takes a dry-run confirmation. Transitional until the deploy row replaces it. |
| `domain add/remove <app> <environment> <hostname> [--primary]`, `domain list [--app --environment]` | Node-mode domain bindings, one primary per environment, serialized atomic Caddy writes and separate route/DNS/certificate/public observations. `remove` is plan-bound and refuses the primary. |
| `secret set/list/archive/export` | Metadata/write-only storage and explicit file export; `set` writes the value and then attaches the name to the environment as a new revision. No plaintext `get`. |
| `logs <app> <environment> [--process NAME] [--follow] [--since] [--tail]` | Reads the serving deployment's Docker containers through node RPC; lines are prefixed with process/replica. No telemetry node is needed. Ctrl-C detaches. |
| `platform inventory`, `ui` | Read-only inventory and the full-screen workspace. The workspace graph still uses the telemetry read adapter for metrics. |
| `operations list/get/events/follow/cancel` | Actor-scoped operation history filtered by app, environment, state and limit, plus snapshot/event observation and explicit cancellation. |
| `bootstrap …`, `system migrate`, `serve`, `worker`, `node-agent …` | Current low-level host/service entry points. Not the desired everyday operator workflow. |
| `upgrade` (`--version`, `--channel`, `--dry-run`, `--yes`), `commands`, `help`, `guide agents`, `skill install`, `version` | `upgrade` replaces only the local CLI from a cosign-verified channel; it never touches a node. `guide agents` prints the embedded agent operating guide; `skill install` prompts for harnesses in a terminal or takes `--harness`, then writes the same text as `SKILL.md`. `commands` is discovery. |

`app create|status|delete`, `env create|update|status|delete`, `process`,
`config`, `domain`, `logs` and `operations list` are the current application
surface. `platform releases …` and `platform deployments …` are the remaining
transitional names. The rest of MASTER section 5 (`deploy`, `rollback`, `build`,
`release`, `restart`, tokens) does not exist yet; renaming is not a substitute
for implementing the flow.

The telemetry-backed `logs search`/`logs tail` commands are deleted; the
workspace metrics graph keeps the telemetry data source. `platform apps …` and
`platform environments …` are deleted (pre-v1, no aliases). `resources …` and
the backup code are deleted (MASTER D19), the Docker
Compose telemetry stack install is deleted (D5), and `dns …`, `access …`,
`platform nodes …`, `platform fleet`, `platform submit`, `metrics …` and
`operations test` are no longer registered (D6, D22, D26). Their internal
packages remain for the after-v1 phases where the work list says they return.

## Release and local upgrade

`internal/buildinfo` carries `Version`, `Commit`, `Channel` and the public
`ReleaseBaseURL` (currently a documented placeholder). `internal/release`
resolves a channel or version manifest, verifies its sigstore bundle against
the pinned publish workflow identity, validates the manifest against the
channel, and caches the platform binary only after its signed SHA256 matches.
TUF/sigstore trust material is cached, not pinned in the binary.

`internal/upgrade` holds the local-only plan/preflight/atomic swap. `upgrade`
downloads the verified binary, checks its reported version and channel, and
replaces only the running CLI. `upgrade.FetchNodeBinary` is the verified
linux/amd64 or linux/arm64 download used when a macOS CLI or an architecture
mismatch prevents pushing the operator's own executable: the node receives the
release matching the CLI's version, the uploaded checksum is verified, and the
API/worker units restart.
There is no Ed25519 manifest, trusted public-key file, rollback journal or
one-host operator/node replacement state any more. Release mechanics are in
[upgrades](upgrades.md).

## Database: embedded schema 013

Authority: `internal/controlplane/database.go` and
`internal/controlplane/migrations/*.sql`. JSON output schema (`eregion/v1`),
node protocol version and database migration version are independent.

| Migration | Change |
| --- | --- |
| `001_operations.sql` | Operations, event sequence and actor/kind/target/key uniqueness. |
| `002_node_execution.sql` | Per-operation step acknowledgement, inputs hash and execution identity. |
| `003_operation_error_code.sql` | Structured failure codes on operations and steps. |
| `004_platform.sql` | Nodes, apps, environments, releases, deployments, changes; permits platform operation kinds. |
| `005_vault.sql` | Secret namespaces and immutable encrypted revisions. |
| `006_integrations.sql` | Private-access grants and integration audit. |
| `007_nodes.sql` | Enrollment, SSH host pin, attested node identity, host probes, build slots and WireGuard public state. |
| `008_webhook_deliveries.sql` | Delivery/body hashes and reserved/submitted operation identity, independent of operation retention. |
| `009_platform_build_node.sql` | Optional environment build-node FK. Does not implement image transfer. |
| `010_platform_domains.sql` | Globally unique domain name -> environment FK; creation timestamp only. |
| `011_environment_revisions.sql` | Immutable numbered environment revisions, current-revision pointer, deployment revision/secret snapshots and domain primary flag. Backfills one baseline revision per existing environment. |
| `012_platform_deploy.sql` | First-class builds with exact commit and lifecycle; release build/commit links; deployment runtime instances, previous deployment and `restart` action; environment serving-deployment pointer. Backfills the serving deployment for existing successful environments. |
| `013_domain_state.sql` | Per-domain desired/applied route revision, last route error and timestamps; recorded expected addresses and DNS state/answers; certificate state and expiry; public response state/status; observation timestamps. |

Tables below have the literal `eregion_` prefix:

| Table(s) | Key and authoritative facts |
| --- | --- |
| `schema_migrations` | Version/checksum/application time. River maintains its own schema history. |
| `operations` | `op_…` ID; actor, kind, target, normalized input/hash, idempotency key, state, attempt, step count, cursor, River job ID and error code. |
| `operation_events` | `(operation_id, sequence)`; JSON event. |
| `operation_steps` | `(operation_id, step)`; inputs hash, execution identity, attempt, state, timestamps and result/error. |
| `platform_nodes` | Name; roles/capabilities, status, SSH address/user/**host key**, attested identity, probes, build concurrency/user and WireGuard public state. Private keys stay in files. |
| `platform_apps` | Opaque app ID and unique name. No stored setup wizard/source configuration. |
| `platform_environments` | App FK + name; runtime/build node, stable/candidate/container ports, bind, health settings/observation, config, pinned secret refs, desired/applied/serving release IDs, `serving_deployment_id` and `current_revision_id`. The deploy worker reads the revision and the serving deployment's recorded instance ports; the mutable legacy config/health/port fields remain only as mirrored compatibility columns for the older CLI surface. `secret_refs` remains the transitional pinned form, used only when the current revision names no secrets. |
| `platform_environment_revisions` | Immutable `erv_…` ID and per-environment number; operation/actor/operation/summary change record; source binding, auto-deploy flag, webhook secret NAME, runtime JSON, processes JSON, plain config and secret NAMEs, plus the first-deploy intent flag. An UPDATE trigger refuses edits; deleting the environment still cascades. |
| `platform_builds` | `bld_…` ID and unique operation FK; app/env, pinned revision and release, source, exact commit, pending/running/succeeded/failed/cancelled lifecycle, timestamps and error. Logs are the owning operation's events. |
| `platform_releases` | Release ID and unique operation FK; app/env FKs, source/build/command JSON, image/image ID, build link, exact commit, pending/ready/failed status and error. |
| `platform_deployments` | Deployment ID and unique operation FK; action (`deploy`, `rollback`, `restart`), release, previous release/deployment, environment revision ID, exact resolved secret revisions (name -> non-secret vault revision identity), recorded process instances/ports, state and errors. |
| `platform_changes` | App/env history, sequence, operation/actor, desired/applied JSON and summary. |
| `platform_domains` | Domain name PK, environment FK, `is_primary` with a partial unique index enforcing one primary per environment. `desired_revision`/`applied_revision` and `route_state`/`route_error` record the Caddy write; `expected_addresses` plus `dns_state`/`dns_addresses` record public-resolver observation; `certificate_state`/`certificate_not_after` and `public_state`/`public_status` keep the other observations separate. |
| `secrets`, `secret_revisions` | Namespace `(app, environment, name)` and revision; state/CAS, key ID, ciphertext and hashes. Not FKs to platform app/env rows. |
| `access_grants`, `integration_audit` | Grant JSON and durable authorization/provider audit JSON. |
| `webhook_deliveries` | Hashed delivery ID, exact-body hash, actor/kind/target, pending/submitted state and operation ID. |

Boundaries around the MASTER app-model work (work list rows 4–5):

- The environment revision and the deployment's recorded process instances are
  the deploy worker's source of truth. The legacy stable/candidate columns are
  still allocated for older clients but no deployment path reads them; Caddy
  reads the serving deployment's dynamic loopback web ports.
- Releases from source run through the `platform_builds` lifecycle. A build's
  exact commit, state and release link are durable; its log stream is the
  operation event stream. `platform.build.stop` signals a running pack process,
  and `platform.build.retry` creates a new build and release at the same commit.
- Environments still hold pinned `secret_refs` only as a fallback for revisions
  that name no secrets. Revisions use secret NAMEs, resolved per deploy (D18);
  rollback resolves the target deployment's recorded exact revisions.
- Deploying from the revision's bound source (including a private HTTPS git
  read credential) now records the build and deployment. Auto-deploy/push and
  the per-environment webhook remain work list row 8.
- Deletion is deliberate, not SQL cascade alone. `platform.environment.delete`
  and `platform.app.delete` first remove containers selected by ownership labels
  and rewrite the Caddy routes without the target, then delete domains,
  deployments, releases, builds, revisions, change rows, vault secret namespaces
  and the environment/app rows in that order. A failed container or Caddy step
  leaves platform rows available for retry. Operations and operation events are
  never deleted; operation `--app`/`--environment` history still shows the
  deletion after the platform record is gone. The node validates the
  target-naming `--confirm` token against the records it loads.
- The deleted resources/backup subsystem had no catalog tables in the
  control-plane migrations. DNS records live at the provider; a domain binding
  is not a DNS record.
- Roles/rows do not prove tools, credentials, services or routes are installed.

## Migration and external database policy

Use a dedicated external database, PostgreSQL 17+. `EREGION_DATABASE_URL` is
supplied from a protected environment/file, never a credential-bearing CLI flag.
The parser requires explicit `sslmode=verify-full`, except `disable` is accepted
for literal loopback and tailnet hosts (CGNAT/tailnet IPv6 or `*.ts.net`). This
parser exception does not make a co-located control database the product target.
`localhost` is not the literal-loopback exception. Host/port/service query
overrides are refused. Direct connections are the validated configuration;
transaction-pooling proxies have not been qualified.

```sh
# On the intended control host, with its DSN securely supplied:
eregion system migrate --dry-run --json
# Only after explicitly authorizing this database's migration:
eregion system migrate --json
```

Migrations serialize with a dedicated PostgreSQL session advisory lock. Domain
versions commit transactionally; River versions commit separately. Services do
not auto-migrate; readiness fails until both are current. A newer schema, gap or
changed applied checksum fails closed. Never edit an applied SQL file, forge
history or reset a database to make an upgrade pass. Add the next migration.

## External control-state recovery

The control database is the PostgreSQL provider's recovery responsibility (D4,
D9); eregion has no backup product or `resources backups restore`. A
maintenance-window recovery must:

1. Stop every API/worker/writer. Preserve the old DB, sanitized schema/version
   facts and current configuration. No two independent restored writers.
2. Take/verify a protected custom dump with PostgreSQL tools and a credential
   source that does not expose passwords in process arguments or logs.
3. Restore to a **new** database, never over the live source. Check table
   contents, operation history, migrations/checksums and River versions with the
   same binary.
4. Restore/verify the matching vault keyring, node identities/transport
   credentials and relevant host state. A DB dump alone is not complete
   recovery.
5. Repoint the protected service DSN deliberately, start one API/worker pair,
   check readiness and reconcile in-flight effects without inventing success.
6. Account for operations accepted after the dump. Retain old DB and backups
   until the restored setup and a new backup are proven. Reverting DSNs after
   new writes is not lossless rollback; reconcile divergence first.

`TestIntegrationControlPlaneRecoveryDrill` exercises a narrower
dump/restore/readiness subset against generated test databases. It is not
evidence of live writer quiescence, DSN cutover, keyring recovery, HA or
recovery-time goals.

## Bootstrap and local state

`server init` follows MASTER 6.1. A local preflight requires the OpenSSH client
(`ssh`, `ssh-keygen`: `missing_tool` with the platform install command), checks
that the target answers, and requires an SSH credential. First contact is root:
host-key TOFU with a fingerprint confirmation in a terminal,
`--host-key-fingerprint` or `--accept-new-host-key`; the credential is
`--identity`, ssh-agent/the default keys (an encrypted key asks for its
passphrase only in a terminal), or `--password-stdin`. A read-only remote
preflight then refuses an unsupported OS or architecture (Debian 13, Ubuntu
26.04; amd64/arm64), a non-root login, less than 1 GiB RAM or 8 GiB free on `/`,
a missing `tar` (`missing_tool` with the apt command), and existing eregion
state. A pending init continues its own state; a new init never adopts a host
that already has eregion state.

Answers are defaulted and each has a flag and an `--answers` TOML key: node
name, server/context name, public hostname (used exactly as given, only the
trailing dot is trimmed), external DSN file (owner-only; `EREGION_DATABASE_URL`
is the environment fallback) and admin user. A terminal is asked only for what
no flag or answer supplied.

```toml
host = "root@203.0.113.10"
node = "main"
context = "prod"
hostname = "app.example.com"
database_url_file = "/root/eregion-db.url"
admin_user = "admin"
``` The remote host plan is shown, then confirmed: a TTY
asks, JSON/no-TTY requires `--confirm <server>`.

Apply runs the versioned host steps with live progress. The node receives an
arch-matching linux binary: the running binary when it is linux/<arch>,
otherwise the verified release artifact for this CLI's version
(`upgrade.FetchNodeBinary`); a dev or unarchived build refuses with the fix
instead of pushing a non-executable macOS binary. The binary is installed once
at `/usr/local/bin/eregion`. The main role also gets
`/etc/eregion/vault-keyring.json` (0600, owned by the control-plane account and
named by `EREGION_VAULT_KEY_FILE` in `control-plane.env`) and node_exporter
1.11.1 with a pinned sha256 per architecture, listening only on
`10.78.0.1:9100`; the access setup allows 9100 on `wg0` and starts the unit once
`wg0` exists. `internal/access` remains the hardening/WireGuard/commit-confirm
lockdown implementation.

After lockdown the CLI pins the WireGuard host key, saves the token-free context,
and calls node RPC `ready` as the final gate. It then checks node_exporter over
the tunnel and builds the handoff: every server secret in plain text (operator
keys, sudo password, DSN, vault keyring) plus an armored age transfer bundle with
a generated passphrase and break-glass instructions. A terminal waits for
`stored`; JSON/no-TTY requires `--secrets-out` before any change and writes the
whole record to a new 0600 file. No API token is in the handoff.

The unfinished init is saved locally under the context store directory:
`pending/<server>/init.json` (answers, learned facts, completed phases),
`known_hosts` and the captured `vault-keyring.json`, all 0600. `--resume`
continues at the first incomplete phase (first contact, operator identity, host
bootstrap, WireGuard access, context, handoff). `--discard --confirm <server>`
drops the local state and the generated identity and warns when the host was
already bootstrapped. New init refuses while a context or pending init for the
same server exists. `server init --dry-run`
still connects, pins the host key and uploads a planning bundle; it stops before
apply and removes the pending state it created.

The age handoff is shown on the interactive path and written to `--secrets-out`
on the unattended path; both carry the same record. DNS/TLS pending is a
warning. Its HTTPS probe accepts an HTTP response irrespective of status: it
proves a TLS connection, not authenticated readiness or an application response.

## Host convergence and inspection

`eregion plan`, `apply`, `doctor`, `ssh` and `server status` are node-mode
commands: the CLI resolves the saved context, opens pinned SSH inside the
embedded WireGuard tunnel, and runs `eregion node rpc` on the node. Host RPC
methods do not open the application service, so plan and doctor still observe a
node whose database migrations are pending or whose API is down.

After a completed host apply the node records its non-secret intent at
`/var/lib/eregion/bootstrap/intent.json`; the database URL stays only in
`/etc/eregion/control-plane.env`, and TLS/key inputs are copied to
`inputs/` because the operator's original upload paths are temporary. Later
plan/apply runs reconstruct the same desired state from that file instead of
requiring flags or plaintext secrets over RPC. The engine still decides each
step by its `Check`; a new step version appears as a different step ID and is
applied, while a second apply reports the remaining steps as skipped.

`apply --auto-upgrade-nodes` runs the version lockstep first. A linux CLI on
the node's architecture uploads its own executable; a macOS CLI or an
architecture mismatch downloads the cosign-verified linux release matching the
CLI's version with `upgrade.FetchNodeBinary`, verifies the upload checksum and
installs it. A development build has no release to fetch and fails with a clear
error instead of refusing silently. `doctor` covers pending steps, sshd root and
password login, the WireGuard-only public SSH rule, the firewall, the Caddy
certificate for the main hostname, disk, CLI/node/schema/River versions and the
role's systemd units; any `fail` finding makes the command exit non-zero.
`ssh` is an interactive shell only (a terminal is required) and uses the
context's pinned host key and identity. `node unlock-ssh` is hidden from the
operator tree: it runs only on Linux as root from the provider console, opens
public SSH for 1–240 minutes through the embedded access scripts, and arms a
transient systemd timer that re-locks without reopening the commit-confirm
window.

| Current path | Ownership/purpose |
| --- | --- |
| `/etc/eregion/control-plane.env` | Service environment incl. DSN, auth/state, vault keyring and transport paths; bootstrap preserves operator-added lines. |
| `/etc/eregion/vault-keyring.json` | Vault master key, 0600 for the control-plane account; never in PostgreSQL and included in the handoff. Existing key material is never overwritten. |
| `/etc/eregion/auth.json` | Owner-only CI/webhook token hashes, actors, scopes and grants. Setup mints no operator token. |
| `/etc/eregion/node.json` | Node name/state directory and optional operator-installed helper configuration. |
| `/var/lib/eregion/bootstrap/` | Pre-database state/journal, protected cached credentials, uploaded bundles, commit-confirm snapshots, the recorded host intent (`intent.json`) and owner-only copies of its TLS/key inputs (`inputs/`). |
| `/var/lib/eregion/node/` | Node journal, per-effect records/locks, detached-task logs and identity. |
| `/var/lib/eregion/access/` | Operator-access snapshot and lockdown state; separate from bootstrap commit-confirm. |
| `/etc/caddy/Caddyfile` | Bootstrap-owned main hostname config, importing routes. |
| `/etc/caddy/eregion.d/routes.caddy` | Worker-owned generated app host routes. |
| `/usr/local/bin/node_exporter`, `/etc/systemd/system/node-exporter.service` | Pinned exporter and its unit, bound to the WireGuard address only; the access setup opens 9100 on `wg0`. |
| OS config dir + `/eregion/contexts.json` | Owner-only context store (SSH/WireGuard route and `auto_upgrade_nodes`); overridden by `EREGION_CONTEXT_FILE`. No API token. |
| Context store's parent + `/servers/<context>/` | Operator SSH key, WireGuard key and sudo password files. |
| Context store's parent + `/pending/<server>/` | Unfinished `server init`: answers, phases, pinned host keys and the captured vault keyring (0700 directory, 0600 files). |
| `/etc/sudoers.d/eregion-node` | Admin's passwordless `/usr/local/bin/eregion node rpc` rule, validated by `visudo`. |

Node effect identity is `operation:step`, stable across attempts. Execution
identity is `operation:attempt:step`. Check durable status before dispatch;
acknowledge completed matching effects rather than reapply. Locks on the node
are host-local. Per-environment PostgreSQL advisory locks serialize deployments
across workers; River queue concurrency is not that lock. Journal compaction
does not automatically delete durable effect records. Corruption fails closed.

## Current application actions

All use `POST /v1/operations`, the shared registry and one River job:

| Kind | Target | Main input |
| --- | --- | --- |
| `platform.app.create` | `platform` | `name`; optional `environment` (first environment + first revision) and `deploy` to queue the first deploy from that revision. |
| `platform.environment.create` | `app/<app>` | `name`, `node`, optional `build_node`, legacy ports/bind/health, `env`, `secret_refs`; revision `source`, `auto_deploy`, `webhook_secret_name`, `runtime`, `processes`, `config`, `secret_names`; optional `deploy` to queue the first deploy from that revision. |
| `platform.environment.update` | `env/<app>/<env>` | Partial patch: `node`, `build_node`, `source`, `auto_deploy`, `webhook_secret_name`, `runtime`, `processes`, `config`, `secret_names`; produces a new full revision. |
| `platform.process.scale` | `env/<app>/<env>` | `process`, `replicas`; produces a new revision. |
| `platform.config.set/unset` | `env/<app>/<env>` | `config` map or `keys` list; produces a new revision and never queues a deploy. |
| `platform.environment.secret.attach` | `env/<app>/<env>` | `name` only; the vault write is separate and no value enters the revision. |
| `platform.release.create` | `env/<app>/<env>` | `source`, optional `build`, `command`, `pull`; records a build row for git/directory sources. |
| `platform.build.stop` | `build/<build ID>` | Cancels the build, marks its operation cancelling, and signals the running pack process. |
| `platform.build.retry` | `build/<build ID>` | New build and release of the same exact commit; it does not deploy. |
| `platform.deploy` | `env/<app>/<env>` | Optional `release_id` and `ref`. Without a release it resolves the current revision's bound source to an exact commit, builds, records the release digest and deploys it with the current revision. With a release it redeploys that release with the current revision. |
| `platform.rollback` | `env/<app>/<env>` | Optional `to_release_id`; default resolves previous successful serving history, then uses that deployment's revision and exact secret revisions. |
| `platform.restart` | `env/<app>/<env>` | Reapplies the serving release with the current revision through the same blue/green path, so a scaled formation takes effect. |
| `platform.domain.add/remove` | `env/<app>/<env>` | `domain`, optional `primary` and `addresses` (the main node's public A/AAAA targets). Removing the primary is refused. |
| `platform.environment.delete` | `env/<app>/<env>` | `confirm` equal to `<app>/<environment>`; the worker removes labeled containers and routes before platform rows. |
| `platform.app.delete` | `app/<app>` | `confirm` equal to `<app>`; every environment is deleted and the app record last. |

Names: lowercase letters/digits/hyphens, start with a letter, max 40 characters.
Unknown input fields fail, including nested revision fields. Refer to
`internal/platform/actions.go` rather than copying JSON types into a second
client/service implementation.

Environment revisions are numbered per environment and immutable. The revision
is the desired state; an accepted action is not an applied route or a running
process. Deployability is the pure `platform.EvaluateDeployability` report over
the current revision plus artifact/domain observations, with DNS reported as
`unknown` until the domain row observes it. Inventory collections `revisions`
and `deployability` expose it, and each environment record carries its current
revision, processes and deployability.

The CLI `app create` and `env create` preview the canonical action on the node,
write the read credential and secret values through `vault.set`, then submit.
Only names enter the revision; a domain is a separate `platform.domain.add`
submission. The preview checks the target node placement, so a refused create
does not leave vault values behind. `env update` reads the current revision and
sends a merged patch; its terminal interview shows the diff before confirmation.
`secret set` previews the attach action, writes the value, then submits
`platform.environment.secret.attach` only when the current revision does not
already name it.

Deploy-time secret resolution prefers the current revision's secret names,
resolving each name's active latest revision and freezing the exact non-secret
revision identities on the deployment. A private git read credential is resolved
by NAME at build execution and passed only to the askpass child environment; the
value never enters the revision, operation input, events or logs. The
transitional pinned `secret_refs` path is used only when the revision names no
secrets. `platform.app.create` and `platform.environment.create` with deploy
intent queue the first deploy-from-revision operation in the same submission
transaction.

Image releases inspect/pull the image. Buildpacks builds require builder role
and capability, allowed source roots for directories, and allowlisted HTTPS git
hosts. `EREGION_BUILD_SOURCE_ROOTS` controls directory roots;
`EREGION_GIT_ALLOWED_HOSTS` replaces the public-forge allowlist. Git is rechecked
at execution, with redirects/other protocols disabled; a private HTTPS read
credential is supplied to git through the embedded askpass helper and the
transient node secret map, never in the build payload. A directory/git build on
a distinct build node is refused before `pack`: registry push/pull and dedicated
credentials do not exist. Installing a registry alone does not move images.

Deploy sequence: preflight -> build from the revision when the release is
pending -> release command -> candidate containers for every process replica on
dynamic loopback ports -> health for every web replica -> Caddy upstream switch
with all web replica backends -> public HTTPS verification through local Caddy ->
remove the previous deployment's containers. A failure before the switch removes
the candidates and leaves the previous release serving; a failure after the
switch restores the previous upstreams. Rollback and restart use the same path
with the recorded revision/secret snapshot and the current revision respectively.
Explicit retry creates new work; it does not rewrite a terminal failed record.

`app status` and `env status` join durable inventory with a read-only node RPC
Docker observation (`platform.workloads`). Each desired process replica is shown
with its container state and dynamic host port; a container from another
deployment is shown but not marked serving. Docker read failure leaves the
durable status visible and marks replica state `unknown`. Domain bindings show
DNS, route, TLS and public response as `unknown` until the domain work observes
them. `logs` uses `platform.logs` to stream the serving deployment's containers
as RPC events, filtered by `--process`; it does not read telemetry. The workspace
metrics graph still uses the telemetry data source.

Domains are changed in the submission transaction, then the worker reads all
bindings, writes one Caddy site per domain with the serving deployment's web
replica upstreams and reloads Caddy. Route writers take one PostgreSQL advisory
lock, adapt the new fragment through Caddy's `/adapt` endpoint, replace the file
with a fsync+rename, then load it; a rejected load restores the prior **file**,
not the domain rows. A deployment switch writes the same file with candidate
upstreams and verifies the primary domain through local Caddy before removing
the previous containers. A periodic maintenance job re-renders on worker start
and every 30 seconds after a failure; it updates `route_state` per domain. A
second periodic job observes DNS through 1.1.1.1 and 8.8.8.8 over UDP, then the
certificate chain/expiry and public response through local Caddy with the real
SNI. Proxied or wrong DNS stays unready. `domain add` records the main node's
public addresses so observation has an expected value. Inventory `domains` and
`domain list` expose each state separately. See [network](network.md) before
changing this path.

## API and extension seams

- `/healthz`: process liveness only; `/readyz`: public, unauthenticated, answers
  only ready or not (database and schema); not worker availability or public app
  health.
- `/v1/operations`: submit/preview; `/{id}`, `/{id}/events`, `/{id}/cancel`:
  actor-scoped state, bounded cursor pages and explicit cancellation.
- `/v1/platform?collection=…`: bounded inventory with app/environment/node/state
  filters. Base collections: summary, nodes, apps, environments, revisions,
  deployability, domains, releases, builds, deployments and changes; the
  internal DNS/access extensions add their own. The removed resources/backups and
  telemetry collections are not registered.
- `/v1/secrets…`: separate authenticated credential API; values are encrypted
  directly, never placed in ordinary action input.
- `/v1/webhooks/github`: bounded HMAC-verified push with one server-wide secret
  (`EREGION_WEBHOOK_SECRET_FILE`) and a repo/branch → app/env allowlist
  (`EREGION_WEBHOOK_ALLOW`); deduplicated **release build** only.
  `internal/api/webhook.go` already accepts a different action kind and input
  per event, and `webhook_deliveries` survives operation retention; MASTER D28
  moves secret and routing into the environment revision and deploys.
- `/private/…`: optional authenticated subject/device-grant proxy, not a VPN
  enrollment implementation.

Caddy currently exposes only `/readyz` and `/v1/webhooks/github` on the main
hostname. There is **no public deploy trigger path** yet; the CI endpoint and
per-environment push-to-deploy routing remain work list row 8. Do not expose all of
`/v1` as a shortcut. Main-node application hostnames are separate Caddy sites.

The operator `Backend` (`internal/cli`) and HTTP handlers share the
`api.AppService` functions for action submission/preview, inventory and vault
operations. New guided commands must use node RPC and share those services, not
enqueue directly or build a second HTTP path. Extensions register
strict normalizers/executors and inventory providers before runtime startup through
`controlplane.RegisterExtensions`; no extension gets a second queue. Registry
availability alone is not readiness: missing host/key/provider configuration
can leave advertised actions deliberately failing closed.
