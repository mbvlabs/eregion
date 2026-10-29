# Implementation reference

Checked against the integrated source on 2026-10-28. This describes **current code**, not the
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
refuses with `node_version_mismatch` unless the node was auto-upgraded. A
`--dry-run` invocation never installs or restarts a node binary: on a mismatch
it returns `node_version_mismatch` naming the upgrade it would have made, for
every command that takes `--dry-run`.
Read-only RPC methods `platform.workloads` and `platform.logs` read Docker
directly and do not need the control database; `operations.list` uses the shared
application service. Operation reads and cancellation over node RPC carry the
SSH-authenticated operator's authority, so they may open any operation on the
node; the public HTTP API stays actor- and grant-scoped. The worker-to-node step
transport is a separate protocol boundary.

Operator WireGuard is shared per tunnel configuration: the first command that
needs the node spawns the hidden `eregion tunnel serve` daemon, which owns the
only userspace WireGuard device for that operator key and proxies TCP dials
over a unix socket in the private tunnel directory under the operator's
eregion config (`0700`, socket and lock `0600`). Every operator-side dial,
including server init, login's host-key probe, node RPC and the node_exporter
check, goes through that daemon; it exits after ten idle minutes and
`eregion tunnel status`/`tunnel stop [--force]` inspect or restart it.
`EREGION_TUNNEL=inprocess` keeps the old in-process tunnel as a fallback, and
non-unix builds use the in-process tunnel because the daemon is unix-only.

API and worker are supervised separately by `eregion-api.service` and
`eregion-worker.service`; `eregion node serve` can also run workers in-process.
Domain intent and queue insertion are atomic. Long remote effects do not hold a
SQL transaction open. Workers recheck placement and reconcile step
acknowledgements. An SSH disconnect never owns the lifetime of accepted work.

## Code map

