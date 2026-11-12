# eregion: deploycrate-ce, shaped like vaire

> Master document: product, decisions, model, command tree, the v1 alpha/beta
> scope and product contracts. [Linear](https://linear.app/mbvlabs/project/eregion-2ff1d2e4d199)
> owns actionable work and execution status. [IMPLEMENTATION](IMPLEMENTATION.md)
> describes current mechanisms; [MANUAL-TESTING](MANUAL-TESTING.md) holds
> acceptance checks and run evidence. Historical work/status records below are
> evidence, not a second active work list.
> Rewritten 2026-10-26 from the alignment spec (D1–D7) after a re-read of
> `../deploycrate-ce` and `../vaire`; D8–D28 settled in review the same day.

## 1. Goal

**One glorious CLI** that gives you what **deploycrate-ce** does — deploy and
operate applications on servers you own — the way **vaire** delivers
PostgreSQL. One signed Go binary, `eregion`, takes a fresh Debian or Ubuntu
VPS, hardens it, connects the operator over WireGuard, and from then on
creates, configures, builds, deploys, routes and observes applications. It is
the only interface: no web UI, and the TUI is a rendering of the same commands.

- **Functional requirements:** deploycrate-ce (`services/application_setup.go`,
  `environment_setup.go`, `release_deployment.go`, `deployment_execution.go`,
  `caddy_route.go`, `environment_dns.go`, `environment_secrets.go`,
  `workload_reconciliation.go`, `CONSTRAINTS.md`, `controllers/api/api.go`).
  Where this document is silent, deploycrate's behavior is the requirement.
- **Shape, flows and conventions:** vaire (`docs/MASTER.md`, `AGENTS.md`,
  `docs/PLAN-v1-release.md`, `internal/cli`, `internal/transport`).

The CLI is **agent-first**: every command is non-interactive when asked,
returns structured JSON, has documented exit codes and dry-run support, and
protects destructive operations with target-naming confirmation. Pointing an
agent at `eregion` is a supported way to operate a server.

The first goal is the **main node**: one VPS that runs the control plane, the
River workers, builds, the registry, Caddy and the applications. Additional
nodes and HA come later, but early decisions must not block them. Telemetry is
node-local in beta; a separate optional telemetry node is a later option, not a
beta deliverable.

### Never

- Kubernetes, Docker Compose deploys or Windows servers. Workloads follow
  deploycrate's design: directly managed Docker containers on Debian or Ubuntu
  hosts (D10).
- A web UI.
- Cloud provisioning. The user brings the server.
- Managing the external control-plane PostgreSQL. Application dependencies may
  instead be external services or optional managed Docker resources (D19).

### Not before v1 final

Multi-tenant hosting and HA; everything listed in section 7 as after v1.

## 2. Design principles

- **One binary, two roles.** The same `eregion` runs on the operator's machine
  (**operator mode**) and on each server (**node mode**). Operator mode drives
  node mode over SSH. Host mutations are small, idempotent bash scripts embedded
  with `go:embed`, as in vaire and `deploycrate-ce/internal/setup`.
- **SSH through WireGuard is the operator's only control channel.** After
  bootstrap sshd listens only on WireGuard. There is no public admin API and no
  operator API token. The one public HTTP surface is the narrow CI deploy
  endpoint (D1, D7).
- **PostgreSQL holds desired state, River executes it.** Unlike vaire's TOML
  file, platform state (apps, environments, releases, deployments, domains,
  secrets) lives in the external PostgreSQL, and every change is an operation
  committed together with its River job. No second queue, no SQLite, no TOML
  store for platform entities.
- **Versioned, journaled host steps.** Each host step has a `Check`/`Apply` pair
  and a versioned ID. Upgrading eregion re-runs only the steps that changed.
- **Desired, applied, observed are different.** A saved binding is not a route;
  an accepted submission is not a completed deployment; a route is not a valid
  certificate or a healthy app. Commands report each separately.
- **Secrets are generated where possible, shown once, never in operation input,
  River arguments, events, journals or ordinary output.** Handoff and explicit
  exports are the protected exceptions.
- **Agent-first, human-friendly.** Interactive prompts are a thin layer over
  flags. Guided commands follow `server init`: defaults, clear questions, a plan,
  confirmation, live progress, a result card.

## 3. Decisions

| # | Topic | Decision |
| --- | --- | --- |
| D1 | Control model | The CLI talks to a node through SSH over embedded userspace WireGuard, always. Each operator command resolves the selected workspace, opens SSH in WireGuard and runs `eregion node <cmd> --json` there, which calls the same application services the API uses. Local workspace connection data holds a WireGuard identity and an SSH key, not a bearer token. |
| D2 | Hardening | vaire's: admin user with a sudo password, root login and SSH passwords off, firewall, commit-confirm switchover of SSH to WireGuard with automatic revert, break-glass via the provider console and `eregion node unlock-ssh`. |
| D3 | Ingress | Caddy on the main node. The main hostname is used exactly as entered. App domains are environment bindings. DNS records, direct or Cloudflare-proxied, and ACME. |
| D4 | Recovery and backups | Revised 2026-11-10: serious durable application state belongs in external services; managed local-volume data is expendable under the current policy. There is no scheduled backup or volume-backup product. External PostgreSQL holds control-plane state and operation history, but is **not** a backup of Docker volumes, local registry artifacts or host-local vault keys and identities. Before RC, document replacement-VPS recovery with the same surviving control DB/external services, including protected key/identity recovery, artifact availability and unsupported replacement-node gaps. A live recovery rehearsal is not a release gate; documentation is not proof of working recovery. External-provider backup and durability remain the provider/operator's responsibility. |
| D5 | Telemetry (beta) | Follow vaire's exporter-and-scrape-configuration model. The main node runs loopback-only node_exporter; its existing HTTPS hostname exposes exactly `/metrics/node` and `/metrics/targets.json`, both authenticated through a separate loopback gateway. Discovery advertises reachable HTTPS targets, and generated Prometheus/Alloy configurations authenticate discovery and scraping with normal TLS verification. Raw exporter/gateway ports are not public. `metrics show` provides a bounded current snapshot with checked CPU deltas, freshness and explicit missing values; it does not define or restrict the external collection model. `metrics endpoint` distinguishes desired configuration from observed readiness. `metrics config` uses placeholders in ordinary output and owner-only `--out` files for actual credentials. Rotation retains vaire's simple model: save the generated password locally before sending only its bcrypt hash, restore the local credential on failed RPC, and acknowledge that a lost successful response may require another rotation. Normal apply must preserve the existing credential, serialize with rotation, and restore all managed files/service state on failure with bounded recovery independent of caller cancellation. Telemetry failure must not prevent serving applications. Keep the exporter textfile directory and parent root-owned 0755, nonsymlink and without special mode bits; restrict the systemd collector to eregion, Caddy, Docker, exporter and WireGuard units, and require real producers for self-metrics. Doctor retains fail-closed collector/scrape checks and distinguishes anonymous denial from authenticated success. Operation history remains in PostgreSQL and never depends on telemetry. No managed Prometheus/Grafana/Alloy/Loki deployment or retained metrics/log store is required for beta/v1. Retain the future optional telemetry-node concept described below. |

| D6 | Command tree | [Section 5](#5-command-tree). `platform …`, operator-facing `bootstrap …`, `serve`/`worker`, `platform submit` and bearer-token `context login` are removed from the operator surface; the service, migration and bootstrap entry points live under the hidden Linux-only `node` group. The old resource surface was removed, but alpha.2 adds a new node-mode resource lifecycle under D19. Pre-v1: delete old paths, no aliases or compatibility shims. |
| D7 | Tokens and the human factor | API tokens are per use, scoped, expiring, listed and revocable, and shown once. Creating one needs a human factor (a TOTP code from `eregion protect init`, or a master password) read without echo at a terminal, so an agent cannot mint its own. No token is minted at setup or handed off. The same factor gates **token create, secret export (`secret export`, `secrets export`), every `app delete`, a protected `env delete`, deletion of the last environment (which deletes the app), and `env unprotect`**. An unprotected non-last environment deletion needs only exact target confirmation (D33). The control plane verifies the code inside the submission transaction that locks the target state; a separate pre-check is UX only. `--yes`, `--json`, stdin and pipes never satisfy it. The factor stops an agent operating through `eregion` from reading secrets, minting tokens or deleting apps; it is not a sandbox: an OS account holding a workspace's local connection files (SSH key, sudo password) is root-equivalent on that server. |
| D8 | Hosts and platforms | Servers: Debian 13 and Ubuntu 26.04, amd64 and arm64. Operator CLI: Linux and macOS; no Windows builds. Operator mode pushes the matching verified Linux binary to the node. |
| D9 | Control database | External PostgreSQL 17+ with `sslmode=verify-full`, never installed on the app server. Its DSN is a server secret and goes in the handoff. |
| D10 | Workloads | deploycrate's: directly managed Docker containers identified by ownership labels, images by immutable digest, restart `unless-stopped`, ports published only on dynamic loopback ports (or the node's WireGuard address). Docker Compose and per-environment systemd units are never state stores. |
| D11 | Builds | Cloud Native Buildpacks with a pinned `pack` and architecture-pinned default Paketo builder, Go apps first. The CLI/TOML user owns explicit buildpack order; Eregion does not inject a Go/Node sequence or custom assets buildpack. With no explicit order, Pack uses its normal source-descriptor/builder detection. Empty builder configuration selects the default on the build node; explicit builders are honored unchanged. Optional typed build inputs map to standard buildpack settings, not runtime selection or a guarantee that frontend scripts run. Builds run on the main node for v1. Finished Buildpacks application images belong in the existing private node registry, with immutable artifact identity; Docker-local copies are runtime materialization, not the only retained artifact. Reuse D30 retention to protect serving and rollback artifacts and clean only eligible owned data. Temporary build cleanup must not remove retained artifacts, broaden registry exposure or switch builds to host networking. This does not add off-server backup or mirroring of arbitrary external image sources. |
| D12 | Confirmation | Destructive commands take a token that names the target (`--confirm <app>`, `--confirm <app>/<env>`, `--confirm <server>`); `--yes` alone never suffices. The control plane validates the token against current state. `app delete` always needs the D7 factor; `env delete` needs it for a protected or last environment and otherwise needs only the exact target token (D33). Their dry run needs neither. |
| D13 | Deletion | deploycrate's policy: deleting an app removes every environment, source, domain, secret, release, deployment and route it owns after workload cleanup succeeds; deleting the last environment deletes the app and therefore needs the app-deletion human factor even when that environment is unprotected (D33). Durable operation history is kept. |
| D14 | Release model | `master` is the combined development and edge stream and publishes on every push; `1.x` is the whole v1 line. Keep release history linear: squash feature PRs into `master`, then fast-forward `1.x` to the exact reviewed `master` commit for a release. Do not squash a second release PR or merge the release line back into `master`; those duplicate changes and create a merge ladder. A non-fast-forward release promotion stops for explicit resolution, never an automatic force push or cherry-pick. Alpha and beta tags publish exact immutable snapshots without a moving channel; RC tags advance an opt-in `rc` channel; stable channels advance only on final tags. Manifest `channel` is the signing family, not a promotion channel: every tagged artifact, including alpha/beta/RC, says `stable` and is signed by `publish-stable.yml@refs/tags/v<version>`; edge says `edge`. Stable and major-line pointers accept only final versions and the rc pointer accepts only rc versions; the build, upload and promotion paths enforce this independently. An RC client also checks its stable major line for a newer final, without automatically moving the RC pointer. Existing release tags and signed snapshots remain immutable. |
| D15 | Release signing | vaire's pipeline and scripts: cosign keyless signatures bound to the pinned workflow identity, `install.sh` per channel, R2 channel manifests. Replaces the Ed25519 manifests. |
| D16 | Order of work | Operator channel first, then the end-to-end app workflow. Linear tracks specific work and execution order; repository docs retain product contracts and acceptance. New app commands are node-mode commands from the start, never built on the token adapter. |
| D17 | Traffic switch | deploycrate's blue/green: the candidate runs on a dynamic loopback port, passes health, Caddy switches its backends, the public response is verified, then the previous containers stop. A failed candidate leaves the old release serving. Public verification requests the primary domain at its check path (D29) and records the host, path and status it observed. Failure to remove a previous or candidate container never fails a verified switch, but is recorded and retried, never silently dropped. Deployment traffic ownership is durable per environment (migration 026, `eregion_platform_deployment_owners`). One deploy, rollback, restart or promotion owner is acquired and released under the environment advisory lock and held across retries, cancellation and restoration until terminal traffic is safe. Other deploy jobs try the same advisory target lock without blocking, then snooze via River without consuming a failure attempt or holding a pool connection. Scale submission takes a nonblocking transaction lock and returns conflict while an owner or another lifecycle operation holds it. Generic route writers select only the owner's `route_target_deployment_id`; without an owner they use `serving_deployment_id`. They never elect a candidate by deployment creation timestamp and never treat a running route step as proof that the apply happened. The owner freezes the predecessor release and deployment at acquisition, and the owner row plus the deployment's previous binding commit in one transaction. Renewal reasserts that frozen binding before any effect, so a crash cannot leave stale predecessor pointers for cleanup or rollback history. When a waiting operation settles a stale/terminal owner first, it reloads the environment before acquiring its own owner, so a restore that changed serving to O cannot be overridden by the pre-settlement snapshot naming A. Candidate route intent is persisted before the switch; only a completed route step plus completed public verification finishes success. A running or failed route step, missing verification, failed public check, cancellation, exhausted attempt or terminal/stale owner restores the exact frozen target before candidate cleanup; if restoration fails, candidates and ownership remain and the operation stays retryable. Cancellation accepted before the success transaction wins and restores the predecessor; the success finalizer rechecks `cancelling` under the operation row lock so success cannot be committed first. Serving and cleanup guards, including container-cleanup retry, read the authoritative current serving/owner rows rather than a stale attempt snapshot, and the frozen restore deployment stays protected for the whole owner lifetime. The owner's own candidate is also protected from maintenance cleanup while its owner lives, including the prepared phase, so partial cleanup intent cannot delete containers a retry still needs. Every terminal exit after owner acquisition persists candidate cleanup intent before settling and releasing a prepared/restored owner, including pre-plan placement or transport-resolution failures, and persistence needs no node transport. Protected cleanup is recorded durably, deferred rows advance their next attempt, and terminal/ambiguous ownership is not released until candidate cleanup rows are durable. Previous-cleanup pre-record or lookup failures keep the deployment nonterminal with its owner until durable; repository unavailability cannot turn into a successful deploy with lost cleanup. A nonretryable post-switch route resync failure restores the frozen predecessor before terminal failure, and a failed restoration retains retryable ownership. Owner release is split: only the actual success finalizer releases a serving owner, while failure/cancellation releases only an owner whose exact restore target is durable. The owner phase is authoritative: a `restoring`/`restored` owner never resumes a completed switch as success even when the restore step was not yet inserted. Recovery post-switch route failures and the live path share the same classified restoration logic. Cancellation/failure persists candidate cleanup intent before terminalizing; a cleanup-intent insert or pinned-revision node lookup failure keeps ownership and the operation nonterminal. A live terminal or post-switch public failure follows the same rule, and retryable pre-switch failures retain candidates and their completed steps so a retry resumes without recreating containers. When interruption precedes `saveDeploymentInstances`, planned candidate names are reconstructed from container-step evidence and the pinned revision before intent is recorded; an evidence-read failure is retryable and retains ownership. A worker context cancellation is not a durable cancellation: it retains candidates and leaves the operation for retry, while an explicit `cancelling` operation still cleans and settles. The restore step uses the owner's frozen `{environment,release}` and always
reasserts the selected route target before recording or releasing. Ambiguous
recovery propagates deployment lookup errors and keeps the ambiguous owner until
every candidate is readable. Migration 026 is a fail-closed cutover with no
legacy owner backfill: it refuses to apply while any deployment lifecycle
operation is in flight or any deployment row is pending/running. Finish or
cancel old work first; a safe upgrade creates the empty owner table. Live retry,
reconciliation, River exhaustion and terminal-owner sweep follow the same rules.
A live retry before any route intent may resume and rebind to the frozen
predecessor; reconciliation without a worker lease fails it. |
| D18 | Secret resolution | deploycrate's: the environment revision names its secrets; each deploy resolves their current values and records the exact secret revisions on the deployment, so a rollback reproduces them. No hand-pinned `vault:…@N` refs. |
| D19 | Application resources | Alpha.2 adds optional, generic Docker resources owned by an environment; an application may instead use external dependencies through secrets. This supersedes the earlier exclusion of managed application databases: a PostgreSQL container is an app resource, never the external control-plane database. Resources use private networking and optional persistent volumes, secret names rather than values in definitions, and retain data when detached; deleting data requires a separate `resource destroy --delete-data` naming the exact resource, which removes only that resource's owned volumes. No backup product is implied, and the external control-plane PostgreSQL is not a backup of these volumes. |
| D20 | Sources | An environment has an **optional** HTTPS Git build recipe (public, or private with an environment read credential): URL, ref, Buildpacks context/builder/settings and push-to-deploy (D28). Git absence is explicit: there is no persistent image source, kind discriminator or image mode in a revision. An app owns one immutable private-workspace image repository (`image_repository`, defaulting to the app slug), which builds and uploads use and environments cannot change. A deploy selects its artifact explicitly with `--image-tag`, `--image-digest`, `--image` (fully qualified public reference) or an existing `--release`; with no selector it builds the configured Git ref. Artifacts are pinned to an immutable manifest and expected config digest at admission. No GitHub App integration is planned (decided 2026-11-04): a per-environment read credential plus the D28 webhook covers private repositories and push to deploy with less setup. |
| D21 | Processes | deploycrate's: an environment declares processes of kind `web` (receives the domains through Caddy), `worker` (no port), `service` (port reachable privately, no domain) and a `release` command, each with a command, arguments and replicas. Caddy balances a `web` process across its replicas. The release command (typically migrations) is **release-scoped**, as in deploycrate: it runs once per release and command, before the release fans out to its deployments (and, after v1, to several nodes); a successful run is recorded, and `restart`, `process scale` and `rollback` to that release never run it again. A failed run blocks that release until a new `deploy`. A forced re-run is a possible later override, not alpha scope. A bare process command runs through the same static Linux `eregion` binary bind-mounted read-only at a fixed container path and execs after searching the effective launch-time PATH (including relative/empty entries, then cwd) with the target container's cwd, filesystem and permissions; the Buildpacks launcher completes its environment first, a plain image uses the resolver directly, and no shell or PATH rewrite is used. Explicit paths keep exact argv (plain image directly, Buildpacks through its launcher); commandless containers keep the native entrypoint. None receives the resolver mount. A configured or secret-valued PATH is applied only through the transient Docker client environment, never argv. |
| D22 | Access modes | Public only in the alpha; basic auth and private-network access after v1. |
| D23 | Promotion | A dedicated node-mode **`env promote <app> <source-environment>`** moves the source environment's current serving release into the application's `production` environment. Production is the only target; there is no general cross-environment deploy. The command pins the source release's exact image digest, commit and source as a new `ready` production release **without rebuilding** and without copying source plain config, secret names/values, domains or release command. The source serving release is resolved and pinned inside the **submission transaction, not at worker execution**, so a later staging deploy cannot change the artifact. The ordinary deploy path then applies production's current revision, config, secrets, domain and release command through the same release-command and blue/green health/Caddy/public-verification/cleanup semantics. The source must have a ready, unpruned serving release; source and production must be environments of the same app on the same node (cross-node artifact transfer is not implemented); production must pass the explicit-release deployability gate, including a primary domain. Promotion admission refuses **any** active production deployment, including a different artifact, under the production row lock; this keeps previous-release cleanup and rollback history unambiguous. Ordinary deploys queued after a promotion rebind their previous release/deployment at execution under the target lock, so whichever deployment runs next chains to the actual serving predecessor. Preview is read-only and honors the supplied idempotency key with the same normalized-input hash fence as submit: identical input replays, changed input conflicts. JSON/no-TTY never prompts. The same idempotency key replays the accepted operation without re-resolving the source; a new key is new work. An artifact already serving in production is refused rather than promoted twice. `deploy --release` continues to accept only a release from the same app and environment; it is not a cross-environment workaround. Durable production release/change history and operation events distinguish promotion from a rebuild deploy. |
| D24 | `server init --local` | After v1; remote bootstrap only in the alpha. |
| D25 | Logs | `logs` reads container logs over the operator channel and works without any telemetry service. |
| D26 | Nodes and DNS automation | Cloudflare DNS automation is required before beta and extends the existing domain flow. Manual DNS stays the default. `domain add … --dns cloudflare --dns-zone ZONE --dns-origin ADDRESS --dns-proxy dns-only\|proxied` plans or applies only A/AAAA records for that exact hostname; origins are explicit public addresses and never derived from the workspace's WireGuard management endpoint. `domain remove` for a managed binding uses the persisted ledger automatically and removes only records carrying that binding's exact ownership marker; unmanaged explicit cleanup still requires the Cloudflare flags. A dry run reads a real provider snapshot and performs no mutation. **Credential setup decision revised 2026-11-09:** a human-only CLI flow accepts zone identity and a no-echo token, validates with bounded read-only provider calls, and atomically stores configuration and revisioned ciphertext in a typed provider/zone scope of the existing PostgreSQL vault. It requires no token file, service-environment editing or restart; failed validation preserves the active credential and stale concurrent replacement is refused. Provider calls resolve the current credential at use time. Plaintext never enters ordinary action input, argv, River arguments, events or logs. The terminal gate is a supported-UX policy, not attestation against an OS-account holder; it adds no TOTP requirement for secret writes. Explicit DNS actions use existing SSH operator authority, not a manually configured DNS-admin allow-list; CI/webhook/system actors gain no DNS authority. Setup is saved/read-validated, not proof of DNS-write permission or readiness. Implementation status remains in section 8. Domain admission and the worker use the same target ordering; a delayed removal revalidates the newest domain intent and binding identity under the target lock, so a readd or promotion admitted later cannot be deleted by the stale operation. Ownership is re-read immediately before each provider mutation; Cloudflare has no conditional-write API, so a final external-writer race remains and is reported on the next plan/reconciliation rather than hidden. Only A/AAAA records are managed; any CNAME is a conflict even with the exact marker. Existing foreign records and legacy `eregion:managed` records are never adopted or deleted implicitly; a conflict fails closed. Provider-observed state is separate from resolver, route, certificate and public readiness. Migration 025 adds an immutable domain ID and a durable per-binding DNS ledger written atomically with the operation/River job before provider effects. It stores provider/zone, exact ownership marker, generation, desired explicit origins/proxy, state and cleanup status, and is retained until exact provider cleanup is verified. Removal and app/environment deletion verify provider cleanup before removing serving workload containers or deleting rows; a provider failure or ownership conflict refuses deletion and preserves the app, workloads, ledger, records and credentials for retry. Deletion preflights every managed binding (operation ownership, authorization, zone/host recovery, provider read, exact ownership/conflict) before the first mutation; after that point Cloudflare effects are not atomic, so a later failure or cancellation can leave earlier bindings cleaned and later bindings unchanged or partially changed. Events report how many bindings were processed instead of claiming records were preserved. The workflow is also not atomic across provider and node effects and is not an availability guarantee: after provider cleanup succeeds, a node-step failure or cancellation can leave DNS removed, the ledger retained and some workloads present, while a later route-write failure can leave rows retryable after DNS and workloads are gone. Events record which effects completed; retry converges the rest and no repair command exists. The primary binding is cleaned through environment/application deletion, or after promoting another primary for an explicit removal. The main node remains the only node in beta: `node add`, additional placement and multi-node operations stay after v1. |
| D27 | Plaintext secret read | No `secret get`. `secret export` is human-only behind the D7 factor. |
| D28 | Push to deploy | In the alpha, as in deploycrate: an environment with auto-deploy on deploys every push to its bound branch through a signed GitHub webhook. Each environment has its own webhook URL and generated secret, shown once when auto-deploy is turned on and replaceable by rotation; the HMAC secret is resolved fresh per delivery, the signature is checked over the raw body before parsing, and pushes to other branches are ignored. A verified push is admitted by one PostgreSQL transaction that stores only a sanitized intent (hashes, validated repository/branch/commit and non-secret environment/revision/secret-revision identities) and its River processing job; the handler replies 202 only after that transaction commits, within one 8-second admission budget covering the fresh secret resolution, body read and database work. A worker then submits the ordinary `platform.deploy` operation and links it to the delivery in another single transaction. A redelivery returns the original operation and never deploys twice; a receipt means the push was accepted for processing, not that a deployment completed. If the environment is deleted or its repository/branch binding changes before the worker submits, the accepted intent fails terminally instead of deploying against the new binding. The routing lives in the environment revision, not in service environment variables. This replaces a GitHub App (qualified on a real server 2026-11-04). Planned for alpha.2, not an alpha.1 gate: optional `gh`-assisted webhook setup checks for an authenticated operator `gh` with repository hook permission and can create or update the hook without copying the secret by hand. Manual setup remains available when `gh` or permission is absent. Any prompt has a flag equivalent; JSON/no-TTY never prompts. Plaintext secrets never enter ordinary output, events or arguments (D7). No GitHub App integration is added. |
| D29 | Domain check path | Each domain binding has an optional check path (`domain add … --check-path PATH`), defaulting to the `web` process health path. The domain observer and the D17 public verification request each host at its own path, so an alias that serves no health route can still be ready. deploycrate has one path per web process; this is an eregion extension found in a real deployment. |
| D30 | Disk retention | Required for alpha.1 (R9). A retention policy (server default, per-environment override) and one planner used by a periodic River job and by `eregion prune [--dry-run]`. Defaults: keep the newest 10 releases per environment plus any newer than 14 days, always the serving release and its rollback target; registry artifacts no release references after 14 days; Buildpacks caches above 10 GiB or 80% disk; ownerless labeled containers and dangling images always. Release rows are never deleted; a pruned release records `pruned_at` and a rollback to it refuses clearly. Registry deletion needs a new registry host-step version with deletes enabled plus garbage collection. A real `eregion prune` requires `--confirm <server>` and a dry run needs no confirmation. `doctor` reports the last run and warns when the Buildpacks cache exceeds the policy's absolute cap; it reads the run's durable cache measurement, and an absent or stale measurement is unknown rather than a zero-byte healthy cache. |
| D31 | Workload repair | As deploycrate's `workload_reconciliation`: on worker start the control plane compares each serving deployment's recorded containers with Docker and queues one restart of the serving release for an environment whose serving containers are missing. A restart never re-runs the release command (D21). |
| D32 | App definition file (planned for alpha.2) | Adopt a Fly-style `eregion.toml` **as input only**, never as a store: `app create --from FILE`, `env create|update --from FILE` and `app export <app> <env>` turn it into ordinary desired state. Existing guided and flag-only workflows remain usable without a file; PostgreSQL stays authoritative. The file is a **shared recipe plus optional overrides**, not an exhaustive environment inventory: `[environments.staging]`, `[environments.production]` and `[environments.<name>]` override the shared fields when present, and `env create --from FILE --name pr-42` applies the shared recipe without adding `pr-42` to the file. The selected name is a command argument, not a document identity. The file names processes, health, an optional Git `[source]` recipe, non-secret config, secret **names** (never values), domains and optional generic Docker resources (D19). Schema version 2 removes `source.kind` and `source.image`; an absent or empty `[source]` means no Git build. Root `image_repository` is app metadata, is consumed once by `app create` and exported with the actual durable value; it is not an environment recipe or override. Domains are environment-specific; export places them under the selected environment's override so a shared recipe cannot make production and staging claim the same hostname. Explicit apply previews and provisions declared resources; an ordinary deploy or push never reads the file. `--dry-run` shows changes, removals require target confirmation, and resource data is retained unless separately destroyed. |
| D33 | Environment protection (alpha.2 redesign) | Every environment has persistent `protected` state. `app create` creates **production and staging atomically from one recipe**, and records the app's immutable image repository in the same transaction; both environments are protected by default, and creation itself never asks for a TOTP code. Other environment names are unprotected by default; `--protected` / `--unprotected` make one creation-time choice explicit. `app delete` always requires the D7 TOTP factor. `env delete` requires it when the environment is protected or when it is the app's **last** environment (that deletion removes the app); an unprotected non-last environment deletion needs only exact `--confirm <app>/<env>` and works with agents, `--json` and no-TTY. `env protect` is agent-safe; `env unprotect` requires the factor. The decision and TOTP verification run in the control-plane submission transaction against locked environment/application rows, so node RPC, HTTP and direct service callers cannot bypass it; HTTP refuses actions whose current state needs the factor because it cannot carry a code. Migration 024 backfills every pre-existing environment as protected (fail closed); an operator may lower it with the factor. |
| D35 | App image repository and artifact admission (beta.5) | Each app owns one exact private-workspace image repository path such as `mbvlabs/andurel`, unique in the control database and immutable for beta.5. It never includes a registry host, port, tag, digest, URL or credentials; omission at app create defaults to the app slug. App create may therefore make an image-only app with no Git recipe and no first deploy; an explicit create-and-deploy without Git fails before any transaction commits. Environments retain an optional Git Buildpacks recipe only; `SourceBinding.Image`, `Kind`, TOML `source.image`/`source.kind`, `--image` on app/env create and env update, and the guided Git-versus-image choice are removed without compatibility aliases. Artifact selection is a deploy input: `--image-tag` and `--image-digest` address the app repository, `--image` is a fully qualified public/private reference, and these are distinct from `--ref` and `--release`, mutually exclusive, with no implicit latest. A fully qualified reference to this private registry must name exactly the app repository. Public external references are resolved anonymously with bounded read-only metadata and pulled by canonical digest; no Docker credentials, private external credential feature or automatic mirroring is added. Admission replays the idempotency hash before registry I/O, resolves the tag/index outside any SQL transaction, rechecks app/repository/environment placement facts under the admission lock, and records a pending release pinned to the canonical immutable manifest and expected config digest plus the requested selector as provenance only. A worker never re-resolves a tag; it pulls the pinned artifact, verifies the config digest, then marks the release ready. A missing artifact after admission fails and leaves the previous release serving with no tag fallback, rebuild or mirror. Multi-architecture input selects the target node architecture's immutable child manifest. Preview remains read-only with no registry I/O, shows the app repository/selector and known config blockers, and labels availability unverified. Existing retention/GC policy is unchanged; exact repository parsing protects every environment sharing an app repository, all accepted pending/serving/rollback digests and moved tags, with no heuristic leading-component strip and no external registry delete. Image upload resolves the durable app repository from the app record and verifies the immutable workspace registry-host binding (public SSH host key and canonical state directory) before reading the payload; it holds the app session advisory lock plus the shared node host lock through identity recheck, payload read and every upload helper. Final app/last-environment deletion verifies the same binding before provider/host cleanup and again before the short metadata transaction, takes the same host lock after effects and deletes by durable app ID, so no delete/recreate or second host can retarget an upload. External registry metadata is public-HTTPS-only on every redirect, connection and token realm with no proxy or ambient credentials, redirects are refused, and requested manifest digests are authenticated against raw bytes at top level and selected index child. Artifact pulls use an explicit isolated environment with a private empty Docker config. The canonical artifact reference is bounded against the actual registry endpoint before app creation. `platform.release.create` and its arbitrary destination/pull toggle are removed; internal release constructors and build retry remain. |

### D5 clarification — vaire telemetry model

The agreed model is exporters, an authenticated scrape endpoint and generated
Prometheus/Alloy configuration, as implemented in vaire. Vaire does not install
the collector or dashboards either. Eregion's existing exporter and private
Caddy endpoint are implemented and configured; that alone does not qualify the
external integration below.

**Reachable HTTPS endpoint (operator correction, 2026-11-10):** provide proper
authenticated HTTPS metrics and discovery URLs that an external collector can
use, as in vaire. A private WireGuard address alone does not meet this contract.
Use the configured main hostname with exact `/metrics/node` and
`/metrics/targets.json` paths. Both endpoints require authentication, and
discovery advertises that hostname on HTTPS port 443. The raw exporter and
dedicated authentication gateway remain loopback-only; there is no retained
WireGuard-listener alias. Public ingress proxies only those fixed telemetry
paths and remains independently operable if telemetry is unavailable. Integrate
through owned ingress rendering/reconciliation and preserve serving application
routes. Reuse the existing credential rather than
rotate it merely for the URL correction. This supersedes the private-only/no
public telemetry endpoint wording in the previous D5 decision; it does not expose a
public management API or introduce a managed collector.

The earlier phrase **node-local and request-scoped** describes the CLI's bounded
current snapshot, not the telemetry architecture or a restriction on external
collection. A WireGuard-peer Prometheus/Alloy collector can scrape the endpoint
for retained history and dashboards without the operator workstation remaining
online. This clarification does not introduce a managed collector, a second
telemetry node or a new deployment requirement; the existing collector/topology
scope decision is unchanged. Do not reopen or reimplement the exporter setup
because of that wording.

**Future telemetry node (operator reconfirmation, 2026-11-10):** retain the
concept of an optional telemetry node that users can add to the workspace's
private network. It can provide centralized collection, retained history and
dashboards in place of a separately operated Grafana-style stack. It is future
scope, not a beta.1 prerequisite; its implementation and topology are not being
added by the current exporter work.

### D34 — Portable workspace identity (alpha.7 contract)

One installation has an immutable generated workspace ID and a slug name,
authoritatively recorded once in external PostgreSQL. **Workspace is the only
named operator target** (operator clarification, 2026-11-10). There is no separate
context or named access/profile concept. Local keys, pins and connection details
are access data for that workspace's single node, not another identity. Setup
asks for workspace name separately from node name. Same workspace/name
initialization reuses its ID; a conflicting name is refused. Reads and migration
do not guess or assign it.

The same singleton also carries an immutable, all-or-none **registry-host
binding** established atomically on first workspace assignment: main node name,
public SSH host-key type/SHA256 fingerprint and canonical node state directory.
This is installation metadata for the physical host that owns the private
registry, not a fleet, node-role or remote-locking feature. Identical
initialization replay is allowed; a different name, host key or state directory
is refused. A workspace row created before the binding remains explicitly
unverified: uploads and deletion finalization refuse it and it is never
backfilled or inferred. No private key is stored or compared.

`--workspace NAME` selects the verified workspace name, never a historical
local alias. Selection precedence is the explicit flag, `EREGION_WORKSPACE`, then
the saved current workspace. `workspace list|show|use|remove` manages workspaces
known to this machine; remove deletes local access only, never the installation,
database identity or applications. Old context commands/flags are removed, not
retained as aliases. Obsolete targeting environment settings fail explicitly
rather than silently selecting another store or target. `EREGION_WORKSPACE_FILE`
selects the private store. **Beta.1 cleanup (operator decision, 2026-11-10):**
the default is `~/.config/eregion/workspaces.json`; current private serialized
fields and implementation names use workspace terminology too. Do not fall back
to the old filename or schema. Fresh setup and protected login publish access in
the default store so plain `workspace list` works without custom store flags.
Preserve existing alpha files and credential paths; beta is fresh-install only,
not an implicit local-store migration or alpha-to-beta upgrade.

Creation is distinct from selection: setup uses
`server init --workspace-name NAME` and assigns identity after database migration.
Beta has no public assignment-upgrade command or unconfigured-access import.
An unfinished current setup keeps its route in pending state and verifies the
configured identity and readiness before publishing completed workspace access.
Handoff/export carry the portable identity; login verifies it against the
node before atomically publishing access. Same name/different ID and same
ID/different name are conflicts. Historical duplicate access records for one ID
are one workspace, not additional named targets; resolution uses its selected
record or sole record and otherwise refuses ambiguity. Verified login replacement
may replace that workspace's local records without disturbing unrelated
workspaces or deleting their credential files. Exact replacement confirmation
and host-key verification are never waived. Beta refuses bundles without
portable workspace identity; the alpha exception below is historical.

This is an identity contract, not team authorization, server sharing, multi-node
management or tenant isolation. The TUI renders it; the TUI is not the identity.
Implementation details belong in IMPLEMENTATION and qualification in
MANUAL-TESTING; Linear tracks execution.

**Historical alpha.7-only transition (operator decision, 2026-11-10):** the existing
old-bundle/unconfigured-access, unfinished-setup and legacy submission-capability
fallbacks may remain for this alpha's upgrade qualification. This is not a
compatibility commitment. Remove legacy format/behavior fallbacks before beta.1
and qualify beta on a fresh installation rather than relying on alpha state or
an in-place alpha-to-beta upgrade. Normal schema creation/migrations are not
permission to retain old
command aliases or parallel behavior. Do not add further compatibility paths.
The fresh beta server and its dedicated control database need explicit target
authorization; this decision does not authorize wiping, deleting or repurposing
the current alpha server/database.

**Beta.1 publication (operator authorization, 2026-11-10):** complete the
remaining workspace cleanup and reviewed fixes, then publish the immutable
`v1.0.0-beta.1` prerelease for the operator's fresh test tomorrow. Publication
does not establish fresh-install acceptance. Keep outstanding readiness and
provider/multi-workspace qualification results explicit; never turn not-run
checks into passes. A fresh target/database remains separately authorized.

**Telemetry identity (operator decision, 2026-11-10):** expose authoritative
workspace identity and logical node identity for dashboard display, including
clients scraping the public metrics URL directly. Preserve the OS hostname in
the exporter's uname metadata. A workspace/node display is not permission to
rename the host or falsify that metadata; stock dashboards may need an explicit
customization. No managed collector or dashboard service is added.

## 4. The application model

The application nouns follow deploycrate-ce. Names are lowercase slugs.

**External-resource boundary:** current external dependencies use plain config
and vault secret names (for example an external database URL). The managed
`resource` lifecycle describes Docker dependencies, not equivalent first-class
external-service records. Extending that model needs a separate product
decision after inspecting deploycrate's resource/source/attachment behavior;
it is not alpha.7 scope. D4's expendable-volume recovery policy does not change
D19's default retention or authorize deletion.

- **Workspace.** Portable installation ID/name, separate from its node name;
  each operator machine stores its connection data directly for that workspace
  (D34). One node for current v1
  scope, not a tenancy or team boundary.
- **Server / node.** A VPS. The first one `server init` sets up is the **main
  node**. The selected workspace identifies which installation to operate.
- **App.** A named group of environments. It owns one immutable private-workspace image repository used by builds and uploads; environments cannot override it.
- **Environment.** The unit of deployment (`staging`, `production`, …). It also
  carries persistent deletion **protection** (D33), separate from its desired
  revisions. Its **desired state** is one complete revision containing:
  - **optional Git build recipe**: an HTTPS git repository, ref, read
    credential, Buildpacks context path/builder/settings and auto-deploy on
    push (D28); absent means no Git source and an explicit artifact selector at
    deploy time;
  - **runtime**: node, health path and timeouts, release command;
  - **processes** (D21): name, kind, command, arguments, replicas, container
    port for `web` and `service`;
  - **config**: plain environment variables;
  - **secrets**: environment-scoped names, resolved per deploy (D18);
  - **domains**: one primary hostname plus aliases, served by the `web`
    process.

  Every change commits a new immutable revision and a change record. A
  **deployability** report lists what is missing before an explicit deploy can
  run (artifact, runtime, domain, DNS), as deploycrate's does.
- **Build.** One attempt to turn a source revision (exact commit) into an image
  in the app image repository, with its own logs and lifecycle; it can be
  stopped and retried.
- **Release.** An immutable deployable artifact (manifest digest plus expected
  image config digest, with requested-selector provenance) for one environment,
  from a build or an explicit artifact selector.
- **Deployment.** Applying a release plus the environment's revision to its
  runtime. Success means every process runs that release and the public check
  passed, not that a job returned. Rollback is a deployment of an earlier
  release.
- **Domain binding.** Desired hostname → environment. The route, the DNS
  record, the certificate and the public response are separate observations.

## 5. Command tree

Target surface for v1. Every mutating command is guided in a terminal and
takes flags only with `--json` or no TTY. Every command has `CommandMeta`,
dry-run where it mutates, and appears in `eregion commands --json`. Node-only
commands are marked Linux-only and hidden from the operator listing.

```
eregion server init [--workspace-name NAME] [--node NAME] [--resume | --discard] [--answers FILE] | status
eregion plan | apply [--auto-upgrade-nodes] [--assign-workspace NAME] | doctor  # explicit alpha workspace assignment; converge/check host steps
eregion login [--replace WORKSPACE] | workspace list|show|use|remove
                                                       # every node command: --workspace NAME / EREGION_WORKSPACE
eregion secrets export                                 # server handoff, human factor
eregion app create|list|status|delete [--image-repository PATH] # create: app repo + production + staging atomically
eregion env create|update|protect|unprotect|list|status|delete
eregion env promote <app> <source-environment>         # exact serving release -> production
eregion process list|scale                             # per environment
eregion deploy <app> <env> [--ref REF | --release ID | --image-tag TAG | --image-digest SHA256 | --image REF] | rollback <app> <env> [--to ID]
eregion image push <local-image> --app <app> [--tag <tag>]   # local image into the app's private repository
eregion build list|logs|stop|retry  ·  release list
eregion domain add|list|remove [--primary] [--check-path PATH]
                                                         # optional explicit --dns cloudflare --dns-zone ZONE --dns-origin ADDRESS --dns-proxy dns-only|proxied
eregion domain cloudflare configure|status               # configure: human-only hidden token -> encrypted provider vault
eregion config set|unset|list                          # plain variables
eregion secret set|list|archive|export                 # values write-only
eregion logs <app> <env> [--process NAME] [--follow]  ·  restart <app> <env>
eregion metrics show|endpoint|config|rotate            # current snapshot; WireGuard-only authenticated scrape config and rotation
eregion retention show|set  ·  prune [--dry-run] [--confirm <server>]
eregion resource apply|attach|detach|list|status|destroy [--delete-data]   # D19
eregion operations list|get|follow|cancel
eregion token create|list|revoke  ·  protect init|status
eregion ssh [node] [--command CMD]  ·  node unlock-ssh # break-glass
eregion upgrade
eregion ui  ·  commands [--path PATH | --search TEXT | --check-meta] | guide agents | skill install | version
eregion node …                                         # node mode, not for operators
```

Beta: explicit Cloudflare DNS automation under the existing domain flow, with
human-only credential configuration under `domain cloudflare`.
`env promote` is in the alpha/beta surface as D23.
After v1: `node add|list|status|remove`, access modes, `server init --local` and
HA. There is no telemetry node or telemetry server in beta/v1; a later optional
telemetry node or Grafana is not prohibited long-term (D5).

## 6. Flows

### 6.1 `server init` (vaire)

1. **Local preflight**: target reachable, an SSH credential, missing tools
   reported as `missing_tool` with the install command.
2. **First contact** as root with `--identity`, ssh-agent, default keys, an
   encrypted-key passphrase or `--password-stdin`. Host key TOFU with
   fingerprint confirmation, `--host-key-fingerprint` or `--accept-new-host-key`.
3. **Remote preflight**: OS, architecture, resources, existing state.
4. **Questions**, each defaulted and each also a flag or an `--answers` TOML
   key: workspace name, node name, public hostname (used as given), external DSN
   file, admin user.
5. **Plan**, then confirm; non-interactive needs `--confirm <server>`.
6. **Apply** versioned steps with live progress: install the binary once,
   hardening, WireGuard hub and operator peer, commit-confirm SSH switchover,
   node_exporter on loopback and its authenticated WireGuard telemetry
   endpoint, DNS record shown and checked against public resolvers, Caddy and
   ACME, Docker, Buildpacks, registry, vault keyring, migrations, services, `doctor` as the final gate.
7. **Handoff**: every server secret in plain text for a password manager plus an
   armored age transfer bundle with a generated passphrase, break-glass
   instructions included. No file unless `--secrets-out`. Init does not finish
   until the operator confirms it is stored. No API token is in it.

A pending init is saved locally. `--resume` continues it, `--discard` drops it,
and init refuses while workspace access or pending init for that server exists.

### 6.2 Operator session

`login` imports verified workspace access; it validates the bundle against the
node before publishing anything, and replacing existing access requires
`--replace <workspace-name>`. Every node command selects `--workspace`, then
`EREGION_WORKSPACE`, then the current workspace. Every command checks the node's
eregion version first and refuses with `node_version_mismatch` unless
`--auto-upgrade-nodes` is given or saved for the workspace. `upgrade` replaces only
the local CLI and says so; `apply --auto-upgrade-nodes` upgrades the node and
converges its steps; `doctor` reports drift, security, certificates, disk,
retention and versions. `eregion ssh` opens a shell over WireGuard.

### 6.3 App setup (deploycrate)

`eregion app create` asks for the app name, an optional shared Git/runtime/process
recipe, an optional private image repository and production's domain, shows the
plan and DNS record, then commits the app (including its image repository),
**production and staging** and each first revision in one transaction. Both are
protected by default; creation never asks for a TOTP code. The shared recipe
defaults to production and staging names, and `--unprotected` is the explicit
creation-time opt-out. Unless `--no-deploy`, the first environment's Git ref is
resolved and its first build and deployment are queued in the same transaction;
a Git-less image-only create has no implicit first deploy, and an explicit
create-and-deploy without Git is refused before commit. Staging starts without a
domain unless a definition override supplies a distinct one. The flag-only form
expresses exactly the same intent. `env create` adds any environment the same
way, defaulting to unprotected unless the name is `production` or `staging` or
`--protected` is passed; its Git recipe is optional and it cannot change the app
image repository. From `--from FILE`, an arbitrary name such as `pr-42` uses the
shared recipe without being listed in the file. `env update` changes any part of
desired state as a new revision; `process scale` changes replicas; `env
protect`/`env unprotect` change durable deletion protection (D33).

### 6.4 Deploy

`eregion deploy <app> <env>` builds the environment's Git source at its ref (or
`--ref`), records the release by canonical manifest digest and deploys it.
Without a selector, a Git-less environment is refused with explicit artifact
guidance rather than guessing latest, newest or the serving image. `--image-tag`
and `--image-digest` select an artifact from the app's private repository;
`--image` takes a fully qualified registry reference (a reference to this
private registry must name exactly the app repository). These selectors are
mutually exclusive with each other, `--ref` and `--release`, and there is no
implicit latest. `--release` redeploys an existing release with the current
revision; that release must belong to the same app and environment. `restart`
reapplies the serving artifact. The pipeline, per D17: preflight →
release command → candidate containers for every process on dynamic loopback
ports → candidate health → Caddy backend switch for `web` → public verification
→ stop the previous containers. A failed candidate leaves the old release
serving. Every step is a durable operation; Ctrl-C detaches, `operations
follow` re-attaches, `cancel` asks for cancellation explicitly. Each environment
has at most one durable deployment owner (migration 026), acquired under the
environment lock and held across retries, cancellation and restore until the
terminal route is safe. Other deploy jobs try the same target lock without
blocking and snooze without consuming a failure attempt or holding a pool
connection; scale submission uses a nonblocking transaction try-lock and
returns conflict on contention. Route writers read only the owner's target,
never deployment creation order or a running step as proof. The owner freezes
the predecessor, persists candidate intent before the switch, and finishes
success only after the route step and public verification complete. Any
unproven route, cancellation, exhaustion or terminal/stale owner restores the
exact frozen target before candidate cleanup; a failed restore keeps candidates
and ownership retryable. The success transaction rechecks cancellation, so a
cancellation accepted before finalization restores instead of committing
success. Serving and guarded cleanup read authoritative current rows; the frozen restore
deployment stays protected, protected cleanup is durable, deferred rows
advance, and ownership is not released until terminal/ambiguous cleanup rows
are recorded. Previous-cleanup lookup or pre-record failures keep ownership and
the operation nonterminal. A nonretryable post-switch resync failure restores
the frozen predecessor before terminal failure; restoration failure keeps
retryable ownership. The owner phase is authoritative, so a restoring owner
cannot revive a completed switch. Live and recovery share the same route-failure
classification and restoration path. Cancellation/failure persists candidate
cleanup rows before terminalizing, and a pinned previous revision must resolve
its exact runtime node or block success. The owner's candidate deployment stays
protected from maintenance cleanup for the owner lifetime, including prepared
and switching phases, so partial cleanup rows cannot race a resumable attempt.
Worker shutdown (`context.Canceled`) is treated as retryable and retains
candidates; only a durable `cancelling` operation takes the cancellation/cleanup
tail. A pre-save interruption reconstructs planned names from container-step
evidence and the pinned revision; an evidence read failure retains ownership and
the operation. Retryable pre-switch failures retain candidates and completed
steps, while terminal live and post-switch public failures persist cleanup
intent before settling. The restore step uses the owner's frozen release and
always reasserts the selected target, even when the step row was already
completed. Ambiguous recovery propagates deployment lookup failures instead of
silently releasing ownership. Only success
finalization releases a serving owner. Migration 026 is a fail-closed cutover,
not a backfill: it refuses to apply while a deploy/rollback/restart/promote
operation is queued, running or cancelling, or while any deployment row is
pending or running. Old in-flight work must finish or be cancelled before
upgrade; a safe cutover creates the empty owner table and leaves terminal
history and serving pointers untouched. A live retry before route intent may
resume; reconciliation without a worker lease fails it.

