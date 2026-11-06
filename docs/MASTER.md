# eregion: deploycrate-ce, shaped like vaire

> Master document: product, decisions, model, command tree, the v1 alpha scope
> and **the only work list and status**. [IMPLEMENTATION](IMPLEMENTATION.md)
> describes current code and carries no status; [MANUAL-TESTING](MANUAL-TESTING.md)
> holds acceptance checks and run evidence.
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
nodes, the telemetry node and HA come later, but early decisions must not block
them.

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
| D1 | Control model | The CLI talks to a node through SSH over embedded userspace WireGuard, always. Each operator command resolves the context, opens SSH in WireGuard and runs `eregion node <cmd> --json` there, which calls the same application services the API uses. Contexts hold a WireGuard identity and an SSH key, not a bearer token. |
| D2 | Hardening | vaire's: admin user with a sudo password, root login and SSH passwords off, firewall, commit-confirm switchover of SSH to WireGuard with automatic revert, break-glass via the provider console and `eregion node unlock-ssh`. |
| D3 | Ingress | Caddy on the main node. The main hostname is used exactly as entered. App domains are environment bindings. DNS records, direct or Cloudflare-proxied, and ACME. |
| D4 | Backups | No backup product work before v1. Control-database recovery is the PostgreSQL provider's job. |
| D5 | Telemetry | Every server runs node_exporter from setup on, bound to its WireGuard address only. The **telemetry node** is a separate self-hosted Grafana with its metrics and log storage, scraping servers over WireGuard; an external backend is the alternative. No ClickHouse. Operation history never depends on telemetry. |
| D6 | Command tree | [Section 5](#5-command-tree). `platform …`, operator-facing `bootstrap …`, `serve`/`worker`, `platform submit` and bearer-token `context login` are removed from the operator surface; the service, migration and bootstrap entry points live under the hidden Linux-only `node` group. The old resource surface was removed, but alpha.2 adds a new node-mode resource lifecycle under D19. Pre-v1: delete old paths, no aliases or compatibility shims. |
| D7 | Tokens and the human factor | API tokens are per use, scoped, expiring, listed and revocable, and shown once. Creating one needs a human factor (a TOTP code from `eregion protect init`, or a master password) read without echo at a terminal, so an agent cannot mint its own. No token is minted at setup or handed off. The same factor gates exactly four actions: **token create, secret export (`secret export`, `secrets export`), `app delete` and `env delete`**. The node verifies the code inside the same request that performs the action; a separate pre-check is UX only. `--yes`, `--json`, stdin and pipes never satisfy it. The factor stops an agent operating through `eregion` from reading secrets, minting tokens or deleting apps; it is not a sandbox: an OS account holding a context's files (SSH key, sudo password) is root-equivalent on that server. |
| D8 | Hosts and platforms | Servers: Debian 13 and Ubuntu 26.04, amd64 and arm64. Operator CLI: Linux and macOS; no Windows builds. Operator mode pushes the matching verified Linux binary to the node. |
| D9 | Control database | External PostgreSQL 17+ with `sslmode=verify-full`, never installed on the app server. Its DSN is a server secret and goes in the handoff. |
| D10 | Workloads | deploycrate's: directly managed Docker containers identified by ownership labels, images by immutable digest, restart `unless-stopped`, ports published only on dynamic loopback ports (or the node's WireGuard address). Docker Compose and per-environment systemd units are never state stores. |
| D11 | Builds | Cloud Native Buildpacks with a pinned `pack` and Paketo builder, Go apps with `go.mod` first. Builds run on the main node for v1. |
| D12 | Confirmation | Destructive commands take a token that names the target (`--confirm <app>`, `--confirm <app>/<env>`, `--confirm <server>`); `--yes` alone never suffices. The node validates the token against current state. `app delete` and `env delete` also need the D7 factor; their dry run does not. |
| D13 | Deletion | deploycrate's policy: deleting an app removes every environment, source, domain, secret, release, deployment and route it owns after workload cleanup succeeds; deleting the last environment deletes the app. Durable operation history is kept. |
| D14 | Release model | `master` is the combined development and edge stream and publishes on every push; `1.x` is the whole v1 line. Alpha and beta tags publish exact snapshots without a moving channel; RC tags advance an opt-in `rc` channel; stable channels advance only on final tags. An RC client also checks its stable major line for a newer final, without automatically moving the RC pointer. Changes reach `1.x` by pull request and `1.x` merges up into `master`, never cherry-picked. |
| D15 | Release signing | vaire's pipeline and scripts: cosign keyless signatures bound to the pinned workflow identity, `install.sh` per channel, R2 channel manifests. Replaces the Ed25519 manifests. |
| D16 | Order of work | Operator channel first (work list rows 1–3), then the app workflow (rows 4–8). New app commands are node-mode commands from the start, never built on the token adapter. |
| D17 | Traffic switch | deploycrate's blue/green: the candidate runs on a dynamic loopback port, passes health, Caddy switches its backends, the public response is verified, then the previous containers stop. A failed candidate leaves the old release serving. Public verification requests the primary domain at its check path (D29) and records the host, path and status it observed. Failure to remove a previous or candidate container never fails a verified switch, but is recorded and retried, never silently dropped. |
| D18 | Secret resolution | deploycrate's: the environment revision names its secrets; each deploy resolves their current values and records the exact secret revisions on the deployment, so a rollback reproduces them. No hand-pinned `vault:…@N` refs. |
| D19 | Application resources | Alpha.2 adds optional, generic Docker resources owned by an environment; an application may instead use external dependencies through secrets. This supersedes the earlier exclusion of managed application databases: a PostgreSQL container is an app resource, never the external control-plane database. Resources use private networking and optional persistent volumes, secret names rather than values in definitions, and retain data when detached; deleting data requires a separate `resource destroy --delete-data` naming the exact resource, which removes only that resource's owned volumes. No backup product is implied. |
| D20 | Sources | HTTPS git (public, or private with a per-environment read credential) and existing images. No GitHub App integration is planned (decided 2026-11-04): a per-environment read credential plus the D28 webhook covers private repositories and push to deploy with far less setup. |
| D21 | Processes | deploycrate's: an environment declares processes of kind `web` (receives the domains through Caddy), `worker` (no port), `service` (port reachable privately, no domain) and a `release` command, each with a command, arguments and replicas. Caddy balances a `web` process across its replicas. The release command (typically migrations) is **release-scoped**, as in deploycrate: it runs once per release and command, before the release fans out to its deployments (and, after v1, to several nodes); a successful run is recorded, and `restart`, `process scale` and `rollback` to that release never run it again. A failed run blocks that release until a new `deploy`. A forced re-run is a possible later override, not alpha scope. |
| D22 | Access modes | Public only in the alpha; basic auth and private-network access after v1. |
| D23 | Promotion | After v1; `deploy --release` of another environment's release covers it by hand. |
| D24 | `server init --local` | After v1; remote bootstrap only in the alpha. |
| D25 | Logs | `logs` reads container logs over the operator channel and works without a telemetry node. |
| D26 | Nodes and DNS automation | `node add` and Cloudflare DNS automation after v1; the alpha uses manual DNS with resolver checks. |
| D27 | Plaintext secret read | No `secret get`. `secret export` is human-only behind the D7 factor. |
| D28 | Push to deploy | In the alpha, as in deploycrate: an environment with auto-deploy on deploys every push to its bound branch through a signed GitHub webhook. Each environment has its own webhook URL and generated secret, shown once when auto-deploy is turned on and replaceable by rotation; the HMAC secret is resolved fresh per delivery, the signature is checked over the raw body before parsing, and pushes to other branches are ignored. A verified push is admitted by one PostgreSQL transaction that stores only a sanitized intent (hashes, validated repository/branch/commit and non-secret environment/revision/secret-revision identities) and its River processing job; the handler replies 202 only after that transaction commits, within one 8-second admission budget covering the fresh secret resolution, body read and database work. A worker then submits the ordinary `platform.deploy` operation and links it to the delivery in another single transaction. A redelivery returns the original operation and never deploys twice; a receipt means the push was accepted for processing, not that a deployment completed. If the environment is deleted or its repository/branch binding changes before the worker submits, the accepted intent fails terminally instead of deploying against the new binding. The routing lives in the environment revision, not in service environment variables. This replaces a GitHub App (qualified on a real server 2026-11-04). Planned for alpha.2, not an alpha.1 gate: optional `gh`-assisted webhook setup checks for an authenticated operator `gh` with repository hook permission and can create or update the hook without copying the secret by hand. Manual setup remains available when `gh` or permission is absent. Any prompt has a flag equivalent; JSON/no-TTY never prompts. Plaintext secrets never enter ordinary output, events or arguments (D7). No GitHub App integration is added. |
| D29 | Domain check path | Each domain binding has an optional check path (`domain add … --check-path PATH`), defaulting to the `web` process health path. The domain observer and the D17 public verification request each host at its own path, so an alias that serves no health route can still be ready. deploycrate has one path per web process; this is an eregion extension found in a real deployment. |
| D30 | Disk retention | Required for alpha.1 (R9). A retention policy (server default, per-environment override) and one planner used by a periodic River job and by `eregion prune [--dry-run]`. Defaults: keep the newest 10 releases per environment plus any newer than 14 days, always the serving release and its rollback target; registry artifacts no release references after 14 days; Buildpacks caches above 10 GiB or 80% disk; ownerless labeled containers and dangling images always. Release rows are never deleted; a pruned release records `pruned_at` and a rollback to it refuses clearly. Registry deletion needs a new registry host-step version with deletes enabled plus garbage collection. A real `eregion prune` requires `--confirm <server>` and a dry run needs no confirmation. `doctor` reports the last run and warns when the Buildpacks cache exceeds the policy's absolute cap; it reads the run's durable cache measurement, and an absent or stale measurement is unknown rather than a zero-byte healthy cache. |
| D31 | Workload repair | As deploycrate's `workload_reconciliation`: on worker start the control plane compares each serving deployment's recorded containers with Docker and queues one restart of the serving release for an environment whose serving containers are missing. A restart never re-runs the release command (D21). |
| D32 | App definition file (planned for alpha.2) | Adopt a Fly-style `eregion.toml` **as input only**, never as a store: `app create --from FILE`, `env create|update --from FILE` and `app export <app> <env>` turn it into ordinary desired state. Existing guided and flag-only workflows remain usable without a file; PostgreSQL stays authoritative. The file names processes, health, source and build settings, non-secret config, secret **names** (never values), domains and optional generic Docker resources (D19), with shared defaults and selected environment overrides. Explicit apply previews and provisions declared resources; an ordinary deploy or push never reads the file. `--dry-run` shows changes, removals require target confirmation, and resource data is retained unless separately destroyed. |