| Location | Responsibility |
| --- | --- |
| `cmd/eregion` | One executable entry point: the operator surface, hidden `node` mode (`rpc`, `serve`, `worker`, `migrate`, `bootstrap`) and hidden `node-agent`. |
| `internal/cli/tree.go`, `meta.go`, `run.go`, `deploy.go` | Command catalog, safety metadata, output selection, dispatch and the deploy/rollback/build/release pipeline commands. |
| `internal/cli/serverinit*.go`, `sshauth.go`, `handoff.go`, `login.go` | Guided setup (preflight, answers/pending, plan/confirm, apply, guarded handoff), first contact, DNS, private access and token-free age handoff/import. |
| `internal/cli/context.go`, `internal/cli/noderpc.go`, `internal/cli/hostnode.go`, `internal/cli/observe_rpc.go`, `internal/host/contextstore.go` | Context resolution, node RPC client/server, version lockstep, host and local RPC, Docker workload/log reads, SSH/WireGuard route. |
| `internal/cli/observe.go`, `internal/cli/logs.go`, `internal/cli/delete.go`, `internal/cli/operations.go` | Extended app/environment status, container logs over the operator channel, target-confirmed deletion and operation-history filters. |
| `internal/cli/host.go`, `internal/cli/serverstatus.go`, `internal/cli/nodeunlock.go` | `plan`, `apply`, `doctor`, `ssh`, `server status` and hidden `node unlock-ssh`. |
| `internal/protect`, `internal/totp`, `internal/tokens` | Root-only TOTP gate on the node and hashed per-use deploy tokens in `/etc/eregion/auth.json`. |
| `internal/cli/protect.go`, `token.go`, `webhook.go`, `secretexport.go` | TOTP setup/status, token mint/list/revoke, push-to-deploy helpers used by `env update`, and the protected handoff re-export. |
| `internal/rpc` | `eregion-rpc/v1` request/event/result wire types and bounded request decoding. |
| `internal/api` | Shared application services plus the HTTP authentication/grants, operation/inventory routes, vault, CI deploy, per-environment push webhook and private proxy adapters. |
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
| `server init` | Guided root first contact; Linux/macOS operator, arch-matching linux node binary, preflight, answer flags/TOML, plan/confirm, vault keyring and node_exporter steps, pending-init resume/discard, the final doctor gate and guarded handoff. First contact requires SSH on port 22; another port is refused before any remote change. The plan lists the host steps and the setup.sh access changes (admin user, wg0, firewall, SSH lockdown switchover). A pending init stores the pinned SSH host key and fingerprint; `--dns-ready` skips the confirmation prompt interactively, non-interactively and on resume. `--skip-doctor` skips the gate with a warning; a failing check returns `doctor_failed` with the failed checks and resume hint. Declining the plan (or any exit that does not apply) removes the uploaded planning bundle from the server. |
| `plan`, `apply [--dry-run]`, `doctor`, `ssh [node] [--command CMD]`, `server status` | Node-mode host convergence and inspection. `apply` records the host intent, streams step progress on stderr and skips completed steps; `doctor` exits non-zero on a failing finding. `ssh` opens a shell at a terminal or runs one command with `--command`; piped or JSON output runs without a PTY, a sudo password prompt becomes an `input_required` fault whose code and fix the runner still prints, and the remote exit status propagates. `--auto-upgrade-nodes` installs the operator's build on a mismatched node: a `dev` node accepts the caller's checksum, while a published node requires its own signed release manifest to match; `--dry-run` never installs. |
| `node unlock-ssh [--minutes N]` | Hidden node-only break-glass command, run as root from the provider console; reopens public SSH for a bounded window and schedules the re-lock. |
| `login --file … --passphrase-file … [--replace NAME]` | Import an age transfer bundle into a token-free context (SSH and WireGuard keys, route). It prompts for the bundle only at a human terminal; `--json` or a non-terminal stderr never prompts and needs `--passphrase-file`. The bundle must carry the context, operator keys, admin user, WireGuard address/public key/endpoint and SSH host key. Login builds and proves the identity in a sibling temporary directory, pins the host key in a copy of known_hosts and runs the readiness check there; only then does it swap the directory and save the context. An existing context name is refused with `conflict` unless `--replace` names it exactly, and any failure leaves the old context and pinned key byte-for-byte unchanged. |
| `context list/show/use/remove` | Saved contexts, target-confirmed removal with its key files. `use` takes `--name`; `remove <name>` takes `--confirm <name>`. Every node command accepts the root `--context NAME`; resolution is the flag, then `EREGION_CONTEXT`, then the current context, and an unknown name is `not_found` with `eregion context list`. The node client, local identity reads (`secrets export`), DNS addresses and endpoint labels all use that one selected context, so they cannot disagree; only `context show/use` with no `--name` read the current context on purpose. `login` and `server init` keep their own naming inputs and refuse a `--context` that disagrees, including on `server init --resume`. `context credential add` is removed: tokens are minted only through `token create` with the human factor (D7). |
| `app create/list/status/delete` | Guided or flag-only first environment: source, Buildpacks settings, processes, plain config, vault secret names and an optional domain. `create` accepts `platform.app.create` with the first domain, primary flag and main-node addresses; the binding commits in the same transaction before the first deploy is queued, and the accepted operation exposes that deploy's ID as `first_deploy_operation_id`. A retry with the operator's `--idempotency-key` previews as a replay, writes no vault revision, and returns the original operation including that ID; without the key an existing name is a conflict. In a terminal with deploy requested and no `--detach`, `create` follows the create operation and then the first deploy with the deploy viewer and outcome; Ctrl-C detaches with the follow command. `--detach`, JSON and no-TTY return at accepted. List/status read inventory. `status` shows the serving release, processes, per-domain state and recent operations; `delete` takes `--confirm <app>` and, unless `--dry-run`, a TOTP code typed at a human terminal (D7, D13). The node verifies the code and confirmation in the same submission; dry-run preview needs neither and stays available to an agent. |
| `env create/update/list/status/delete` | `create` accepts an environment with its first revision, optional domain binding and its first deploy unless `--no-deploy`; the accepted operation exposes the queued deploy's `first_deploy_operation_id`, and a terminal follows create then the first deploy with the deploy viewer unless `--detach` is given. A retry with the operator's `--idempotency-key` replays the accepted operation including that ID without rewriting vault values; `update` merges source, placement, runtime and process changes into a new immutable revision. In a terminal with no change flags and no webhook flags, `update` walks the current revision prefilled, shows the diff and confirms; `--auto-deploy[=false]` and `--rotate-webhook-secret` configure push to deploy and show the URL and secret once at a terminal. With flags or JSON/no-TTY it is flag-only. List/status include revision, processes and deployability. `status` shows serving release, per-process replicas, domains and recent operations; `delete` takes `--confirm <app>/<env>` and, unless `--dry-run`, a TOTP code typed at a human terminal (D7, D13). |
| `process list/scale` | Read the desired formation; scale one process's replicas in a new revision. A serving environment also queues a blue/green restart in the same transaction so the new formation runs immediately; without a serving deployment the next deploy applies it. |
| `config set/unset/list` | Plain variables as a new revision or read from the current revision. No deploy is queued. |
| `deploy <app> <env> [--ref REF \| --release ID]` | Node-mode `platform.deploy`. In a terminal it prints source, ref/commit, revision and processes, then follows the operation with the shared viewer (build log lines and stages). `--detach` returns after acceptance. |
| `rollback <app> <env> [--to ID]` | Node-mode `platform.rollback`; requires `--confirm <app>/<env>`, validated by the node and control plane against the current target (D12). There is no plan-token scheme for rollback. |
| `build list/logs/stop/retry`, `release list`, `restart <app> <env>` | Build lifecycle over `platform_builds`; `build logs` reads (and with `--follow` streams) the build operation's events. `build get` is served by the `builds` inventory filter. `restart` reapplies the serving release through the blue/green path. |
| `domain add/remove <app> <environment> <hostname> [--primary] [--check-path PATH]`, `domain list [--app --environment]` | Node-mode domain bindings, one primary per environment, serialized atomic Caddy writes and separate route/DNS/certificate/public observations. `add --check-path` sets the binding's optional observation path; empty defaults to the web health path. `domain add --dry-run --json` returns the preview object and DNS records under `data`. `remove` takes `--confirm <hostname>` and refuses the primary. |
| `secret set/list/archive/export`, `secrets export` | Metadata/write-only storage and explicit file export; `set` writes the value and then attaches the name to the environment as a new revision. No plaintext `get`. Both exports need a TTY and a TOTP code; `--yes`, `--json`, and a pipe are refused, and their command metadata declares that they prompt. `secret export` carries the code in the same node request that returns the value (`vault.export`); there is no separate pre-check request to skip. `secrets export` rebuilds the server-init handoff (including the vault keyring) and honours `--secrets-out`. |
| `token create/list/revoke` | Per-use deploy tokens for one app/environment, expiring, hashed in `/etc/eregion/auth.json`, shown once. `create` needs a TTY and TOTP code, never `--json`/`--yes`. |
| `protect init/status` | Node-root TOTP setup (`/etc/eregion/protect.json`, shown once as QR/otpauth) and non-secret status. |
| `logs <app> <environment> [--process NAME] [--follow] [--since] [--tail]` | Reads the serving deployment's Docker containers through node RPC; lines are prefixed with process/replica. No telemetry node is needed. Ctrl-C detaches. `--json` without `--follow` returns one envelope; NDJSON requires `--output ndjson`, and `--json --follow` is a usage error that names that fix. |
| `ui` | Full-screen workspace over the same inventory and node-mode action service. The graph still uses the telemetry read adapter for metrics. |
| `operations list/get/follow/cancel` | Over node RPC the SSH-authenticated operator has server authority: an unfiltered list shows every operation on the node newest first (bounded by `--limit`), and snapshot/follow/cancel may open any operation, including CI tokens, the GitHub webhook and other operators' submissions. `--app`/`--environment` select the platform target and include the environment's `build/<id>` operations. State and limit filters apply. The public HTTP API remains strictly actor- and grant-scoped. |
| `node serve`, `node worker`, `node migrate`, `node bootstrap …` | Hidden node-mode entry points on the Linux server. They are not part of the operator listing; systemd runs the services, bootstrap/apply converges the host, and `node migrate` applies explicit schema changes. `node rpc` and `node unlock-ssh` remain hidden node paths. |
| `upgrade` (`--version`, `--channel`, `--dry-run`, `--yes`), `commands`, `help`, `guide agents`, `skill install`, `version` | `upgrade` replaces only the local CLI from a cosign-verified channel; it never touches a node. `guide agents` prints the embedded agent operating guide; `skill install` prompts for harnesses in a terminal or takes `--harness`, then writes the same text as `SKILL.md`. `commands` is discovery. |