`eregion env promote <app> <source-environment>` is the only cross-environment
path (D23). It resolves and pins the source environment's current serving image
digest at submission, records it as a new production release without rebuilding,
then applies production's current revision, config, secrets, domains and release
command through the same blue/green pipeline. It does not copy source config,
secrets, domains or release command and never adopts a domain. A source that is
not serving or not ready, any active production deployment (including a
different artifact), an artifact already serving production, a different node,
or a production environment that fails the explicit-release deployability gate
is refused before acceptance. Ordinary deploys queued after a promotion rebind
their previous release/deployment at execution under the target lock.

### 6.5 Domains

`domain add` saves the binding, shows the exact DNS record, then the worker
renders and applies the Caddy route, and the command reports DNS, route
application, certificate and public response separately as
pending/applied/failed. Routes are reconciled on worker start and after a
failed reload. One primary per environment; others are aliases.

For managed Cloudflare DNS, a human first runs `domain cloudflare configure`
and enters zone identity plus a hidden token. The CLI read-validates and
previews the credential revision before confirmation; configuration and
encrypted provider-scope vault state commit atomically. Rotation uses the same
flow and takes effect without restart. `domain cloudflare status` is an
agent-safe metadata read. No token file or service-environment editing is part
of this flow, and credential setup is not DNS-write or public qualification.