## 4. The application model

The nouns follow deploycrate-ce. Names are lowercase slugs.

- **Server / node.** A VPS. The first one `server init` sets up is the **main
  node**. A context names a server.
- **App.** A named group of environments. Owns nothing at runtime by itself.
- **Environment.** The unit of deployment (`staging`, `production`, …). Its
  **desired state** is one complete revision containing:
  - **source binding**: an HTTPS git repository, ref and optional read
    credential, or an image reference; Buildpacks context path, builder and
    buildpack settings; auto-deploy on push (D28);
  - **runtime**: node, health path and timeouts, release command;
  - **processes** (D21): name, kind, command, arguments, replicas, container
    port for `web` and `service`;
  - **config**: plain environment variables;
  - **secrets**: environment-scoped names, resolved per deploy (D18);
  - **domains**: one primary hostname plus aliases, served by the `web`
    process.

  Every change commits a new immutable revision and a change record. A
  **deployability** report lists what is missing before a deploy can run
  (source, artifact, runtime, domain, DNS), as deploycrate's does.
- **Build.** One attempt to turn a source revision (exact commit) into an image,
  with its own logs and lifecycle; it can be stopped and retried.
- **Release.** An immutable deployable artifact (image digest) for one
  environment, from a build or an existing image.
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
eregion server init [--resume | --discard] [--answers FILE] | status
eregion plan | apply [--auto-upgrade-nodes] | doctor  # converge and check host steps
eregion login [--replace NAME] | context list|show|use|remove
                                                       # every node command: --context NAME / EREGION_CONTEXT