All of MASTER section 5's app, environment, deployment and observation surface
exists. The `platform` command group is deleted: `app`, `env`, `process`,
`config`, `domain`, `deploy`, `rollback`, `restart`, `build`, `release`, `logs`
and `operations` are the operator commands. Token/protect commands remain for
work list row 8. `resources …` and the backup code are deleted (MASTER D19), the
Docker Compose telemetry stack install is deleted (D5), and `dns …`, `access …`,
`platform …`, `metrics …` and `operations test` are no longer registered
(D6, D19, D22, D26). Their internal packages remain for the after-v1 phases
where the work list says they return. `bootstrap …`, `serve`, `worker` and
`system migrate` are not operator commands; they live under the hidden Linux-only
`node` group (`node rpc`, `node unlock-ssh`, `node serve`, `node worker`,
`node migrate`, `node bootstrap …`) and `commands --json` lists only the section
5 tree plus `help`.

## Human presentation and waiting

Human plain output and JSON stay separate schemas. When stderr is the process's
terminal and the invocation is ordinary plain output, `nodeClient.call` draws a
single `ui.Live` status line with a spinner and present-tense label
(`Reading app andurel…`, `Submitting…`) while a remote RPC waits; the line is
cleared when the answer arrives. The wait is not drawn for JSON/NDJSON, a
redirected or captured stderr, a prompt, or a call that streams events
(`apply`, `logs`, follows, and the live tasks in `server init`). Nested waits
keep the outer label, and the existing `NO_COLOR`/colour-profile handling
applies through `ui.Writer` and the plain-output writer.