DNS stays manual by default. With explicit `--dns cloudflare --dns-zone ZONE
--dns-origin ADDRESS --dns-proxy dns-only|proxied`, the same command plans or
reconciles only the exact A/AAAA records for that hostname; origins are explicit
public addresses and are never taken from the workspace's WireGuard management
endpoint. `--dry-run --json` includes a real Cloudflare provider snapshot,
including the explicit origins, and changes nothing. An already-bound hostname
can be reconciled the same way. Domain admission and execution are serialized;
a delayed removal revalidates that it is still the newest domain intent and that
the binding was neither promoted nor re-created under the target lock. Removal
uses the persisted managed-DNS ledger; an unmanaged explicit cleanup uses
`--dns cloudflare --dns-zone ZONE`. Only records whose comment is this binding's
exact ownership identity are deleted. Ownership is re-read immediately before
each mutation; Cloudflare has no conditional-write API, so the final
external-writer race remains visible in the plan/reconciliation contract rather
than being claimed closed. A foreign record at the same hostname makes the
operation fail closed; legacy zone-wide `eregion:managed` records are never
adopted. Explicit provider success is not public readiness: resolver, route,
certificate and response stay separate observations. The durable
`eregion_platform_domain_dns` ledger records provider/zone, explicit origins,
proxy mode, generation, exact marker and cleanup state in the same transaction
as the operation and River job. Removal and app/environment deletion mark it
cleanup-pending, verify exact provider cleanup, then delete ledger and domain
rows; a provider failure or ownership conflict refuses deletion and keeps the
ledger, records and credentials retryable. A primary binding is cleaned by
environment/application deletion, or by promoting another primary before an
explicit removal.

**Approved TOML plan/apply behavior (2026-11-12; implementation authorized for
beta.4, not yet qualified).** A definition declares the hostname, such as `mbvlabs.com` or
`staging.mbvlabs.com`. With explicitly configured Cloudflare management and public
origin settings, the server resolves the zone and previews the exact records:
create missing records, leave records already matching the intended addresses
and proxy settings unchanged, and require explicit approval of the concrete diff
before replacing conflicting addresses or a conflicting CNAME. Without configured
managed DNS, retain manual DNS and explain the prerequisite. Credential presence
alone does not select DNS management; never infer public origins from WireGuard.
The plan reports current evidence and blockers, not guaranteed provider write
permission or application readiness. Revalidate relevant records before effects;
material drift requires a refreshed plan and approval. An unchanged pre-existing
record is satisfied external state, not implicitly adopted: matching it grants
no future mutation or deletion authority. Cleanup remains restricted to exact
ledger-backed ownership. This revises the matching-record refusal and permits
explicit conflict replacement for the TOML workflow; the current command
behavior described above remains unchanged until that work ships.

### 6.6 Config and secrets

`config set` changes plain variables, `secret set` stores a value (write-only,
encrypted with the vault keyring that `server init` provisions). Neither
deploys by itself, as in deploycrate; the next deploy resolves the environment's
latest revision and records the exact secret revisions it used (D18).
`secret export` is the only plaintext read (D27).

### 6.7 CI

The main hostname exposes one deploy endpoint per environment, as in
deploycrate: `POST /v1/environments/<app>/<env>/deployments` with a git ref and a
deploy-scoped token builds and deploys that ref; a status read for the returned
operation is the only other route. Tokens follow D7.

Push to deploy (D28): `env update --auto-deploy` prints the environment's
webhook URL (`/v1/webhooks/github/<app>/<env>`) and a generated secret once, to
paste into the repository's GitHub webhook settings; `--rotate-webhook-secret`
replaces it. A verified push is durably admitted and answered 202, then a worker
submits the same deploy as `eregion deploy` at the pushed commit and links the
operation to the delivery atomically; a redelivery returns the original
operation. The receipt means accepted, not completed.

### 6.8 Observe and remove

`app status` and `env status` show the serving release, each process and its
replicas, the desired revision, domains with their DNS/route/TLS/public state,
the last operations and the deployability report. `logs` reads container logs
over the operator channel (D25). `app delete`, `env delete`, `env protect` and
`env unprotect` follow D13/D33: every deletion names its exact target;
`app delete` always needs the TOTP factor, `env delete` needs it for a
protected or last environment and is otherwise available to agents with
`--confirm` alone. `status` shows the persistent protection state.

## 7. v1 alpha and beta

