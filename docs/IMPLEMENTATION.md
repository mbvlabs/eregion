# Implementation reference

Baseline checked against the integrated source on 2026-11-09; alpha.7's changed
mechanisms were updated on 2026-11-10. The managed-resource comparison with
deploycrate-ce was checked on 2026-11-07. This describes
**current code**, not the target command tree or a real-host certification.
Product decisions belong in [MASTER](MASTER.md); actionable work and execution
status belong in the [Linear project](https://linear.app/mbvlabs/project/eregion-2ff1d2e4d199).
Run evidence belongs in [MANUAL-TESTING](MANUAL-TESTING.md).

## Execution and control boundaries

```text
operator CLI
  -> saved workspace access: embedded userspace WireGuard -> pinned SSH -> sudo -n eregion node rpc
  -> NDJSON request/events/result -> shared application services (api.AppService)
  -> external PostgreSQL: domain intent + operation + River job, one transaction
  -> River worker -> local or pinned-SSH node transport -> Check/Apply effects
  -> durable events/state -> CLI plain/JSON/NDJSON/terminal presentations
```

Every operator command uses node-mode dispatch: the CLI writes one
`eregion-rpc/v1` request to `eregion node rpc` on stdin over SSH inside
WireGuard and reads NDJSON back. Saved workspace access records carry the WireGuard identity and SSH
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

River worker client IDs use a fixed host identity:
`eregion-<first 16 hex of SHA-256(hostname)>-` plus a unique suffix per start,
and River writes the client ID into each job's `attempted_by`. Before it starts,
a worker recovers `running` jobs on the `platform` queue whose last attempt
belongs to this host but not to the new client. A conditional update rechecks
the observed attempt and last `attempted_by` entry, so a job that changed owner
after the scan is left alone. Jobs with attempts left move back to `available`;
an exhausted job (`attempt >= max_attempts`) is discarded with `finalized_at`
set and an error appended, and its operation is failed. The job change and its
operation events commit in one transaction; a recovery failure fails worker
startup instead of silently dropping history. Jobs last attempted by another
host are left to River. This recovers accepted work from a SIGKILLed
predecessor immediately instead of waiting for the platform worker's 30-minute
job timeout, which is longer than River's two-minute rescue threshold.
Diagnostic, maintenance and domain jobs do not override the client's one-minute
job timeout, so River's rescuer already covers them. The re-queued attempt is
safe because operation writes are fenced to the River attempt, `claimPlatform`
transfers ownership to the newer attempt, and node effects are reconciled from
durable step records before dispatch. This assumes one worker process per host;
running two workers on one host against one database would let the newer client
re-queue the older one's live platform jobs.

## Code map

| Location | Responsibility |
| --- | --- |
| `cmd/eregion` | One executable entry point: the operator surface, hidden `node` mode (`rpc`, `serve`, `worker`, `migrate`, `bootstrap`) and hidden `node-agent`. |
| `internal/cli/tree.go`, `meta.go`, `run.go`, `deploy.go`, `promote.go` | Command catalog, safety metadata, output selection, dispatch and the deploy/rollback/promote/build/release pipeline commands. |
| `internal/cli/serverinit*.go`, `sshauth.go`, `handoff.go`, `login.go` | Guided setup (preflight, answers/pending, plan/confirm, apply, guarded handoff), first contact, DNS, private access and token-free age handoff/import. |
| `internal/cli/workspace_access.go`, `internal/cli/noderpc.go`, `internal/cli/hostnode.go`, `internal/cli/observe_rpc.go`, `internal/host/workspacestore.go` | Workspace resolution, node RPC client/server, version lockstep, host and local RPC, Docker workload/log reads, SSH/WireGuard route. |
| `internal/cli/observe.go`, `internal/cli/logs.go`, `internal/cli/delete.go`, `internal/cli/operations.go` | Extended app/environment status, container logs over the operator channel, target-confirmed deletion and operation-history filters. |
| `internal/cli/host.go`, `internal/cli/serverstatus.go`, `internal/cli/nodeunlock.go` | `plan`, `apply`, `doctor`, `ssh`, `server status` and hidden `node unlock-ssh`. |
| `internal/protect`, `internal/totp`, `internal/tokens` | Root-only TOTP gate on the node and hashed per-use deploy tokens in `/etc/eregion/auth.json`. Every protect-state mutation (`Init`/rotate, `Confirm`, `Verify`) takes an exclusive `flock` on the stable `<protect.json>.lock` file before its load-verify-save sequence, so independent node RPC processes cannot replay a code step, lose failure counts or confirm/rotate twice. `Status` stays lock-free because `save` publishes a complete file by rename; the lock file is never renamed and is released by the kernel on process death. |
| `internal/cli/protect.go`, `token.go`, `webhook.go`, `github_webhook.go`, `secretexport.go` | TOTP setup/status, token mint/list/revoke, manual and optional `gh`-assisted push-to-deploy setup, and the protected handoff re-export. |
| `internal/appdef` | Strict versioned `eregion.toml` parsing and resolution: a shared recipe plus optional production/staging/named overrides (the selected name need not appear), complete-state update patches, deterministic export with domains under the selected environment override, and managed-resource declarations. Input only; never a state store. |
| `internal/cli/resource.go`, `internal/platform/resource*.go`, `internal/controlplane/resource.go` | Generic environment-owned Docker resources: durable identity and desired state, private per-environment networking, image identity, vault secret-name injection, a cumulative owned-volume ledger written before volume creation, exec health gating, attach/detach revisions, deployment-aware destroy refusals and container destroy that retains data by default. |
| `internal/rpc` | `eregion-rpc/v1` request/event/result wire types and bounded request decoding. |
| `internal/api` | Shared application services plus the HTTP authentication/grants, operation/inventory routes, vault, CI deploy, per-environment push webhook and private proxy adapters. |
| `internal/controlplane` | pgx transactions, River, platform state/workers, deployment reconciliation including staging-to-production release promotion, node resolution, extension wiring. |
| `internal/platform` | Validated action/input types, inventory model, placement rules, source/runtime steps, disk-retention inventory and removal/GC node effects, Caddy route renderer and the domain DNS/TLS/HTTP observation helpers. |
| `internal/retention` | D30 retention policy, environment override merge/validation, Buildpacks cache limits and run/item records shared by the planner, CLI and node effects. |
| `internal/engine`, `internal/node`, `internal/noded`, `internal/transport` | Check/Apply journal, effect identity, detached tasks, pinned SSH and userspace WireGuard. |
| `internal/host`, `internal/access`, `internal/doctor` | Bootstrap steps (incl. vault keyring and node_exporter), local pending-init state, recorded host intent, read-only observations, admin/firewall/WireGuard/SSH lockdown and break-glass scripts, and pure doctor checks. |
| `internal/guide` | Embedded agent operating guide printed by `guide agents` and installed as `SKILL.md` by `skill install`. |
| `internal/vault`, `internal/secrets` | Revisioned ciphertext/keyring and redaction. |
| `internal/integrations`, `internal/telemetry` | The domain-scoped Cloudflare planner/executor used by explicit `domain add/remove` (fresh vault-backed provider credentials, exact per-binding ownership, no zone-wide delete), retained internal DNS/private-access actions for later phases, and the node-local telemetry primitives: the bounded node_exporter parser/snapshot, the WireGuard-only HTTP discovery/config generation, and the basic-auth credential helpers used by Caddy. There is no central telemetry service, external data source, public scrape endpoint, metric store or history. |
| `internal/viewer`, `internal/tui`, `internal/workspaceui`, `internal/cli/ui` | Reducer/follow, operation view, full-screen workspace and guided inline components. |
| `internal/release`, `internal/upgrade`, `scripts/*.sh`, `.github/workflows/publish-*.yml` | Cosign-verified channel manifests and artifacts; local CLI replacement plus the verified linux/<node arch> binary used by `apply --auto-upgrade-nodes`. |

## Current command surface

### Operator and agent quick reference

Start with offline discovery, without choosing a server or reading credentials:

```sh
eregion
eregion commands --json
eregion help env update --json
eregion guide agents
```

For a node operation, identify the selected **workspace** first. The default
private store is the OS configuration directory's `eregion/contexts.json` (the
historical file name is private storage, not a public identity); the
`--workspace-file` flag or `EREGION_WORKSPACE_FILE` selects another existing
store. `server init` requires an explicit store directory exclusive to that
store file because the historical `servers/` and `pending/` roots live beside
it; a second JSON store in the same directory is refused rather than sharing
credentials or pending state. `workspace list`
describes saved workspaces, one entry per authoritative ID. `--workspace NAME`
selects the verified portable workspace name; otherwise `EREGION_WORKSPACE`,
then the saved current workspace applies. Historical local access aliases are
never selectors. A workspace identifies the installation and its node access;
a node name identifies that server. Neither is an application environment.

```sh
eregion workspace list --json
eregion --workspace thelab server status --json
eregion --workspace thelab domain cloudflare status --json
```

Use explicit workspace selection in automation rather than relying on a human's
last `workspace use`. Do not copy secret-containing workspace stores into
evidence or use the handoff as an ordinary configuration file. `login` imports
protected access material and verifies portable workspace identity and node
trust before publishing it; exact `--replace WORKSPACE` does not waive
fingerprint verification.

| Authority / credential | Role and supported flow |
| --- | --- |
| Operator SSH + WireGuard | Privileged node-mode control via saved workspace access. No operator bearer token is required. The OS account holding workspace access keys is effectively privileged on that server. |
| Git read credential / `--github-auth` | Clone a private repository. Local authenticated `gh` can supply it explicitly; this is not the webhook HMAC or a CI deploy token. |
| GitHub webhook HMAC | Authenticate signed pushes for one environment. Generated and stored through the webhook setup/rotation flow; repository hook administration is a distinct gh permission. |
| CI deploy token | Scoped, expiring deploy authority for the narrow HTTP deploy endpoint. Minting requires the human factor; possession does not grant general operator/DNS authority. |
| Cloudflare provider credential | Hidden human terminal input, read-validated and encrypted in the typed provider/zone vault scope. Agents can inspect non-secret status and use explicitly authorized DNS actions, not export the token. |
| TOTP human factor | Protects token minting, secret/handoff export and protected destructive actions. `--yes`, JSON and stdin do not satisfy it. |
| Telemetry credential | WireGuard-only authenticated scrape access, distinct from operator control. Ordinary config output uses a placeholder; explicit protected files contain the real credential. |

`--github-auth` invokes the authenticated `gh` on the operator's machine during
setup. The acquired repository-read credential is encrypted in the environment
vault; a later private clone and build run on the build node, not on the
operator's machine. GitHub delivers webhooks directly to the server, which
admits and executes the deployment through PostgreSQL and River. The workstation
does not need to remain online after setup. Local hook administration, the
stored clone credential and the webhook signing secret remain separate roles;
rotating one does not automatically rotate the others.

Cloudflare setup needs the **zone ID**, not the account ID: select the DNS zone
in Cloudflare and use its zone identity with the matching zone name. A rejected
read validation activates no credential and is not repaired by repeated apply.
An accepted credential proves read validation, not write permission, DNS
reconciliation, valid TLS or public reachability. Missing workspace access must be fixed
before entering the token.

