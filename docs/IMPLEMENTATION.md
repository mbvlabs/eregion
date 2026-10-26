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
refuses with `node_version_mismatch` unless the node was auto-upgraded. The
worker-to-node step transport is a separate protocol boundary.

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
| `internal/cli/serverinit*.go`, `sshauth.go`, `handoff.go`, `login.go` | Guided setup, first contact, DNS, private access and token-free age handoff/import. |
| `internal/cli/context.go`, `internal/cli/noderpc.go`, `internal/host/contextstore.go` | Context resolution, node RPC client/server, version lockstep and SSH/WireGuard route. |
| `internal/rpc` | `eregion-rpc/v1` request/event/result wire types and bounded request decoding. |
| `internal/api` | Shared application services plus the HTTP authentication/grants, operation/inventory routes, vault, webhook and private proxy adapters. |
| `internal/controlplane` | pgx transactions, River, platform state/workers, deployment reconciliation, node resolution, extension wiring. |
| `internal/platform` | Validated action/input types, inventory model, placement rules, source/runtime steps and Caddy route renderer. |
| `internal/engine`, `internal/node`, `internal/noded`, `internal/transport` | Check/Apply journal, effect identity, detached tasks, pinned SSH and userspace WireGuard. |
| `internal/host`, `internal/access` | Bootstrap steps and host journal; admin/firewall/WireGuard/SSH lockdown scripts. |
| `internal/guide` | Embedded agent operating guide printed by `guide agents` and installed as `SKILL.md` by `skill install`. |
| `internal/vault`, `internal/secrets` | Revisioned ciphertext/keyring and redaction. |
| `internal/integrations`, `internal/telemetry` | Internal DNS/private-access actions (no operator CLI before v1) and the telemetry read adapters used by `logs` and the workspace graph. |
| `internal/viewer`, `internal/tui`, `internal/workspaceui`, `internal/cli/ui` | Reducer/follow, operation view, full-screen workspace and guided inline components. |
| `internal/release`, `internal/upgrade`, `scripts/*.sh`, `.github/workflows/publish-*.yml` | Cosign-verified channel manifests and artifacts; local CLI replacement only. Nodes are not upgraded by this path. |

## Current command surface

Use the **built binary**, not a stale `bin/eregion`, to inspect exact arguments:

```sh
just build
bin/eregion commands --json
bin/eregion help platform environments create --json
```

| Current commands | Meaning/limit |
| --- | --- |
| `server init` | Guided root first contact; Linux operator, matching server arch, IPv4-oriented DNS/access setup. |
| `login --file … --passphrase-file …` | Import an age transfer bundle into a token-free context (SSH and WireGuard keys, route). |
| `context list/show/use`, `context credential add` | Saved contexts and local server-auth-file editing. `use` takes `--name`; no remove command. |
| `platform apps create/list/status` | App record only; no guided full setup, update or delete. |
| `platform environments create/list/status` | Separate env record with node, port, bind, health and `--env`; no update/delete or secret-reference convenience flags. |
| `platform releases create/list` | Exactly one of `--image`, `--directory`, `--git-url`. Directory is on the server. |
| `platform deployments plan/deploy/list/retry/rollback` | Durable deployment actions; rollback takes a dry-run confirmation. |
| `domain add/remove <app> <environment> <hostname>`, `domain list` | Main-node desired host bindings + Caddy reload. Not observed TLS/route status. |
| `secret set/list/archive/export` | Metadata/write-only storage and explicit file export; no plaintext `get`. |
| `platform inventory`, `logs search/tail`, `ui` | Read-only inventory, telemetry reads and the full-screen workspace. |
| `operations get/events/follow/cancel` | Durable operation observation and explicit cancellation. |
| `bootstrap …`, `system migrate`, `serve`, `worker`, `node-agent …` | Current low-level host/service entry points. Not the desired everyday operator workflow. |
| `upgrade` (`--version`, `--channel`, `--dry-run`, `--yes`), `commands`, `help`, `guide agents`, `skill install`, `version` | `upgrade` replaces only the local CLI from a cosign-verified channel; it never touches a node. `guide agents` prints the embedded agent operating guide; `skill install` prompts for harnesses in a terminal or takes `--harness`, then writes the same text as `SKILL.md`. `commands` is discovery. |