**v1.0.0 (every alpha, beta and the final) is one main node running
applications end to end:** sections 6.1–6.8 on the main node, the command tree
in section 5 minus its "after v1" line, node_exporter on the server, and the
release model in D14–D15. Beta adds node-local telemetry (D5) and Cloudflare
DNS automation (D26) to the alpha.3 flows. The telemetry primitives include the
WireGuard-only authenticated endpoint, HTTP service discovery, generated
Prometheus/Alloy configs and credential rotation; they do not add a telemetry
server, telemetry database, metric/log history, public scrape endpoint, central
Prometheus/Grafana deployment, backup product or additional nodes. The local
textfile directory, narrow systemd collector, authenticated endpoint and doctor
health checks are host primitives, not a telemetry service. Cloudflare DNS
automation is implemented in source for beta as an explicit, domain-scoped
flow. Alpha.6 qualified human credential setup, DNS create/readback/no-op and
exact-owned alias cleanup on `thelab`; provider-failure/interrupted deletion,
multi-binding deletion and actual foreign-record apply refusal remain live
qualification gaps. No beta qualification is claimed here.
After v1.0.0 final: additional nodes, access modes, `server init --local` and
HA; a later optional separate telemetry node or Grafana remains an allowed
future option, not a beta/v1 deliverable (D5).

### Future design questions (not commitments)

- **Workspace boundaries:** D34 settles portable identity only. A future
  platform/team boundary needs separate isolation, authorization and ownership
  decisions, not just an additional database field.
- **Sharing a server:** Explore whether a physical server could participate in
  more than one workspace, and what isolation, ownership, networking and
  lifecycle boundaries that would require. This is not supported by the current
  one-install, single-main-node setup.
- **Ingress across nodes:** Consider whether the first node should act as the
  reverse proxy/load balancer for applications on other nodes. Its role,
  routing, failure behavior and alternatives are open questions, not a decision
  to make the first node a permanent or sole ingress point.

These questions do not change current v1 scope or authorize implementation.

**v1 is pre-release: nothing is frozen.** The `eregion/v1` JSON, the handoff and
bundle formats, workspace access files, host step IDs and the database schema may change
between alphas, and a later alpha may require a fresh `server init`. A v1
compatibility contract in vaire's form (formats, JSON, commands, the
`upgrade` + `apply --auto-upgrade-nodes` path) starts at `v1.0.0` final.

An alpha is cut when the operator accepts that alpha's MANUAL-TESTING
acceptance criteria and the recorded evidence for the candidate build. A
fresh-server baseline is **not** required for every alpha. Alpha.2 is qualified
incrementally on its own criteria using the authorized existing main node,
fixture app, hostname and GitHub repository. "Builds" or "appears in
`commands --json`" is never the gate. Disk retention (D30) was the alpha.1 gate
(R9): the policy, daily run, `prune [--dry-run]`, registry deletes and garbage
collection, and the doctor retention and cache rows qualified with the rest of
the alpha.1 baseline.

**Alpha.1 is qualified.** The operator explicitly confirmed that all alpha.1
testing is done, including the disk-retention gate, and alpha.1 is recorded as
qualified by that confirmation. This is not a reconstructed baseline: no build,
date, operation, individual observation or pass result is invented or
backfilled. The detailed records in MANUAL-TESTING remain the evidence available
in this checkout, including their partial and not-run entries; those are
evidence gaps for those runs, not open alpha.1 blockers.

Exception (R10): the macOS half of the install and upgrade checks is recorded
as **not run (no Mac available)**. It must be qualified before v1.0.0 final
and is not an alpha.1 gate; CI's macOS CLI job and the darwin/arm64 cross-build
remain, but neither is host acceptance.

Final qualification for later alphas (R3, R11) is **incremental against the
alpha's own acceptance criteria**, on an explicitly authorized existing target
such as `main`, a disposable fixture app, the authorized hostname and the
authorized GitHub repository. A complete fresh-server baseline is not required
for each alpha. A fresh-server v1.0.0-final rehearsal remains a separate final
decision if the operator requires it. Alpha.1 stays qualified by the operator
confirmation above; no evidence is reconstructed or backfilled. macOS
acceptance (R10) remains required before v1.0.0 final.

## 8. Recorded implementation and qualification history