eregion secrets export                                 # server handoff, human factor
eregion app create|list|status|delete
eregion env create|update|list|status|delete
eregion process list|scale                             # per environment
eregion deploy <app> <env> [--ref REF | --release ID] | rollback <app> <env> [--to ID]
eregion image push <local-image> --app <app> [--tag <tag>]   # local image into the node registry
eregion build list|logs|stop|retry  ·  release list
eregion domain add|list|remove [--primary] [--check-path PATH]
eregion config set|unset|list                          # plain variables
eregion secret set|list|archive|export                 # values write-only
eregion logs <app> <env> [--process NAME] [--follow]  ·  restart <app> <env>
eregion retention show|set  ·  prune [--dry-run] [--confirm <server>]
eregion resource apply|attach|detach|list|status|destroy [--delete-data]   # D19
eregion operations list|get|follow|cancel
eregion token create|list|revoke  ·  protect init|status
eregion ssh [node] [--command CMD]  ·  node unlock-ssh # break-glass
eregion upgrade
eregion ui  ·  commands | guide agents | skill install | version
eregion node …                                         # node mode, not for operators
```

After v1: `node add|list|status|remove`, `env promote`, access modes, the
telemetry node and `metrics`.

## 6. Flows

### 6.1 `server init` (vaire)

1. **Local preflight**: target reachable, an SSH credential, missing tools
   reported as `missing_tool` with the install command.
2. **First contact** as root with `--identity`, ssh-agent, default keys, an
   encrypted-key passphrase or `--password-stdin`. Host key TOFU with
   fingerprint confirmation, `--host-key-fingerprint` or `--accept-new-host-key`.
3. **Remote preflight**: OS, architecture, resources, existing state.
4. **Questions**, each defaulted and each also a flag or an `--answers` TOML
   key: node and context name, public hostname (used as given), external DSN
   file, admin user.
5. **Plan**, then confirm; non-interactive needs `--confirm <server>`.
6. **Apply** versioned steps with live progress: install the binary once,
   hardening, WireGuard hub and operator peer, commit-confirm SSH switchover,
   node_exporter on WireGuard, DNS record shown and checked against public
   resolvers, Caddy and ACME, Docker, Buildpacks, registry, vault keyring,
   migrations, services, `doctor` as the final gate.
7. **Handoff**: every server secret in plain text for a password manager plus an
   armored age transfer bundle with a generated passphrase, break-glass
   instructions included. No file unless `--secrets-out`. Init does not finish
   until the operator confirms it is stored. No API token is in it.

A pending init is saved locally. `--resume` continues it, `--discard` drops it,
and init refuses while a context or pending init for that server exists.

### 6.2 Operator session

`login` imports a bundle into a context; it validates the bundle against the
node before it replaces anything, and an existing context name needs
`--replace <name>`. Every node command targets `--context`, then
`EREGION_CONTEXT`, then the current context, as in vaire. Every command checks the node's
eregion version first and refuses with `node_version_mismatch` unless
`--auto-upgrade-nodes` is given or set on the context. `upgrade` replaces only
the local CLI and says so; `apply --auto-upgrade-nodes` upgrades the node and
converges its steps; `doctor` reports drift, security, certificates, disk,
retention and versions. `eregion ssh` opens a shell over WireGuard.

### 6.3 App setup (deploycrate)

`eregion app create` asks for the app name, the first environment, its source
(HTTPS git repo, ref and optional read credential, or an image), Buildpacks
settings, processes (a `web` process with its port and health path by
default), config, secrets and a domain, shows the plan and the DNS record, then
commits the app, the environment's first revision and — unless `--no-deploy` —
resolves the ref to an exact commit and queues the first build and deployment
in one transaction. The flag-only form expresses exactly the same intent.
`env create` adds an environment the same way; `env update` changes any part of
its desired state as a new revision; `process scale` changes replicas.

### 6.4 Deploy

`eregion deploy <app> <env>` builds the environment's source at its ref (or
`--ref`), records the release by digest and deploys it. `--release` redeploys an
existing release with the current revision. The pipeline, per D17: preflight →
release command → candidate containers for every process on dynamic loopback
ports → candidate health → Caddy backend switch for `web` → public verification
→ stop the previous containers. A failed candidate leaves the old release
serving. Every step is a durable operation; Ctrl-C detaches, `operations
follow` re-attaches, `cancel` asks for cancellation explicitly.

### 6.5 Domains

`domain add` saves the binding, shows the exact DNS record, then the worker
renders and applies the Caddy route, and the command reports DNS, route
application, certificate and public response separately as
pending/applied/failed. Routes are reconciled on worker start and after a
failed reload. One primary per environment; others are aliases.

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
over the operator channel (D25). `app delete` and `env delete` follow D13 and
need `--confirm`.

## 7. v1 alpha

**v1.0.0 (every alpha and the final) is one main node running applications
end to end:** sections 6.1–6.8 on the main node, the command tree in section 5
minus its "after v1" line, node_exporter on the server, and the release model
in D14–D15. After v1.0.0 final: additional nodes, the telemetry node,
Cloudflare DNS automation, promotion, access modes,
`server init --local` and HA.

**v1 is an alpha: nothing is frozen.** The `eregion/v1` JSON, the handoff and
bundle formats, context files, host step IDs and the database schema may change
between alphas, and a later alpha may require a fresh `server init`. A v1
compatibility contract in vaire's form (formats, JSON, commands, the
`upgrade` + `apply --auto-upgrade-nodes` path) starts at `v1.0.0` final.

An alpha is cut when the operator accepts MANUAL-TESTING's baseline for the
tagged build. "Builds" or "appears in `commands --json`" is never the gate.
Disk retention (D30) was the alpha.1 gate (R9): the policy, daily run, `prune
[--dry-run]`, registry deletes and garbage collection, and the doctor retention
and cache rows qualified with the rest of the baseline.

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

Final qualification for later alphas (R3, R11) remains **one complete
MANUAL-TESTING baseline run on a freshly initialized server with the candidate
build**. `main` serves only intermediate testing and the node-upgrade checks;
earlier passes are context, not qualification.

## 8. Current state

**Alpha.1 is qualified.** The operator explicitly confirmed that all alpha.1
testing is done. That confirmation is the qualification record; no build, date,
operation, individual observation or pass result is invented or backfilled here.
The older run records below describe the evidence available in this checkout,
including partial and not-run checks; they are historical evidence gaps, not
open alpha.1 blockers.

Alpha.2 is implemented at source level on `master`: strict `eregion.toml`
input and export, shared/environment overrides, explicit resource declarations,
generic environment-owned Docker resources with private networking, retained
data and deployment gating, and optional `gh` webhook assistance. Isolated
build, vet and package tests pass.

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
networks left. The fixture app/environment remains because real deletion needs
the D7 human factor. The four existing public URLs stayed HTTP 200 throughout.
Alpha.2 is not qualified.

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
check. Task-level detail for rows 11–15 lives in the temporary
[ALPHA-SPEC](ALPHA-SPEC.md), deleted at the alpha.1 tag.

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
| `server init` | vaire flow: preflight, `--answers`, `--password-stdin`, plan with `--confirm <server>`, binary installed once, vault keyring, node_exporter on WireGuard, guarded handoff with `--secrets-out`, `--resume`/`--discard`, `context remove`. SSH on port 22 only. | Closed by operator confirmation. |
| Operator channel | `eregion node rpc` over SSH in WireGuard; token-free contexts and handoff; version lockstep and `--auto-upgrade-nodes`. | Closed by operator confirmation. |
| Host convergence | `plan`, `apply`, `doctor`, `server status`, `ssh`, `node unlock-ssh`. | Closed by operator confirmation. |
| Apps and environments | Revisions with source binding, processes, config and secret names; guided and flag-only `app create`, `env create|update`; `process`, `config`; deployability gate. | Closed by operator confirmation. |
| Build, release, deploy | Deploy from the bound source (public or private git) or a release; build lifecycle; per-process containers with replicas; blue/green switch with public verification; rollback with pinned secret revisions; restart. | Closed by operator confirmation. |
| Domains | Primary and aliases; DNS, route, certificate and public state; serialized atomic Caddy writes; reconciliation and observation. | Closed by operator confirmation. |
| Observe and remove | `app/env status`, `logs`, `operations list`, `app/env delete` per D13. Over node RPC the operator reads, follows and cancels any operation; the public HTTP path stays actor- and grant-scoped. | Closed by operator confirmation. |
| CI and push to deploy | D7 tokens behind `protect init`; per-environment deploy endpoint; per-environment GitHub webhook via `env update --auto-deploy` with durable admission and atomic deploy linking; `secrets export`/`secret export` behind the factor. | Host checks pass for token reload, secret-export metadata, disposable deletes and webhook-secret recovery; migration 021 is applied on main; a real push deployed `59017d6` as `rel_d231a28c…` with delivery 202 and all four URLs 200. On the same build redelivery returned the original operation without another release, an invalid signature returned 401, and an existing other-branch fixture push was ignored. Old-secret rejection after rotation was not recorded; alpha.1 is operator-confirmed, and no result is backfilled. |
| Surface | `commands --json` lists only section 5; node-only commands hidden under `eregion node`. | — |
| Release | Cosign keyless, `install.sh`, edge on push to `master`, opt-in RC and finals-only stable pointers, exact alpha/beta snapshots; origin `eregion.mbvlabs.com`. | Edge publishes verified for `527592419f06` and `ef4a6f879f45`; the latter installed on CLI and `main` with schema 23/23. The authorized alpha.1 tag was later moved to `37d4f19` and its snapshot published; see the release record above. Channel manifest and signature are two objects with a pre-existing publication race; partial tagged uploads can block retry. Both remain follow-up risks, not blockers to the channel model. |

## 9. Work list

One row at a time, in order (D16). A row is done when its user-visible flow
works with flags, JSON, failure behavior and regression tests. Real-host checks
are in MANUAL-TESTING. Exception: rows 11–14 touch disjoint behavior and may
run in parallel per ALPHA-SPEC; row 15 follows them. Rows 1–17 are complete for
alpha.1 by the operator's confirmation that all alpha.1 testing is done (section
8). The acceptance criteria below remain the definition of each flow; this
document does not reconstruct the individual results or backfill evidence. The
R10 macOS exception remains outside the alpha.1 gate and is still required
before v1.0.0 final. Rows 18–19 are alpha.2 work: implemented at source level
and not qualified.

| # | Outcome | Done when |
| --- | --- | --- |
| 1 | **Node-mode dispatch.** `eregion node <cmd> --json` over SSH/WireGuard calls the application services; contexts and handoff carry no API token; version lockstep and `--auto-upgrade-nodes`; binary installed once. | Separate-install login works; mismatched version refuses before mutation; no token in context or handoff. |
| 2 | **`server init` to vaire's shape.** Preflight, `--answers`, `--password-stdin`, `--confirm <server>`, `--resume`/`--discard`, handoff with stored gate and `--secrets-out`, vault keyring, node_exporter on WireGuard, `context remove`, `secrets export` behind the human factor. | Interrupted-init and lockdown-revert checks pass; JSON/no-TTY never prompts. |
| 3 | **Host convergence.** `plan`, `apply`, `doctor`, `status`, `ssh`, `node unlock-ssh`. | A second `apply` is a no-op; an upgraded node re-runs only changed steps. |
| 4 | **App and environment model.** Migration for source binding, revisions, processes and deployability; `app create`, `env create|update|list|status`, `process list|scale`, `config`, secrets per D18. | Guided and flag-only paths save the same revision; a new environment can deploy without raw JSON. |
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
| 18 | **Optional app definitions and managed resources (D19, D32), alpha.2.** Implemented at source level; host qualification partial. `eregion.toml` input for `app create`, `env create|update` (`--from`) and `app export`, with shared defaults and selected environment overrides. Generic Docker resources have environment ownership, private networking, secret references, optional persistent volumes and durable lifecycle; explicit apply provisions them, while ordinary deploys never read the file. Existing file-free workflows remain supported. On 2026-11-06 `edge-ef4a6f879f45` passed definition create/override/export/reimport, secret-name-only storage and strict validation on `main`; resource apply is blocked by the Docker 29.1.3 image-inspect template defect recorded in section 8, and file-based removal of the last resource or last secret name was reported as a change but not applied. Resource health, secret injection, deployment gating and data retention remain unqualified; explicit detach and confirmed destroy worked. | File and equivalent flags save equivalent supported desired state; export round-trips without secret values; unknown keys and secret-value fields are refused. Dry-run previews changes; destructive removals require target confirmation; detaching a resource retains its data and destruction is separate. A managed resource can be consumed by the app, and an external dependency still works without one. |
| 19 | **Optional `gh`-assisted webhook setup (D28), alpha.2.** Implemented at source level; real GitHub qualification pending. Check authenticated `gh` and repository hook permission; create or update the matching hook, retaining manual setup without `gh` or permission. No GitHub App; no plaintext secrets in ordinary output, events or arguments. | Authenticated, authorized `gh` can create or update the hook; missing `gh` or permission leaves manual setup available; explicit flags cover prompts and JSON/no-TTY never prompts; regression tests cover these paths. |

## Reference index

[Contracts](contracts.md) · [Secrets](secrets.md) · [Routes and DNS](network.md) ·
[Telemetry](telemetry.md) · [TUI design](tui-design.md) ·
[Terminal presentation](workspace.md) · [Upgrades](upgrades.md) ·
[Security notes](security-review.md)