Read command plain output (`env/app status`, app/env/domain/config/secret/
process/release/build/token/operations lists, `server status`, `doctor`) uses
the shared `internal/cli/ui` palette: a section header, tables whose headers are
dim and whose state words carry success/caution/failure colour, and aligned
key/value fields. The styled text is built once and the output writer resolves
the destination's colour profile before `SafeText`, so a pipe, file, dumb
terminal or `NO_COLOR` gets the same information without ANSI. JSON output is
unchanged.

Following a terminal failed operation renders its outcome and returns the
recorded error code's nonzero exit status (fallback `precondition_failed`, exit
5, for missing/unknown codes). JSON/NDJSON return a failure envelope with the
operation ID and state. Successful follows exit 0; remote cancellation exits
130 with wording distinct from viewer detach. Guided create and followed deploys
share these semantics; accepted-only submissions do not claim completion.

## Release and local upgrade

`internal/buildinfo` carries `Version`, `Commit`, `Channel` and the public
`ReleaseBaseURL` (`https://eregion.mbvlabs.com`). `internal/release`
resolves a channel or version manifest, verifies its sigstore bundle against
the pinned publish workflow identity, validates the manifest against the
channel, and caches the platform binary only after its signed SHA256 matches.
TUF/sigstore trust material is cached, not pinned in the binary.

`internal/upgrade` holds the local-only plan/preflight/atomic swap. `upgrade`
downloads the verified binary, checks its reported version and channel, and
replaces only the running CLI. `upgrade.FetchNodeBinary` is the verified
linux/amd64 or linux/arm64 download used when a macOS CLI or an architecture
mismatch prevents pushing the operator's own executable: the node receives the
release matching the CLI's version and the API/worker units restart. A dry run
never reaches this path.

`upgrade.install` params are frozen at `{path, sha256}`: an older node decodes
them strictly and must accept the request that upgrades it. The operator CLI's
`Version` and `Channel` travel instead as `release.json`, uploaded next to the
binary. A node built for a published channel reads that declaration (an upload
without one is refused) and resolves that version's signed manifest itself (`release.Client.Resolve`, cosign-verified) and
accepts only the `linux/<arch>` artifact whose sha256 matches the staged file; a
dev CLI against such a node is refused with an instruction to use a released
CLI, and an unreachable manifest returns `unavailable`. The node never falls
back to trusting the upload. A node whose own `buildinfo.Channel` is `dev` keeps
the checksum-only developer path, which never applies to a published node. The
API and worker units restart only after a verified install.
There is no Ed25519 manifest, trusted public-key file, rollback journal or
one-host operator/node replacement state any more. Release mechanics are in
[upgrades](upgrades.md).

## Database: embedded schema 016

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
| `014_release_command_runs.sql` | One durable release command run per `(release_id, command_digest)`: state, operation, error and timestamps. A release-scoped row, cascading with its release. |
| `015_domain_check_path.sql` | Optional per-domain `check_path`; empty uses the environment's web health path. |
| `016_container_cleanup.sql` | Leaked-container retry rows: container name PK, environment/node/deployment, reason, attempts, last error and next attempt. Cascades with the environment. |

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
| `platform_domains` | Domain name PK, environment FK, `is_primary` with a partial unique index enforcing one primary per environment, and an optional `check_path`. `desired_revision`/`applied_revision` and `route_state`/`route_error` record the Caddy write; `expected_addresses` plus `dns_state`/`dns_addresses` record public-resolver observation; `certificate_state`/`certificate_not_after` and `public_state`/`public_status` keep the other observations separate. |
| `platform_release_command_runs` | `(release_id, command_digest)` PK; the owning operation, running/succeeded/failed state, error code/message and timestamps. One row per release and command specification; it cascades with its release. |
| `platform_container_cleanup` | Container name PK; environment/node/deployment, reason, attempts, last error and next attempt. A failed removal is retried with backoff; the row cascades with its environment. |
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
  on every node recorded by the target's deployment revisions (plus the current
  environment node), delete the row-6 domain state for the target environments,
  then rewrite the routes through the one serialized writer (`syncRoutes`, one
  advisory lock) and reload Caddy. Only then are deployments, releases, builds,
  revisions, change rows, vault secret namespaces and the environment/app rows
  deleted. A failed container or Caddy step leaves the platform rows available
  for retry. Operations and operation events are never deleted; operation
  `--app`/`--environment` history still shows the deletion after the platform
  record is gone. The node validates the target-naming `--confirm` token against
  the records it loads.
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
eregion node migrate --dry-run --json
# Only after explicitly authorizing this database's migration:
eregion node migrate --json
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
dump/restore/readiness subset against generated test databases. It runs the
local PostgreSQL client tools when their major version is at least the
server's, and otherwise runs `pg_dump`/`pg_restore` from the matching
`postgres:<major>-alpine` image, so an older runner client cannot abort the
dump. It is not evidence of live writer quiescence, DSN cutover, keyring
recovery, HA or recovery-time goals.

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
`/etc/eregion/node.json` (node name, state directory and main hostname, owned by
the control-plane account) and
`/etc/eregion/vault-keyring.json` (0600, owned by the control-plane account and
named by `EREGION_VAULT_KEY_FILE` in `control-plane.env`) and node_exporter
1.11.1 with a pinned sha256 per architecture, listening only on
`10.78.0.1:9100`; the access setup allows 9100 on `wg0` and starts the unit once
`wg0` exists. `internal/access` remains the hardening/WireGuard/commit-confirm
lockdown implementation.