**Linear execution tracking (operator decision, 2026-11-10).** Use the Labs
[Eregion project](https://linear.app/mbvlabs/project/eregion-2ff1d2e4d199)
for actionable work, issue status and release execution. Product decisions and
contracts remain here; mechanisms are in IMPLEMENTATION and acceptance/run
evidence in MANUAL-TESTING. The dated records below preserve provenance and
useful issue links, not an active ticket-status mirror or scheduling authority.
Keep source verification, publication and live qualification distinct in Linear.

**Fresh-VPS alpha.5 follow-up (operator report, 2026-11-09).** Bootstrap,
handoff storage and login using a separate context store succeeded; reported
status is schema 26/26, River 7/7, doctor 23/0/0 and dry-run apply 0 of 21.
The real second apply and fresh app/provider/webhook workflow remain unverified
on that VPS. Login initially failed because distinct servers share the
`10.78.0.1` SSH host-key pin; the separate store is a workaround, not a fix.
Fix server-scoped pin isolation without weakening host-key verification before
claiming this multi-context flow qualified. The operator also requests direct
local `gh` integration for private-source setup, without manually exporting a
GitHub token file, and a human-only Cloudflare credential setup handoff. These
UX gaps are under investigation; no provider qualification or beta readiness
is implied. See MANUAL-TESTING for the reported run boundary.

Agent follow-up on the operator-confirmed `thelab` target (`46.224.129.96`)
verified installed CLI/node `1.0.0-alpha.5` / `71c678362029`, schema 26/26 and
River 7/7. The second real apply passed **0 applied / 21 skipped**
(`op_9c1f5d04ddadbdc5ccc453eac65fb85d`), metrics returned a fresh snapshot,
endpoint/config descriptions preserved secret placeholders, and doctor passed
23/0/0. The domain inventory was empty. The designated fresh-test hostnames
are production `git-test2.mbvlabs.com` and staging `git-test3.mbvlabs.com`;
no app, webhook or DNS mutation has yet been performed in this follow-up.

**Cloudflare setup correction requested (2026-11-09).** Replace the manual
token-file/service-environment setup with a human-only operator CLI flow:
paste zone identity and a no-echo token at a terminal, store the credential
encrypted in the existing PostgreSQL vault, and resolve it there for provider
operations. No human-supplied credential path or plaintext token in ordinary
output/action input is acceptable. This supersedes D26's token-file setup
requirement. The CLI/vault correction and mechanism documentation are now
integrated; final verification and real-host qualification remain pending.
The operator authorized a new alpha release after verification, followed by
real CLI/node upgrade qualification from alpha.5 on `thelab` and continuation
of the provider/app/webhook tests. This is not authorization to call beta
qualified or to bypass human credential entry or DNS previews.

Implementation boundary for this correction: provider credentials have a typed
installation-wide provider/zone scope inside the existing vault, separate from
application namespaces. Configuration and encrypted revisions commit atomically;
bounded read-only validation precedes activation, and stale concurrent
replacement is refused. Each provider operation resolves the current credential
without restart. The human-only supported CLI is a no-echo terminal-entry policy,
not attestation against an OS-account holder; no new TOTP requirement is added
for secret writes. DNS authorization uses the existing SSH operator authority
for the explicit DNS read/write/create/update/delete actions, not a manually
configured DNS-admin environment allow-list; CI/webhook/system actors receive
no new authority. Audit, exact configured-zone checks and ownership protections
remain mandatory. Source verification and the separate live evidence follow.

**Alpha.6 is published; its CLI/node upgrade is host-qualified on `thelab`.**
Integrated implementation, review and
regression verification passed. The context-scoped login/init pin fix is
integrated, including the reviewed resume regression correction; its targeted
offline tests, build, vet and Darwin cross-build passed in isolation. Live
multi-server login remains unqualified. Direct local `gh` private-source
authentication is implemented and review-cleared, including credential-role
collision checks at both early CLI entry and shared effective-revision
normalization; worker-run isolated regressions, build and vet pass. Real
private-repository acquisition/build subsequently passed in the alpha.6
continuation below (LABS-337). Cloudflare credential
setup is integrated and static-review-cleared;
independent disposable-PostgreSQL, repeated CAS race, broad offline, build/vet,
cross-build and final cleanup checks passed. The disposable cluster is removed.
Release publication and live provider qualification are separate outcomes.
PR #5 merged the verified tree into `1.x` as
`b8211a2c44f65ade07eff4e15063c4a901b9408b`; annotated `v1.0.0-alpha.6` points
there. Release ancestry was reconciled without rewriting history: merge
`6301c36` preserved the exact candidate tree, and `5073d1b` merged the alpha.6
release line back into `master`. Fresh full CI `42673387396` passed Linux
check/race/build, PostgreSQL integration and macOS CLI checks. The tag's first
Publish stable `42674342682` and CI `42674342778` attempts failed before any
job during GitHub's Actions/Checks outage. Both retries subsequently passed at
the same tagged commit. Independent public verification confirmed the manifest
and all four binaries' checksums and cosign bundles against the exact tagged
workflow identity; linux/amd64 reports `1.0.0-alpha.6` / `b8211a2c44f6`.
Root, `1.x` and RC pointers remain absent.

On authorized `thelab`, no queued/running/cancelling operations existed before
upgrade. Normal `upgrade --version 1.0.0-alpha.6 --yes` replaced only the CLI;
node-upgrade dry-run refused with `node_version_mismatch` and made no change.
Real `apply --auto-upgrade-nodes` (`op_effa34cc656b7e48116e1243a58b6886`)
applied migration 027 and restarted the services (4 applied / 17 skipped).
Immediately after upgrade, both sides reported alpha.6 / `b8211a2c44f6`,
schema 27/27, River 7/7 and
doctor **23 pass / 0 warn / 0 fail**. Repeat real apply
`op_3a6a7b76ae9ef2f5a0688ee17abad10b` was **0 applied / 21 skipped** and
dry-run reported zero changes. Cloudflare status was initially empty;
subsequent human/provider/app qualification continued below. These upgrade
results are not a substitute for the latest final gate.

Post-upgrade human setup exposed LABS-343: Cloudflare configure prompts for a
token before resolving the selected context, so a missing local context wastes
secret entry. The terminal used the default store (`main`) rather than the
isolated fresh-test store (`thelab`); selecting the correct store unblocks the
attempt. Correct prompt ordering is implemented and review-cleared with
isolated regression coverage, but remains uncommitted/unpublished. No provider request or credential save occurred in that failed
attempt; details are in MANUAL-TESTING. No follow-up publication is authorized
by this defect report alone.
After selecting the correct store, a second human attempt reached Cloudflare's
read validation and was rejected; the operator later confirmed it used the
account ID rather than the zone ID. That failed attempt activated no credential
and changed no DNS.

**Testing resumed:** the operator corrected account ID to zone ID and completed
the human Cloudflare flow, saving revision 1 for `mbvlabs.com`; agent metadata
status confirmed it. `alpha-five` creation with local `--github-auth` completed,
with protected production/staging and separate plain environment labels. The
old `git-test2`/`git-test3` records were foreign and left untouched. After clean
previews, the operator's replacement hostnames `eregion-test1.mbvlabs.com`
(production) and `eregion-test2.mbvlabs.com` (staging) were created as exact-owned
proxied A records to `46.224.129.96`; subsequent provider snapshots report
`unchanged=1` each. Production deployment `op_44d64f9d23c4a773ba02fa9dfe138482`
completed successfully, serving v1 over valid public HTTPS. Same-key create
replay left the Git credential revision unchanged; LABS-337 private-source
qualification passes. A safe non-disclosing fixture probe then confirmed
production v1 and staging v2 each use their own synthetic runtime secret.
Promotion `op_575e312067e0f4293559c973a010e657` passed with the exact staging
digest and no rebuild, retaining production config and its own matching secret;
same-key replay returned the original operation. Rollback
`op_2ab5dffd4364714e688af47cc96bfbde` restored production v1 with its own
config/secret while staging stayed v2. The independent five-minute observer
refreshed both domains to valid TLS/public 200 without repair. Real production
DNS repeat apply reported 0 changed / 1 unchanged. On 2026-11-10, the explicitly
authorized cleanup alias was created and removed through ordinary managed DNS;
provider readback was empty and both serving domains were unaffected. Agent
webhook enable was refused by the terminal gate (LABS-344), then the human
completed gh-assisted setup. A signed master push automatically deployed v3 to
staging; redelivery created no duplicate deployment, and an explicitly
authorized temporary non-master branch was ignored and removed. Production
remained v1. Telemetry authenticated/unauthenticated scraping and one rotation
passed over WireGuard: old credential 401, new 200, anonymous 401 on both paths,
with no credential output. The operator subsequently confirmed protection
setup and private handoff export passed (2026-11-10); no exported content was
read by the agent.

An earlier doctor gate hit intermittent loopback readiness timeout (22 pass /
1 fail). Final read-only verification on 2026-11-10 at 09:31Z passed doctor
23/0/0, schema 27/27, River 7/7 and all six service checks. Protection metadata
is configured with no pending setup or failures. Both public apps retained their
expected version/environment/secret-match responses. Five bounded readiness
samples all returned 200, with loopback readiness 0.334–0.628s; API and worker
had zero restarts. The current health gate passes, but the intermittent timeout
was not reproduced or explained: LABS-346 root-cause triage remains open.
Provider-failure/interrupted variants were not injected live. Workspace naming
is LABS-340, safe gh-assisted no-TTY webhook setup LABS-344, and the requested
post-test operator documentation pass LABS-345. No beta qualification is claimed.
MANUAL-TESTING holds exact operation/release IDs, retained artifacts and skips.

**Approved history cleanup (2026-11-09).** After the operator approved the exact
force-with-lease scope, `master` was aligned from `5073d1b` to the already-linear
`1.x`/alpha.6 commit `b8211a2`; tracked trees were identical. All remote tag refs
were compared byte-for-byte before/after and remained unchanged. Uncommitted
MASTER changes and the pre-existing untracked spec were hash-checked unchanged.
The old graph is retained in the verified local recovery bundle
`.git/history-backups/pre-linear-alpha6-20261109.bundle`; the original bundle
and pending documentation diff are also under
`/tmp/opencode/eregion-history-backup.Lqvixt/`. The linear promotion policy in D14 supersedes
the earlier release-PR/squash/merge-back process; this cleanup is not permission
for future automatic force pushes.

**Alpha.1 is qualified.** The operator explicitly confirmed that all alpha.1
testing is done. That confirmation is the qualification record; no build, date,
operation, individual observation or pass result is invented or backfilled here.
The older run records below describe the evidence available in this checkout,
including partial and not-run checks; they are historical evidence gaps, not
open alpha.1 blockers.

Alpha.2 is implemented on `master` and published in `edge-6d1ae98b9ac1`:
strict `eregion.toml` input and export, shared/environment overrides, explicit
resource declarations, generic environment-owned Docker resources with private
networking, retained data and deployment gating, optional `gh` webhook
assistance, and durable environment protection. Isolated build, vet and package
tests pass; the incremental host record is below. Alpha.2 is tagged as
annotated `v1.0.0-alpha.2` at
`85f2a4f173de61eb86b843ce66ea62850c804a68` (tag object
`736a5edb332c74645692972cc392a1e96b0b1695`); with explicit authorization the
earlier tag at `7e74edb` was replaced after Publish stable `42312127636` failed
its `1.x` ancestry check. PR #1 squash-merged the tested source onto `1.x` as that
commit; its tree `48fa59d6653247e34c87cb21f13f4c149465c2dc` is identical to
`7e74edb`, the `edge-7e74edbed8bc` source. The replacement tag's Publish stable
`42363078147` and CI `42363078061` passed. The immutable snapshot at
`releases/1.0.0-alpha.2/` reports version `1.0.0-alpha.2` and channel `stable`;
cosign verified its manifest and all four binaries against the tag workflow
identity, every manifest SHA-256 matched, and linux/amd64 reported commit
`85f2a4f173de`. The tagged snapshot was then installed on the authorized
`main`: CLI and node report `1.0.0-alpha.2` / commit `85f2a4f173de`, schema
24/24, `doctor` 20/0/0, `apply --dry-run` 0 of 20; the targeted smoke is in
MANUAL-TESTING. The tag annotation is not GPG-signed; authenticity comes from
the cosign bundles.

Alpha.3 source is published as `edge-9e71713eeff9` (commit `9e71713`,
Publish edge `42387096819`, CI `42387096776`) and host-qualified for the shared
read-only container resolver: a plain Docker image resolved bare `migrate`
commands from cwd fallback, and a Buildpacks image resolved the same bare
release/service/worker/web commands through the launcher's launch-layer `bin`
PATH, with public health and app responses. CLI and `main` were upgraded; node
apply `op_162c42a9c556258cd7123fd40b06965b`; schema stayed **24/24**, `doctor`
20/0/0 and `apply --dry-run` 0 of 20. The test environments, domain bindings and
images were removed and the fixture app was left intact. Alpha.3 is tagged as
annotated `v1.0.0-alpha.3` at
`b9fa7b962fa1520dde8a9acec4c0823389b1633c` (tag object
`9517303a3f5904151018f985a26c8d05fcb5ddc6`) after PR #2 squash-merged the
tested tree onto `1.x`; its tree
`46c7190129a5b65832aa2cdea6e5fcc451e3d1ca` is identical to `fa996d8` and
differs from candidate `9e71713` only in the two docs files. Publish stable
`42390263289` and CI `42390263242` passed. The signed immutable snapshot at
`releases/1.0.0-alpha.3/` reports version `1.0.0-alpha.3`, channel `stable`,
and all four binaries match the manifest; the installer is version-pinned and
syntax-checked. The tag itself was not installed on the authorized node or
manually host-tested there; this is a targeted host qualification of the same
source tree on
`edge-9e71713eeff9`, not a repeated full manual acceptance. Root, `1.x` and RC
pointers remain absent; the edge pointer is `edge-fa996d809378` from the master
merge. The schema-1 release safety guards (snapshot-only alpha/beta, rc-only
pointer, final-only stable/line pointer, strict final stable tag) remain
source-tested;
the existing schema-1 alpha.2 node/client path is compatible, with no
manifest-schema migration or compatibility shim.

Alpha.4 candidate `f1b5440c146bcd605b4ebb4aec978d0b64eb96f1` is published on
the edge channel as `edge-f1b5440c146b`: Publish edge `42518693443` and CI
`42518693394` passed, the signed edge manifest and all four binaries verified
against the pinned `publish-edge.yml@refs/heads/master` identity, linux/amd64
SHA-256 is
`580cdf6c24e91c9af0d91bbe3006cad33e7b52a66e84e6b3f43196a8eec3444a`, and the
downloaded binary reports `edge-f1b5440c146b` / commit `f1b5440c146b`. Root,
`1.x` and `rc` manifests remained 404; no stable pointer moved. The CLI and
`main` were upgraded from `edge-fa33befb675d` to `edge-f1b5440c146b`; node apply
`op_ae5673fc2d52cd61c867489aac65c849` ran `host-database-v1` (migration 026,
schema 25/26 to 26/26), `host-api-service-v1`, `host-worker-service-v1` and
`host-node-exporter-v2` (4 applied / 16 skipped). Migration 026's fail-closed
cutover passed with no queued/running/cancelling deploy, rollback, restart or
promote operation; no worker was cancelled and no SQL repair was used.
`server status` reports both sides `edge-f1b5440c146b`, schema 26/26, River 7/7;
a second real apply was 0/20, `apply --dry-run` had no pending step, and
`doctor` is 22 pass / 0 warn / 0 fail including `telemetry.local`. The
textfile/systemd collector extension is host-qualified on this build. D23
promotion has a targeted host pass on the authorized `a2defs1106` fixture:
preview, exact staging digest `sha256:b472757e…` in production, production
`/build` config, same-key replay, duplicate refusal, a forced failed candidate
that left the previous release serving, and rollback were observed. A second
fixture pass pushed two scratch images through `eregion image push` and completed
the release-command and runtime-secret rows: staging's command ran only in
staging (`build=staging`, `staging=set`, `promo=unset`); production's command ran
once on promotion (`build=production`, `promo=match`, `staging=unset`); restart
and rollback logged `release command already succeeded for this release` and
executed no command step; and rollback to the earlier promoted release restored
its recorded secret/revision snapshot (`/secret` `promo=match`, `/`
`alpha4-prod` while the current revision carried the rotated secret/hash and
`alpha4-prod2`). The fixture reported only set/match state and never printed a
secret value. No Cloudflare token or DNS record was touched, and no app deletion
or TOTP action was attempted. The designated hostname was reused from its
existing proxied record and returned to unbound/525. The candidate was
subsequently tagged `v1.0.0-alpha.4` at `518a006` (tag object
`fb11ba2d932c4d5978151353e4db918cf2fbd6e73`) and published by Publish stable
`42527080717`; this record describes its edge candidate and targeted host
qualification, not a beta claim or a stable pointer change.

The 2026-11-06 alpha.2 protection redesign is host-qualified on `main` at
`edge-6d1ae98b9ac1` (commit `6d1ae98`, edge run 42285442054); the cancellation
builds `edge-81998646916d` and `edge-7e74edbed8bc` followed; the tagged
`1.0.0-alpha.2` snapshot at `85f2a4f` (same tree) was installed, and
`edge-9e71713eeff9` now supersedes it on the CLI and `main`; their host
verification is recorded below. The CLI and node
were upgraded; node apply `op_fca44c39b625e2216d05d7581de99c18` applied
migration 024, the node reports schema **24/24**, River 7/7, `doctor` 20 pass /
0 warn / 0 fail, and the repeat `apply --dry-run` was 0 of 20. Migration 024
backfilled every pre-existing environment (`deployab`, `githello`, `guided`,
`img`, production) as protected; none was unprotected, and no existing row was
lowered.

On fixture app `a2defs1106`, guided file-free `app create`
(`op_0e2c1e66043489dc1941825c705a8c1a`) committed production and staging
protected in one submission without a TOTP prompt, and `SHARED_SECRET` existed
in both namespaces. Shared-recipe `pr-42` was created unprotected
(`op_1724f76965e70df26a026cfde545ceb8`) and deleted with exact confirmation in
JSON/no-TTY (`op_9eb5aa06b06766f163adfbe61b2c74bc`). Protected `env delete`,
`env unprotect` and `app delete` refused with `input_required`; direct node
`platform.app.delete` and protected `platform.environment.delete` refused
`precondition_failed` with `reason=code_required`. `env protect` raised
`prot-audit` agent-safely (`op_e248d36683e6317674fc36b9c5711439`), after which
its delete and unprotect refused. `env update --from` with `--confirm-domain
eregion-alpha-two.mbvlabs.com` removed that alias from production
(`op_134d51a4d723161563903870b4389275`), leaving only the placeholder primary
`a2-primary.invalid`.

Fresh arbitrary-name creation on the same fixture used the name default:
`fresh-audit` (`op_4fa609d1c8502d17ab4e11baa2b4de02`) and shared-recipe
`recipe-audit` (`op_a1505d66121293de4f0c8b9931de94c4`) were both unprotected,
and a same-key replay of the former returned the same operation. Both were
deleted unattended with exact confirmation (`op_d1144d2a44c97b3b8b284f827b6806ba`,
`op_17179495fec1805fad3341a51366b0a8`); `FRESH_ONLY` stayed only in the
`fresh-audit` namespace. Protected delete, unprotect and app-delete refusals
were re-confirmed with `input_required`.

The same fixture passed the remaining audited cases. A deliberately unhealthy
resource apply failed and a corrected retry reused volume
`er-res-e8ea4b4fc7468548f507969953efa8e0-cache-data` and reached ready
(`op_452dd3b405d027e46fc6e98fbc0cf6d9`); destroying it while a revision
referenced it was refused; file-based removal of its declaration retained the
container, volume and Redis key (`op_4aebafcf4f3afdce9fc64caccfb3803d`);
`--delete-data` removed only the owned volume
(`op_298de5b1c2fc97cdc4b45c8ca40d6017`) while an unrelated pre-created foreign
volume remained untouched. That did not test a deletion target with foreign
ownership. A separate `foreign` resource was default-destroyed, its owned
volume replaced with the exact same name carrying foreign labels, and
`resource destroy --delete-data` (`op_6eefcf4cb76445e54433544241fb65cf`)
refused `precondition_failed` with “belongs to another owner; refusing to
delete data”; the foreign volume remained. After removing that test volume, a
second `--delete-data` (`op_aab313d6e42aed65888ca7564a742f62`) succeeded. An
external dependency without a managed resource deployed and served
(`op_14d8ba9c5463d48b8b726c64060c363a`, `/external` = `external 200`); a
degraded resource blocked its dependent deploy while the serving release stayed
up; an ordinary deploy ignored a rogue `eregion.toml` in the working directory;
and failed `gh` setup followed by `--rotate-webhook-secret --github-webhook`
created one hook (`708330096`, secret revision 3). Push `8fa8c11` produced
GitHub delivery HTTP **202** and
`op_73c9e2addecfd556686d431b1fd4c916`, serving
`rel_c2df6de6c35011f6be98b270c859fc7e`. The hook and test branch were deleted;
only the pre-existing `githello` hook and original branches remain.

Actual interruption was tested with a worker SIGKILL during the health step of
resource `interrupt2` (`op_2eee5d4a5320d71203a4bf075288cf58`): the worker
recovered the orphaned job at attempt 2, the interrupted apply failed, and the
corrected retry (`op_c457f164e6709764f59cd15c48120e8a`) reused the same volume
and reached ready. A separate cancelled interruption attempt left
`resource-audit/interrupt` stuck at `provisioning`
(`op_3eded76779595d3f9d9fbf7b6b8f4d55`). After the `edge-81998646916d`
upgrade, normal reconciliation moved it to `degraded` without SQL; ordinary
destroy (`op_8a880ab27ae28ad86b3b88e6a90d4066`) and noninteractive environment
deletion (`op_905fdccf680fbb8c7d8925702e394d64`) succeeded. Its physical volume
had already been removed during the earlier manual cleanup, so this verified the
state/ledger recovery path; the earlier cache default-destroy/re-apply test
covers physical data retention. A second no-kill retest on
`cancel-ok/cancelok3` accepted cancellation while the health step was in
flight; the step succeeded and the operation still ended `succeeded` with the
resource `ready` (`op_80c0f44da0189678f2faeb427ba35e34`), proving
step-boundary and final-commit cancellation bugs on `edge-81998646916d`. The
fix was published in `edge-7e74edbed8bc`: cancellation is checked at every
resource apply/destroy step boundary, and both actions finalize under the
operation row lock with a `cancelling` recheck before the resource callback. New
regressions cover the successful-step case and the reviewer P1 race between the
last check and transactional finalization for both apply and destroy. The
selected platform, CLI and control-plane packages pass. A host successful-step
cancellation test on disposable `cancel-final` ended `cancelled` with the
resource `degraded`; destroy and environment deletion succeeded. The earlier
`cancel-audit` retest ended failed at the health deadline, not cancelled; no
worker was killed in these retests and no direct SQL was used.

The resource-cancellation state-settlement fix was published in
`edge-81998646916d` (commit `8199864`, Publish edge run `42302477645`). A
cancelled or failed resource operation that had entered `provisioning` or
`destroying` settles the resource row to `degraded` in the same terminal
transaction, fenced by the row's `operation_id`; image, container, network and
the cumulative volume ledger are retained. The normal maintenance reconciler
also sweeps rows left in those transient states by operations that were already
`cancelled` or `failed`, so the existing `resource-audit/interrupt` stuck row
converged after the fixed worker started, without a repair command or direct
database edit. Ordinary destroy and environment deletion then succeeded. No
schema migration was required.

The follow-up step-boundary/finalization fix was published in
`edge-7e74edbed8bc` (commit `7e74edb`, Publish edge run `42309997476`, CI
`42309997630`). Resource apply/destroy now check cancellation at every step
boundary and finalize under the operation row lock, rechecking `cancelling`
before the resource callback through the scoped `finishResourceOperation`.
Other completion policy is unchanged. The signed manifest, linux/amd64 hash
`1bee0caf35bb0f1dd6462005ca0b592d18d5915812013c1d13f317594df09284` and binary
version were verified; CLI and `main` were upgraded with node apply
`op_fc5bec9e0e40767839fa8f298682fb87`. Schema stayed **24/24**, River 7/7;
`apply --dry-run` was 0 of 20 and repeated `doctor` runs were 20/0/0 (one
transient host-readiness drift and one known sporadic retention-read warning
were recorded and cleared on retry). A host successful-step cancellation test on
disposable unprotected `cancel-final` ended `cancelled` at a completed network
step boundary with the resource `degraded`
(`op_3bc86507be687b2fa91245565942ee91`); destroy and environment deletion
succeeded. The finalization race is covered deterministically by the new
apply/destroy integration tests. No known cancellation-state defect remains on
this build. MANUAL-TESTING records the evidence.

Residual: `a2defs1106` has production, staging and `prot-audit`, all protected,
and no unprotected disposable test environment. Production serves
`rel_b19f552b5afb5a727b7e7b897f59c181` on the placeholder
primary `a2-primary.invalid`; staging and `prot-audit` have no serving release,
and the `hook` environment is gone. No fixture resource container, volume or
network remains; the fixture registry manifests (`pgv3`, `pgv4`) remain
referenced by the environment revisions and serving app, their local image tags
also remain, and the unused hook image was removed after a reference check. The
designated hostname has no binding and
returns Cloudflare `525`; its pre-existing DNS record remains because no safe
Cloudflare access is available. App deletion still needs the operator's TOTP
factor: `eregion --context main app delete a2defs1106 --confirm a2defs1106`.
No compatibility alias or shim was added: the create input replaces the single
`environment` field with `environments`. No migration text was changed. The
multi-environment same-key replay and last-environment positive deletion were
not re-run on the host. A separate disposable PostgreSQL test on `6d1ae98`
covered the atomic rollback/replay and the last-environment factor admission
with an injected verifier; that verifier was not a real TOTP and the queued
delete was not completed by a worker. The isolated protect-store/CLI tests and
`TestIntegrationDirectDeleteWithRealTOTPCode` use generated local secrets and
real TOTP codes (the direct test uses a local verifier, not the live store); the
live node protect store and any live code were never read. MANUAL-TESTING has
the details.

On 2026-11-06 `edge-ef4a6f879f45` (edge run 42242438039, source `ef4a6f8`)
published and was installed on the CLI and `main`. The node upgrade applied
migrations 022–023 (**schema 23/23, River 7/7**), the second `apply --dry-run`
was 0 of 20, and eleven consecutive `doctor` runs were 20 pass / 0 warn / 0
fail. Alpha.2 definition-file create, shared+environment merge, deterministic
export, export/reimport no-op, secret-name-only output and strict validation
passed on the host without a domain and without a first deploy. A declarative
resource update was accepted, but both resource applies failed on Docker 29.1.3
in the image-identity step: `docker image inspect --format '{{.Id}}|{{join
.RepoDigests ","}}'` returns `wrong type for value; expected []string; got
[]interface {}`. No resource container or volume was created; the fixture
network was removed. Resource health, secret injection, deployment gating and
data retention therefore remain unqualified.

A second alpha.2 defect was found in file-based removal: `env update --from`
reported the last resource as removed and required exact confirmation, but the
new revision still listed it. The patch encoded the empty set as JSON `null`,
which the node treats as "omit and preserve"; `resource detach` removed it
correctly. The same defect was confirmed for removing the last `secret_names`
entry, while a non-empty `config` map removal applied correctly. A
control-database/Funnel outage
(`thelab.tailbbed89.ts.net:10000`) then produced the generic `host.retention`
read warning; it recovered on its own, after which a second `apply` was 0 of
20, `doctor` returned three consecutive 20/0/0 runs, and both degraded resource
fixtures were destroyed with exact confirmation and no containers, volumes or
networks left. At that point the fixture app/environment remained pending the
D7 human factor; it was later deleted. The four existing public URLs stayed HTTP
200 throughout.

Both defects were fixed in `ba17535` and retested on `edge-ba175353f00a` (edge
run 42250484640): CLI and node upgraded, schema stayed **23/23, River 7/7**, the
second `apply` was 0 of 20, and Redis and PostgreSQL resources provisioned with
the Docker 29.1.3 image-inspect fix, healthy exec checks and set secret
injection. File-based provisioning and explicit empty `resources` /
`secret_names` removals applied; resource attach/detach and default
destroy/re-apply preserved Redis and PostgreSQL data; `--delete-data` removed
only the owned volumes and left a separately created foreign volume untouched
(not an ownership-refusal test). The fixture
network, containers and volumes were removed, and both resource rows are
`destroyed`. The fixture app/environment was later deleted with the D7
factor; the cleanup verification is recorded below.

Deployment gating was exercised on the same fixture. With the managed Redis
deliberately unhealthy, `deploy` refused `resource:cache`; with it healthy and
the authorized `eregion-alpha-two.mbvlabs.com` primary domain (pre-existing
proxied DNS, no record changed), deploy `op_a8229010…` succeeded, Caddy issued
a valid certificate, and HTTPS `https://eregion-alpha-two.mbvlabs.com/` returned
`a2defs1106 app; alpha2:key=alpha2-serving`, proving the serving application
read the managed Redis. A wrong-port nginx candidate failed health and left
the previous release serving; a corrected nginx revision then served 200.

The authorized `gh`-assisted webhook flow created hook `708267694` for
`a2defs1106/production`, returned `unchanged` with the same id on repeat,
rotated/updated the same hook, and never touched the pre-existing
`githello/production` hook. Auto-deploy was then disabled and the hook deleted;
the manual already-enabled path returned the URL and secret name without a
value. At that point a real push delivery had not been sent because
commits/pushes were outside the authorization; it was later authorized and
passed in the end-to-end test below.

The user then completed the TOTP-gated app deletion as
`op_90e6c3530ef81a575bba1542a70cda0b`: workload and route cleanup succeeded,
and the app, environment, domain, release, deployment, build and secret rows
are gone. Read-only checks found no fixture container, network or volume; the
hostname now returns Cloudflare `525` because Caddy no longer has its route or
certificate, while the pre-existing Cloudflare DNS record still resolves and
was not changed. At that point the registry image
`127.0.0.1:5000/a2defs1106:alpha2` remained as a non-platform artifact; the
identified image artifacts were removed later. The four existing public URLs
stayed HTTP 200 and `doctor` was 20/0/0.

The user then authorized recreating the fixture and a test push to
`mbvlabs/eregion-hello`. The existing default branch, absence of workflows or
deployments, and the single `githello` hook were inspected first. A separate
`alpha2-webhook-20261106` branch added a `/marker` endpoint and was never merged
into `main`. `a2defs1106/production` was recreated from that branch with the
authorized domain and `GIT_READ_TOKEN`; `--auto-deploy --github-webhook` created
hook `708277604`. Push `f6bbfa7` produced GitHub delivery `2a29a758…` with HTTP
**202** and operation `op_1380428ab35058512af820f5d7807a61`, which built release
`rel_29c45e28…` and served `/marker` 200. The manual path was then exercised:
the one-time secret was captured only in mode-0600 temporary files, sent to the
hook on `gh api` stdin, never printed, and shredded; hook update succeeded and
the secret reached revision 3. Push `c4507c1` produced delivery `c68a318a…`
with HTTP **202** and operation `op_b4dc5f8b225d318510c10293aff9ebaf`, which
built `rel_df70f2bf15a82d6cc0204635460c5e92` and served `/marker` 200. With
`gh` absent from `PATH`, the JSON command reported `skipped` and kept manual
setup available without prompting; the missing-admin-permission variant was
not tested at that point. A later simulated permission response covered the
fallback without changing repository permissions or credentials.

Auto-deploy was disabled, hook `708277604` and the test branch were deleted,
and the desired source ref returned to `main`. The user then completed the
recreated fixture's TOTP-gated app deletion as
`op_362bea6789b9617855c43b37a093e16e`: read-only checks found no fixture
app/environment/domain/resource/release/secret rows, container, network or
volume; the hook and test branch are gone and `main` is unchanged at `59017d6`.
The authorized hostname now returns Cloudflare `525` because no route or
certificate remains, while the pre-existing DNS record still resolves and was
not changed. The three identified fixture registry/image artifacts were removed
after confirming no container referenced them: registry manifest delete
returned 202 for `127.0.0.1:5000/a2defs1106@sha256:2c7b048f…`, and local tags
`a2defs1106:alpha2`, `eregion/a2defs1106-production:1380428ab350` and
`eregion/a2defs1106-production:b4dc5f8b225d` are gone. The DNS record cleanup
remains blocked: no Cloudflare CLI, configuration or authorized API access was
available without reading a secret. Under the incremental qualification policy,
alpha.2 does not require a fresh-server baseline; it is not qualified because
several acceptance criteria remained unrecorded. A later disposable pass
covered the items below.

A further authorized disposable pass on `edge-ba175353f00a` closed more alpha.2
evidence. File-created staging and flag-created `flags` desired states exported
identically after normalising the environment name and reimported with no
change; production and staging had distinct overrides. A managed PostgreSQL
resource with the `POSTGRES_PASSWORD` vault name was consumed by a test app
(`PGHOST=db`, `PGSSLMODE=disable`), served `/db=pg-persist` over HTTPS, and kept
the same marker across `restart` and a `config set`/redeploy that served
`/build=v2`. A direct node `operations.submit` carrying an old
`expected_revision_id` was refused with `conflict` (“the environment changed
since the definition was read”) and wrote no revision. A simulated `gh`
permission response made `--github-webhook` report `skipped` with manual setup
available, without changing repository permissions or credentials.

`--confirm-domain` remains untested because only one authorized hostname exists
and the last primary domain cannot be removed. An explicit external dependency
without a managed resource was not tested because no authorized external
endpoint exists. The Postgres resource was default-destroyed after moving the
environment to nginx; its owned volume
`er-res-2d99b1ef6d8ddc69b01e2271e5a071a2-db-data` remained after the user's
TOTP app deletion `op_c113f640932b31df52c4a92117f3d105`, with its
`io.eregion.*` ownership labels and no container references. That confirmed the
untracked-volume outcome; the authorized volume was then deleted and no fixture
container, network, volume or image remains. The app, environments and domain
binding are gone, the hostname returns Cloudflare `525`, and the pre-existing
DNS record still resolves. MANUAL-TESTING has the detailed evidence.

The operator then changed the qualification policy: a fresh-server baseline is
not required for every alpha; alpha.1 stays operator-qualified; alpha.2 is
qualified incrementally against its own criteria; macOS acceptance remains
required before v1.0.0 final. During the next audited pass, guided file-free
create passed (`op_29b5ffe04dbda9e724960853e8aef430`) and an interrupted
PostgreSQL provisioning retry preserved the volume identity and reached
`ready` (`op_8c6283fba697d32cfd66791073895178`). The operator then stopped new
mutating tests. The unattached resource was destroyed with `--delete-data`
(`op_40b47b8f8d0d242c802c4d6509bb450e`), its network and test images were
removed, and no fixture container/network/volume/image remains. The recreated
`a2defs1106/production` app, domain binding and secret row remained pending the
TOTP-gated delete. The later `edge-6d1ae98b9ac1` pass recorded above recreated
the fixture with protection and completed the outstanding audited cases; its
residual state is recorded there and in MANUAL-TESTING.

`host.retention` remains intermittently 19/1 while its data is healthy; the
retention check shares one five-second deadline across three control-database
reads and hides the underlying error, consistent with a transient Funnel read
rather than retention state. `host-readiness-v1` likewise timed out in some
`apply --dry-run` runs while `/readyz` returned 200 in 0.35–8.1s; immediate
repeats were 0 of 20, consistent with the same transient control-database
latency rather than a host configuration change.

On 2026-11-06 the authorized alpha.1 tag was moved to `37d4f19` and its
immutable snapshot published through the `edge` GitHub environment. The
tagged workflow succeeded with the stable-environment job skipped. Its signed
manifest and all four platform binaries verified against the exact tag workflow
identity and their manifest hashes. Stable and RC pointers remained absent;
the edge pointer did not change during tag publication. The CLI and `main` run
`edge-37d4f19bee05`, schema 21/21, River 7/7, doctor 20/0/0, and a second
apply was a no-op; four serving URLs returned 200. This release publication is
separate from the qualification record; alpha.1 qualification rests on the
operator confirmation above and MANUAL-TESTING.

Beta scope status (2026-11-09): node-local telemetry is host-qualified on
`edge-fa33befb675d` for the snapshot and on `edge-f1b5440c146b` for the
textfile/systemd extension. The alpha.5 WireGuard-only dedicated Caddy telemetry
instance, HTTP service discovery, Prometheus/Alloy config generation and bcrypt
credential rotation are host-qualified on `edge-2767500209f1` (commit
`2767500209f1`, Publish edge `42611197118`, CI `42611197237`). The signed edge
manifest and all four platform binaries verified against the pinned
`publish-edge.yml@refs/heads/master` identity; linux/amd64 SHA-256 is
`2be86705a94497d12bcf152c73746654460fae00c1d63fc1357da47eab3345d2`. The CLI and
`main` were upgraded; node apply `op_591b1885bcaf69397d1e1313917e4bcc` applied
`host-api-service-v1`, `host-worker-service-v1`, `host-node-exporter-v2` and
`host-telemetry-v1` (4 applied / 17 skipped), leaving schema 26/26 and River
7/7. node_exporter is loopback-only on `127.0.0.1:9100`; the dedicated telemetry
unit binds `10.78.0.1:9101` only, is separate from public ingress Caddy, and is
checked by `telemetry.endpoint`. Before rotation there was no telemetry
Caddyfile or local credential, so the first rotation invalidated no existing
scraper credential; a second rotation then made the previous password 401 and
the new one 200. Unauthenticated `/targets.json` and `/node` returned 401; with
the credential both returned 200 through the operator's WireGuard tunnel, with
one target `10.78.0.1:9101` and `__metrics_path__=/node`. Generated Prometheus
and Alloy configs use that WireGuard discovery URL and a password placeholder in
ordinary output; owner-only `0600` output files carried the real credential and
the Prometheus form passed `promtool check config`. Public
`46.224.129.96:9101` was unreachable and no wildcard telemetry listener was
bound. `doctor` is 23 pass / 0 warn / 0 fail; repeated real applies before and
after the second rotation were 0 applied / 21 skipped and left the telemetry
Caddyfile, unit and process unchanged; the
four existing serving URLs stayed HTTP 200. No app, domain, DNS record or
Cloudflare token was mutated, and public Caddy was not restarted. Stopping
WireGuard or node_exporter was not tested, and the hostname-less control-plane
variant was not exercised. `eregion metrics show` still reads one request-scoped
snapshot through the operator channel. There is no telemetry server,
Prometheus/Loki/Grafana deployment, external backend, global PromQL, metric/log
store, retained history, public endpoint or central scrape target; missing
observations and exporter errors are explicit, and CPU utilization is a checked
counter delta that is unavailable after a reset. The `main` node's `server
status` and `doctor` include `node-exporter.service` and `caddy.service`; the
dedicated telemetry unit is checked by `telemetry.endpoint`. Migrations 025 and
026 remained applied in that historical run (schema 26/26). Cloudflare DNS
automation was implemented but **not host-qualified at that run**; no token or
DNS record was read or changed then. Alpha.6's later core provider qualification
is recorded above. Backups were deferred at that run (D4 now defines the
documentation-only recovery gate), and external
PostgreSQL is control-plane state, not a backup of managed-resource volumes. A
future optional separate telemetry node or Grafana is not prohibited long-term;
it is out of beta/v1 scope and needs its own topology, retention and access
decisions.

Historical source state 2026-11-04: rows 1–15 and 17 were implemented in the
integrated source on `master`. Row 17 (retention) had isolated tests and was an
alpha.1 gate; host qualification was then partial. Defects 5 (worker registry
inventory permissions) and 6 (local image removal during prune) and the two
2026-11-04 retention findings (same-day daily retry and imageless-release
resolution) are fixed and host-verified; at that point the row still needed
registry-GC coverage, the 30-release check and a fresh-server baseline. Row 16
(alpha qualification) was then in progress; both rows are now closed for
alpha.1 by the operator confirmation above.
Rows 11–15 went through isolated implementation lanes and independent review,
with rework and orchestrator acceptance; the final release-command
evidence-classification correction was made by the orchestrator.
Combined format, build, vet, darwin/arm64 build and selected isolated package
tests pass. The PostgreSQL integration suite passes (without the race detector)
against a disposable PostgreSQL 17 after the 2026-10-28 fixture fixes. Schema:
embedded migrations through 020; applied migrations 001–013 are unchanged.
**At that point alpha qualification was incomplete.** On 2026-10-29 the existing
`main` node upgraded from edge-8a7179d6d95d through the fixed signed upgrade path;
CLI/node now run edge-e344781ced4b, schema 16/16, River 7/7, doctor 18 pass,
and a second apply is a no-op. Private git cloning and a Buildpacks build were
verified. The first app deploy exposed a release-command launcher/timeout leak,
fixed and published in e344781; the retry verified that command and candidate
health but failed TLS/public verification. A further diagnostic retry confirmed
route reconciliation removes the candidate route before verification ends.
The route-selection correction and longer public-check window are published
in edge-5329590a3f48, installed on the CLI and main, with green CI, clean doctor
and a no-op second apply. Host polling verified the candidate route survives
reconciliation for the full verification window. Initial retries still failed TLS. After disabling proxy on the primary exposed
an incorrect DNS origin (46.225.150.17 rather than main's 46.224.129.96), the
operator corrected both test hosts. The first deployment then succeeded, with
web HTTPS 200 and worker heartbeats. A newly bound alias obtained a valid origin
certificate and served HTTPS 200 while Cloudflare-proxied throughout. The
primary is currently DNS-only; Caddy was manually upgraded to 2.11.4 before
DNS correction, so no version-specific certificate defect was established.
On edge-6d0e6bcdf65a (Caddy 2.11.4 pin published), the app baseline progressed:
second-commit/config/secret deploy, continuous requests through both domains,
failed-health isolation, rollback with pinned secret revisions, web×2 and
restart with release-command skipping, monorepo build, moved-branch build retry,
create replay and serving-container repair passed. A build-stop test exposed a
host-lock deadlock-by-serialization: its cancellation marker waited for pack to
finish. The narrow fix lets only that private signal bypass the host lock,
with regressions; published edge-ef91d70f6215 passed the host retest (pack
interrupted, deploy exit 130, prior release preserved). Route-write failure and
automatic recovery, alias removal and unsigned/dev upload rejection also
passed; doctor remains 18/0/0. A resumed run on the same build passed a changed
release command re-running, restart/scale refusal after a failed command,
previous-container cleanup obstruction with later maintenance removal, and a
rejected Caddy reload with restore and automatic recovery. It also found that
the D17 switch is **not** zero-downtime under concurrent clients: each Caddy
config load resets a few in-flight TLS handshakes (2–3 per switch with four
parallel clients) because Caddy rebinds its SO_REUSEPORT listener. A runtime
`net.ipv4.tcp_migrate_req=1` removed every reset in four dense switches. The
fix, a new `host-ingress-sysctl-v1` step, is published as edge-42a0541c09d4
and installed on main (second apply no-op, doctor 18/0/0); four dense switches
then had zero failures in 6,262 requests. Fresh init, human-factor-positive flows
and other baseline variants were not yet qualified then; alpha.1 is now closed
by the operator confirmation above, without inventing those results. See
MANUAL-TESTING's “Resume testing” section for the saved target, fixture state,
evidence locations and historical gaps, and its run records for operation IDs.
Row 16 was then in progress; row 17 was implemented at source level and an
alpha.1 gate, not yet fully host-qualified (defects 5 and 6 and the two
2026-11-04 retention findings are fixed and host-verified; retention still
lacked registry-GC, 30-release and fresh-server coverage). Both are closed for
alpha.1 by that confirmation.

On 2026-10-30/31 a further edge-42a0541 session passed release-changing dense
switches (7,673/7,673 switch-window HTTP 200, 8,235 including the pre-switch
phase, across a fresh build, a redeploy and a rollback, with the body changing
between v3 and v3 src3), relative and absolute symlink-escape rejection,
same-key replay after the branch was deleted,
rollback refusal for a release whose command never succeeded, failed-candidate
isolation with a cleanup-removal fault (the leak was recorded; later maintenance
removal was not re-observed), interleaved domain changes with 7,074/7,074
monitor responses, the dev-CLI dry-run version mismatch with the node unchanged,
and terminal detach/replay plus explicit cancel. Live `build logs --follow` was
observed without saved evidence. Two defects are fixed after the run and one
remains open. Fixed: the Caddy public path `/v1/environments/*/deployments`
matched one segment and returned 404 (2943e18, with a matcher regression test),
and a failed container cleanup was summarised as "candidate cleanup completed"
(4e827aa, now a warning event naming the containers). **Open defect 3:** a
worker SIGKILL leaves its platform operation running until River's rescue,
about 30 minutes (30m21s observed on op_f0d20fed6c26eb6bd138c84bd211e170); the
fix was then in progress for alpha.1 by rescuing this host's orphaned platform
jobs on worker start. Neither fixed defect had been host-retested at that point.

On 2026-11-03 a further run on `main` upgraded `edge-42a0541c09d4` ->
`edge-a77d3bfa4fe6` -> `edge-13fdb2518757` (both signed; the previous CLI binary
was kept) and host-retested defect 1: the API's 405/401 now replace Caddy's 404
on `/v1/environments/githello/production/deployments` while `/v1/apps` stays
404. A second apply is a no-op (0 of 19) and doctor is 18/0/0. The ingress step
reloaded Caddy without restarting it, and continuous curls saw 306/306,
1,773/1,773 and 300/300 HTTP 200 across the reload, a worker-kill deploy and a
node upgrade. Defect 3 is fixed and host-verified: a SIGKILLed worker's
operation was rescued in about 9 seconds instead of 30 minutes, with a warning
event naming the orphaned job. The signed-manifest check passed: with the
release host blackholed on the node, `apply --auto-upgrade-nodes` refused with
`unavailable` naming the version manifest and installed nothing; after the fault
was removed the upgrade completed and applied the new `host-journal-access-v1`
step, after which admin reads the eregion journal without sudo. The same retest
found **defect 4** (fixed in source, not host-verified): a rescued deployment
attempt that already recorded its secret snapshot loses the git read credential
and fails at the build step with `precondition_failed` "git read credential is
unavailable", although a fresh deploy of the same revision succeeds. The
credential's non-secret revision is now pinned into the deployment's secret
snapshot on the first attempt; a legacy snapshot without it gains the pin on the
next attempt while keeping the application pins, and every retry, rescue or
River retry resolves it from that snapshot. Regression tests lose the first
attempt after the snapshot, and rotate the credential between a legacy snapshot
and its retry, requiring the pinned revision. Details and IDs are in
MANUAL-TESTING's 2026-11-03 record.

The evening 2026-11-03 run on `main` upgraded `edge-13fdb2518757` ->
`edge-1e541a7fad3e`, applied migrations 017–019 (schema 19/19, River 7/7) and
`host-registry-v2` once (registry restarted; Caddy was not), and confirmed the
access-changing apply within its rollback window. A second apply was 0 of 19 and
continuous curls saw 633/633 HTTP 200. **Defect 4 is fixed and host-verified:**
after a SIGKILL during the fresh git build, the worker rescued the operation in
about 9 seconds and attempt 2 cloned the private repository with the pinned
credential and served `rel_b7099da06361a0bfaa8ee6866e178152` (op_181c9da0…,
423/423 HTTP 200, evidence 240–246). Retention policy show/set (new environment
revision `erv_b8f5163889b07167f955396f2f5da061`), the `host-registry-v2`
upgrade and the prune dry run passed host-side; the plan kept both apps'
serving and rollback releases and changed nothing. **Defect 5 (fixed in source,
not host-verified):** the worker's daily retention run failed `unavailable`
"the node's registry storage is unreadable" because
`host-directories-v1` created `/var/lib/eregion/registry` as `root:root 0700`
while the worker runs as `eregion`; the operator dry run masked it because its
node RPC path runs as root. `host-directories-v2` now creates the directory as
`root:eregion 0750` on fresh installs and converges an existing node once,
without making the storage writable by `eregion`. The inventory's permission
failure names the path and the `eregion apply` fix, and `host.retention` points
at the same step. `host-packages-v2` installs `polkitd` and
`host-registry-gc-polkit-v1` lets the worker start, stop and restart only
`eregion-registry.service`; the fail-closed GC script is unchanged.

The late 2026-11-03/04 run on `main` upgraded `edge-1e541a7fad3e` ->
`edge-690baafe8c91`. `host-directories-v2` converged the registry to
`root:eregion 0750` (worker read true), `host-registry-gc-polkit-v1` wrote the
root-owned rule and a restart as `eregion` succeeded; `host-packages-v2` found
nothing missing and Docker 29.1.3 / containerd 2.2.2 were not upgraded or
restarted. The automatic daily run became the real prune: 16 githello release
rows were marked pruned exactly as planned, serving `rel_b709` and rollback
`rel_b383` stayed, and monitors saw 2,190/2,190 HTTP 200. **Defect 5 is fixed
and host-verified.** **Defect 6 (fixed in source, not host-verified):** four
images with multiple tags failed
`docker image rm <id>` ("image is referenced in multiple repositories") and a
1.3 GB dangling image was held by the unlabeled stopped container
`ea1e68560301`, which the retention inventory did not see. The run recorded 35
errors, `op_0f541f120149580a1124893389f720b2` failed `unavailable`, four images
and the dangling image remain, registry GC was not reached, and `doctor` is
19 pass / 1 warn (`host.retention`). Rollback to pruned `rel_0170` refuses
correctly. The source now re-reads and verifies every current repository tag
before untagging it (never `--force`), reports every container's immutable image
ID so an unlabeled container protects its image, and records deterministic
conflict refusals as per-item skips with a `succeeded_with_skips` run and a
doctor pass note. Real-prune completion, registry GC and deploy-after-GC
therefore remained unqualified at that point; later runs in MANUAL-TESTING
covered them and alpha.1 is operator-confirmed. Details are in its late 2026-11-03
record.

The 2026-11-04 run on `main` upgraded `edge-690baafe8c91` ->
`edge-786fdcfda4a8` and applied migration 020 (schema 20/20). The startup daily
catch-up did not retry the failed 2026-11-04 operation: its UTC-day idempotency
key was already bound to that terminal operation, so no new automatic run was
accepted. A manual prune `op_11f4602b7a2fd234d352779d74f7bd4a` succeeded in one
attempt: four multi-tagged images were untagged and deleted, the 1.3 GB dangling
image used by unlabeled stopped container `pedantic_jackson` is now
`image_skipped` "used by container", both apps stayed serving through
1,200/1,200 responses, 142,430,208 bytes were freed, and `doctor` reached
20 pass / 0 warn / 0 fail. **Defect 6 is fixed and host-verified.** Registry GC
still was not exercised: the dry run's `registry_skipped` names retained failed
release `rel_53a22cfea2e1f3a2fa9dd523ac4c5510` (no immutable identity), and the
registry contains only retained andurel artifacts. A doctor query intermittently
warns "cannot read the retention runs" (2 of 5 runs); the underlying error was
not captured, so the cause is unknown. Every successful read is 20/0/0. Details are in MANUAL-TESTING's
2026-11-04 record.

Both 2026-11-04 retention findings are **fixed and host-verified on
`0a1a492`**. The daily submitter checks hourly and worker start and retries a
failed or cancelled UTC-day run after a one-hour backoff with a numbered
idempotency key. On the candidate, after the failed 00:06 operation aged past
one hour, it submitted `op_9b72a9e32c2b5b49351f3ba75edb4664` at 01:47:24 with
the next numbered key; it planned 0 items and succeeded at 01:48:03. The manual
prune `op_11f4602b…` was not counted because the submitter only sees
`maintenance` operations with the `retention-daily:<date>` prefix. This is
intended: the one-success-per-day rule covers automatic daily runs only, and
IMPLEMENTATION now says so (two runs succeeded that day: manual at 01:11 and
automatic at 01:48).
The planner now treats a failed or pending release with no image identity as
never having produced an image, and no longer emits `registry_skipped` for a
repository absent from inventory; the candidate dry run contains only the
`image_skipped` for `pedantic_jackson`. The candidate apply was 2 of 20, the
second apply 0 of 20, `doctor` was 20/0/0 in all three runs, a serving-revision
restart succeeded, and monitors saw 1,212/1,212 HTTP 200. Registry GC remained
unexercised in that candidate because the registry held only retained andurel
artifacts; later runs exercised it. Details are in MANUAL-TESTING's 2026-11-04
candidate record.

The failed-follow exit defect is corrected at source level: followed failed
operations return the recorded error code's nonzero status (fallback exit 5),
including guided creates. Success exits 0; remote cancellation and viewer detach
both exit 130 but have distinct messages and state evidence. Isolated regressions
cover plain, JSON and NDJSON. Real-host failed deploy follows returned exit 5,
success returned 0 and the build-stop retest returned cancellation exit 130;
terminal Ctrl-C detach/replay and explicit cancel passed (153/154). Guided
failure and remaining output variants were not recorded in that step; alpha.1
is operator-confirmed without backfilling them.

A source re-check on 2026-10-28 (snapshot `a7478c4`) and a real deployment of a
second app (andurel-site, edge-8a7179d: image push, digest deploy, release
command, web×2 + service + worker, Cloudflare-proxied domains) found open
defects at the edges; the source changes for rows 11–15 address them. The run is evidence, not a
baseline pass: older build, no failed candidate, rollback or zero-downtime
check. Task-level detail for those historical rows lived in the temporary
ALPHA-SPEC, deleted at the alpha.1 tag and available in Git history only.
Current execution tracking is in Linear.

On 2026-11-05 the three reviewed fixes (env-delete preview count, cancelled-pack
cleanup and durable GitHub webhook admission with migration 021) were committed
as `5275924` and published as edge-`527592419f06`. The edge workflow passed and
the signed manifest and binary were verified; the CLI and main node were
upgraded to the same build, migration 021 applied (**schema 21/21, River 7/7**),
`doctor` is 20 pass/0 warn/0 fail, and a second `apply` was a no-op. The
fresh-server batch then passed the corrected token reload (new CI token:
deploy 202, same-actor status 200, revoke, 401), individual secret-export
metadata, disposable `deployab/previewtwo` and `packcancel` deletes, and
webhook-secret recovery after an initial 422 (vault revision 3 active; hook
config PATCH 200; real redelivery same GUID new attempt 202/OK). All four
serving URLs stayed 200 and the API did not restart. The fixture git read
credential was then replaced with the existing authenticated `gh` OAuth token
through `secret set` (`GIT_READ_TOKEN` revision 3; the environment revision
already named it, so no new revision was written). A real empty push to
`mbvlabs/eregion-hello@main` produced a 202 delivery and
`op_232f45c1cd645304e92df57409fb5b83` succeeded: the clone used the new
credential, the build and release command passed, the new
`rel_d231a28c17ce1aa345e6fa3cdfd7ae03` served `59017d6`, and all four URLs
returned 200. The previous ready release remains for rollback. The earlier
fixture-token rejection's root cause was not established. At that point a
baseline pass still had gaps: old-secret rejection after rotation was not run
(the old secret was not available and was not read), and the broader alpha
baseline was unrun. Alpha.1 is now operator-confirmed; these historical gaps are
not backfilled.

Retention preflight on the same build did not run the 30-release check: no
unused verified primary hostname was available for a distinct disposable app,
and the global dry-run plan contained two unrelated dangling images. No fixture
or prune was created. Four serving URLs returned 200; doctor showed cache pass
but a retention-run read warning (19/1/0). Five subsequent bounded doctors
were 20/0/0; the warning cleared but its underlying cause remains unknown.
The query shares a five-second deadline with two preceding database reads and
suppresses the underlying error, so neither a route nor a timeout is proven.
The exact evidence and remaining prerequisites are recorded in MANUAL-TESTING.

On 2026-11-06, `edge-527592419f06` deployed 30 distinct image releases
successfully to the new disposable `retentionqual1106/production` app. Its
Cloudflare-proxied hostname reached applied/valid/public 200, and the four
existing serving URLs remained 200 at the per-deploy checks. Scoped backdating
produced 25 planned release marks, but also 25 image removals attributed to the
existing `img` app because the reused images share identities with its old
releases. No manual prune was authorized for those items and none was run; the
30-release acceptance was incomplete at that point. The backdates were
reversed, and a keep-30 override protected the fixture while the app was held
for review.
After explicit authorization to remove the 25 shared historical test images,
the resumed run pruned exactly 25 of its 30 release rows and 25 local images;
the newest five stayed, pruned-release rollback/deploy refused, and rollback
to the kept target succeeded. A separate prune of an aged fixture's registry
artifacts completed GC and a retained release deployed afterward. Four existing
URLs remained healthy. The 30-release row is partial: those 25 releases'
registry artifacts were still inside the 14-day grace, so their manifest removal
was not observed; registry size could not be measured without human sudo.
The operator then completed factor-gated fixture app deletion; its app,
environment, release and domain rows and Caddy hostname route are gone, and
the deletion operation remains. A subsequent global dry run lists five
previously retained shared historical `img` images, now eligible; no further
prune was run at that point. On further authorization, a second run temporarily
set the server artifact grace to one day and removed those five images plus
120 historical test registry manifests, including the 25 pruned fixture
releases' manifest identities; GC completed and the original 14-day policy
was restored. This is complementary evidence, not a single 14-day-grace
30-release pass: the fixture rows had already been deleted and registry `du`
remained unavailable without interactive sudo. Details are in MANUAL-TESTING.

The subsequent 2026-11-06 `edge-527592419f06` run **passed the 30-release
retention row** in one prune with all 30 rows present: 25 release marks, their
25 distinct images and their 25 matching registry manifests were removed; the
newest five were retained. Synthetic-aged image fixtures and a temporary
environment-level one-day artifact grace were used, and grace was restored to
14 days. Pruned-release refusals and rollback to kept release 29 succeeded;
release 30 served again after GC. Registry storage size was not measured on
this build, so that separate GC acceptance measurement remained unqualified on
this run. At that point the alpha baseline was still incomplete; alpha.1 is now
operator-confirmed by the record above. The fixture was held for human-factor
cleanup; see MANUAL-TESTING for operations and evidence. The operator then
deleted it with `op_777bfba4daf68d989fde2083461ed592`; app, environment,
release and domain rows are gone, operation history remains, doctor is 20/0/0
and the four existing URLs are 200. Five aged, unreferenced fixture registry
manifests were subsequently removed by the measured-GC test below. The server
grace is 14 days.

On 2026-11-06 prune `op_91ba5356aa14c019b8fb7f5138d31e3c` removed exactly
the five previewed fixture manifests and completed registry GC. The operator
transcribed sudo `du` readings of 3,517,816 and 3,303,852 bytes (a 213,964-byte
decrease); those readings were not independently captured. The registry was
active and returned 200. A locally prebuilt image was first pushed after GC and
deployed as `rel_c5403a89567eed250689b3192dcb2347`; its new marker and all four
existing URLs returned 200, and doctor was 20/0/0. The reported decrease is
smaller than the planner's 12,046,645-byte sum, which counts shared blobs per
manifest; this run's measurement and failure-path variants are not a full GC
row pass. Those are historical evidence limits, not open alpha.1 blockers; see
MANUAL-TESTING for the evidence.

| Area | What exists | Alpha.1 qualification |
| --- | --- | --- |
| `server init` | vaire flow: preflight, `--answers`, `--password-stdin`, plan with `--confirm <server>`, binary installed once, vault keyring, loopback node_exporter and WireGuard-only authenticated telemetry endpoint, guarded handoff with `--secrets-out`, `--resume`/`--discard`, `context remove`. SSH on port 22 only. | Closed by operator confirmation for alpha.1; alpha.5 telemetry extension is host-qualified. |
| Operator channel | `eregion node rpc` over SSH in WireGuard; token-free contexts and handoff; version lockstep and `--auto-upgrade-nodes`. | Closed by operator confirmation. |
| Host convergence | `plan`, `apply`, `doctor`, `server status`, `ssh`, `node unlock-ssh`. | Closed by operator confirmation. |
| Apps and environments | App image repository plus revisions with optional Git recipe, processes, config and secret names; guided and flag-only `app create`, `env create|update`; `process`, `config`; deployability gate. | Closed by operator confirmation. |
| Build, release, deploy | Deploy from the bound source (public or private git) or a release; build lifecycle; per-process containers with replicas; blue/green switch with public verification; rollback with pinned secret revisions; restart. | Closed by operator confirmation. |
| Domains | Primary and aliases; DNS, route, certificate and public state; serialized atomic Caddy writes; reconciliation and observation. | Closed by operator confirmation. |
| Observe and remove | `app/env status`, `logs`, `operations list`, `app/env delete` per D13. Over node RPC the operator reads, follows and cancels any operation; the public HTTP path stays actor- and grant-scoped. | Closed by operator confirmation. |
| CI and push to deploy | D7 tokens behind `protect init`; per-environment deploy endpoint; per-environment GitHub webhook via `env update --auto-deploy` with durable admission and atomic deploy linking; `secrets export`/`secret export` behind the factor. | Host checks pass for token reload, secret-export metadata, disposable deletes and webhook-secret recovery; migration 021 is applied on main; a real push deployed `59017d6` as `rel_d231a28c…` with delivery 202 and all four URLs 200. On the same build redelivery returned the original operation without another release, an invalid signature returned 401, and an existing other-branch fixture push was ignored. Old-secret rejection after rotation was not recorded; alpha.1 is operator-confirmed, and no result is backfilled. |
| Surface | `commands --json` lists only section 5; node-only commands hidden under `eregion node`. | — |
| Release | Cosign keyless, `install.sh`, edge on push to `master`, opt-in RC and finals-only stable pointers, exact alpha/beta snapshots; origin `eregion.mbvlabs.com`. | Edge publishes verified for `527592419f06`, `ef4a6f879f45`, `ba175353f00a`, `6d1ae98b9ac1`, `81998646916d`, `7e74edbed8bc`, `9e71713eeff9` and `fa33befb675d`; CLI and `main` now run `edge-fa33befb675d` at schema **25/25**, and the tagged `1.0.0-alpha.2` snapshot remains published (it was installed earlier and then superseded by these edge candidates). The authorized alpha.1 tag was later moved to `37d4f19` and its snapshot published; see the release record above. The authorized alpha.2 tag was replaced at `85f2a4f` after PR #1; Publish stable `42363078147` and CI `42363078061` passed, and the signed `releases/1.0.0-alpha.2/` manifest and four binaries match the tag identity and manifest hashes. Tag replacement and its unsigned annotation are explicit release caveats; `install.sh` is version-pinned but outside the cosign-signed set. Alpha.3 was published by Publish stable `42390263289` (CI `42390263242`) after PR #2; `releases/1.0.0-alpha.3/` verifies against the tag identity and all four binaries match, while root/`1.x`/RC remain absent; the alpha.3 tag was not installed on the host. The edge channel later advanced through beta candidates to `edge-fa33befb675d`. Channel manifest and signature are two objects with a pre-existing publication race; partial tagged uploads can block retry. Both remain follow-up risks, not blockers to the channel model. |

## 9. Historical acceptance reference

**Retired as a work list on 2026-11-10.** Linear owns actionable work and status.
The numbered outcomes below preserve historical contracts and evidence links;
they are not an execution order or a current release checklist. Current
acceptance and run evidence are in MANUAL-TESTING. Older source-only and
not-run statements describe their dated runs, not the latest qualification.

Rows 1–17 are complete for
alpha.1 by the operator's confirmation that all alpha.1 testing is done (section
8). The acceptance criteria below remain the definition of each flow; this
document does not reconstruct the individual results or backfill evidence. The
R10 macOS exception remains outside the alpha.1 gate and is still required
before v1.0.0 final. Rows 18–20 are alpha.2 work, implemented and host-qualified
incrementally on the authorized existing node; `v1.0.0-alpha.2` is published
at `85f2a4f` and installed on CLI and `main`, with the recorded evidence
boundaries and residual TOTP/DNS items in MANUAL-TESTING. The `v1.0.0-alpha.2`
tag publication succeeded at `85f2a4f` after PR #1; its signed immutable
snapshot is published. Rows 21–23 are beta/alpha-ready work: node-local
telemetry and its textfile/systemd/`telemetry.local` extension are host-qualified
on `edge-fa33befb675d` and `edge-f1b5440c146b`. Alpha.6 subsequently qualified
the core Cloudflare flow on `thelab`: human vault setup, create/readback/no-op,
repeat apply and exact-owned alias cleanup. Failure/interruption, multi-binding
deletion and actual foreign-record apply refusal remain live gaps.
Staging-to-production promotion has a targeted host pass on the authorized
`a2defs1106` fixture, including release-command execution, once-only restart and
rollback skip, production secret injection and the recorded secret/revision
snapshot on rollback; concurrent host changes remain offline-only. None of these
rows adds a telemetry server, telemetry database/history, backup product or
additional nodes. The authenticated endpoint, discovery and rotation were
subsequently implemented and qualified in alpha.5/alpha.6.

| Historical # | Outcome / historical evidence | Acceptance contract |
| --- | --- | --- |
| 1 | **Node-mode dispatch.** `eregion node <cmd> --json` over SSH/WireGuard calls the application services; contexts and handoff carry no API token; version lockstep and `--auto-upgrade-nodes`; binary installed once. | Separate-install login works; mismatched version refuses before mutation; no token in context or handoff. |
| 2 | **`server init` to vaire's shape.** Preflight, `--answers`, `--password-stdin`, `--confirm <server>`, `--resume`/`--discard`, handoff with stored gate and `--secrets-out`, vault keyring, loopback node_exporter with an authenticated WireGuard-only telemetry endpoint, `context remove`, `secrets export` behind the human factor. | Interrupted-init and lockdown-revert checks pass; JSON/no-TTY never prompts. |
| 3 | **Host convergence.** `plan`, `apply`, `doctor`, `status`, `ssh`, `node unlock-ssh`. | A second `apply` is a no-op; an upgraded node re-runs only changed steps. |
| 4 | **App and environment model.** Migration for the app image repository, optional Git recipe, revisions, processes and deployability; `app create`, `env create|update|protect|unprotect|list|status`, `process list|scale`, `config`, secrets per D18. | Guided and flag-only paths save the same revision; image-only and Git deployments both work without raw JSON. |
| 5 | **Deploy.** `deploy` from source (public or private git) or release, build lifecycle commands, release command, per-process containers with replicas, blue/green per D17, public verification, rollback, restart. | Two deploys and a rollback verified at the public HTTPS URL with no failed requests during the switch; a failed candidate leaves the old release serving; a worker process runs alongside web. |
| 6 | **Domains.** Primary/aliases, DNS check, desired/applied/observed route state, serialized atomic Caddy writes, reconciliation on start and retry. | Failed reload and interrupted change recover; removing one domain leaves others intact. |
| 7 | **Observe and remove.** `app/env status`, `logs`, `operations list`, `app delete`, `env delete` per D13. | Status shows serving release, processes and per-domain state; deletion cleans containers, routes and secrets and keeps history. |
| 8 | **CI and push to deploy.** Per-environment deploy endpoint through Caddy, `token create|list|revoke`, `protect init`; per-environment GitHub webhook deploying pushes to the bound branch (D28), replacing `EREGION_WEBHOOK_SECRET_FILE` and `EREGION_WEBHOOK_ALLOW`. | CI deploy works; expired, revoked and wrong-target tokens fail; `--yes`, JSON and stdin cannot mint a token; a push deploys, a redelivery does not deploy twice, a bad signature or other branch does nothing; a signed push returns 202 only after its durable admission commits, a slow control database returns a bounded retryable 503 rather than an optimistic 202, and a redelivery returns the original operation. |
| 9 | **Surface cleanup.** Remove the D6 surfaces and the resources/backup code (D19); node-only commands hidden; `guide agents`, `skill install`. | `commands --json` lists only the section 5 tree. |
| 10 | **Release pipeline.** D14–D15: edge on push, `1.x`, tags, cosign, install script, channel manifests, Linux/macOS only. | An edge build installs on Linux and macOS and `upgrade` moves between two edge builds. |
| 11 | **Trust boundary (D7, D12).** The factor is verified inside the same request for token create, both exports, `app delete` and `env delete`; dry run never installs a node binary; a node on a release build installs only a cosign-verified release binary. | A direct `vault.export`, `handoff.export` or delete submission without a valid code fails on the node; `--dry-run --auto-upgrade-nodes` leaves the node build unchanged; an unsigned upload to a release-build node is refused. |
| 12 | **Deterministic builds.** Git builds honour `context_path`; the ref is resolved to an exact commit at submission and stored on the build; build, retry and rollback use only that commit. | A monorepo subdirectory builds; a retry of a failed branch build rebuilds the original commit after the branch moved. |
| 13 | **Workloads and probes (D17, D21, D29, D31).** Release-scoped release command; recorded and retried container cleanup; serving-container repair at worker start; per-domain check path; public verification logs host, path and status; the observer covers every domain. | Scale, restart and rollback do not re-run a succeeded release command; a removed serving container returns after a worker restart; an alias with its own check path becomes ready; a leaked container shows in `env status` and is removed later. |
| 14 | **Operator contracts.** Atomic `login` with `--replace`; same-key create retry returns the original operation without writing secrets again; `--context`/`EREGION_CONTEXT`; `logs --json` is one envelope (NDJSON only with `--output ndjson`); every operation-following command streams the same NDJSON events; `domain add --json` returns an object; `ssh --command`. | Each has a regression test; a failed import leaves an existing context working. |
| 15 | **Guided finish.** Guided `app create`/`env create` follow build and deploy to serving or failed, Ctrl-C detaches (vaire's flow); `server init` ends with vaire's doctor gate (`doctor_failed`, `--resume`, `--skip-doctor`). | Guided create ends at a serving URL or a failed operation with its error; init on a healthy host ends with a clean doctor summary. |
| 16 | **Alpha qualification.** MANUAL-TESTING baseline on a real server for the tagged build. | Complete: the operator confirmed all alpha.1 testing is done. No baseline record is reconstructed here; release authorization is separate. |
| 17 | **Disk retention (D30), alpha.1 gate.** Policy, planner, periodic job, `retention show|set`, `prune [--dry-run]`, registry deletes and GC, `doctor` retention and Buildpacks cache rows. Implemented and tested at source level; alpha.1 host qualification is operator-confirmed. | A node with 30 releases keeps exactly the policy's set; `prune --dry-run` lists what would go and the bytes; rollback to a pruned release refuses clearly; a second `apply` stays a no-op. |
| 18 | **Optional app definitions and managed resources (D19, D32), alpha.2.** Implemented and published; host-qualified across `edge-6d1ae98b9ac1`, `edge-81998646916d`, `edge-7e74edbed8bc` and the tagged `1.0.0-alpha.2` snapshot (identical tree). `eregion.toml` input for `app create`, `env create|update` (`--from`) and `app export`, with shared defaults and selected environment overrides. Generic Docker resources have environment ownership, private networking, secret references, optional persistent volumes and durable lifecycle; explicit apply provisions them, while ordinary deploys never read the file. Existing file-free workflows remain supported. On 2026-11-06 `edge-ef4a6f879f45` passed definition create/override/export/reimport, secret-name-only storage and strict validation on `main`. Its first host run found the Docker 29.1.3 image-inspect and empty-slice removal defects; both fixes were retested on `edge-ba175353f00a`: Docker resource provisioning, file-based empty resource/secret removal, attach/detach, default destroy/re-apply data retention and delete-data cleanup passed for own Redis and PostgreSQL fixtures. An unhealthy resource was refused at the gate; after the resource was healthy, a real deployment served `a2defs1106` over HTTPS at the authorized `eregion-alpha-two.mbvlabs.com`, and its root response read the managed Redis (`alpha2:key=alpha2-serving`). A failed candidate left the previous release serving. The user then deleted the fixture with the D7 factor (`op_90e6c3530ef81a575bba1542a70cda0b`); read-only checks found no fixture app/environment/domain/resource rows, containers, networks or volumes. The pre-existing DNS record remains; identified fixture images were later removed too. A later disposable pass covered file-vs-flags/staging overrides, managed PostgreSQL app consumption and restart/redeploy persistence, stale-plan refusal and a simulated missing-admin fallback. The user then deleted the recreated fixture (`op_c113f640932b31df52c4a92117f3d105`); the default-retained PostgreSQL volume persisted untracked with its labels and no container references, then the authorized volume was removed. A later guided file-free fixture and failed-health provisioning retry passed; after the policy stop its unattached resource, network and images were removed, but that app/domain/secret row remained pending TOTP deletion. On `edge-6d1ae98b9ac1`, a failed-health provisioning retry reused its volume and reached ready; an actual worker-SIGKILL interruption was recovered at attempt 2 and a corrected retry reused the same volume and reached ready; destroying a resource with a dependent revision was refused; removing its declaration retained the container, volume and Redis data; `--delete-data` removed only the owned volume while an unrelated pre-created foreign volume remained untouched; a separately named deletion target with foreign ownership was explicitly refused; an external dependency without a managed resource served; a degraded resource blocked its dependent deploy while the serving release stayed up; a rogue `eregion.toml` in the working directory was ignored; and `env update --from` with `--confirm-domain` removed the designated alias, leaving a `.invalid` primary. A cancelled interruption attempt left `resource-audit/interrupt` stuck provisioning; after the `edge-81998646916d` cancellation fix, normal reconciliation moved it to degraded, and ordinary destroy plus noninteractive environment deletion succeeded. A no-kill cancellation retest then proved step-boundary and final-commit bugs: an accepted cancel during a successful health step still ended succeeded with a ready resource, and a cancel could commit between the last check and transactional finalization. The fix was published in `edge-7e74edbed8bc`: cancellation is checked at every resource step boundary and a scoped row-locked finalization rechecks `cancelling` before the resource callback. The apply/destroy finalization regressions pass, and a host successful-step cancellation test ended `cancelled` with the resource `degraded`; destroy and environment deletion succeeded. No known cancellation-state defect remains on this build. Details are in MANUAL-TESTING. | File and equivalent flags save equivalent supported desired state; export round-trips without secret values; unknown keys and secret-value fields are refused. Dry-run previews changes; destructive removals require target confirmation; detaching a resource retains its data and destruction is separate. A managed resource can be consumed by the app, and an external dependency still works without one. |
| 19 | **Optional `gh`-assisted webhook setup (D28), alpha.2.** Implemented and host-qualified for the main paths; extended on `edge-6d1ae98b9ac1`. Check authenticated `gh` and repository hook permission; create or update the matching hook, retaining manual setup without `gh` or permission. No GitHub App; no plaintext secrets in ordinary output, events or arguments. | On 2026-11-06 against the authorized private `mbvlabs/eregion-hello`, `--auto-deploy --github-webhook` created hook `708277604` for `a2defs1106/production`, a repeat reported `unchanged`, and rotation updated the same hook. A real push (`f6bbfa7`) produced delivery HTTP 202 and webhook operation `op_1380428…`, which built `rel_29c45e28…` and served `/marker` 200. The manual one-time secret was handled in 0600 temporary files, sent on stdin, never printed and shredded; push `c4507c1` produced delivery 202 and `op_b4dc5f8b…`, serving `rel_df70f2b…`. The missing-`gh` fallback reported `skipped` with manual setup available. A temporary `gh` shim returning `permissions.admin=false` made the fallback report the missing-admin case and keep manual setup available without changing repository permissions or credentials; no real non-admin account was tested. Hook and test branch were deleted. The recreated fixture was later deleted with the D7 factor (`op_c113f640932b31df52c4a92117f3d105`); read-only checks found no fixture rows, container, network or image, and the default-retained PostgreSQL volume was observed untracked with its ownership labels then removed. The three identified image artifacts were removed after reference checks; the pre-existing DNS record remains because no safe Cloudflare access was available. On `edge-6d1ae98b9ac1`, a simulated failing hook call enabled auto-deploy without creating a hook; after disabling it, the real `--rotate-webhook-secret --github-webhook` created hook `708330096` (secret revision 3). Push `8fa8c11` produced delivery HTTP 202 and `op_73c9e2addecfd556686d431b1fd4c916`, serving `rel_c2df6de6c35011f6be98b270c859fc7e` with `/` returning `eregion sample: hello-v1 src3`; hook and test branch were then deleted. |
| 20 | **Environment protection and app-create defaults (D33), alpha.2 redesign.** Published in `edge-6d1ae98b9ac1` (commit `6d1ae98`) and host-qualified on `main` for the main flows. `app create` records production and staging from one recipe, both protected by default and without TOTP enrollment. Migration 024 adds persistent protection and backfills existing environments protected; on 2026-11-06 node apply `op_fca44c39b625e2216d05d7581de99c18` applied it (schema 24/24), and all five pre-existing environments read protected. On `a2defs1106`, guided create `op_0e2c1e66043489dc1941825c705a8c1a` made production and staging protected; shared-recipe `pr-42` `op_1724f76965e70df26a026cfde545ceb8` was unprotected and deleted with `--confirm --json` `op_9eb5aa06b06766f163adfbe61b2c74bc`; protected `env delete`, `env unprotect` and `app delete` refused `input_required`, while direct node app/protected-env delete refused `precondition_failed`/`code_required`; `env protect` raised `prot-audit` agent-safely as `op_e248d36683e6317674fc36b9c5711439`, after which delete and unprotect refused. Fresh arbitrary-name `fresh-audit` (`op_4fa609d1c8502d17ab4e11baa2b4de02`) and shared-recipe `recipe-audit` (`op_a1505d66121293de4f0c8b9931de94c4`) were unprotected by default; a same-key `env create` replay returned the original operation, and both were deleted unattended (`op_d1144d2a44c97b3b8b284f827b6806ba`, `op_17179495fec1805fad3341a51366b0a8`). A separate disposable PostgreSQL test on `6d1ae98` passed atomic app-create rollback/replay and denied a last unprotected environment for missing, wrong and invalid verifier input; its valid injected verifier queued (not completed) a delete with workers stopped and was not a real TOTP; isolated protect-store/CLI tests and a direct-service local-verifier test cover generated local secrets with real TOTP codes. The multi-environment app-create same-key replay and the last-environment positive path were not re-run on the host; the operator must complete the residual app deletion with TOTP. | Create an app and verify two protected environments commit in one transaction and replay idempotently; protected/last deletion refuses without a code over direct node/HTTP paths; unprotected non-last deletion succeeds with `--json`/no-TTY and exact confirmation; unprotect without a terminal refuses; recipe create for `pr-42` needs no file entry; migration 024 backfills safely. |
| 21 | **Node-local telemetry (D5), beta.** Host-qualified on `edge-fa33befb675d`; migration 025 applied to the authorized main node (schema 24/24 to 25/25). Node-mode `metrics.show` reads the loopback node_exporter twice with a bounded gap, parses only needed families and returns one current snapshot over the operator RPC: node/exporter identity, fetched/observed timestamps, freshness, CPU utilization from a checked counter delta, memory, real filesystems, load, uptime and explicit issues. `metrics show` has plain and JSON forms and never prompts; the workspace Fleet detail shows the same current CPU sample plus memory/filesystem/load/uptime facts, and Envs has no invented request graph. No telemetry server, Prometheus/Loki, external backend, global PromQL, metric store or history exists. The exporter also reads the root-owned `/var/lib/node_exporter/textfile` directory (and its root:root 0755 parent, no symlink or special mode bits) and a narrow systemd unit include for `eregion-*`, `caddy`, `docker`, `node-exporter` and `wg-quick@wg0` service/timer units; `doctor` adds `telemetry.local` for exporter reachability, strict textfile directory state, both collector results, and an unambiguous `node_textfile_scrape_error=0`; a strict directory error still fails independently of WireGuard. `server status` and `doctor` include `node-exporter.service`. | Offline parser/collector/node-RPC/CLI/workspace tests pass, including text fixtures, counter reset and missing metrics; CPU is never a fabricated zero. On the authorized host, `metrics show --json` reported schema `eregion/node-metrics/v1`, WireGuard scrape details, accurate fresh timestamps and real values; plain/noTTY and JSON matched a direct exporter scrape within sampling tolerance; CPU was numeric and matched the counter delta; `doctor` was 21/0/0 on the pre-extension build and a second apply was a no-op. The later `host-node-exporter-v2` textfile/systemd collectors and `telemetry.local` row are host-qualified on `edge-f1b5440c146b`: apply executed the step (schema moved 25/26 to 26/26), the parent and textfile directories are root:root 0755, the unit carries the textfile and narrow systemd flags, the listener is `10.78.0.1:9100` only, both collectors report success=1, `node_textfile_scrape_error` is exactly 0, narrow-systemd series for the eregion, Caddy and Docker units were observed, `metrics show` plain/JSON were fresh and matched a direct scrape, and `doctor` is 22/0/0. The temporary textfile-producer/malformed-file host variants were not run because noninteractive sudo was unavailable; the destructive stopped-exporter test was not authorized and is marked skipped. Unit tests cover those paths, and no self-metrics are claimed without a real producer. This is telemetry qualification, not a full beta release; Cloudflare row 22 remains not host-qualified. **Alpha.5 extension, host-qualified on `edge-2767500209f1` (2026-11-09):** node_exporter moves to loopback; a dedicated Caddy telemetry instance on the WireGuard address adds an explicit `bind 10.78.0.1`, the basic-auth proxy and `/targets.json` HTTP discovery, isolated from public ingress Caddy; the unit is triggered by `wg-quick@wg0` starts and `host-telemetry-v1` writes the discovery document, unit and firewall rule; `metrics endpoint|config|rotate` add configuration generation and bcrypt rotation that saves locally before the request and sends only the hash, with a node-local telemetry lock, Caddy validation/restart and previous-file restoration; `server init` generates or reuses the credential and hands off username/password; `doctor` adds `telemetry.endpoint`; no self-metric is written without a real producer. On `main`, apply `op_591b1885bcaf69397d1e1313917e4bcc` installed the step (4 applied / 17 skipped), node_exporter moved to `127.0.0.1:9100` and telemetry to `10.78.0.1:9101`; two live rotations made the previous password 401 and the new one 200, unauthenticated `/targets.json` and `/node` returned 401, authenticated requests returned 200 through the operator WireGuard tunnel, the generated Prometheus config passed `promtool check config`, public 9101 was unreachable, `doctor` was 23 pass / 0 warn / 0 fail, a second apply was 0 applied / 21 skipped, and the existing serving URLs stayed HTTP 200. No app, domain or DNS state was mutated. The stop-WG, stopped-exporter and hostname-less variants were not run; no public listener, central Prometheus/Grafana, metric store or history is added. |
| 22 | **Cloudflare DNS automation (D26), beta.** Core provider flow qualified on alpha.6 on `thelab`; beta adverse cases remain open in Linear. Explicit domain add/remove keeps manual DNS as default, exact A/AAAA ownership, explicit public origins/proxy, durable ledgers and cleanup-before-workload-deletion. Human `domain cloudflare configure` stores revision-CAS-fenced encrypted credentials in PostgreSQL (027); agent-safe status is metadata only. D26/section 6.5 retain partial-effect and external-writer-race boundaries. | Source review, isolated PostgreSQL CAS/isolation/delete-order tests, offline build/vet and Darwin cross-build passed for alpha.6. Signed upgrade reached schema 27/27, doctor 23/0/0 and repeat apply 0/21. Subsequent real human setup, DNS create/readback/no-op/repeat apply and exact-owned alias cleanup passed. Foreign-record preview refusal was observed; actual apply refusal, provider failure/interruption and multi-binding deletion were **not** qualified live. Exact evidence is in MANUAL-TESTING; no beta acceptance is inferred. |
| 23 | **Staging-to-production promotion (D23), alpha-ready.** Implemented with a targeted host pass on `edge-f1b5440c146b` on the authorized `a2defs1106` fixture, including release-command execution, once-only restart and rollback skip, production secret injection and the recorded secret/revision snapshot; concurrent host changes remain offline-only. The alpha.4 release is published. `env promote <app> <source-environment>` promotes the source environment's current ready, unpruned serving release to the app's fixed `production` target. The source serving release is resolved and pinned **at submission, not execution**, and creates a new production release pinned to the exact source image digest, commit and source without a build; production's current revision, plain config, secret names/values, domains and release command then run through the existing release-command/blue-green/public-verification path. It never copies or adopts source config, secrets or domains. Admission refuses any active production deployment under the production row lock, including a different artifact, so previous cleanup/rollback history cannot be captured stale; ordinary deploys queued after a promotion rebind their previous release/deployment at execution under the target lock. Preview honors the supplied idempotency key with the same normalized-input hash fence as submit: identical input replays, changed input conflicts. An artifact already serving production, cross-app input, a different node, or a target that fails the explicit-release deployability gate is refused. Durable target release/change history and operation events distinguish promotion from a rebuild deploy. | Offline action-normalization, CLI, pure validation and PostgreSQL/River integration tests cover the exact image pin at submission, no build, production config/secret/command use, blue/green serving switch, same-key replay including dry-run preview, changed-input conflict, duplicate refusal, source-not-serving refusal, queued-deploy admission conflict, concurrent-promotion serialization and execution-time previous-deployment rebind when a later deploy runs first. The updated source/DB integration tests were not rerun in the final host pass because no private disposable PostgreSQL was provided; host acceptance is recorded separately in MANUAL-TESTING. On 2026-11-08 the authorized `a2defs1106` fixture verifies preview with no operation count change, promotion of staging serving release `rel_c2aa240bf7f3032b64324b0cf601ce19` (image `sha256:b472757e…`) as production `rel_17e95932c935f0ca3cf937455fe39255` with no build ID, production `/build` returning `production`, same-key replay and preview replay, duplicate refusal, a forced failed candidate that left `rel_b19f552b5afb5a727b7e7b897f59c181` serving, retry success, and rollback. A second host pass pushed two scratch images through `eregion image push`: staging's release command ran with `build=staging`, `staging=set`, `promo=unset`; production's ran once with `build=production`, `promo=match`, `staging=unset`; restart and rollback logged `release command already succeeded for this release` with no command step; and rollback to the earlier promoted release restored its recorded secret/revision snapshot (`/secret` match, `/` `alpha4-prod` while the current revision was `alpha4-prod2` with a rotated secret). No secret value was printed. Production config was independently verified through `/build`; no Cloudflare/DNS provider call was made. |
| 24 | **CLI overview and discovery.** The original alpha.6 overview request is superseded by the alpha.7 source changes: catalog-generated offline overview, explicit metadata checks, searchable visible commands, richer help and deterministic suggestions. Root JSON retains its existing catalog/version/stage payload. | Offline regressions and manual candidate presentation checks are separate from host qualification; use the built binary's catalog, not this historical table, to discover the implemented surface. |

## Reference index

[Contracts](contracts.md) · [Secrets](secrets.md) · [Routes and DNS](network.md) ·
[Telemetry](telemetry.md) · [TUI design](tui-design.md) ·
[Terminal presentation](workspace.md) · [Upgrades](upgrades.md) ·
[Security notes](security-review.md)