The target tree is MASTER section 5. Its commands (`app create`, `env update`,
`deploy`, `doctor`, `ssh`, …) do not exist yet; renaming is not a substitute for
implementing the flow. `resources …` and the backup code are deleted (MASTER
D19), the Docker Compose telemetry stack install is deleted (D5), and `dns …`,
`access …`, `platform nodes …`, `platform fleet`, `platform submit`, `metrics …`
and `operations test` are no longer registered (D6, D22, D26). Their internal
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
linux/amd64 or linux/arm64 download for the future
`apply --auto-upgrade-nodes`; no node install or restart is implemented here.
There is no Ed25519 manifest, trusted public-key file, rollback journal or
one-host operator/node replacement state any more. Release mechanics are in
[upgrades](upgrades.md).

## Database: embedded schema 011

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

Tables below have the literal `eregion_` prefix:

| Table(s) | Key and authoritative facts |
| --- | --- |
| `schema_migrations` | Version/checksum/application time. River maintains its own schema history. |
| `operations` | `op_…` ID; actor, kind, target, normalized input/hash, idempotency key, state, attempt, step count, cursor, River job ID and error code. |
| `operation_events` | `(operation_id, sequence)`; JSON event. |
| `operation_steps` | `(operation_id, step)`; inputs hash, execution identity, attempt, state, timestamps and result/error. |
| `platform_nodes` | Name; roles/capabilities, status, SSH address/user/**host key**, attested identity, probes, build concurrency/user and WireGuard public state. Private keys stay in files. |
| `platform_apps` | Opaque app ID and unique name. No stored setup wizard/source configuration. |
| `platform_environments` | App FK + name; runtime/build node, stable/candidate/container ports, bind, health settings/observation, config, pinned secret refs, desired/applied/serving release IDs and `current_revision_id`. The mutable config/health/container fields mirror the current revision for the existing single-container deploy worker; they go away with the per-process deploy row. `secret_refs` remains the transitional pinned form, used only when the current revision names no secrets. |
| `platform_environment_revisions` | Immutable `erv_…` ID and per-environment number; operation/actor/operation/summary change record; source binding, auto-deploy flag, webhook secret NAME, runtime JSON, processes JSON, plain config and secret NAMEs, plus the first-deploy intent flag. An UPDATE trigger refuses edits; deleting the environment still cascades. |
| `platform_releases` | Release ID and unique operation FK; app/env FKs, source/build/command JSON, image/image ID, pending/ready/failed status and error. |
| `platform_deployments` | Deployment ID and unique operation FK; action, release, previous release, environment revision ID, exact resolved secret revisions (name -> non-secret vault revision identity), state and errors. |
| `platform_changes` | App/env history, sequence, operation/actor, desired/applied JSON and summary. |
| `platform_domains` | Domain name PK, environment FK, `is_primary` with a partial unique index enforcing one primary per environment, creation timestamp. Still no backend, TLS, observation or failure columns. |
| `secrets`, `secret_revisions` | Namespace `(app, environment, name)` and revision; state/CAS, key ID, ciphertext and hashes. Not FKs to platform app/env rows. |
| `access_grants`, `integration_audit` | Grant JSON and durable authorization/provider audit JSON. |
| `webhook_deliveries` | Hashed delivery ID, exact-body hash, actor/kind/target, pending/submitted state and operation ID. |

Boundaries around the MASTER app-model work (work list rows 4–5):

- Runtime and candidate ports are globally unique in the current schema, not
  scoped per node. Defaults allocate 20000–29999 and 30000–39999 respectively.
  The environment revision is now the source of truth, but the deploy worker
  still reads these mirrored legacy columns.
- Source/build configuration exists both on releases (for the current release
  action) and on environment revisions. Deploying from the revision's bound
  source, including private git credentials and auto-deploy, is work list row 5.
- Environments still hold pinned `secret_refs` for compatibility with the
  current deploy worker. New revisions use secret NAMEs and resolve their
  current revisions per deploy (D18); the fallback and the legacy column go away
  with row 5 once all deployment paths use revisions.
- One container per environment on a stable port; the revision already declares
  the per-process formation and replicas, but MASTER D17/D21 per-process
  containers on dynamic loopback ports are implemented by row 5.
- The first-deploy flag on a revision records intent only; no build or
  deployment is queued until deploy-from-revision lands.
- SQL `ON DELETE CASCADE` is not a safe application deletion implementation:
  release/history references, secret namespaces, containers, Caddy and file-backed
  state need deliberate handling. There are no app/env delete actions today.
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

`server init` collects host/identity, node/context name, hostname and external DSN;
pins the host key; checks root/OS/arch; uploads the running binary; previews and
applies bootstrap; sets up admin/WireGuard; tests the private channel; arms and
confirms lockdown from a fresh connection; saves its token-free context and
offers the interactive handoff. `login` imports that handoff. Bootstrap installs
the binary once at `/usr/local/bin/eregion` (the systemd units use that path) and
installs `/etc/sudoers.d/eregion-node` with only
`NOPASSWD: /usr/local/bin/eregion node rpc`; the separate access rule covers
setup/break-glass lockdown.

Current bootstrap accepts an identity, ssh-agent/default keys and interactive
encrypted-key passphrases. There is no answers TOML, password-stdin, robust local
pending-init phase machine or cross-platform binary selection. Low-level journal
resumption is not proof that re-running `server init` after root SSH closes works.
The age handoff is shown only on the interactive setup path; unattended handoff
parity is not established. DNS/TLS pending is currently a warning. Its HTTPS
probe accepts an HTTP response irrespective of status: it proves a TLS connection,
not authenticated readiness or an application response.

`server init --dry-run` still connects, pins trust and uploads a planning bundle.
It stops before host apply; it is not an entirely side-effect-free local command.

| Current path | Ownership/purpose |
| --- | --- |
| `/etc/eregion/control-plane.env` | Service environment incl. DSN, auth/state and transport paths; bootstrap preserves operator-added lines. |
| `/etc/eregion/auth.json` | Owner-only CI/webhook token hashes, actors, scopes and grants. Setup mints no operator token. |
| `/etc/eregion/node.json` | Node name/state directory and optional operator-installed helper configuration. |
| `/var/lib/eregion/bootstrap/` | Pre-database state/journal, protected cached credentials, uploaded bundles and commit-confirm snapshots. |
| `/var/lib/eregion/node/` | Node journal, per-effect records/locks, detached-task logs and identity. |
| `/var/lib/eregion/access/` | Operator-access snapshot and lockdown state; separate from bootstrap commit-confirm. |
| `/etc/caddy/Caddyfile` | Bootstrap-owned main hostname config, importing routes. |
| `/etc/caddy/eregion.d/routes.caddy` | Worker-owned generated app host routes. |
| OS config dir + `/eregion/contexts.json` | Owner-only context store (SSH/WireGuard route and `auto_upgrade_nodes`); overridden by `EREGION_CONTEXT_FILE`. No API token. |
| Context store's parent + `/servers/<context>/` | Operator SSH key, WireGuard key and sudo password files. |
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
| `platform.app.create` | `platform` | `name`; optional `environment` (first environment + first revision) and `deploy` intent. |
| `platform.environment.create` | `app/<app>` | `name`, `node`, optional `build_node`, legacy ports/bind/health, `env`, `secret_refs`; revision `source`, `auto_deploy`, `webhook_secret_name`, `runtime`, `processes`, `config`, `secret_names`. |
| `platform.environment.update` | `env/<app>/<env>` | Partial patch: `node`, `build_node`, `source`, `auto_deploy`, `webhook_secret_name`, `runtime`, `processes`, `config`, `secret_names`; produces a new full revision. |
| `platform.process.scale` | `env/<app>/<env>` | `process`, `replicas`; produces a new revision. |
| `platform.config.set/unset` | `env/<app>/<env>` | `config` map or `keys` list; produces a new revision and never queues a deploy. |
| `platform.environment.secret.attach` | `env/<app>/<env>` | `name` only; the vault write is separate and no value enters the revision. |
| `platform.release.create` | `env/<app>/<env>` | `source`, optional `build`, `command`, `pull` |
| `platform.deploy` | `env/<app>/<env>` | `release_id`; pins the current environment revision and resolved secret revisions. |
| `platform.rollback` | `env/<app>/<env>` | Optional `to_release_id`; default resolves previous successful serving history, not simply current applied release. |
| `platform.domain.add/remove` | `env/<app>/<env>` | `domain` (app/env resolved from target) |

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

Deploy-time secret resolution prefers the current revision's secret names,
resolving each name's active latest revision and freezing the exact non-secret
revision identities on the deployment. The transitional pinned `secret_refs`
path is used only when the revision names no secrets. A first deploy requested by
`platform.app.create` is recorded on the revision as intent; deploy-from-revision
is work list row 5.

Image releases inspect/pull the image. Buildpacks builds require builder role
and capability, allowed source roots for directories, and allowlisted HTTPS git
hosts. `EREGION_BUILD_SOURCE_ROOTS` controls directory roots;
`EREGION_GIT_ALLOWED_HOSTS` replaces the public-forge allowlist. Git is rechecked
at execution, with redirects/other protocols disabled. A directory/git build on
a distinct build node is refused before `pack`: registry push/pull and dedicated
credentials do not exist. Installing a registry alone does not move images.

Deploy sequence: preflight -> optional release command -> candidate -> candidate
health -> stable-port serving replacement -> serving health -> cleanup. Failed
candidates preserve the previous server; post-switch failures attempt restoration.
This is not Caddy backend blue/green. Rollback is another deployment with history.
Explicit retry creates new work; it does not rewrite a terminal failed record.

Domains are changed in the submission transaction, then the worker reads all
bindings, writes routes to localhost stable ports and reloads Caddy. A rejected
reload restores the prior **file**, not the domain rows. Writes are currently
not atomic/serialized globally; domain actions are not journaled node steps;
there is no general route reconciler. Inventory/list exposes desired bindings
even after a failed apply. See [network](network.md) before changing this path.

## API and extension seams

- `/healthz`: process liveness only; `/readyz`: public, unauthenticated, answers
  only ready or not (database and schema); not worker availability or public app
  health.
- `/v1/operations`: submit/preview; `/{id}`, `/{id}/events`, `/{id}/cancel`:
  actor-scoped state, bounded cursor pages and explicit cancellation.
- `/v1/platform?collection=…`: bounded inventory with app/environment/node/state
  filters. Base collections: summary, nodes, apps, environments, revisions,
  deployability, releases, deployments and changes; the internal DNS/access
  extensions add their own. The removed resources/backups and telemetry
  collections are not registered.
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
hostname. There is **no public deploy trigger path** yet. Do not expose all of
`/v1` as a shortcut. Main-node application hostnames are separate Caddy sites.

The operator `Backend` (`internal/cli`) and HTTP handlers share the
`api.AppService` functions for action submission/preview, inventory and vault
operations. New guided commands must use node RPC and share those services, not
enqueue directly or build a second HTTP path. Extensions register
strict normalizers/executors and inventory providers before runtime startup through
`controlplane.RegisterExtensions`; no extension gets a second queue. Registry
availability alone is not readiness: missing host/key/provider configuration
can leave advertised actions deliberately failing closed.