After lockdown the CLI pins the WireGuard host key, saves the token-free context,
and calls node RPC `ready`. It then checks node_exporter over the tunnel, asks the
public resolvers once, waits for Caddy's certificate, and runs the doctor gate
(`host.doctor` with `init: true`). Init mode carries an `AtInit` filter for checks
that cannot pass before the first handoff; no current doctor check is excluded,
and a pending certificate for the main hostname stays a warning. Any remaining
`fail` stops init with `doctor_failed`, the failing checks and their fixes, and
the resume instruction. `--skip-doctor` skips the gate and
leaves a warning in the result. Only then does init build the handoff: every
server secret in plain text (operator keys, sudo password, DSN, vault keyring)
plus an armored age transfer bundle with a generated passphrase and break-glass
instructions. A terminal waits for `stored`; JSON/no-TTY requires `--secrets-out`
before any change and writes the whole record to a new 0600 file. No API token is
in the handoff.

The unfinished init is saved locally under the context store directory:
`pending/<server>/init.json` (answers, learned facts, completed phases),
`known_hosts` and the captured `vault-keyring.json`, all 0600. `--resume`
continues at the first incomplete phase (first contact, operator identity, host
bootstrap, WireGuard access, context, readiness/certificate checks, doctor gate,
handoff). After a failed doctor gate, resume therefore re-runs only the gate and
the handoff; once the gate has passed it is not repeated, and the passing report
(or the `--skip-doctor` marker) is stored in the pending init so the resumed
result shows the same doctor line without inventing a new count.
`--discard --confirm <server>` drops the local state and the generated identity
and warns when the host was already bootstrapped. New init refuses while a context or pending init
for the same server exists. `server init --dry-run` still connects, pins the host
key and uploads a planning bundle; it stops before apply and removes the pending
state it created. The interactive result card includes the doctor summary line.

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
`ssh` uses the context's pinned host key and identity. Without `--command` it
needs a terminal and opens an interactive shell; `--command` runs one remote
command and exits with its status. The command gets a PTY only when stdin and
stdout are terminals and output is plain, so `ssh --command … | cat` returns
the remote bytes unchanged and sudo's no-terminal refusal becomes an
`input_required` fault instead of a bare exit; because the remote streams are
already written, the runner separately emits that fault and its fix on stderr
(plain) or as the failure envelope (`--json`). `node unlock-ssh` is hidden from the
operator tree: it runs only on Linux as root from the provider console, opens
public SSH for 1–240 minutes through the embedded access scripts, and arms a
transient systemd timer that re-locks without reopening the commit-confirm
window.

| Current path | Ownership/purpose |
| --- | --- |
| `/etc/eregion/control-plane.env` | Service environment incl. DSN, auth/state, vault keyring and transport paths; bootstrap preserves operator-added lines. |
| `/etc/eregion/vault-keyring.json` | Vault master key, 0600 for the control-plane account; never in PostgreSQL and included in the handoff. Existing key material is never overwritten. |
| `/etc/eregion/auth.json` | Owner-only deploy-token hashes, names, actors, scopes, grants and expiry. Setup mints no operator token; `token create` restarts the API so a new hash is loaded. |
| `/etc/eregion/protect.json` | Root-only confirmed/pending TOTP secret and its replay/lockout state. Never in the database, revision, operation, or journal. |
| `/etc/eregion/node.json` | Node name, state directory, main hostname and optional operator-installed helper configuration. The control-plane role writes it too, so node RPC, CI/push URLs and `secrets export` work on the main node. |
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

All use the shared action registry and transactional River submission. Operators
reach these services through node RPC; the HTTP adapter is not the operator
control channel and refuses real app/environment deletes:

| Kind | Target | Main input |
| --- | --- | --- |
| `platform.app.create` | `platform` | `name`; optional `environment` (first environment + first revision, including its `domain`, `domain_primary` and `domain_addresses`) and `deploy` to queue the first deploy from that revision after the binding commits. |
| `platform.environment.create` | `app/<app>` | `name`, `node`, optional `build_node`, legacy ports/bind/health, `env`, `secret_refs`; revision `source`, `auto_deploy`, `webhook_secret_name`, `runtime`, `processes`, `config`, `secret_names`; first `domain`, `domain_primary`, `domain_addresses`; optional `deploy` to queue the first deploy after the binding commits. |
| `platform.environment.update` | `env/<app>/<env>` | Partial patch: `node`, `build_node`, `source`, `auto_deploy`, `webhook_secret_name`, `runtime`, `processes`, `config`, `secret_names`; produces a new full revision. |
| `platform.process.scale` | `env/<app>/<env>` | `process`, `replicas`; produces a new revision and, when a serving deployment exists, queues a blue/green `platform.restart` of the serving release in the same transaction. |
| `platform.config.set/unset` | `env/<app>/<env>` | `config` map or `keys` list; produces a new revision and never queues a deploy. |
| `platform.environment.secret.attach` | `env/<app>/<env>` | `name` only; the vault write is separate and no value enters the revision. |
| `platform.release.create` | `env/<app>/<env>` | `source`, optional `build`, `command`, `pull`; records a build row for git/directory sources. |
| `platform.build.stop` | `build/<build ID>` | Cancels the build, marks its operation cancelling, and signals the running pack process. |
| `platform.build.retry` | `build/<build ID>` | New build and release of the same exact commit; it does not deploy. |
| `platform.deploy` | `env/<app>/<env>` | Optional `release_id` and `ref`. Without a release it resolves the current revision's bound source to an exact commit, builds, records the release digest and deploys it with the current revision. With a release it redeploys that release with the current revision. CI and push to deploy submit `ref`. |
| `platform.rollback` | `env/<app>/<env>` | Optional `to_release_id`; default resolves previous successful serving history, then uses that deployment's revision and exact secret revisions. |
| `platform.restart` | `env/<app>/<env>` | Reapplies the serving release with the current revision through the same blue/green path, so a scaled formation takes effect. |
| `platform.domain.add/remove` | `env/<app>/<env>` | `domain`, optional `primary`, `addresses` (the selected main node's public A/AAAA targets) and `check_path` (an absolute HTTP path without control characters). Removal also requires `confirm` equal to the current hostname; removing the primary is refused. |
| `platform.environment.delete` | `env/<app>/<env>` | `confirm` equal to `<app>/<environment>`; the worker removes labeled containers and routes before platform rows. |
| `platform.app.delete` | `app/<app>` | `confirm` equal to `<app>`; every environment is deleted and the app record last. |

Names: lowercase letters/digits/hyphens, start with a letter, max 40 characters.
Unknown input fields fail, including nested revision fields. Refer to
`internal/platform/actions.go` rather than copying JSON types into a second
client/service implementation. `platform.rollback` is the destructive action:
`eregion rollback` sends `--confirm <app>/<env>` and `eregion node rpc` validates
that exact target before submission; a deployment ref, an operation ID or `--yes`
alone never satisfies it.

Environment revisions are numbered per environment and immutable. The revision
is the desired state; an accepted action is not an applied route or a running
process. Deployability is the pure `platform.EvaluateDeployability` report over
the current revision plus artifact/domain observations, with DNS reported as
`unknown` until the domain row observes it. A deploy checks that report before
recording a build or creating a container: source, runtime, a web process, a
primary domain and an observed wrong DNS answer block with
`precondition_failed` and a `missing` list. A merely unobserved DNS record does
not block, matching deploycrate's gate on an unapplied managed binding; a source
deploy produces its own artifact, so artifact readiness is not a blocker there.
An explicit release already carries its artifact, so that deploy does not need
the revision's source binding.
Inventory collections `revisions` and `deployability` expose the same report,
and each environment record carries its current revision, processes and
deployability.

The CLI `app create` and `env create` preview the canonical action on the node,
write the read credential and secret values through `vault.set`, then submit.
Only names enter the revision; the optional domain, its primary flag and
main-node addresses travel in the create input and are inserted in the same
transaction before the first deploy is queued, so the deploy cannot race its
own route. The preview checks the target node placement and domain availability,
so a refused create does not leave vault values behind. `env update` reads the
current revision and sends a merged patch; its terminal interview shows the diff
before confirmation. `secret set` previews the attach action, writes the value,
then submits `platform.environment.secret.attach` only when the current revision
does not already name it.

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
transient node secret map, never in the build payload. A git revision's
`context_path` selects the Buildpacks directory inside the checkout: the step
evaluates symlinks, requires the result to stay inside the repository and to be
a directory, and never falls back to the repository root. A directory/git build on
a distinct build node is refused before `pack`: registry push/pull and dedicated
credentials do not exist. Installing a registry alone does not move images.

`eregion image push <local-image> --app <app> [--tag <tag>]` moves a local image
into that registry without port forwarding: the request line is followed on the
same stdin by a `docker save` tar, the node's `image.push` method pipes it to
`docker load` with an 8 GiB cap, tags and pushes
`<RegistryListen>/<app>:<tag>`, and returns the pushed
`<RegistryListen>/<app>@sha256:…` reference with sparse progress events. An image
revision deploy pulls that recorded reference on the node before inspecting its
immutable image ID.

Deploy sequence: deployability gate -> preflight -> build from the revision when
the release is pending -> release command -> candidate containers for every
process replica on dynamic loopback ports -> health for every web replica ->
Caddy upstream switch with all web replica backends -> public HTTPS verification
through local Caddy -> remove the previous deployment's containers. A container
removal that fails is recorded in `platform_container_cleanup` and retried;
the verified switch stays successful and the failure is never silently dropped.
The gate
refuses before any build or container work. The release command is release
scoped: it runs once per release and command digest, before candidate
containers, and a succeeded result is recorded. A repeat deploy of the same
release and command skips it; a changed command digest runs again; a failed
command fails the deploy and leaves the previous release serving. The command
runs in a one-off `--rm` container of the release image on the environment
network, through the Buildpacks launcher for a Buildpacks image exactly like a
process command (otherwise the image's default web process would start). It
carries the app, environment, release and deployment ownership labels with
process kind `release`, so status, logs and app/environment deletion find it. A
nonzero exit, a container that does not start and a timeout are terminal for
that deployment; on timeout the node removes the container, because killing the
docker client leaves it running. An unknown
node outcome leaves the run row running; a later deploy never overwrites that
ownership blindly. It resolves the original operation's recorded command step
and node status: a completed step record becomes succeeded and is skipped; any
other step state is resolved by node status because cancellation and retry
exhaustion mark unfinished steps failed without proving what ran. A
node-reported terminal failure with a nonempty matching inputs hash becomes
failed and permits a new run. Missing or mismatched hashes (including failed
status-query responses), running/absent/stale/unreachable effects and insufficient
identity keep the original owner and defer. Recovery is read-only
and never dispatches the command. `restart`,
`process scale` and `rollback` never run it: they skip a succeeded command and
refuse with `precondition_failed` ("deploy the release first") otherwise. A
failure before the switch removes the candidates and leaves the previous
release serving; a failure after the switch restores the previous upstreams.
Rollback and restart use the same path with the recorded revision/secret
snapshot and the current revision respectively.
Explicit retry creates new work; it does not rewrite a terminal failed record.

A source deploy resolves the revision's git ref (or `--ref`) to an exact commit
before its submission transaction opens, reading a private repository with the
revision's credential by name from the vault; an unreachable repository creates
no operation. The submission-time read refuses the same internal host addresses
as the build, so loopback/link-local answers are refused, and public forges also refuse
private answers; explicitly allowlisted private forges retain their private-address
policy. CI and push to deploy already carry the pushed commit. The commit
is stored on the build and release rows when they are inserted, the pack step
checks out exactly that commit and refuses if `HEAD` differs, and a concurrent
source-kind or URL change makes the submission fail with `conflict`. A ref change
also conflicts unless an explicit `--ref` owns the effective ref. Matching
idempotency replays return before git resolution; concurrent first submissions
still arbitrate transactionally. A failed git build is
retried only at its recorded commit; without one the retry refuses with "no
exact commit" and never falls back to a branch name.

Node step events are persisted while the step runs in bounded, attempt-fenced
batches, so `build logs --follow` and the deploy viewer show build output live;
the worker's own secrets (control DSN, vault keyring material and the values
handed to a step) are redacted before an event is stored. Previous-deployment
cleanup resolves the node recorded on that deployment's pinned revision, not only
the environment's current node.

`app status` and `env status` join durable inventory with a read-only node RPC
Docker observation (`platform.workloads`). Pending cleanup rows are shown as
`N leaked containers pending cleanup` in plain output and by name in JSON. Each desired process replica is shown
with its container state and dynamic host port; a container from another
deployment is shown but not marked serving. Docker read failure leaves the
durable status visible and marks replica state `unknown`. Domains come from the
`domains` inventory collection with the recorded route, DNS, certificate and
public-response state plus the ready flag; a failed domain read is reported
separately instead of inventing `unknown` per domain. `logs` uses
`platform.logs` to stream the serving deployment's containers as RPC events,
filtered by `--process`; it does not read telemetry. The workspace metrics graph
still uses the telemetry data source.

Domains are changed in the submission transaction, then the worker reads all
bindings, writes one Caddy site per domain with the serving deployment's web
replica upstreams and reloads Caddy. `domain add --check-path` sets a binding's
HTTP observation path; empty uses the current desired revision's web health
path for background observation. Deployment public verification instead defaults
to that deployment's pinned revision path, including rollback. The observer
walks every domain in keyset pages at its own check path, and a deploy's public
check uses the primary binding's path and records the host, path and status in
operation events. Aliases are observed, not deploy-gated. Route writers take one PostgreSQL advisory
lock, adapt the new fragment through Caddy's `/adapt` endpoint, replace the file
with a fsync+rename, then load it; a rejected load restores the prior **file**,
not the domain rows. A deployment switch writes the same file with candidate
upstreams and verifies the primary domain through local Caddy before removing
the previous containers. A periodic maintenance job re-renders on worker start
and every 30 seconds after a failure; it updates `route_state` per domain. A
second periodic job observes DNS through 1.1.1.1 and 8.8.8.8 over UDP, then the
certificate chain/expiry and public response through local Caddy with the real
SNI. Wrong DNS stays unready. A Cloudflare-proxied record (deploycrate's
Cloudflare mode) counts as reaching the node: the certificate and public checks
still run against local Caddy, so readiness means the origin serves the host. `domain add` records the main node's
public addresses so observation has an expected value. Inventory `domains` and
`domain list` expose each state separately. A third periodic maintenance job
retries due `platform_container_cleanup` rows with one-minute-to-one-hour
backoff on worker start and every five minutes, deleting a row on success. A
fourth job compares every environment's recorded serving instances with the
node's labelled `docker ps --all` observation on worker start and every five
minutes. A missing or exited serving container queues one `platform.restart`
with the system actor and key `repair:<deployment>:<missing-names hash>`. The
submission rechecks, under the environment row lock, that the observed serving
deployment is still current and that no deploy, rollback, restart or delete is
active; a stale queued repair refuses without touching workloads. The queued
restart does not rerun the release command. See
[network](network.md) before changing this path.

## API and extension seams

- `/healthz`: process liveness only; `/readyz`: public, unauthenticated, answers
  only ready or not (database and schema); not worker availability or public app
  health.
- `/v1/operations`: submit/preview; `/{id}`, `/{id}/events`, `/{id}/cancel`:
  actor-scoped state, bounded cursor pages and explicit cancellation. CI tokens
  and the webhook never gain another actor's operations through this route.
- `/v1/platform?collection=…`: bounded inventory with app/environment/node/state/
  build filters. `build=<build ID>` returns that exact build for `build logs`.
  Base collections: summary, nodes, apps, environments, revisions,
  deployability, domains, releases, builds, deployments and changes; the
  internal DNS/access extensions add their own. The removed resources/backups and
  telemetry collections are not registered.
- `/v1/secrets…`: separate authenticated credential API; values are encrypted
  directly, never placed in ordinary action input.
- `/v1/environments/<app>/<env>/deployments`: the public CI deploy endpoint.
  `POST {"ref":"…"}` authenticates a deploy-scoped Bearer token (the grant is
  exactly `platform.deploy` on that environment), submits `platform.deploy`
  with `ref`, and returns the operation and its status URL. `GET` the returned
  operation only; expired, revoked, wrong-environment and wrong-action tokens
  fail. No other `/v1` route is public.
- `/v1/webhooks/github/<app>/<env>`: per-environment GitHub push delivery. The
  current revision must have auto-deploy on and a `webhook_secret_name`; the
  HMAC secret is read from that environment's vault namespace per delivery.
  The signature is checked over the raw body before parsing, the repository and
  bound branch must match, `X-GitHub-Delivery` is deduplicated in
  `webhook_deliveries`, and an accepted push submits `platform.deploy` with the
  pushed commit SHA. A redelivery returns the original operation. The old
  server-wide secret file, allowlist and release-only webhook are removed.
- `/private/…`: optional authenticated subject/device-grant proxy, not a VPN
  enrollment implementation.

Caddy is generated by the bootstrap ingress step and exposes only `/readyz`,
`/v1/environments/*/deployments*` and `/v1/webhooks/github/*/*` on the main
hostname. Do not expose all of `/v1` as a shortcut. Main-node application
hostnames are separate Caddy sites.

The operator `Backend` (`internal/cli`) and HTTP handlers share the
`api.AppService` functions for action submission/preview, inventory and vault
operations. New guided commands must use node RPC and share those services, not
enqueue directly or build a second HTTP path. Extensions register
strict normalizers/executors and inventory providers before runtime startup through
`controlplane.RegisterExtensions`; no extension gets a second queue. Registry
availability alone is not readiness: missing host/key/provider configuration
can leave advertised actions deliberately failing closed.