For setup followed by DNS changes, create a Cloudflare API token with
**Zone → DNS → Edit**, scoped to **Include → Specific zone → the configured
zone**. Edit includes read/list access, so a separate DNS Read grant is not
needed. A **Zone → DNS → Read** token can pass setup but cannot authorize
record changes. Cloudflare's API reference calls the mutation permission
**DNS Write**; the dashboard calls it **DNS Edit**. Current validation lists
records using the supplied zone ID; it neither discovers zones nor fetches zone
details, so **Zone → Zone → Read** is not required and the supplied zone name
is not independently matched to that ID. See Cloudflare's
[token permissions](https://developers.cloudflare.com/fundamentals/api/get-started/create-token/)
and [DNS listing API](https://developers.cloudflare.com/api/resources/dns/subresources/records/methods/list/).

Human flows may prompt only at a real terminal; agent flows need explicit
flags and JSON/NDJSON and must not prompt. Human-only factor/secret-display
operations are deliberate exceptions, not missing agent inputs to bypass.
A submission result means **accepted**. Follow its operation to terminal state,
then inspect serving, DNS/route/TLS and the external response separately.

Keep the protected handoff current after changes to workspace identity,
recoverable host/access material, vault keyring or telemetry credentials. A
successful rotation does not update old exports. A human runs `secrets export`
with the factor and stores the refreshed protected artifact; an agent records
only confirmation/metadata, never its contents. Local GitHub authentication,
provider credentials and application secrets retain their separate scopes and
must not be assumed interchangeable or all recoverable from private workspace access.

Use the **built binary**, not a stale `bin/eregion`, to inspect exact arguments:

```sh
just build
bin/eregion commands --json
bin/eregion help app create --json
```

| Current commands | Meaning/limit |
| --- | --- |
| `server init` | Guided root first contact; Linux/macOS operator, arch-matching linux node binary, preflight, answer flags/TOML, plan/confirm, vault keyring, loopback node_exporter and telemetry endpoint prerequisites, pending-init resume/discard, the final doctor gate and guarded handoff. First contact requires SSH on port 22; another port is refused before any remote change. The plan lists the host steps and the setup.sh access changes (admin user, wg0, firewall, SSH lockdown switchover). A pending init stores the pinned SSH host key and fingerprint; `--dns-ready` skips the confirmation prompt interactively, non-interactively and on resume. `--skip-doctor` skips the gate with a warning; a failing check returns `doctor_failed` with the failed checks and resume hint. Declining the plan (or any exit that does not apply) removes the uploaded planning bundle from the server. |
| `plan`, `apply [--dry-run]`, `doctor`, `ssh [node] [--command CMD]`, `server status` | Node-mode host convergence and inspection. `apply` records the host intent, streams step progress on stderr and skips completed steps; `doctor` exits non-zero on a failing finding. `ssh` opens a shell at a terminal or runs one command with `--command`; piped or JSON output runs without a PTY, a sudo password prompt becomes an `input_required` fault whose code and fix the runner still prints, and the remote exit status propagates. `--auto-upgrade-nodes` installs the operator's build on a mismatched node: a `dev` node accepts the caller's checksum, while a published node requires its own signed release manifest to match; `--dry-run` never installs. |
| `node unlock-ssh [--minutes N]` | Hidden node-only break-glass command, run as root from the provider console; reopens public SSH for a bounded window and schedules the re-lock. |
| `login --file … --passphrase-file … [--replace WORKSPACE]` | Import an age transfer bundle into private workspace access (SSH and WireGuard keys, route). It prompts for the bundle only at a human terminal; `--json` or a non-terminal stderr never prompts and needs `--passphrase-file`. The bundle's Workspace/Workspace ID are the target identity; historical Context annotations are ignored. Login verifies the portable name/ID and node pins using a private staging directory before publishing it as the active credential directory. An existing workspace requires `--replace` with its exact canonical name; same name/different ID and same ID/different name are refused even with `--replace`. Under the store mutation lock, login reloads and replaces all local records for that ID with one verified record while preserving unrelated workspaces and their credentials. Old files are not deleted by canonicalization. Failures leave the store, old credentials and pins untouched. Old bundles without identity remain a bounded alpha access import only on a store with no verified workspace. |
| `workspace list/show/use/remove` | Saved workspaces, one entry per authoritative workspace ID. `show [NAME_OR_ID]`, `use NAME_OR_ID` and `remove NAME_OR_ID --confirm NAME_OR_ID` use the verified workspace name or generated ID; a private access alias is never a selector. Every node command accepts the root `--workspace NAME`; resolution is the flag, then `EREGION_WORKSPACE`, then the saved current workspace. Same-name/different-ID and same-ID/different-name groups are conflicts; exact IDs can address a name conflict; multiple access records for one ID are ambiguous unless one is current or only one exists. The node client, local identity reads (`secrets export`), DNS addresses, endpoint labels and telemetry credential reads use one resolved access-record snapshot so they cannot disagree. `login` and `server init` reject an explicit global `--workspace`; their creation/assignment inputs are separate. `workspace remove` deletes local access only, never the database identity, applications or server. |
| `app create/list/status/delete` | Guided or flag-only creation of **production and staging in one submission**: source, Buildpacks settings, processes, plain config, vault secret names and production's optional domain. Both environments and their first revisions commit in the same transaction; both are protected by default, with `--unprotected` as the explicit creation-time opt-out. Supplied `--secret` values and the Git read credential are written before submission into every created environment namespace whose revision names them; a shared recipe therefore writes both production and staging, while an environment-specific name or credential is never copied into another namespace. `--github-auth` is an explicit opt-in alternative to `--git-credential-file`: for a github.com source it reads the local authenticated `gh` token with `gh auth token --hostname github.com` in memory and writes it through the same vault path. Dry runs and idempotent replays never call gh, non-GitHub sources, conflicting credential flags and credential names already used as runtime or webhook secrets are refused before any gh read or vault write, and the token never enters argv, operation input, output or an intermediate file. `create` accepts `platform.app.create` with an `environments` array, the first domain, primary flag and main-node addresses; the first environment's binding commits before its optional first deploy is queued, and the accepted operation exposes that deploy's ID as `first_deploy_operation_id`. A retry with the operator's `--idempotency-key` previews as a replay, writes no vault revision, and returns the original operation including that ID; without the key an existing name is a conflict. Duplicate hostnames across the two environments are refused before or inside the transaction. In a terminal with deploy requested and no `--detach`, `create` follows the create operation and then the first deploy with the deploy viewer and outcome; Ctrl-C detaches with the follow command. `--detach`, JSON and no-TTY return at accepted. List/status read inventory. `status` shows the serving release, processes, persistent protection, per-domain state and recent operations; `delete` always takes `--confirm <app>` and, unless `--dry-run`, a TOTP code typed at a human terminal (D7, D13, D33). The control plane verifies the code and confirmation against locked state in the submission transaction; dry-run preview needs neither and stays available to an agent. |
| `env create/update/protect/unprotect/promote/list/status/delete` | `create` accepts an environment with its first revision, optional domain binding and its first deploy unless `--no-deploy`; the accepted operation exposes the queued deploy's `first_deploy_operation_id`, and a terminal follows create then the first deploy with the deploy viewer unless `--detach` is given. Protection defaults from the name (production and staging protected, every other name unprotected); `--protected`/`--unprotected` override it at creation. `--from FILE` applies the shared recipe to any name, including a PR name absent from the file. A retry with the operator's `--idempotency-key` replays the accepted operation including that ID without rewriting vault values; `update` merges source, placement, runtime and process changes into a new immutable revision. `--github-auth` replaces the read credential with the local authenticated `gh` token for a github.com source; it is refused for another host, with `--git-credential-file`, with `--from`, and when the selected name is already a runtime or webhook secret. The same role check applies to `--git-credential-file`, before that file is read, and shared revision normalization enforces it for `--from` and every API path using the effective desired revision, including an unchanged source binding and the preserved webhook name. On a dry run or replay it never calls gh. In a terminal with no change flags and no webhook flags, `update` walks the current revision prefilled, shows the diff and confirms; `--auto-deploy[=false]` and `--rotate-webhook-secret` configure push to deploy and show the URL and secret once at a terminal. With flags or JSON/no-TTY it is flag-only. `protect` records durable protected state and is agent-safe; `unprotect` lowers protection and needs a TOTP code at a human terminal, refusing JSON/no-TTY. `promote <app> <source-environment>` is the dedicated D23 path: the target is the app's fixed `production` environment; it resolves and pins the source's current serving image digest, commit and source **inside the submission transaction** as a new production release without a build, then applies production's current revision, config, secret values and release command through the same blue/green path. It refuses a source without a ready serving release, **any** active production deployment (including a different artifact), an artifact already serving production, a different node, a non-production target, cross-app input and a target that fails the explicit-release deployability gate. It never copies domains, config, secrets or the source release command. Dry-run and interactive previews pass the supplied idempotency key, so an accepted same-key submission previews as a replay. List/status include revision, processes, protection and deployability. `status` shows serving release, per-process replicas, domains and recent operations; `delete` takes `--confirm <app>/<env>`; a protected or last environment also needs a TOTP code at a human terminal, while an unprotected non-last environment deletion is available to agents with the exact confirmation alone (D33). |
| `app create|env create|env update --from FILE`, `app export` | Opt-in definition input turns strict TOML into ordinary desired state; existing flag/guided flows never read a file. The document is a shared recipe plus optional `[environments.production]`, `[environments.staging]` and `[environments.<name>]` overrides; `app create --from` resolves production and staging, while `env create --from --name pr-42` applies the shared recipe without requiring an override entry; the environment name is a command argument, not a document identity. File updates are complete supported state, carry a revision fence, refuse domain drift, require target confirmation when they remove processes/config/secret names/resources, and never deploy. Declared Docker resources are explicitly applied after the environment exists. Export is deterministic, places domains under the selected environment's override, and refuses unrepresentable auto-deploy, webhook and retention state rather than dropping it. |
| `resource apply|attach|detach|list|status|destroy` | Node-mode generic Docker resource lifecycle. Apply resolves an immutable image, ensures the environment's private bridge network, records the cumulative owned volume identities before creating any volume, injects only vault secret names/values transiently, starts one container with no published host port and probes its exec health. An interrupted apply or a later declaration that drops a mount never loses a recorded volume identity; the row keeps every identity this resource owns. Attach/detach publishes a revision; deployment refuses while an attached resource is absent or unhealthy. Health is observed at apply and deployability checks the recorded state; there is no periodic resource-health repair job, so a container that degrades after apply needs an explicit re-apply. A failed image update leaves the new container degraded instead of restoring the previous image; named data volumes are unaffected. Destroy executes under the environment target lock, refuses while the current or serving revision still attaches the resource or a queued/running deployment may use it in a switch, removes the container and retains named volumes and identity by default; `--delete-data` separately deletes only volumes carrying this resource's ownership labels, including previously mounted identities and volumes whose create preceded an interrupted durable write, and is refused otherwise. Repeating `destroy --delete-data` after a default destroy is supported. App or environment deletion does not delete resource volumes. Cancelling or failing a resource operation after it entered `provisioning`/`destroying` settles the row to `degraded` in the same terminal transaction, fenced by the row's `operation_id`; image, container, network and the cumulative volume ledger are retained, so a later apply or destroy is a normal retry. Cancellation is checked at every resource apply/destroy step boundary, so a cancel accepted during an in-flight step cannot be converted into success; finalization then locks the operation row and rechecks `cancelling` before the resource update, closing the last-check-to-commit race. The maintenance reconciler also sweeps rows left in those transient states by operations that were already terminal before this fix, so an old stuck fixture converges without a repair command or direct database edit. |
| `process list/scale` | Read the desired formation; scale one process's replicas in a new revision. A serving environment also queues a blue/green restart in the same transaction so the new formation runs immediately; without a serving deployment the next deploy applies it. |
| `config set/unset/list` | Plain variables as a new revision or read from the current revision. No deploy is queued. |
| `deploy <app> <env> [--ref REF \| --release ID]` | Node-mode `platform.deploy`. In a terminal it prints source, ref/commit, revision and processes, then follows the operation with the shared viewer (build log lines and stages). `--detach` returns after acceptance. `--release` requires a release from that same app and environment; cross-environment artifact movement is only `env promote` (D23), never a disguised deploy. |
| `rollback <app> <env> [--to ID]` | Node-mode `platform.rollback`; requires `--confirm <app>/<env>`, validated by the node and control plane against the current target (D12). There is no plan-token scheme for rollback. |
| `build list/logs/stop/retry`, `release list`, `restart <app> <env>` | Build lifecycle over `platform_builds`; `build logs` reads (and with `--follow` streams) the build operation's events. `build get` is served by the `builds` inventory filter. `restart` reapplies the serving release through the blue/green path. |
| `domain add/remove <app> <environment> <hostname> [--primary] [--check-path PATH] [--dns cloudflare --dns-zone ZONE (add also needs --dns-origin ADDRESS --dns-proxy dns-only\|proxied)]`, `domain list [--app --environment]` | Node-mode domain bindings, one primary per environment, serialized Caddy writes and separate route/DNS/certificate/public observations. Manual DNS is default. Explicit Cloudflare DNS resolves the configured zone and encrypted credential freshly from PostgreSQL; token values never enter argv, action input, River arguments, events or ordinary output. Add requires explicit validated public origins and proxy choice; the WireGuard endpoint is never an origin. Dry-run returns a real read-only provider snapshot. Immutable per-binding ownership, newest-intent fences and exact-owned cleanup preserve foreign records and retain the ledger on failure. Remove needs exact hostname confirmation and refuses the primary; app/environment deletion supports primary cleanup. Provider success is not public readiness, and multi-binding/provider/node effects are not one atomic transaction or an availability guarantee. The detailed DNS section below describes cleanup and recovery boundaries. |
| `domain cloudflare configure [--zone ID --zone-name NAME]`, `domain cloudflare status [--zone ID_OR_NAME]` | Human-only hidden-token setup/rotation plus agent-safe metadata status. Configure read-validates the exact zone, previews current/next revision, confirms, then atomically commits CAS-fenced encrypted vault state and sanitized audit attribution. JSON/NDJSON/no-TTY configuration refuses before token read/RPC; no token flag, file, environment or stdin fallback. Current credentials resolve without restart. Status never decrypts or prints a token. Setup proves DNS-list access only, not write capability. |
| `secret set/list/archive/export`, `secrets export` | Metadata/write-only storage and explicit file export; `set` writes the value and then attaches the name to the environment as a new revision. No plaintext `get`. Both exports need a TTY and a TOTP code; `--yes`, `--json`, and a pipe are refused, and their command metadata declares that they prompt. `secret export` carries the code in the same node request that returns the value (`vault.export`); there is no separate pre-check request to skip. `secrets export` rebuilds the server-init handoff (including the vault keyring and the local telemetry credential when present) and honours `--secrets-out`; a missing telemetry credential is reported as a warning, never invented. |
| `token create/list/revoke` | Per-use deploy tokens for one app/environment, expiring, hashed in `/etc/eregion/auth.json`, shown once. `create` needs a TTY and TOTP code, never `--json`/`--yes`. |
| `protect init/status` | Node-root TOTP setup (`/etc/eregion/protect.json`, shown once as QR/otpauth) and non-secret status. |
| `logs <app> <environment> [--process NAME] [--follow] [--since] [--tail]` | Reads the serving deployment's Docker containers through node RPC; lines are prefixed with process/replica. No telemetry service is needed. Ctrl-C detaches. `--json` without `--follow` returns one envelope; NDJSON requires `--output ndjson`, and `--json --follow` is a usage error that names that fix. |
| `metrics show` | Read-only node RPC (`metrics.show`) executed on the node: two bounded reads of the loopback node_exporter with a one-second gap, then one current snapshot. Returns node/exporter identity, fetched and observed timestamps, freshness and explicit issues; CPU utilization is computed from a checked counter delta (nil and an issue after a reset or missing counter), and memory, real filesystems, load and uptime are parsed from the needed metric families. There is no eregion-run telemetry server, Prometheus/Loki deployment, external backend, global PromQL, metric store or history. Plain and JSON are the same snapshot; JSON/no-TTY never prompts. It needs no control database and no credential because the exporter is loopback-only. |
| `metrics endpoint` | Read-only node RPC (`metrics.endpoint`): the WireGuard-only authenticated endpoint's listen address, `/node` and `/targets.json` URLs, basic-auth username, whether the telemetry Caddyfile is configured, and whether this machine holds a local credential. It never prints a password or hash and is independent of PostgreSQL. |
| `metrics config` | Generates Prometheus and Grafana Alloy scrape configs that follow the endpoint's HTTP service discovery and use basic auth. Ordinary output (including JSON) always uses a password placeholder. `--format prometheus|alloy|targets --out FILE` writes one new owner-only file with the real credential when one is held locally; the file is never echoed. Targets output carries no secret and is also valid `file_sd`. |
| `metrics rotate --confirm <node>/metrics` | Generates a 256-bit password, saves it owner-only in the resolved workspace credential directory before the RPC, and sends only its bcrypt hash. Under a node-local telemetry lock the node renders and validates the dedicated WireGuard-only Caddyfile, restarts only `eregion-telemetry.service`, and restores the previous Caddyfile on failure. Public Caddy and its route writer are never touched. A failed RPC restores the previous local credential. There is no generation compare-and-swap, pending file or recovery journal; a lost successful response can leave the local password stale until the next rotation. No password or hash appears in plain/JSON output, logs, events or journals. |
| `ui` | Full-screen workspace over the same inventory and node-mode action service. Fleet details show one current node_exporter CPU sample plus current memory, filesystem, load and uptime facts through `metrics.show`; no history or request graph is invented. |
| `operations list/get/follow/cancel` | Over node RPC the SSH-authenticated operator has server authority: an unfiltered list shows every operation on the node newest first (bounded by `--limit`), and snapshot/follow/cancel may open any operation, including CI tokens, the GitHub webhook and other operators' submissions. `--app`/`--environment` select the platform target and include the environment's `build/<id>` operations. State and limit filters apply. The public HTTP API remains strictly actor- and grant-scoped. |
| `node serve`, `node worker`, `node migrate`, `node bootstrap …` | Hidden node-mode entry points on the Linux server. They are not part of the operator listing; systemd runs the services, bootstrap/apply converges the host, and `node migrate` applies explicit schema changes. `node rpc` and `node unlock-ssh` remain hidden node paths. The telemetry endpoint is a dedicated Caddy unit, not an eregion node service. |
| `upgrade` (`--version`, `--channel`, `--dry-run`, `--yes`), `commands`, `help`, `guide agents`, `skill install`, `version` | `upgrade` replaces only the local CLI from a cosign-verified channel; it never touches a node. `guide agents` prints the embedded agent operating guide; `skill install` prompts for harnesses in a terminal or takes `--harness`, then writes the same text as `SKILL.md`. `commands` is discovery. |

All of MASTER section 5's app, environment, deployment and observation surface
exists. The `platform` command group is deleted: `app`, `env` (including
`env promote`), `process`, `config`, `domain`, `deploy`, `rollback`, `restart`,
`build`, `release`, `logs` and `operations` are the operator commands. Token/
protect commands serve CI and human-factor flows (MASTER D7/D28). The old plural `resources …`
surface and the backup code are deleted (MASTER D19); alpha.2's singular
`resource apply|attach|detach|list|status|destroy` lifecycle is registered.
The Docker Compose telemetry stack install is deleted (D5); the local textfile
directory and narrow systemd collector are node_exporter unit arguments, not a
telemetry stack. `dns …`,
`access …`, `platform …` and `operations test` are no longer registered
(D6, D19, D22, D26); `metrics show`, `metrics endpoint`, `metrics config` and
`metrics rotate` are registered as the node-local read/config primitives
described above. The DNS/access internal packages remain for the after-v1
phases only after a product scope decision. `bootstrap …`, `serve`, `worker`
and `system migrate` are not operator commands; they live under the hidden
Linux-only `node` group (`node rpc`, `node unlock-ssh`, `node serve`,
`node worker`, `node migrate`, `node bootstrap …`) and `commands --json` lists
only the section 5 tree plus `help`.

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

Every command that can follow an accepted operation (`deploy`, `rollback`,
`restart`, `build stop/retry`, `build logs --follow`, `app create`,
`env create`, `domain add/remove`, `process scale`, `operations follow`)
accepts `--output ndjson` and streams the same event records as
`operations follow --output ndjson`, followed by one terminal result record.
`--json` on a submitting command keeps returning the accepted operation without
waiting. NDJSON never prompts: a guided command with missing input, or a
factor-gated delete, refuses instead of asking.

## Node-local telemetry

`metrics show` is a node-mode read over the normal operator channel: the CLI
calls `metrics.show` in `eregion node rpc`, which runs on the node as root and
does not open PostgreSQL. The node side reads
`http://127.0.0.1:9100/metrics` twice, one second apart, with an HTTP client
that ignores ambient proxies and does not follow redirects, a five-second
request timeout and a 4 MiB body bound. The parser keeps only the families it
needs and returns one `eregion/node-metrics/v1` snapshot; the samples are not
written anywhere. node_exporter has no WireGuard or public listener.

The snapshot carries the logical node name from `node.json` when readable, the
exporter's uname identity and build version, `fetched_at` recorded after the
second response completes, the second scrape's `node_time_seconds` as
`observed_at`, freshness (`fresh`, `stale` or `unknown` against a 30-second
threshold) and a `scrape` object naming the loopback HTTP endpoint,
`access: loopback`, `public: false`, no authentication and the operator-RPC
collection mode. A missing second timestamp stays unknown; it never borrows the
first scrape. A materially future exporter timestamp is also unknown and its
future `observed_at` is not exposed as current. External scrapers use the
separate authenticated endpoint below; `metrics show` never claims that
endpoint is being scraped.

CPU utilization is `(1 - idle_delta/total_delta) * 100` over the reported CPU
mode counters. The pinned node_exporter v1.11.1 emits the eight modes `user`,
`nice`, `system`, `idle`, `iowait`, `irq`, `softirq` and `steal` on
`node_cpu_seconds_total`; guest time is a separate
`node_cpu_guest_seconds_total` family and is ignored. Legacy
`guest`/`guest_nice` series are tolerated but excluded from the denominator
because Linux counts that time inside user/nice. A missing counter family, a
changed counter set, a counter reset, a missing any of the expected eight Linux
modes for any observed CPU, a non-positive total delta, a missing idle delta or
inconsistent movement returns `cpu: null` plus an issue; it is never a
fabricated zero. Memory uses `MemTotal`/`MemAvailable`; a missing or
inconsistent pair is null plus an issue. Filesystems pair
`node_filesystem_size_bytes` with `node_filesystem_avail_bytes` and omit
pseudo/tmpfs/overlay/Docker mounts. Load requires all three `node_load*` values,
and uptime uses `node_time_seconds - node_boot_time_seconds`. Unavailable
values stay null and are named under `issues`; the human form prints the reason.
An unparseable, timestamped, duplicated or otherwise invalid sample for any
consumed family is a hard `unavailable` refusal; valid label whitespace and one
trailing comma are accepted, and unknown unrelated lines are ignored. Exporter
label values are stripped of control and format characters before they can
reach terminal output.

Plain and JSON render the same snapshot. The command has no prompt, dry-run,
confirmation or mutation path. The workspace Fleet detail reuses the same node
RPC and turns it into one current CPU sample plus current facts; Envs has no
graph because no per-request history exists. The workspace carries the
source's explicit freshness independently of CPU availability, never replaces
unknown exporter time with collection time, adds monotonic local elapsed time to
the node-computed age against the node threshold, re-queries metrics on a
focused-detail refresh, fences an older response behind the latest query
generation, and refuses a query for a different node instead of relabeling the
snapshot. `ServiceUnits` includes `node-exporter.service` and `caddy.service` for
the main role, so `server status` displays both; the dedicated telemetry unit
is checked by `telemetry.endpoint`, not the shared service list.

### Authenticated endpoint, discovery and configuration

Eregion adapts vaire's nginx metrics endpoint to a **dedicated Caddy instance**,
separate from the public ingress Caddy. `host-telemetry-v1` ensures
`/etc/eregion-telemetry/targets.json` (`root:caddy 0640`, no secret), the
`eregion-telemetry.service` unit and the WireGuard-only firewall rule. The
Caddyfile is `/etc/eregion-telemetry/Caddyfile`, written by `metrics rotate`,
not by `apply`. The unit uses isolated `XDG_CONFIG_HOME`/`XDG_DATA_HOME` and
`StateDirectory`, `admin off`, and its own Caddyfile; public `caddy.service`,
`/etc/caddy/Caddyfile` and `/etc/caddy/eregion.d/*.caddy` are never touched by
telemetry. A missing wg0 or a bad telemetry config cannot stop public ingress.

The unit runs the pinned Caddy binary as `caddy`, waits for
`network-online.target` and `wg-quick@wg0.service`, and requires the telemetry
Caddyfile before starting. It is `BindsTo=wg-quick@wg0.service` and
`WantedBy=multi-user.target wg-quick@wg0.service`, so a later wg start or
restart also triggers telemetry; it is not skipped permanently when wg0 is
absent at boot. The telemetry Caddyfile uses an explicit `bind 10.78.0.1`; the
site address alone is only a Host matcher. The site listens only on the node's
WireGuard address at `10.78.0.1:9101`; port 9101 is opened only on `wg0`, a
failing ufw rule fails apply instead of being swallowed, and Check compares
`ufw show added` against the exact rule so drift is repaired. If ufw is
unavailable, Check records the firewall as unobserved rather than failing the
step. It serves exactly two basic-auth paths:

- `/node` reverse-proxies `http://127.0.0.1:9100/metrics` and strips the
  scraper's `Authorization` and `Cookie` headers before the loopback request;
- `/targets.json` serves the node's HTTP service-discovery document, one target
  with `__metrics_path__: /node`, `__scheme__: http`, `eregion_node`,
  `eregion_component: node`, `eregion_access: wireguard` and
  `eregion_public: false`.

The telemetry Caddyfile (`root:caddy 0640`) carries only the basic-auth username
and a bcrypt hash, never the password. There is no TLS terminator because
WireGuard already encrypts the transport; there is no public listener. Caddy's
own request handling and timeouts apply; there is no custom endpoint process,
connection limiter or rate-limit table.

`metrics endpoint` reads the non-secret endpoint description through
`metrics.endpoint`: listen address, URLs, access, basic-auth username, whether
the telemetry Caddyfile exists, and whether this machine holds a local
credential. `metrics config` generates a Prometheus `http_sd_configs` snippet
and the equivalent Alloy `discovery.http` plus `prometheus.scrape` snippet.
Ordinary output, including `--json`, always uses `<metrics-password>`.
`--format prometheus|alloy|targets --out FILE` writes one new owner-only file
(`0600`) with the real credential when the format needs one; it fails without a
local credential. The targets file contains no secret and is also usable as
Prometheus `file_sd`.

The control-plane ingress step installs the pinned Caddy package even without a
public hostname. It checks the actual `/usr/bin/caddy` path for the pinned
version and the `caddy:caddy` account, so a matching manual binary elsewhere on
`PATH` cannot satisfy the prerequisite. Before installation it inspects
`/etc/caddy/Caddyfile` and `caddy.service`/`caddy-api.service`; any existing
config or enabled/active Caddy service is unmanaged and the step refuses rather
than overwriting or stopping it. On a fresh host it installs the pinned package,
ensures the account, removes only the package-created default Caddyfile and
disables only a package-started service, leaving no public site. A hostname adds
the existing public API/ACME site.

`metrics rotate --confirm <node>/metrics` follows vaire's rotate path. It
generates a 256-bit password, saves it owner-only in the resolved workspace credential directory before
the RPC, and sends only the bcrypt hash. Under a node-local telemetry lock the
node renders the telemetry Caddyfile, validates it with `caddy validate`,
restarts only `eregion-telemetry.service`, and restores the previous Caddyfile
if validation or restart fails. The public route writer's PostgreSQL advisory
lock and public Caddy reload are not involved. If the RPC fails, the CLI
restores the previous local credential (or removes the first credential it
generated). Rotation never prints the password; it warns that scrapers must be
updated and that `secrets export` is stale. There is no generation
compare-and-swap, pending file or recovery journal, so a lost successful
response can leave the local password stale until the next rotation. `server
init` generates or reuses the credential and performs the same rotate before
its doctor gate; the protected handoff and `login` carry username and password
only. `secrets export` includes the local credential when present and reports a
missing one as an explicit omission. A node upgraded from the older layout has
no telemetry Caddyfile until `metrics rotate` is run or a fresh handoff is
imported; `doctor` reports that as a warning.

`host-node-exporter-v2` creates `/var/lib/node_exporter` and
`/var/lib/node_exporter/textfile` (both root:root 0755 real directories; a
symlink or non-directory is refused rather than followed), enables
node_exporter's textfile collector and a narrow systemd collector include for
eregion, Caddy, Docker, node_exporter and `wg-quick@wg0` service/timer units.
Apply refuses an unsafe/non-root-owned or group/other-writable existing parent
before any mutation, rather than repairing an attacker-controlled path.
Setuid/setgid/sticky bits are preserved in the observation; the strict check
rejects them, and a safe root-owned non-writable path with those bits is
repaired with `chmod 0755` followed by an explicit `chmod u-s,g-s,o-t`, because
numeric chmod can preserve setgid on a directory. Apply then verifies the exact
`0755` mode and fails closed otherwise. The exporter unit is loopback-only and
has no `wg-quick@wg0.service` `Wants`/`After` dependency; startup no longer
waits for WireGuard. The access setup opens 9101 on `wg0` and restarts only
`eregion-telemetry.service` after WireGuard comes up.

The textfile directory is a foundation, not a metrics source. No eregion
self-metric is written without a real producer.

The `telemetry.local` doctor check uses the same strict parent+child validator
and reads one bounded loopback exporter response; it requires successful
`textfile` and `systemd` collector families with unambiguous success samples,
and exactly one unlabelled `node_textfile_scrape_error` sample with value zero.
Collector success alone does not detect a malformed or unreadable `.prom`; a
missing, duplicated, malformed, labelled, timestamped or non-zero scrape error
fails, and duplicate/timestamped collector success is ambiguous fail-closed.
The separate `telemetry.endpoint` check probes `/targets.json` without a
credential: a healthy endpoint answers 401, an unauthenticated 200 fails, a
node without the telemetry Caddyfile warns, and WireGuard down leaves the
endpoint unobserved as warn while a strict directory error still fails.

### Scrape examples

A host with a WireGuard route to the node can ask `eregion metrics config` for
the current snippets. The generated Prometheus form is:

```yaml
scrape_configs:
  - job_name: eregion-main
    scheme: http
    basic_auth:
      username: metrics
      password: <metrics-password>
    http_sd_configs:
      - url: http://10.78.0.1:9101/targets.json
        refresh_interval: 5m
        basic_auth:
          username: metrics
          password: <metrics-password>
```

Grafana Alloy uses `discovery.http "eregion_main"` and
`prometheus.scrape "eregion_main"` with the same discovery URL and basic auth.
`metrics config --format prometheus|alloy --out FILE` replaces the placeholder
with the local credential and writes an owner-only file. The examples assume a
`prometheus.remote_write.default` receiver for Alloy. The endpoint is not
installed or managed by Prometheus/Grafana and has no public listener.

A future optional separate telemetry node or Grafana is not prohibited
long-term, but is out of beta/v1 scope. A future central scraper must be a
WireGuard peer; adding one would need topology, retention and access decisions.

## Release and local upgrade

`internal/buildinfo` carries `Version`, `Commit`, `Channel` and the public
`ReleaseBaseURL` (`https://eregion.mbvlabs.com`). `internal/release`
resolves a channel or version manifest, verifies its sigstore bundle against
the pinned publish workflow identity, validates the manifest against the
channel, and caches the platform binary only after its signed SHA256 matches.
TUF/sigstore trust material is cached, not pinned in the binary.

Edge combines development and published `master` builds. Alpha and beta tags
are exact immutable snapshots with no moving pointer; RC tags advance the opt-in
`rc` pointer, and only final tags advance stable pointers. Manifests remain
schema 1. Their `channel` field is the signing family, not the promotion
channel: every tagged artifact, including alpha/beta and RC, says `stable` and
is signed by `publish-stable.yml@refs/tags/v<version>`; edge says `edge`. This
is the protocol already understood by alpha.2 clients and is not itself proof
of stable-channel membership. Stable and major-line build, pointer-install,
upload and promotion paths reject every prerelease by inspecting the version;
the rc pointer accepts only rc. An explicit `--version` remains the snapshot
install path. Alpha/beta use the build script's `snapshot` channel,
which creates no pointer; RC uses `rc`; final tags use their stable line. The
stable publish job accepts only a final tag whose commit is on
`origin/<major>.x`, while alpha/beta/RC tags use the `edge` environment. RC
clients also check their stable major line for a newer final; publishing a
final does not move the RC pointer. Channel manifests and signature bundles are
two separately published objects, so the existing publication race can
temporarily fail verification; an interrupted tagged upload can leave a partial
prefix that blocks retry. These are follow-up risks, not a change to the
channel model.
The container resolver and these schema-1 safety guards were introduced for
alpha.3. Publication and qualification provenance is in MANUAL-TESTING; this
mechanism reference is not a release-status tracker or permission to publish.
Commit, push, branch, tag and release actions still require operator authority.

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

## Database: embedded schema 028

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
| `017_retention.sql` | D30 retention: the singleton `server_settings` row, the revision `retention` override block, `releases.pruned_at` and `retention_runs` observability. |
| `018_retention_measurements.sql` | Durable cache measurement (`cache_bytes`, `cache_measured_at`, `cache_measurement_error`) on `retention_runs` and the node-wide `retention_gc_pending` follow-up on `server_settings`. Never rewrites published 017. |
| `019_retention_gc_generation.sql` | Per-run `gc_generation` so each registry garbage-collection pass has a distinct durable node-effect identity. |
| `020_retention_skipped_status.sql` | The `succeeded_with_skips` retention-run status for a completed run whose only unfinished items were deterministic per-item skips. |
| `021_webhook_admissions.sql` | Durable GitHub push admission: sanitized intent, non-secret environment/revision/secret-revision identities, River webhook job ID, admitted/submitted/failed lifecycle and terminal error code. Preserves legacy pending/submitted replay rows; adds no foreign key from the stored environment/revision identity so a deleted environment remains observable. |
| `022_managed_resources.sql` | Environment-owned generic Docker resource identity, image and immutable image ID, spec, private network/container, a cumulative owned-volume ledger, exec health observation and lifecycle state. Named volumes are runtime state and are never deleted by this migration; a default resource destroy retains them and only explicit `--delete-data` removes owned volumes. |
| `023_environment_resources.sql` | Revision-level managed-resource dependency names; deployment is gated on their observed readiness. Names only, never secret values. |
| `024_environment_protection.sql` | Persistent `eregion_platform_environments.protected`. Existing environments are backfilled protected (fail closed); new rows receive the creation default or explicit value. The column is mutable state, not a desired-state revision. |
| `025_domain_dns_ownership.sql` | Immutable `eregion_platform_domains.id` (`dom_…`) and the `eregion_platform_domain_dns` ledger. The ledger stores provider, zone, exact ownership marker, generation, desired origins/proxy, state, last operation and cleanup status. It is written atomically with the operation/River job before provider effects and is retained until exact owned provider cleanup is verified; `domain_id` restricts domain deletion while it exists. |
| `026_deployment_lifecycle_owner.sql` | One durable deployment lifecycle owner per environment. Stores owner operation/deployment/attempt, phase, exclusive route target, frozen restore release/deployment and timestamps. Route writers read this row exclusively. The cutover is fail closed with no legacy backfill: it refuses while a deploy/rollback/restart/promote operation is queued, running or cancelling, or while any deployment row is pending/running. Finish or cancel old work before upgrading; a safe upgrade creates the empty owner table. |
| `027_provider_credentials.sql` | Installation-wide provider/zone metadata and immutable encrypted credential revisions in the existing vault. Separate from app/environment secret namespaces; uses the existing keyring with provider-bound AAD and revision hashes. Credential activation uses a revision CAS and commits its sanitized audit entry atomically. No token-file import/backfill is performed. |
| `028_workspace_identity.sql` | Empty singleton installation identity table, generated immutable workspace ID/name and creation time, with update/delete refusal. Migration does not infer or seed identity; setup or explicit `apply --assign-workspace NAME` performs assignment under an advisory transaction lock and insert CAS. |

Tables below have the literal `eregion_` prefix:

| Table(s) | Key and authoritative facts |
| --- | --- |
| `schema_migrations` | Version/checksum/application time. River maintains its own schema history. |
| `workspace` | Singleton workspace ID/name (`ws_…`, slug) and creation time. Authoritative installation identity, not a tenant, team or multi-node boundary; an absent row is explicitly unconfigured. |
| `operations` | `op_…` ID; actor, kind, target, normalized input/hash, idempotency key, state, attempt, step count, cursor, River job ID and error code. |
| `operation_events` | `(operation_id, sequence)`; JSON event. |
| `operation_steps` | `(operation_id, step)`; inputs hash, execution identity, attempt, state, timestamps and result/error. |
| `platform_nodes` | Name; roles/capabilities, status, SSH address/user/**host key**, attested identity, probes, build concurrency/user and WireGuard public state. Private keys stay in files. |
| `platform_apps` | Opaque app ID and unique name. No stored setup wizard/source configuration. |
| `platform_environments` | App FK + name; runtime/build node, stable/candidate/container ports, bind, health settings/observation, config, pinned secret refs, desired/applied/serving release IDs, `serving_deployment_id`, `current_revision_id` and persistent `protected`. The deploy worker reads the revision and the serving deployment's recorded instance ports; the mutable legacy config/health/port fields remain only as mirrored compatibility columns for the older CLI surface. `secret_refs` remains the transitional pinned form, used only when the current revision names no secrets. |
| `platform_environment_revisions` | Immutable `erv_…` ID and per-environment number; operation/actor/operation/summary change record; source binding, auto-deploy flag, webhook secret NAME, runtime JSON, processes JSON, plain config and secret NAMEs, the `retention` override JSON, plus the first-deploy intent flag. An UPDATE trigger refuses edits; deleting the environment still cascades. |
| `platform_builds` | `bld_…` ID and unique operation FK; app/env, pinned revision and release, source, exact commit, pending/running/succeeded/failed/cancelled lifecycle, timestamps and error. Logs are the owning operation's events. |
| `platform_releases` | Release ID and unique operation FK; app/env FKs, source/build/command JSON, image/image ID, build link, exact commit, pending/ready/failed status and error, and `pruned_at` when disk retention removed the artifact. The row is never deleted. |
| `platform_deployments` | Deployment ID and unique operation FK; action (`deploy`, `rollback`, `restart`), release, previous release/deployment, environment revision ID, exact resolved secret revisions (name -> non-secret vault revision identity), recorded process instances/ports, state and errors. |
| `platform_deployment_owners` | One row per environment: owning operation/deployment/attempt, lifecycle phase, exclusive route-target deployment, frozen restore release/deployment and timestamps. Route writers read `route_target_deployment_id` exclusively; deploy-family retries renew the same owner and release it only after terminal traffic is safe. Cascades with its environment/deployment. |
| `platform_changes` | App/env history, sequence, operation/actor, desired/applied JSON and summary. |
| `platform_domains` | Immutable `id` (`dom_…`, unique) and domain name PK, environment FK, `is_primary` with a partial unique index enforcing one primary per environment, and an optional `check_path`. `desired_revision`/`applied_revision` and `route_state`/`route_error` record the Caddy write; `expected_addresses` plus `dns_state`/`dns_addresses` record public-resolver observation; `certificate_state`/`certificate_not_after` and `public_state`/`public_status` keep the other observations separate. |
| `platform_domain_dns` | One managed-DNS ledger per immutable domain ID: provider, zone ID/name, exact ownership marker, generation, desired public origins/proxy, state (`pending`/`applied`/`failed`/`cleanup_pending`/`cleanup_verified`/`cleanup_failed`), last operation, errors and timestamps. It is retained until provider cleanup is verified; the domain FK restricts deletion while it exists. |
| `platform_release_command_runs` | `(release_id, command_digest)` PK; the owning operation, running/succeeded/failed state, error code/message and timestamps. One row per release and command specification; it cascades with its release. |
| `platform_container_cleanup` | Container name PK; environment/node/deployment, reason, attempts, last error and next attempt. A failed removal is retried with backoff; the row cascades with its environment. |
| `server_settings` | Singleton row (`singleton` boolean PK); one `retention` jsonb block for the server-level default. An absent row or empty block means the D30 defaults in `internal/retention`. |
| `retention_runs` | `rtr_…` ID, optional owning operation, pending/running/succeeded/failed/cancelled status, planned/removed item arrays, planned/reclaimed bytes, per-item errors JSON, start/finish timestamps, the latest Buildpacks cache measurement (`cache_bytes` NULL means unknown, measured-at, error), and the per-run registry GC generation. |
| `secrets`, `secret_revisions` | Namespace `(app, environment, name)` and revision; state/CAS, key ID, ciphertext and hashes. Not FKs to platform app/env rows. |
| `access_grants`, `integration_audit` | Grant JSON and durable authorization/provider audit JSON. |
| `webhook_deliveries` | Hashed delivery ID and exact-body hash, actor/kind/target, sanitized repository/branch/commit, non-secret environment/revision/secret-revision identities, admitted/submitted/failed state, River job ID, operation ID and terminal sanitized error code. Legacy pending/submitted rows remain valid; pending legacy rows are reconciled only on authenticated redelivery. |

Application-model execution boundaries (MASTER sections 4 and 6):

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
  the per-environment webhook follow MASTER D28.
- Deletion is deliberate, not SQL cascade alone. The submission transaction
  locks the app row (and the environment row) and decides the human factor from
  durable state: app delete is always gated; environment delete is gated when
  the environment is protected or is the app's last environment. The TOTP code
  arrives as transient request data and is verified inside that transaction by
  the node process's verifier; it never enters operation input, events or River
  arguments. HTTP refuses a real action that currently needs the factor because
  it has no channel for a code, and direct service callers with no verifier fail
  closed. An unprotected non-last environment delete needs only the exact
  target confirmation. Sibling environment deletions for one app are
  serialized at submission, so two already-accepted deletions cannot each look
  non-last and together remove the app without the factor. `platform.environment.protect`
  updates the persistent column without a revision; lowering protection is
  factor-gated and raising it is not. `platform.environment.delete` and `platform.app.delete` then remove
  containers selected by ownership labels
  on every node recorded by the target's deployment revisions (plus the current
  environment node), delete the row-6 domain state for the target environments,
  then rewrite the routes through the one serialized writer (`syncRoutes`, one
  advisory lock) and reload Caddy. Only then are deployments, releases, builds,
  revisions, change rows, vault secret namespaces and the environment/app rows
  deleted. A failed container or Caddy step leaves the platform rows available
  for retry. Operations and operation events are never deleted; operation
  `--app`/`--environment` history still shows the deletion after the platform
  record is gone. The node validates the target-naming `--confirm` token against
  the records it loads. The `--dry-run` preview keeps target-scoped row counts,
  but its last-environment warning uses the application-wide environment count,
  so a named environment delete never claims it removes the app while a sibling
  remains; the worker re-counts remaining environments after deleting the target.
- Resource operations are durable but their row is a separate observed effect.
  A cancel or failure that terminalizes the operation updates the resource row
  in the same transaction only when the row's `operation_id` still names that
  operation, moving `provisioning`/`destroying` to `degraded` while retaining
  the image/container/network identity and cumulative volume ledger. A newer
  resource operation has already replaced `operation_id`, so it is not
  clobbered. `Reconcile` also sweeps rows still in those transient states whose
  recorded operation is already `cancelled` or `failed`; this is how a
  pre-existing stuck row from an interrupted/cancelled operation converges on
  the normal worker maintenance schedule. `resource apply` and `resource
  destroy` then accept the degraded row as an ordinary retry. No resource
  repair command or database edit exists.
- The deleted old `resources`/backup subsystem had no catalog tables in the
  control-plane migrations; alpha.2's managed-resource tables are 022/023. DNS
  records live at the provider; a domain binding is not a DNS record.
- Roles/rows do not prove tools, credentials, services or routes are installed.

## Disk retention schema (D30)

Migration 017 adds D30's durable retention state (an alpha.1 gate);
migration 018 adds the durable Buildpacks cache measurement and the node-wide
registry GC follow-up without rewriting the published 017.
`internal/retention`
owns the D30 numbers (`DefaultPolicy`: newest 10 releases per environment, any
release newer than 14 days, unreferenced registry artifacts after 14 days,
Buildpacks caches above 10 GiB or 80% filesystem utilization), the environment
`Override` block, field-by-field `Merge`, range validation and the cache-limit
check; the serving release and its rollback target remain a planner invariant.
The server default is the single `server_settings.retention` block, read and
written by `Service.ServerRetentionPolicy` and
`Service.SetServerRetentionPolicy`; an absent/empty block means the D30
defaults. `Service.EffectiveRetentionPolicy` returns the server default with the
current revision's non-nil override fields replacing it, and unrelated revision
changes carry the block forward unchanged. `releases.pruned_at` marks a release
whose artifact was removed while its row and history stay; `preparePlatform`
refuses `deploy --release` and `rollback` to such a release with
`precondition_failed`, naming the release and prune time. `retention_runs`
stores the operation, status, planned/removed items, planned/reclaimed bytes,
per-item errors and the latest complete cache measurement for observability.
Migration 020 adds the `succeeded_with_skips` status a run uses when the only
unfinished items were deterministic per-item skips.

`retention.Plan` is the pure planner both the periodic job and `prune
[--dry-run]` call. It receives `retention.Input` plain values filled from
the platform rows and node inventory through `retention.InputFromInventory`,
keeps each environment's serving release,
its rollback target (selected with the same rule as
`previousRollbackRelease`), the newest `keep_releases` and every release newer
than `keep_newer_than_days`, protects in-flight releases and containers, keeps
registry artifacts shared with a kept release, and plans release-row, immutable
image, registry artifact, Buildpacks cache, ownerless container and dangling
image items with a total-bytes helper. Release and image removals are separate:
a release item only marks `pruned_at`, and an image item is emitted only for a
release `ImageID` that no retained release or existing container still uses.
Releases with no immutable `ImageID` get an `image_skipped` diagnostic and are
never removed by tag. A pruned release whose image is still present keeps
producing an image item on later runs. An incomplete cache measurement produces
`cache_skipped` instead of a zero-byte healthy cache. Registry matching
normalizes registry hosts and ports.
A release tag protects whatever manifest it currently points to, but a tag never
resolves a retained release: resolution requires an immutable manifest digest
(`Image@sha256` or `Artifacts`), a matching inventory `ConfigDigest`, or an
`ImageID` mapped to a manifest through local image `RepoDigests`
(`Input.LocalImages`). Removal items are repository-qualified (`repo@sha256:…`)
and never collapse across repositories. If a retained release has no resolvable
immutable identity, the planner emits a `registry_skipped` diagnostic and plans
no deletion for its repository (or all repositories when none can be
determined). A release that never reached ready and records no `ImageID`, no
`Artifacts` entry and no digest in its `Image` reference is treated as never
having produced an image: it neither blocks nor contributes to registry
protection. A failed release that did push an image still records its
`ImageID`, so it still requires resolution. A skip diagnostic is emitted only
for a repository the inventory contains; when a retained release names a
repository with no inventory artifacts, there is nothing to protect or remove
and no diagnostic is added.

### Retention node effects

`retention.inventory` is a read-only node RPC (`inventory` on the node
transport) returning `retention.Inventory`: registry manifests keyed by
`(repository, digest)` with their tags, config digest, created time and the
bytes of the manifest plus its unique blobs; the Buildpacks cache volumes and
sizes; every Docker container with its immutable image ID, ownership state and
lifecycle state; dangling images; local images with their `RepoTags` and
`RepoDigests`; and statfs total/free bytes for the data directory. Cache sizes
come from Docker's `GET /system/df` calculation over the Docker socket; the
worker never walks `/var/lib/docker`. A missing entry, `UsageData.Size == -1`,
an API error or the five-minute bound is an unknown size, never zero. Identical
digests in two repositories stay two entries, because a registry delete names
one repository. Registry storage is read in Distribution's layout
(`blobs/sha256/<first two hex>/<hex>/data`) and a revision is live only while
its `_manifests/revisions/.../link` is valid. A missing manifest or referenced
blob is an inventory warning, not a zero-byte artifact.
Every container is reported, labeled or not, so an image used by a stopped
container the operator created outside eregion is protected. Only a container
with the `io.eregion.app` label is ever a removal candidate.

The inventory runs on the node transport that serves the target node. An
enrolled node is read by the root `eregion node rpc` process; the main node's
local transport, which `eregion-worker.service` uses for its daily run, runs
in-process as the unprivileged `eregion` account. `host-directories-v2` creates
`/var/lib/eregion/registry` as `root:eregion 0750` (`host-directories-v1` used
`root:root 0700`) so that local worker can read the tree the root registry
container writes with the default umask, without write access. A permission
failure names the registry path and the `eregion apply` fix; other unreadable
states stay generic. Removal and the Docker/HTTP parts of GC do not need that
read permission: removal uses the Docker group (`SupplementaryGroups=docker` on
the API and worker units) and the registry HTTP API, and GC runs `registry
garbage-collect` in a one-off container. The GC script also stops and starts
`eregion-registry.service`. `host-registry-gc-polkit-v1` writes
`/etc/polkit-1/rules.d/50-eregion-registry-gc.rules`, which authorizes only the
configured service user (`eregion` by default) on only
`org.freedesktop.systemd1.manage-units` for only `eregion-registry.service`,
and only the `start`, `stop` and `restart` verbs. Every other user, unit and
verb keeps the distribution's default authentication policy. `host-packages-v2`
adds `polkitd` on the registry role so minimal Debian 13 and Ubuntu 26.04
images have the policy daemon and system D-Bus. The worker unit's
`NoNewPrivileges`, `ProtectSystem=strict` and `PrivateTmp` do not block the
system D-Bus call; it has no `RestrictAddressFamilies`, `PrivateUsers` or
`PrivateNetwork` setting. The GC script itself is unchanged, so its fail-closed
stale-collector path, restart trap and journaled step semantics are retained.
A polkit rule is smaller than a root-owned trigger/path-unit protocol and keeps
the existing script as the single GC implementation.

Removal is one journaled node step per planner item:
`platform-retention-remove-<12 hex>-v1`, where the suffix hashes the item kind
and reference. The kinds are `release` and `dangling_image` (local image
removal), `registry_artifact` (`DELETE /v2/<repo>/manifests/<digest>`
through the loopback registry API), `buildpack_cache` (`docker volume rm`) and
`container` (`docker rm --force`). A local image is never removed by `--force`
and never by its ID while tags still point at it: the step re-reads the image's
current `RepoTags`, verifies each tag still resolves to exactly the planned
immutable image ID, and runs `docker image rm <repo:tag>` for that tag; removing
the last tag removes the image. A tag that has moved to another image is left
alone and recorded as a per-item skip. After untagging, the step re-inspects the
immutable image ID; an image kept alive by a digest-only reference or a
concurrently added reference is a per-item skip ("image still referenced by …"),
never a claimed removal. The untag is fenced by the node host lock against every
eregion image mover: pack builds, deploy pulls, image push (load, tag and push),
container and release-command runs (`--pull never`), the host-registry apply
step, and registry garbage collection. A manual operator `docker tag` or
`docker pull` on the node during a prune is outside that fence and out of scope:
Docker has no identity-bound untag, so manipulation of the node's Docker state
by hand while a prune runs is not protected. Already-gone is success, so a
retried prune converges; a registry without deletes enabled fails with `405`
naming `host-registry-v2`. Container removal re-inspects the container at
execution time, requires the `io.eregion.app` ownership label and the payload's
app and environment, and removes the immutable container ID; the registry
container and any unowned container are refused. A deterministic Docker refusal
(an image referenced in multiple repositories after tag handling, an image or
volume in use, a 409 conflict) is a per-item skip with its reason recorded in
the run; daemon unavailability and timeouts stay retryable. Removal and GC
steps run under the node host lock, so they cannot race a pack build. A GC
effect,
`platform-retention-gc-01-v<N>`, stops `eregion-registry.service`, removes an
orphaned `eregion-registry-gc` container from an interrupted attempt, verifies
no registry garbage-collect process remains, runs `registry garbage-collect` in
a one-off container against the same storage, and restarts the unit. If the
stale collector cannot be proven gone, the step fails with the registry left
stopped and a recovery instruction; the restore trap is only armed after the
orphan is verified gone and our collector is created. A failed collection or a
failed restart fails the step; a restart failure is never suppressed. It never
passes `--delete-untagged`: releases are referenced by digest, so an untagged
manifest can still be a live release; only manifests explicitly deleted through
the API become unreferenced, and plain garbage collection removes their blobs.
`image.push` takes the same host lock, so a registry restart or collection
cannot interrupt a push.

`host-registry-v2` enables manifest deletion (`storage.delete.enabled: true`
plus `REGISTRY_STORAGE_DELETE_ENABLED=true` in the unit) and restarts the
registry so a v1 node takes the change. A v1 node reports the new version as
pending once; the second plan is a no-op. The control plane/deploy path builds
with explicit build and launch cache volumes,
`eregion-build-cache-<hex app>_<hex environment>` and
`eregion-launch-cache-<hex app>_<hex environment>`, through two `pack build
--cache` flags. The encoding is reversible, cannot collide (`a-b`/`c` versus
`a`/`b-c`) and avoids pack's reserved-name rewriting. Inventory also reports
pre-upgrade `pack-cache-*.<build|launch>` volumes as ownerless caches; pruning
one is safe under the node host lock because no pack build is running.

### Retention execution and operator surface

`retention.InputFromInventory` is the one adapter from the node wire inventory
and the platform rows to `retention.Input`. Container ownership comes from the
same `io.eregion.app`, `io.eregion.environment`, `io.eregion.release` and
`io.eregion.deployment` labels the deploy path writes. Every container, labeled
or not, protects the immutable image it runs; only a container with the
`io.eregion.app` label can be pruned. Ownerless legacy pack
caches stay eligible; the disk used/total figures use statfs total minus free.
A registry artifact with no reported creation time is treated as new, so an
unknown age can never authorize deletion. A truncated inventory plans no
registry deletion at all and emits one `registry_skipped` item naming the
truncation; inventory warnings are returned beside the plan.

`retentionWorker` runs on the maintenance queue. An hourly River periodic job
checks whether the UTC day still needs a `platform.retention.prune` operation
on `node/<server>`; it also runs on worker start. The submitter does nothing
when the day has a queued, running, cancelling or succeeded operation. When
every operation for the day is failed or cancelled and the newest is at least
an hour old, it submits one new operation with the next numbered idempotency
key (`retention-daily:<date>:<n>`); the first run of a day keeps
`retention-daily:<date>`. Thus at most one automatic daily run succeeds per
UTC day (a manual `prune` is separate and does not count toward it), a failed
or cancelled day can retry the same day, and a deterministic failure is bounded
to one new attempt per hour instead of a loop. The operation's own River job
claims the operation and then
acquires sorted PostgreSQL advisory locks for `platform`, `node/<server>` and
every environment target before reading state, so accepted deploys, rollbacks,
restarts and app/environment deletes serialize with it. The plan is evaluated
under those locks, but `pruned_at` is still set in a separate transaction that
re-locks the environment row and re-checks the release: a release that became
serving, the current rollback target, or part of a pending/running deployment
or active release operation is removed from that run's planned items and is
never pruned. The same check protects a deploy accepted after the periodic job
was scheduled.

A run inserts one `retention_runs` row with its planned items and bytes, then
executes in order: mark each release row `pruned_at`, dispatch the journaled
`platform-retention-remove-*` steps for release images, registry artifacts,
cache volumes, containers and dangling images, then run
`platform-retention-gc-01-v<N>` after all manifest deletions in that attempt
have settled and none remain failed, if any registry manifest was actually
deleted.
Release rows are marked separately from image removal: an `image` item names
only an immutable image ID, and a release with no `ImageID` is never removed by
its mutable tag. The planner protects an image shared with any retained,
serving, rollback or in-flight release and with any existing container (running
or stopped, labeled or not), reporting an `image_skipped` diagnostic instead.
A release already
marked `pruned_at` whose image is still present is planned again on later runs,
so an exhausted removal failure converges instead of becoming invisible.
Every successful item is appended to `removed_items` and its bytes to
`reclaimed_bytes`; every failed item is recorded in `errors` and the operation
returns a retryable error, so River retries the unfinished items from the same
run without repeating completed effects. A deterministic conflict is recorded
as a per-item `skipped` diagnostic, leaves the plan, and does not fail the run;
a run whose only unfinished items were skips finishes `succeeded_with_skips`.
Before every remaining effect and
before GC, the worker re-runs the planner under its held locks and intersects
the persisted items with the fresh eligibility set; a registry artifact,
container, cache or image that became protected or disappeared is dropped with
a recorded reason and never silently removed. A registry delete sets the
node-wide `retention_gc_pending` flag before the manifest is touched; it is
cleared only by a GC effect that completes successfully in this attempt after
all manifest deletions have settled, and any later run drains it even with no
new deletions. Each new GC pass increments the run's persisted
`gc_generation`, so `platform-retention-gc-01-v<N>` is distinct from an earlier
completed pass and newly orphaned blobs are collected. A permanent node
refusal fails the operation after recording the run; the next daily run retries
it after the host is fixed. Operation events carry planned/removal/protection
progress. The
build path records the local image config digest (`image_id`) but has no pushed
manifest digest in its existing pack/inspect output; planning therefore
resolves manifests through inventory `ConfigDigest` and local-image
`RepoDigests`, and no extra registry read is added to every build.

The operator surface is node-mode RPC: `retention.plan` evaluates the shared
planner read-only, `retention.show` returns the server default or an
environment's effective policy with per-field `server`/`environment` sources,
and `retention.set` replaces the server default. Environment overrides submit a
normal `platform.environment.update` revision, never a direct row edit.
`prune --dry-run` prints the plan items and bytes; a real `prune` needs
`--confirm <server>` and submits the operation, following it through the same
viewer as other mutating commands.

### Retention doctor rows

`host.retention` reads the newest `retention_runs` row on the main role and
reports its finish time, status, removed-item count, reclaimed bytes and the
run's planned reclaimable bytes. A `succeeded_with_skips` run passes with an
informational note naming the skipped items; a failed, cancelled or stale
running row warns;
a last success older than 48 hours warns once the daily job has had time to run.
When the newest failed run's first error names unreadable registry storage, the
warn points at `eregion apply` and `host-directories-v2` instead of a generic
retry.
Before the first run, the row passes with "no retention run yet" so a fresh
install stays 0 warn / 0 fail; it warns only when `eregion-worker.service` has
been continuously active for more than 48 hours without recording a run. The
48-hour grace is not a policy value and does not hide a failed run.

`host.build_cache` reads the newest durable cache measurement from
`retention_runs` (`cache_bytes`, `cache_measured_at`,
`cache_measurement_error`) and compares it with the server policy's absolute
`buildpack_cache_max_bytes` cap. Doctor never walks Docker volumes or cache
directories synchronously. A missing measurement or one older than 48 hours is
reported as unknown (pass), never as a healthy 0; a complete empty cache is a
real 0. Above the cap it warns and names `prune --dry-run`/`prune --confirm
<server>`; the daily planner removes least-recently-used entries. The
filesystem-percentage limit still makes cache entries eligible for the daily
planner; general filesystem pressure is already surfaced by `host.disk`. If the
inventory could not size a cache volume, or the volume list was truncated, the
retention run persists NULL and the planner emits `cache_skipped` rather than
treating unreadable data as empty. Both rows are `AtInit` checks, so the
main-node report is 20 results: the previous 18 plus retention and cache.

`retention_gc_pending` in `server_settings` is the durable follow-up for a
registry manifest delete. The worker sets it before the first delete, keeps it
set while any registry delete attempt is unsettled or failed, and clears it
only after a GC effect completes successfully in that attempt after all
deletions settled. Any later retention run checks it even when it has no new
deletions, runs GC when registry protection is resolvable, and clears it. Every
new GC pass uses the next persisted `gc_generation` in its step ID, so a
completed earlier pass cannot satisfy a later one.

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

### Readiness budget and diagnostics

The HTTP `/readyz` handler uses one four-second child context, respecting any
earlier caller deadline. Other API routes retain their existing ten-second
context budget. This leaves headroom before the host probe's five-second HTTP
timeout; it is cancellation containment, not a demonstrated fix for the
intermittent alpha.6 timeout. The handler remains synchronous and public errors
are generic. HTTP server `ReadTimeout` is not the handler-execution deadline.

Readiness still validates Eregion migration history/checksums and River
versions through the persistent API pool. Three top-level read stages include
River's internal table/column/version queries: the initialized path involves
five logical SQL executions. The redundant outer River-table lookup is removed;
River still performs its own existence check and handles an absent table. This
eliminates one serial exchange on the initialized path without weakening
migration validation, but is not a demonstrated timeout fix. The API's pool
maximum remains 12; worker connections belong to a separate process/pool in the
installed layout.

Request-scoped pgx acquire/query/release traces record fixed stage names,
durations and counts, including row lifetime. Each stage also retains bounded
per-query ordinal timing (first eight queries, fixed `ok`/`failed` outcomes);
these details appear only in the existing slow or failed summary. **Acquire
time is not pure queue wait:** idle validation, ping and reconnection can
contribute. Before/after whole-pool counts are not attributed exclusively to the
readiness request. A process-local reporter emits one safe summary for slow (at
least one second) or failed probes, rate-limited to one per ten seconds with
suppression counts. A failed summary carries one fixed `error_class` (`canceled`,
`deadline`, `postgres`, `network_timeout`, `network_io` or `other`) derived from
the wrapped error chain, plus a separate `context_state` (`active`, `canceled` or
`deadline`) so a canceled caller does not relabel the actual error. Probes are
attributed from their call site as runtime `api`, `worker` or `node_rpc` and
phase `startup` or `request`; an unattributed path reports `unknown` rather than
guessing. SQL, arguments, connection strings, URLs and raw driver errors are not
retained in the report. These thresholds are diagnostic defaults, not an
established latency SLO. No new telemetry backend is introduced.

Instrumented healthy samples alone cannot close LABS-346. A cause-specific fix
still needs causal timing evidence under an explicitly authorized workload and
its regression; current instrumentation does not establish the live cause.

## External control-state recovery

### Replacement VPS: current recovery boundary

The recovery objective is to reuse the **same surviving external control
database and external application services** on a replacement VPS. This is not
currently a supported one-command replacement-node flow. Merely supplying the
old DSN to `server init` does not reconstruct trusted access, placement or lost
artifacts. Do not run init against an existing database as an undocumented repair.

Under D4, managed local volumes hold expendable data. Put serious application
state in external services with their own backup/durability policy. The control
database is not a backup of local volume contents. Eregion currently represents
external dependencies through config and vault secret names; it does **not**
provide first-class external-resource records equivalent to its managed Docker
resource lifecycle. That product-model gap requires a separate decision.
Expendable-for-recovery is not permission to delete data: D19's default volume
retention and exact-owned `resource destroy --delete-data` contract are unchanged.
Do not close a resource-retention qualification ticket merely from this policy.

Before any attempted recovery, inventory these protected prerequisites without
putting their values into tickets, logs or ordinary command output:

| Asset | Why it is needed / current limitation |
| --- | --- |
| Surviving control PostgreSQL | Desired revisions, operations, releases, domain ledgers and encrypted secrets live here. Verify schema/checksums/River against the selected signed binary. |
| Original vault keyring, including old keys | Ciphertext in PostgreSQL is unusable without the matching host-local keys. A fresh keyring cannot decrypt it. Retain a current protected handoff outside the lost VPS. |
| Node and transport identities | Node attestation is pinned in PostgreSQL; SSH/WireGuard identities and operator keys also have local state. A newly created VPS identity is not automatically the old trusted node. There is no supported replacement-node enrollment/rebinding flow yet. |
| Protection and deploy-token state | Root-only TOTP/replay state and deploy-token hashes are host files, not control-DB rows. Account for their protected recovery separately; never reset the human factor or mint replacement tokens as an undocumented shortcut. |
| Runtime placement and in-flight work | Deployments refer to their recorded node. Worker repair restarts missing containers on that node; it is not a general migration to a different node. An unreachable old node is not proof its containers are gone. Fence the old writers before replacement. |
| Immutable images and source | Local registry loss can remove the only copy of an exact release digest. An external digest-addressable image can be recovered if still available; rebuilding source may create a different image and does not recover historical rollback artifacts. |
| Host intent, DNS and access configuration | Public IP/DNS, TLS routing, service configuration and pinned operator routes must match the replacement deliberately. Never disable host-key verification to make access work. |
| External application services | Keep the same external data endpoints and correct credentials. Their consistency, backups and recovery objectives are operator/provider responsibilities. |

A current handoff is essential for the material it explicitly contains, but do
not assume it is a complete host image or contains every attestation, SSH-host,
WireGuard-server and protection-state file. Verify recovery coverage against
the export implementation without exposing the private artifact. Missing
host-local identity material is an unsupported recovery gap, not something the
surviving database can regenerate safely.

The safe sequence is a **recovery design/checklist**, not an executable promise:
fence the lost/old node and writers; verify DB and protected recovery assets;
establish replacement host and trusted identities through a reviewed supported
mechanism; reconcile placement and in-flight effects; restore available exact
artifacts or explicitly create new releases; restore routes/access; verify
readiness, serving HTTPS and secret-match markers. Stop where a required
replacement/identity/placement mechanism is unsupported. Do not substitute SQL
edits, forged journals, ad-hoc repair commands or assumed successful operations.

Before RC this inventory, procedure and unsupported-gap disclosure must be
reviewed. A live replacement-VPS rehearsal is **not required** by the current
release policy and has not been established by documenting these steps. There
is no scheduled backup/volume-backup product or recovery-time/data-loss guarantee.

### Control database restore (different failure mode)

The control database is the PostgreSQL provider's recovery responsibility (D4,
D9); eregion has no backup product or resource backup/restore commands. A
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

### Portable workspace identity

Migration 028 adds `eregion_workspace` but seeds no row. `ReadWorkspace` reports
both a missing pre-028 table and an empty table as unconfigured; reads never
assign. `AssignWorkspace` takes an installation-specific transaction advisory
lock and an insert CAS. Same-name assignment returns the existing `ws_…` ID;
another name conflicts. The table refuses updates/deletes. Names are 1–64
lowercase alphanumeric/hyphen characters, with alphanumeric ends; IDs are
generated non-secret identifiers, not authorization capabilities.

Fresh setup accepts `--workspace-name NAME` and answers-file `workspace-name`,
independently of `--node`. The global `--workspace` selects an existing verified
workspace only and is rejected by init/login. Pending init stores the requested
name before host apply on real execution, then the DB-authoritative ID before
final handoff. A dry-run resume keeps a newly supplied name in memory only.
Identical persisted input is valid on resume; a conflicting name fails before
contact. A pre-alpha.7 pending init that already applied its host phase cannot
introduce a workspace there: finish that flow, then use the ordinary explicit
assignment path below.

An existing alpha.6 installation initializes through
`apply --assign-workspace NAME` after upgrading/migrating. `host-workspace-v1`,
immediately after the database step, assigns the identity and mirrors only
`workspace_id`/`workspace_name` into `node.json`, preserving node name and
unknown keys. That file is not an authority or a DB-unavailable fallback. The
recorded host intent keeps the requested name so future convergence checks the
same identity. A conflicting request is checked again under the whole-apply lock
before state, credentials, journals or host effects. Dry run plans but does not
assign.

Successful main-node apply reads the authoritative identity for local metadata.
The refresh targets the private access record and route captured by the RPC, not
whatever workspace another terminal has since selected. Workspace-store mutation
uses a shared lock and reload; a replaced route or configured identity conflicts
rather than being overwritten. A local refresh failure is a warning after
successful host convergence, not a claim that the host rolled back. Unrelated
workspace records and the latest current selection are preserved.

The protected handoff carries `Workspace` and `Workspace ID`; imported private
access metadata carries `workspace:{id,name}` alongside its route and
credentials. Login verifies the declared workspace and node identity before
publishing files, retaining exact replacement confirmation and scoped host-key
pins. Within one local store, records group by authoritative workspace ID; the
same name with different IDs and the same ID with different names conflict.
Multiple access records for one ID are one workspace, resolved by the selected
record or the sole record and otherwise refused as ambiguous. A verified login
may replace all records for that ID with one canonical record while preserving
unrelated workspaces. Legacy bundles remain explicitly unconfigured and are
confined to the bounded alpha path; refresh the human-protected export to
transfer portable identity.

`node.info` and server status read PostgreSQL, not the mirror. A failed identity
read is unknown (`workspace_error`), not unconfigured; trust-sensitive login and
setup refuse it. The separate version RPC and host-control dispatch remain
available without a workspace DB read. The TUI labels actual node identity and
workspace separately from the SSH route, and shows unknown versus
not-configured explicitly. No workspace membership, team ACL, multi-node
lifecycle or server-sharing feature is introduced.

The old-bundle/unfinished-setup and legacy submission-capability handling is an
explicitly temporary alpha.7 transition, not a retained compatibility API. The operator's
beta.1 decision requires removing those fallbacks and qualifying a fresh
installation. This reference describes current alpha behavior; it does not
promise an in-place alpha-to-beta upgrade or authorize resetting existing data.

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

Answers have a flag and an `--answers` TOML key: workspace name via `--workspace-name` or answers key `workspace-name` (required on
fresh setup), node name, public hostname (used exactly as given, only the
trailing dot is trimmed), external DSN file (owner-only; `EREGION_DATABASE_URL`
is the environment fallback) and admin user. A terminal is asked only for what
no flag or answer supplied.

```toml
host = "root@203.0.113.10"
workspace-name = "lab"
node = "main"
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
1.11.1 with a pinned sha256 per architecture, listening only on loopback
`127.0.0.1:9100`; `host-node-exporter-v2` also creates
`/var/lib/node_exporter` and `/var/lib/node_exporter/textfile` (both root:root
0755 real directories, no symlinks), enables the textfile collector
and the narrow systemd collector set
`(eregion-.+|caddy|docker|node-exporter|wg-quick@wg0)[.](service|timer)`.
`host-telemetry-v1` ensures `/etc/eregion-telemetry/targets.json`, the dedicated
`eregion-telemetry.service` unit and the WireGuard-only firewall rule; `metrics
rotate` writes the rotation-owned telemetry Caddyfile and restarts only that
unit. The dedicated Caddy instance listens on `10.78.0.1:9101` with basic auth,
`/node` and `/targets.json`; public Caddy is untouched. The access setup allows
9101 on `wg0` only and restarts `eregion-telemetry.service` after WireGuard
comes up. `node-exporter.service` and `caddy.service` are part of the main
role's service readiness, so `server status` and `doctor` show their active
state; the telemetry instance is checked by `telemetry.endpoint`.
`telemetry.local` also reads the loopback exporter and fails when the strict
textfile directory is absent/wrong, either collector is missing/failed, or
`node_textfile_scrape_error` is missing, ambiguous or non-zero.
`telemetry.endpoint` independently requires 401 without a credential, fails an
unauthenticated 200 and warns until rotation configures the telemetry Caddyfile. Neither
check stores samples. `internal/access` remains the
hardening/WireGuard/commit-confirm lockdown implementation.

After lockdown the CLI pins the WireGuard host key, saves the token-free workspace access,
generates the operator-side telemetry credential (or reuses the one already in
the workspace credential directory), sends only its bcrypt hash to `metrics.rotate`, and calls node RPC
`ready`. It then checks the authenticated telemetry endpoint over the tunnel,
asks the public resolvers once, waits for Caddy's certificate, and runs the
doctor gate (`host.doctor` with `init: true`). Init mode carries an `AtInit`
filter for checks that cannot pass before the first handoff; no current doctor
check is excluded, and a pending certificate for the main hostname stays a
warning. Any remaining `fail` stops init with `doctor_failed`, the failing
checks and their fixes, and the resume instruction. `--skip-doctor` skips the
gate and leaves a warning in the result. Only then does init build the handoff:
every server secret in plain text (operator keys, sudo password, DSN, vault
keyring, telemetry credential) plus an armored age transfer bundle with a
generated passphrase and break-glass instructions. A terminal waits for
`stored`; JSON/no-TTY requires `--secrets-out` before any change and writes the
whole record to a new 0600 file. No API token is in the handoff.

The unfinished init is saved locally under the private workspace store directory:
`pending/<server>/init.json` (answers, learned facts, completed phases),
`known_hosts` and the captured `vault-keyring.json`, all 0600. `--resume`
continues at the first incomplete phase (first contact, operator identity, host
bootstrap, WireGuard access, workspace, readiness/certificate checks, doctor gate,
handoff). After a failed doctor gate, resume therefore re-runs only the gate and
the handoff; once the gate has passed it is not repeated, and the passing report
(or the `--skip-doctor` marker) is stored in the pending init so the resumed
result shows the same doctor line without inventing a new count.
`--discard --confirm <server>` drops the local state and the generated identity
and warns when the host was already bootstrapped. New init refuses while saved workspace access or a pending init
for the same server exists. `server init --dry-run` still connects, pins the host
key and uploads a planning bundle; it stops before apply and removes the pending
state it created. The interactive result card includes the doctor summary line.

The age handoff is shown on the interactive path and written to `--secrets-out`
on the unattended path; both carry the same record. DNS/TLS pending is a
warning. Its HTTPS probe accepts an HTTP response irrespective of status: it
proves a TLS connection, not authenticated readiness or an application response.

## Host convergence and inspection

`eregion plan`, `apply`, `doctor`, `ssh` and `server status` are node-mode
commands: the CLI resolves the saved workspace access, opens pinned SSH inside the
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
certificate for the main hostname, disk, the last retention run, the Buildpacks
cache against the policy cap, CLI/node/schema/River versions and the role's
systemd units; every service unit contributes its own result row, so the total
grows with the role. Any `fail` finding makes the command exit non-zero.

An unreachable control database is classified before it reaches operator output:
`dns_resolution`, `tcp_connect` (refused or unreachable), `tls_handshake`,
`tls_verification`, `authentication`, `timeout`, or `other`. The message names
the DSN's host and port only and never its user, password, database name, query
parameters, full connection string, or raw driver error. `plan` adds the
class's fix as the step hint, `apply` carries it in the step fault, the existing
`version.schema` doctor row reports the class and fix (so the doctor check count
does not change), and `server status` shows it in `schema_error` plus
`schema_fix`.

`host-journal-access-v1` adds the admin account to the `systemd-journal` group
so `journalctl` can read the eregion units without sudo. Server init's access
setup applies the same membership on a fresh host; the versioned step converges
an existing host. Check is complete when the membership or an absent admin
account already matches, so a second apply does not run it again.

`host-directories-v2` keeps the bootstrap, node, configuration and data
directories private and makes only the registry storage readable by the service
group: `/var/lib/eregion/registry` is `root:eregion 0750`, never writable by
`eregion`. A node that ran `host-directories-v1` reports the new version as
pending once; a second plan is a no-op. The registry container still runs as
root, so the group mode does not affect its writes. This is what lets the
worker's local retention inventory read manifests and blobs as `eregion`.

`host-packages-v2` adds `polkitd` to the registry role so a minimal image has
the policy daemon. Its apply computes the missing set with `dpkg-query` and
passes only those names to
`apt-get install --no-upgrade --no-install-recommends`, so an
already-installed package (notably `docker.io`) is never upgraded and Docker is
not restarted by an unrelated apply. A fresh host has every package missing and
installs the full list. `host-registry-gc-polkit-v1` writes the narrow rule
above; the file is root-owned `0644` in a root:polkitd `0750` directory so
`polkitd` can read it and cannot modify it. A node without the rule reports
`host-registry-gc-polkit-v1` pending once; a node without `polkitd` installs it
through `host-packages-v2`. Both steps are part of fresh init, and a second
plan is a no-op.

`ssh` uses the workspace access record's pinned host key and identity. Without `--command` it
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
| `/etc/eregion/auth.json` | Owner-only deploy-token hashes, names, actors, scopes, grants and expiry. Setup mints no operator token and creates the store only when absent; host convergence validates the existing store and never overwrites minted or revoked tokens. The API rejects token auth if the file becomes unreadable or malformed, and reloads it when it changes so create/revoke need no API restart. |
| `/etc/eregion/protect.json` | Root-only confirmed/pending TOTP secret and its replay/lockout state. Never in the database, revision, operation, or journal. |
| `/etc/eregion/node.json` | Node name, state directory, main hostname and optional operator-installed helper configuration. The control-plane role writes it too, so node RPC, CI/push URLs and `secrets export` work on the main node. |
| `/var/lib/eregion/bootstrap/` | Pre-database state/journal, protected cached credentials, uploaded bundles, commit-confirm snapshots, the recorded host intent (`intent.json`) and owner-only copies of its TLS/key inputs (`inputs/`). |
| `/var/lib/eregion/node/` | Node journal, per-effect records/locks, detached-task logs and identity. |
| `/var/lib/eregion/access/` | Operator-access snapshot and lockdown state; separate from bootstrap commit-confirm. |
| `/etc/caddy/Caddyfile` | Bootstrap-owned main hostname config, importing routes, when a public hostname is configured. Absent on a hostname-less control plane; existing unmanaged config/service is refused rather than overwritten. |
| `/etc/caddy/eregion.d/routes.caddy` | Worker-owned generated app host routes. |
| `/etc/eregion-telemetry/Caddyfile` | Rotation-owned basic-auth Caddyfile for the dedicated telemetry instance (`10.78.0.1:9101`), `/targets.json` HTTP discovery and `/node` loopback proxy. It carries the bcrypt hash, never the password; apply never overwrites it. |
| `/etc/systemd/system/eregion-telemetry.service` | Dedicated Caddy instance for telemetry. `host-telemetry-v1` writes it; it waits for `wg0` and the Caddyfile, and is independent of public `caddy.service` and its route reload. |
| `/usr/local/bin/node_exporter`, `/etc/systemd/system/node-exporter.service` | Pinned exporter and its unit, bound to loopback only; `host-node-exporter-v2` enables the textfile collector at `/var/lib/node_exporter/textfile` and the narrow systemd collector set. No WireGuard or public port is opened for the exporter. |
| `/etc/eregion-telemetry/targets.json` | Root:caddy `0640` HTTP service-discovery document (no secret). `host-telemetry-v1` writes it; the access setup opens 9101 on `wg0` only. |
| `/var/lib/node_exporter/`, `/var/lib/node_exporter/textfile/` | Root:root `0755` real directories (no symlinks) read by node_exporter's textfile collector. Apply refuses to follow or mutate a symlink/non-directory; no eregion self-metric is written without a real producer. |
| OS config dir + `/eregion/contexts.json` | Owner-only private workspace store (verified workspace identity plus historical access keys/routes and `auto_upgrade_nodes`); overridden by `EREGION_WORKSPACE_FILE`. No API token. |
| Private workspace store parent + `/servers/<access>/` | Operator SSH key, WireGuard key and sudo password files for that private access record. |
| Private workspace store parent + `/pending/<server>/` | Unfinished `server init`: answers, phases, pinned host keys and the captured vault keyring (0700 directory, 0600 files). |
| `/etc/sudoers.d/eregion-node` | Admin's passwordless `/usr/local/bin/eregion node rpc` rule, validated by `visudo`. |

Node effect identity is `operation:step`, stable across attempts. Execution
identity is `operation:attempt:step`. The engine serializes host mutations with
a node-local host lock. The build-stop step alone opts out through the trusted
`HostLockIndependent` step interface: it only writes an operation-private
cancellation marker and must run while pack holds the host lock. It still takes
its own effect lock and uses the normal durable journal; the request payload
cannot select this behaviour. Check durable status before dispatch;
acknowledge completed matching effects rather than reapply. Locks on the node
are host-local. Per-environment PostgreSQL advisory locks serialize deployments
across workers; River queue concurrency is not that lock. Journal compaction
does not automatically delete durable effect records. Corruption fails closed.

## Current application actions

### Webhook activation and replay

`env update --auto-deploy --github-webhook --json` explicitly opts into local
authenticated gh assistance without displaying the generated HMAC secret.
`--rotate-webhook-secret --github-webhook` replaces the active secret. Without
explicit gh assistance, enable/rotation remains a human-terminal flow. This is
distinct from `--github-auth`, which supplies a repository clone credential.

The canonical environment-update input includes non-secret
`rotate_webhook_secret` intent. Explicit enable retains its requested intent
even when the environment is already enabled, so the same key/input can replay
after the first submission changes current state. Known replay is resolved
before gh offers/preflight, candidate generation or writes. Dry run performs
none of those effects. gh preflight verifies local authentication and repository
admin access before generating a candidate; it is not a guarantee that a later
GitHub write or signed push will succeed.

The node transport carries candidate bytes only in transient request secrets.
The shared admission service inserts the operation's unique key before
activation; only the winning transaction activates a revision through
`vault.Store.Set(ctx, tx, ...)`. Ciphertext activation, applicable desired-state
revision, operation/events and River enqueue commit together. A rotation-only
operation has real rotation semantics without fabricating an unchanged desired
revision. Plaintext is excluded from canonical input/hash, stored operations,
events, River arguments and ordinary results. HTTP cannot supply that transient
channel; a fresh rotation without material is refused.

Concurrent first invocations can prepare candidate bytes after their previews;
the authoritative admission receipt, not the preview, decides who is fresh.
A loser activates no candidate and never installs a hook or displays a secret.
Do not describe this as preventing all concurrent candidate generation.

GitHub installation runs **after** the transaction and outside PostgreSQL using
stdin, never secret-bearing argv. Only a fresh admission may install or manually
display the value. A failure after activation returns sanitized partial state
(`platform_activation`, `secret_rotated`, operation ID when present, and the gh
result), not a false rollback or a fallback secret display. The same key returns
the original acceptance and explicitly does not retry/verify hook installation.
After inspecting state and fixing gh, deliberate recovery is another rotation
with a **new** key. Hook existence is not proof the current HMAC works, a signed
push was admitted, or a deployment completed.

This boundary covers explicit enable/rotation. It is not a claim that every
pre-existing direct-service edit of a webhook secret-name reference verifies
the existence of an active vault namespace. Use the supported activation flow
rather than changing names to invent a configured secret.

All use the shared action registry and transactional River submission. Operators
reach these services through node RPC; the HTTP adapter is not the operator
control channel and refuses a real action whose locked state currently needs
the human factor:

| Kind | Target | Main input |
| --- | --- | --- |
| `platform.app.create` | `platform` | `name`; optional `environments` array (each a complete first revision with its `domain`, `domain_primary`, `domain_addresses` and optional `protected`) and `deploy` to queue the first environment's first deploy after its binding commits. The CLI sends production and staging; the whole array commits atomically. |
| `platform.environment.create` | `app/<app>` | `name`, `node`, optional `build_node`, legacy ports/bind/health, `env`, `secret_refs`; revision `source`, `auto_deploy`, `webhook_secret_name`, `runtime`, `processes`, `config`, `secret_names`; first `domain`, `domain_primary`, `domain_addresses`; optional `deploy` to queue the first deploy after the binding commits. |
| `platform.environment.update` | `env/<app>/<env>` | Partial patch: `node`, `build_node`, `source`, `auto_deploy`, `webhook_secret_name`, `runtime`, `processes`, `config`, `secret_names`; produces a new full revision. |
| `platform.environment.protect` | `env/<app>/<env>` | `protected` boolean; updates persistent protection without a revision. Raising protection is agent-safe; lowering it requires the human factor. |
| `platform.process.scale` | `env/<app>/<env>` | `process`, `replicas`; produces a new revision and, when a serving deployment exists, queues a blue/green `platform.restart` of the serving release in the same transaction. |
| `platform.config.set/unset` | `env/<app>/<env>` | `config` map or `keys` list; produces a new revision and never queues a deploy. |
| `platform.environment.secret.attach` | `env/<app>/<env>` | `name` only; the vault write is separate and no value enters the revision. |
| `platform.release.create` | `env/<app>/<env>` | `source`, optional `build`, `command`, `pull`; records a build row for git/directory sources. |
| `platform.build.stop` | `build/<build ID>` | Cancels the build, marks its operation cancelling, and signals the running pack process. |
| `platform.build.retry` | `build/<build ID>` | New build and release of the same exact commit; it does not deploy. |
| `platform.deploy` | `env/<app>/<env>` | Optional `release_id` and `ref`. Without a release it resolves the current revision's bound source to an exact commit, builds, records the release digest and deploys it with the current revision. With a release it redeploys that release with the current revision. CI and push to deploy submit `ref`. |
| `platform.promote` | `env/<app>/production` | `source_environment`; the app comes from the target. Submission locks the source environment row and production row, resolves and pins the source's current serving release (ready, unpruned, immutable image), requires both environments on the same node and production's explicit-release deployability, and refuses **any** active production deployment or an artifact already serving production. It records a new ready production release with the exact source image/image ID/commit/source but no build row and no command; the ordinary deployment path runs production's revision release command and blue/green switch. No source config, secret, domain or command is copied. Same-key submission replays without re-resolving the source, including the dry-run preview. |
| `platform.rollback` | `env/<app>/<env>` | Optional `to_release_id`; default resolves previous successful serving history, then uses that deployment's revision and exact secret revisions. |
| `platform.restart` | `env/<app>/<env>` | Reapplies the serving release with the current revision through the same blue/green path, so a scaled formation takes effect. |
| `platform.domain.add/remove` | `env/<app>/<env>` | `domain`, optional `primary`, `addresses` (the selected main node's public A/AAAA targets), `check_path` (an absolute HTTP path without control characters), and optional `dns_mode`/`dns_zone`/`dns_origins`/`dns_proxied`. Manual is the default; `cloudflare` requires a configured zone, explicit public origins on add, and an explicit proxy choice on add. The token file never enters input. Removal also requires `confirm` equal to the current hostname; removing the primary is refused. |
| `platform.environment.delete` | `env/<app>/<env>` | `confirm` equal to `<app>/<environment>`; a protected or last environment also needs the transient human factor. The worker preflights and verifies managed DNS cleanup, then removes labeled containers and routes before platform rows. |
| `platform.app.delete` | `app/<app>` | `confirm` equal to `<app>` plus the transient human factor on every real submission; every environment's managed DNS is preflighted and cleaned before workloads, then the environments and app record are deleted last. |

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
by NAME and pinned into that same snapshot on the first deploy attempt; later
attempts of that deployment (River retry or rescued orphaned job) use the pinned
revision. A snapshot written before the credential was pinned keeps its
application pins and gains the credential pin on the next attempt, so a rotation
between attempts cannot change the credential used by that deployment. A
standalone release or build retry has no deployment snapshot and resolves the
active revision on each attempt. The credential is passed only to the askpass
child environment; the value never enters the revision, operation input, events
or logs. The transitional pinned `secret_refs` path is used only when the
revision names no secrets. `platform.app.create` and
`platform.environment.create` with deploy intent queue the first
deploy-from-revision operation in the same submission transaction.

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

`PackStep.Apply` gives each build attempt a dedicated Docker network labeled with
`io.eregion.build.owner=pack` plus the operation, step and attempt, and passes
`--network` to pack so every lifecycle phase container is attributable to that
attempt. Cancellation sends SIGTERM with a five-second grace before SIGKILL so
pack can remove its own containers and volumes, then a bounded deferred cleanup
verifies the network's ownership labels, removes only `author=pack` containers
attached to that network and removes the network. A foreign or pre-existing
network is never touched; an ambiguous `docker network create` error is cleaned
only after the same label check. A retry has a fresh attempt number and therefore
a fresh network. This closes the leak where a cancelled pack container kept the
build-cache volume busy.

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
through local Caddy (retried for up to 2 minutes, as in deploycrate, because a
new domain's site and its first certificate only appear at the switch) ->
remove the previous deployment's containers. A container
removal that fails is recorded in `platform_container_cleanup` and retried;
the verified switch stays successful and the failure is never silently dropped.

Migration 026 adds `eregion_platform_deployment_owners`, one row per
environment. It is the sole route-intent authority: `desiredRoutesQuery` joins
the owner and uses `route_target_deployment_id`, falling back to
`serving_deployment_id` only when no owner row exists. It does not scan running
deployments, order by creation time or infer proof from a running route step.
The migration itself is fail closed and performs no backfill: it refuses while
any deploy/rollback/restart/promote operation is queued, running or cancelling,
or while any deployment row is pending/running. Operators finish or cancel old
work before upgrading; a safe cutover creates an empty owner table and leaves
serving pointers and terminal history unchanged.
The owner is acquired and released while holding `env/<app>/<env>`; it records
the frozen predecessor release/deployment, owner operation/deployment/attempt,
phase and exact restore target. Owner creation and the deployment's
previous-release/deployment binding commit in the same transaction; renewal
reasserts the binding from the frozen owner before effects, so a crash cannot
leave a stale predecessor. After settling a terminal/stale owner, a waiting
operation reloads the environment before reacquiring, because that settlement
may have restored serving to a different deployment. Deploy-family workers use
`pg_try_advisory_lock`; contention returns job snooze, so deferral neither
consumes a River attempt nor holds a pool connection in a blocking wait. Scale
submission uses `pg_try_advisory_xact_lock` on the same env target before its
row lock and returns conflict immediately on contention.

The live deployment records candidate intent (`switching`, route target =
candidate) before route application. Success requires both the completed route
step and completed public step. Recovery for any other state moves the owner to
the frozen restore target, writes that exact deployment's upstreams, updates
serving to the restored target, cleans candidates only after a successful
restore, and then fails/cancels. A failed restore retains both candidates and
the owner. Cancellation before route intent cancels under the owner; cancellation
after intent restores first. The live path checks cancellation after the public
step and after resync, and the success transaction rechecks `cancelling` under
the operation row lock, returning to restoration rather than committing success.
Reconciliation acquires the same owner; if a worker lease is gone before any
route evidence, it cleans and fails rather than resuming a deployment from
maintenance. River exhaustion leaves the operation running with its owner until
the reconcile worker restores the route and records the terminal failure. A
sweep settles terminal/stale owners and ambiguous legacy rows under the same
lock, using the post-restore target rather than a pre-restore environment
snapshot. Previous-container cleanup lookup and pre-record errors propagate:
success is not finalized and ownership/operation remain retryable until the rows
are durable, including the recovered-success path. A successful terminal
operation cannot own new node cleanup steps, so a best-effort post-commit pass
uses the maintenance cleanup identity and leaves failures for the guarded retry.
`retryContainerCleanup` takes the env lock and reads the current serving/owner
rows, protecting the owner's frozen restore target for its whole lifetime and
also the owner's own candidate deployment in every owner phase, including
`prepared`. A partial cleanup-intent insert therefore cannot be removed by
maintenance while a retry may still resume onto those containers. Every early
terminal exit after owner acquisition, including `deploymentPlan` placement or
node lookup failures and pre-step transport-resolution failures, routes through
the deployment failure helper so retained candidates get a durable cleanup row
before the prepared owner is released; recording rows does not require a node
transport. Protected rows
reschedule without incrementing attempts, so they cannot starve later rows;
cleanup record failures retain terminal/ambiguous ownership. A worker's own
`context.Canceled` is not a durable cancellation: it returns without removing
candidates or terminalizing, while an explicit durable `cancelling` state still
runs the cleanup/settlement tail. A nonretryable post-`recordServing` resync
failure restores the frozen predecessor before terminal failure; restoration
failure retains retryable ownership. The owner phase is authoritative:
`restoring`/`restored` never enters the success proof branch, even if the
restore step row is missing after a crash between phase update and step insert.
Live and recovery post-switch route failures use one classified restoration
helper. Cancellation/failure recovery persists candidate cleanup intent before
terminalizing; insert or pinned-revision node lookup failures retain ownership
and the nonterminal operation. The live terminal and post-switch public-failure
paths persist the same intent before settling, and the evidence read for
pre-save reconstruction propagates database errors instead of treating them as
"no containers". When a crash or cancellation precedes
`saveDeploymentInstances`, planned names are reconstructed from
`platform-container-*` step evidence and the pinned revision; a pinned revision
without a readable runtime node fails closed. Retryable pre-switch failures
retain candidates and their completed step rows so the retry resumes without
recreating containers, while terminal and cancellation failures clean up. The
restore step uses the owner's frozen release, reasserts the selected target even
when the step row was already completed, and records/releases only after that
route write succeeds. Previous cleanup
resolves a pinned revision's exact runtime node and fails closed on lookup/empty
node; only an unpinned legacy deployment falls back to the environment node.
Ambiguous recovery propagates deployment lookup errors and retains the ambiguous
owner. Owner release is split: the success finalizer releases a serving owner,
while failure/cancellation releases only a prepared/restored owner whose exact
target is durable. Migration 026 uses no legacy backfill: it fails closed while
any deployment lifecycle operation is in flight or any deployment row is
pending/running, and a safe cutover creates the empty owner table.

Promotion is a separate submission kind, not a cross-environment
`platform.deploy`. `preparePromotionTx` locks the source environment row before
the production row, resolves and pins the source's current serving release at
**submission time** (not when the worker later runs), validates
ready/unpruned/immutable-image and same-node placement, runs the explicit-release
deployability gate against production, and refuses a release whose image is
already serving or while **any** production deployment is active, including a
different artifact. The deployment row and its previous release/deployment are
then recorded in the same transaction. It inserts a new production release with
the source image reference, image ID, commit and source JSON, status `ready`, no
build link and no command, then calls `prepareDeploymentTx` with the deploy
action. The worker follows the ordinary deployment path: no build, production
preflight, production revision release command, production config/secret
resolution and blue/green switch. Success and failure change history use
`promotion.succeeded`/`promotion.failed`, while the deployment action remains
`deploy` so release-command and restart/rollback semantics are the shared ones.
If an ordinary deploy is queued after an accepted promotion and runs first, the
promotion's `deploymentPlan` rebinds its previous release/deployment to that
actual serving predecessor under the target lock; the same applies when the
promotion runs first. Cleanup and rollback therefore follow execution order,
not a stale admission snapshot. `env promote` is the only CLI path; `--release`
on `deploy` still refuses another environment's release. Operation input carries
only app and source environment; the pinned artifact identity is in the new
release row, its image/commit and operation events, so no source secret or
domain can flow into production. A preview passes the supplied idempotency key
and compares the same normalized-input hash as submit: an identical accepted
submission reports the replay, while the same key with changed input returns
`conflict` before any source resolution. `Service.Preview` therefore cannot hide
a changed-input conflict behind a stale preview.

The cleanup warning names the containers and the node's own removal reason
(one redacted, bounded line), so a failed removal does not report only a
generic internal error.
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

Command executable resolution is one contract for release commands and process
containers. For a Buildpacks image a bare command runs through
`/cnb/lifecycle/launcher -- /run/eregion/eregion __eregion_container_exec ...`,
so the launcher completes the buildpack launch environment before the resolver
runs. A plain image uses the same resolver directly as its entrypoint. The
resolver is the running static Linux `eregion` binary bind-mounted read-only at
the fixed `/run/eregion/eregion`; the source is canonicalized from
`os.Executable()`, never from user input, and mounted with
`type=bind,...,readonly=true`. Node argv validation separately accepts a bind
mount only when its source equals that canonical executable, its destination is
the reserved path, and `readonly=true`; other bind mounts are refused. No shell
or external helper is added. The resolver searches the effective launch-time
PATH in order, including relative and empty entries, against the container's
own cwd/filesystem and checks that a candidate is a regular file with execute
permission; it then falls back to the working directory and uses `exec` with the
original argv and environment. It does not rewrite PATH, sort or clean entries,
resolve symlinks lexically, or change the command's environment. Explicit
relative/absolute commands keep exact argv and receive no resolver mount: a
plain image uses the direct entrypoint, while a Buildpacks image still runs the
explicit path through its launcher. A commandless container keeps the image
entrypoint and Cmd. The hidden invocation performs only this search/exec and
cannot access node configuration, the control database, host secrets or any
host path beyond the read-only binary. Its executable must be the node's static
Linux binary matching the container platform/architecture; a dynamically
linked or foreign-architecture binary will fail inside the container. Image
inspection reads only the Buildpacks label, so it is bounded independently of
PATH, image Env size or working directory. Configured and secret PATH values
remain ordinary transient Docker client environment entries (`-e PATH` key
form); no PATH value enters node argv, and the resolver observes exactly the
environment Docker or the CNB launcher gives the container.
A missing image, invalid template, invalid reference or permission failure is
terminal; daemon and transport failures remain retryable. A container step
prepares the image command before removing an existing container, so an
inspection or secret failure leaves that container in place.

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
filtered by `--process`; it does not read node metrics. The workspace Fleet
detail reads one current `metrics.show` snapshot over node RPC and shows
current CPU, memory, filesystem, load and uptime facts; it retains no samples
and does not query PromQL or an external backend.

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
the previous containers. Every writer preserves the saved candidate instances
while the deployment and operation are running and the switch step is running
or completed, until public verification fails or restoration begins. Otherwise
writers use the serving deployment. This prevents periodic reconciliation and
unrelated domain writes from undoing the switch during verification; the
candidate is a routing target, not yet a verified serving deployment. A periodic maintenance job re-renders on worker start
and every 30 seconds after a failure; it updates `route_state` per domain. A
second periodic job observes DNS through 1.1.1.1 and 8.8.8.8 over UDP, then the
certificate chain/expiry and public response through local Caddy with the real
SNI. Wrong DNS stays unready. A Cloudflare-proxied record (deploycrate's
Cloudflare mode) counts as reaching the node: the certificate and public checks
still run against local Caddy, so readiness means the origin serves the host. `domain add` records the main node's
public addresses so observation has an expected value. Inventory `domains` and
`domain list` expose each state separately.

Explicit Cloudflare DNS is a separate provider effect under the same domain
operation. `platform.DomainInput` carries only `dns_mode`, `dns_zone`, explicit
`dns_origins` and the explicit proxy choice; the credential remains in the
encrypted provider scope of the PostgreSQL vault. Origins are validated public addresses supplied by the
operator, never derived from the workspace access record's WireGuard endpoint. Public
origin validation rejects private, RFC1918, CGNAT (`100.64/10`), loopback,
link-local, multicast, unspecified, documentation/benchmarking and other
non-public ranges for IPv4 and IPv6; IPv6 origins must also be in the global
`2000::/3` unicast range. The node executes the effect through
`integrations.DomainDNS`, which resolves the configured zone and current
credential from PostgreSQL for each operation, lists only the exact hostname, and writes only records whose comment
equals the immutable binding marker (`DomainDNSOwnership(domain_id, zone_id)`).
The marker uses the stable `dom_…` binding ID, not app/environment/hostname
names, so deleting and re-creating a hostname cannot adopt the previous
incarnation's record. Admission validates that the configured zone actually
contains the hostname before writing any operation/ledger intent; an invalid
pending/failed ledger with no applied generation can be discarded during
removal without a provider call, while an invalid applied ledger fails closed.
A plan applies creates/updates before obsolete deletions, then re-reads and
verifies the complete desired A/AAAA set before deleting any obsolete owned
record. An A-to-AAAA or AAAA-to-A replacement therefore keeps the old address
until the replacement exists; a failed or cancelled replacement create leaves
the old record intact for an idempotent retry.

### Human-only Cloudflare credential setup

Use the operator CLI in a human terminal; no server file or environment editing
is needed:

```sh
eregion --workspace thelab domain cloudflare configure --zone ZONE_ID --zone-name mbvlabs.com
eregion --workspace thelab domain cloudflare status --json
```

Zone fields also prompt when omitted. The token is accepted only by a no-echo
terminal prompt, never a flag, environment variable, file or stdin fallback.
Configuration refuses JSON/NDJSON/no-TTY before token entry or RPC; metadata-only
status is agent-safe. No new TOTP requirement is added for secret writes. This
is a supported-workflow gate, not human attestation against an OS-account holder.

The dedicated AppService/node RPC setup path sends the token transiently in
request secrets, not platform operation input or River arguments. Bounded
read-only DNS listing validates the exact zone before preview/activation. The
human confirms the observed current revision; replacement is CAS-fenced.
Metadata, immutable ciphertext and sanitized audit attribution commit together.
Definite rollback leaves the old credential active; an ambiguous commit error
instructs the operator to inspect status before retrying rather than claiming
the old revision is unchanged. A successful setup is **read-validated**, not
proof of DNS-write capability or public readiness.

The existing vault owns typed provider/zone metadata and revision tables,
encrypted with the existing keyring and provider-specific integrity binding.
Generic application-secret APIs cannot address this scope. Previews and workers
resolve the current credential freshly without service restart; rotation uses
the same configure command. The old Cloudflare token-file/zone environment
configuration is removed with no fallback or automatic import. Existing managed
ledgers remain intact and require human configuration of their zone credential.

Domain DNS authorization permits validated operator actors for only `dns.read`,
`dns.write`, `dns.create`, `dns.update` and `dns.delete`; CI/webhook/system actors
and unknown actions are refused. It does not depend on `EREGION_DNS_ADMINS` or
private-access administrator configuration. Workers retain the submitting actor.
The durable PostgreSQL audit sink must still record every intent and result.
Provider previews authorize `dns.read` before token
access; admission authorizes `dns.write` before any ledger/operation mutation,
and the worker rechecks `dns.write` before token access and each specific
create/update/delete action before its mutation. The provider plan invokes the
attempt/cancellation guard immediately before each A/AAAA mutation. Foreign
A/AAAA records and legacy `eregion:managed` records are conflicts; any CNAME is
a conflict even when its comment exactly matches the binding marker, because the
managed policy is A/AAAA only. None are adopted, updated or deleted. Non-managed
TXT/CNAME content, comments, TTL and proxy state are redacted from preview
output.
The `eregion_platform_domain_dns` ledger is written in the same transaction as
the operation and River job, before any provider effect. It persists provider,
zone, marker, generation, desired public origins/proxy, state, last operation
and cleanup status. Adds apply from the ledger, never from transient operation
input; a successful apply marks the applied generation. Domain admission locks
the app and environment rows and refuses while another domain change or
app/environment deletion is queued, running or cancelling. The worker holds the
same ordered target lock, verifies the operation is still the newest domain
intent for the target, and the ledger's `last_operation_id` fences the exact
intent; a later readd, promotion or file update cannot be deleted by a delayed
removal. Managed re-add/promotion preview and result use the persisted zone,
origins, proxy and state rather than a false manual input. Adds apply before the
route write. The worker's claimed attempt is passed explicitly through domain
and environment/application deletion effects, so a newer attempt loaded from the
operation snapshot is never accepted as the worker's own. An attempt-fenced
cancellation check runs before and after every DNS provider effect and before
each mutation in a multi-record plan. A stopped guard leaves earlier convergent
A/AAAA changes for an idempotent retry and does not turn cleanup into a failure.
Cancellation is honored at safe boundaries after provider effects and before
binding deletion; binding/domain row deletion and finalization recheck
cancellation under the operation row lock, and completed effects are not
reported as success. Removal
and app/environment deletion mark the ledger `cleanup_pending` and verify
provider cleanup before removing serving workload containers. Deletion first
preflights every managed binding: the operation-ownership fence, authorization,
zone/host recovery, a real provider read and exact ownership/conflict checks. An
obvious refusal therefore stops before the first provider mutation. After that
point Cloudflare effects are not atomic: a later binding's read/delete failure or
a cancellation can leave earlier bindings cleaned while later bindings are
unchanged or partially changed. The worker removes only exact-owned provider
records, re-reads the exact hostname and marks `cleanup_verified` only when no
exact-owned A/AAAA remains and no conflict appeared. A read failure, reappeared
owned record or conflict leaves the ledger retryable, refuses deletion without
touching workloads, and records how many bindings the pass processed. It never
claims all records were preserved or that public availability was maintained.
Retry re-plans the remaining bindings and converges the cleanup. After verification
the workload and route deletion continues; the ledger and domain rows are
deleted only after the workload step succeeds. The primary binding is cleaned through
environment/application deletion (or after promoting another primary for an
explicit domain removal). Ownership is re-read immediately before each provider
mutation. Cloudflare has no conditional-write/CAS API, so a concurrent external
writer can still race the final request; the plan says so and the next
reconciliation reports the resulting conflict. A dry run of an explicit
managed-DNS change authorizes read access, then calls the same read-only planner
and returns a real provider snapshot with the explicit origins or the persisted
ledger. A manual re-add/promotion preview reports the persisted managed state
without a provider read or change. Provider success does not update `dns_state`;
the resolver, Caddy route, certificate and public response remain separate
observations.

A third periodic maintenance job
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

## Managed resources versus deploycrate-ce

Checked against `internal/platform/resource*.go`,
`internal/controlplane/resource.go`, `internal/cli/resource.go` and
`../deploycrate-ce`'s `services/resource_management.go`,
`services/resource_health.go`, `models/resource_engine.go` and the other
`models/resource*.go`, `models/environment_resource.go`,
`services/resource_caddy_route.go`, `services/resource_dns.go`,
`services/resource_private_access.go` and its backup services
(`services/backup_scheduler.go`, `services/database_backups.go`,
`services/database_restore_workflow.go`) on 2026-11-07. This is current
behavior, not a plan for missing features. In short, eregion alpha.2 ships one
generic Docker container per declaration, while deploycrate ships an
engine-aware resource subsystem with a typed catalog and managed service
concerns.

| Concern | eregion alpha.2 | deploycrate-ce |
| --- | --- | --- |
| Shape | One environment-owned `eregion_platform_resources` row and one generic Docker container (`er-res-<id>`) on the environment's private bridge network, with the resource name as its alias. There are no installation, endpoint, credential, volume or publication sub-entities, and a resource belongs to exactly one environment. | A `resources` entity with separate installations, endpoints, credentials, volumes/mounts and health checks; an `environment_resources` binding gives one environment an alias, endpoint and optional credential, and one resource can be bound to several environments. |
| Engine awareness | No engine catalog. The declaration is an image plus optional command, plain environment, vault secret-name environment and named-volume mounts; validation is generic. | `models/resource_engine.go` defines postgresql, mysql, redis, clickhouse, registry, opentelemetry, http and tcp, each with a resource type (database/cache/service), protocols, endpoint roles, TLS modes, credential fields, environment-key projections, health-check kinds and default port/protocol/TLS. |
| Exposure | No host-published port. Application containers and resources share the per-environment private network, so the alias resolves to the resource container; `port` is declaration/display metadata and is not published, routed or projected. No resource Caddy route, DNS record, TLS or WireGuard private-access endpoint. | Docker host-port mapping plus `resource_endpoints` (primary/wireguard/local), optional managed Caddy publication and Cloudflare DNS, and a WireGuard device-grant private-access path. |
| Credentials | Only vault secret NAMES in `secret_env`; values are resolved from the environment namespace and passed transiently to `docker run` during apply. The secret revision fingerprint contributes to the container configuration hash, so a rotation takes effect on the next explicit apply. No credential entity, engine administrator/application roles, or credential projection to the application. | Encrypted, engine-validated resource credentials with administrator/application purpose, rotation and dependency checks, plus PostgreSQL role/password reconciliation. Connection values are projected into environment secrets (`*_HOST`, `*_PORT`, `*_USER`, `*_PASSWORD`, optional `*_URL`) with per-binding key overrides. |
| Health | One generic `docker exec` probe during apply (`timeout_seconds`, `interval_ms`), recording `ready`/`degraded` and a health state. No periodic health job, no engine client and no container repair; an out-of-band container loss is not re-observed until an explicit apply or destroy. | A five-second periodic sweep selects due checks: tcp/http/mysql/redis/postgresql/clickhouse probes with per-check interval, timeout and success/failure thresholds, persisted unknown/healthy/degraded/unhealthy state, latency and message. |
| Reconciliation and deploy gate | Apply and destroy are journaled operation steps with retries and cancellation boundaries. The maintenance reconciler only settles rows left in `provisioning`/`destroying` by a terminal operation; it does not repair containers or re-probe health. A deploy is refused while any attached resource row is not `ready` and `healthy`. | `DeployResource` runs active installations, and installation observation records Docker state/health with a 30-second expiry. The deploy path attaches the resource container to the environment network and projects connection values; it does not read resource health status as a gate. |
| Data | Deterministic `er-res-<id>-<mount>` named volumes, a cumulative ownership ledger and label checks before deletion. Destroy retains data by default; `--delete-data` deletes only that resource's owned volumes. Host bind mounts, multiple containers per resource and node moves are not supported. | Separate volume and mount entities, multiple mounts, and archive dependency checks. |
| Engine management and backups | No database create/drop, engine administrator management, backup destination, policy, schedule, verification or restore. The resource is the image, its container and its data; detach and default destroy retain that data. Control-database recovery remains the PostgreSQL provider's job (D4), and D30 retention is artifact pruning, not backup or resource-data management. | PostgreSQL resource database create/drop and database definitions; server filesystem (restic) and resource PostgreSQL logical (`postgresql`/`tar.age`) backup policies with schedules, retention, verification and restore workflows, plus a ClickHouse backup path. |

The practical alpha.2 consequence is that a managed resource is an explicitly
declared container, not a managed service catalog. The application gets no
engine-derived variables and no automatic credential projection or rotation; it
must use the resource's private name and port through its own `config` and
`secret` entries. The same image in two environments is two independent
resources with separate data; neither can move nodes, run more than one
container or be exposed outside the environment network. Health and data are
driven by explicit resource operations, not by background convergence; a
cancelled or failed operation only settles the row to `degraded`.

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
  deployability, domains, releases, builds, deployments, changes and resources;
  the internal DNS/access extensions add their own. The removed backups and
  telemetry collections are not registered.
- `/v1/secrets…`: separate authenticated credential API; values are encrypted
  directly, never placed in ordinary action input.
- `/v1/environments/<app>/<env>/deployments`: the public CI deploy endpoint.
  `POST {"ref":"…"}` authenticates a deploy-scoped Bearer token (the grant is
  exactly `platform.deploy` on that environment), submits `platform.deploy`
  with `ref`, and returns the operation and its status URL. `GET` the returned
  operation only; expired, revoked, wrong-environment and wrong-action tokens
  fail. No other `/v1` route is public. The public status read is
  `GET /v1/environments/<app>/<env>/deployments/<operation>`; the internal
  `/v1/operations` route is not exposed through Caddy. The status handler
  authenticates `operations:read` against the environment and then reads the
  operation with the token actor's authority, so a token can read only
  operations submitted by its own actor.
- `/v1/webhooks/github/<app>/<env>`: per-environment GitHub push delivery. The
  current revision must have auto-deploy on and a `webhook_secret_name`; the
  HMAC secret is read fresh per delivery from that environment's vault
  namespace in one joined environment/revision/secret-ciphertext read, and the
  signature is checked over the raw body before parsing. The repository and
  bound branch must match. One PostgreSQL transaction stores a sanitized push
  intent and its River webhook job; the handler replies 202 only after that
  commit. One absolute 8-second admission deadline bounds the fresh resolver,
  transport body read and database work, so a slow client or database returns a
  retryable 503 instead of waiting for the server timeout. The webhook worker
  then submits `platform.deploy` with the pushed commit SHA and links the
  operation to the delivery in the same transaction as the operation, platform
  state and River platform job. A redelivery returns the original operation; a
  receipt means accepted, not completed. If the environment was deleted or its
  repository/branch binding changed before the worker submits, the admission
  fails terminally rather than deploying against the new binding. An old pending
  row whose operation is gone is reported as ambiguous and is never blindly
  resubmitted. The old server-wide secret file, allowlist and release-only
  webhook are removed. The explicit-DSN PostgreSQL integration cases for this
  path pass against PostgreSQL 17.11 (2026-11-05); host latency, first delivery,
  redelivery and rotation remain manual checks.
- `/private/…`: optional authenticated subject/device-grant proxy, not a VPN
  enrollment implementation.

The `host-ingress-v2` bootstrap step pins Caddy 2.11.4 on amd64 and arm64,
installing its official Debian package only after verifying the embedded SHA-256.
Checks require the exact version; package installation preserves existing
configuration and app routes, and apply validates the configuration and restarts
Caddy so the new executable is active. Fresh-host installation is a manual
acceptance area, distinct from an already-upgraded host passing Check; see
MANUAL-TESTING.
The preceding `host-ingress-sysctl-v1` step writes
`/etc/sysctl.d/60-eregion-ingress.conf` and sets `net.ipv4.tcp_migrate_req=1`
in the running kernel. Caddy binds a new `SO_REUSEPORT` listener on every config
load and closes the old one; without migration the kernel resets connections
queued on the closed socket, failing in-flight TLS handshakes at each D17
switch. It is a separate step so enabling it neither reinstalls nor restarts
Caddy.
Caddy is generated by the bootstrap ingress step and exposes only `/readyz`,
`/v1/environments/*/*/deployments[/*]` and `/v1/webhooks/github/*/*` on the main
hostname; each `*` in an inner Caddy wildcard is one path segment. Do not
expose all of `/v1` as a shortcut. Main-node application
hostnames are separate Caddy sites.

The operator `Backend` (`internal/cli`) and HTTP handlers share the
`api.AppService` functions for action submission/preview, inventory and vault
operations. New guided commands must use node RPC and share those services, not
enqueue directly or build a second HTTP path. Extensions register
strict normalizers/executors and inventory providers before runtime startup through
`controlplane.RegisterExtensions`; no extension gets a second queue. Registry
availability alone is not readiness: missing host/key/provider configuration
can leave advertised actions deliberately failing closed.
