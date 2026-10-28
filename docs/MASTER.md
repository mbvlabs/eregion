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
- Managing application databases. Apps reach a database the operator runs
  elsewhere through a secret such as `DATABASE_URL` (D19).

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
| D6 | Command tree | [Section 5](#5-command-tree). `platform …`, `resources …`, operator-facing `bootstrap …`, `serve`/`worker`, `platform submit` and bearer-token `context login` are removed from the operator surface; the service, migration and bootstrap entry points live under the hidden Linux-only `node` group. Pre-v1: delete old paths, no aliases or compatibility shims. |
| D7 | Tokens and the human factor | API tokens are per use, scoped, expiring, listed and revocable, and shown once. Creating one needs a human factor (a TOTP code from `eregion protect init`, or a master password) read without echo at a terminal, so an agent cannot mint its own. No token is minted at setup or handed off. The same factor gates exactly four actions: **token create, secret export (`secret export`, `secrets export`), `app delete` and `env delete`**. The node verifies the code inside the same request that performs the action; a separate pre-check is UX only. `--yes`, `--json`, stdin and pipes never satisfy it. The factor stops an agent operating through `eregion` from reading secrets, minting tokens or deleting apps; it is not a sandbox: an OS account holding a context's files (SSH key, sudo password) is root-equivalent on that server. |
| D8 | Hosts and platforms | Servers: Debian 13 and Ubuntu 26.04, amd64 and arm64. Operator CLI: Linux and macOS; no Windows builds. Operator mode pushes the matching verified Linux binary to the node. |
| D9 | Control database | External PostgreSQL 17+ with `sslmode=verify-full`, never installed on the app server. Its DSN is a server secret and goes in the handoff. |
| D10 | Workloads | deploycrate's: directly managed Docker containers identified by ownership labels, images by immutable digest, restart `unless-stopped`, ports published only on dynamic loopback ports (or the node's WireGuard address). Docker Compose and per-environment systemd units are never state stores. |
| D11 | Builds | Cloud Native Buildpacks with a pinned `pack` and Paketo builder, Go apps with `go.mod` first. Builds run on the main node for v1. |
| D12 | Confirmation | Destructive commands take a token that names the target (`--confirm <app>`, `--confirm <app>/<env>`, `--confirm <server>`); `--yes` alone never suffices. The node validates the token against current state. `app delete` and `env delete` also need the D7 factor; their dry run does not. |
| D13 | Deletion | deploycrate's policy: deleting an app removes every environment, source, domain, secret, release, deployment and route it owns after workload cleanup succeeds; deleting the last environment deletes the app. Durable operation history is kept. |
| D14 | Release model | vaire's: `master` is edge and publishes on every push; `1.x` is the whole v1 line; tags `v1.0.0-alpha.N`, then `v1.0.0`; changes reach `1.x` by pull request and `1.x` merges up into `master`, never cherry-picked. |
| D15 | Release signing | vaire's pipeline and scripts: cosign keyless signatures bound to the pinned workflow identity, `install.sh` per channel, R2 channel manifests. Replaces the Ed25519 manifests. |
| D16 | Order of work | Operator channel first (work list rows 1–3), then the app workflow (rows 4–8). New app commands are node-mode commands from the start, never built on the token adapter. |
| D17 | Traffic switch | deploycrate's blue/green: the candidate runs on a dynamic loopback port, passes health, Caddy switches its backends, the public response is verified, then the previous containers stop. A failed candidate leaves the old release serving. Public verification requests the primary domain at its check path (D29) and records the host, path and status it observed. Failure to remove a previous or candidate container never fails a verified switch, but is recorded and retried, never silently dropped. |
| D18 | Secret resolution | deploycrate's: the environment revision names its secrets; each deploy resolves their current values and records the exact secret revisions on the deployment, so a rollback reproduces them. No hand-pinned `vault:…@N` refs. |
| D19 | Databases | eregion does not provision or manage databases. `resources …` and its backup code leave the operator surface. An app gets its database through environment secrets. |
| D20 | Sources | Alpha: HTTPS git (public, or private with a per-environment read credential) and existing images. GitHub App after v1. |
| D21 | Processes | deploycrate's: an environment declares processes of kind `web` (receives the domains through Caddy), `worker` (no port), `service` (port reachable privately, no domain) and a `release` command, each with a command, arguments and replicas. Caddy balances a `web` process across its replicas. The release command (typically migrations) is **release-scoped**, as in deploycrate: it runs once per release and command, before the release fans out to its deployments (and, after v1, to several nodes); a successful run is recorded, and `restart`, `process scale` and `rollback` to that release never run it again. A failed run blocks that release until a new `deploy`. A forced re-run is a possible later override, not alpha scope. |
| D22 | Access modes | Public only in the alpha; basic auth and private-network access after v1. |
| D23 | Promotion | After v1; `deploy --release` of another environment's release covers it by hand. |
| D24 | `server init --local` | After v1; remote bootstrap only in the alpha. |
| D25 | Logs | `logs` reads container logs over the operator channel and works without a telemetry node. |
| D26 | Nodes and DNS automation | `node add` and Cloudflare DNS automation after v1; the alpha uses manual DNS with resolver checks. |
| D27 | Plaintext secret read | No `secret get`. `secret export` is human-only behind the D7 factor. |
| D28 | Push to deploy | In the alpha, as in deploycrate: an environment with auto-deploy on deploys every push to its bound branch through a signed GitHub webhook. Each environment has its own webhook URL and generated secret, shown once when auto-deploy is turned on and replaceable by rotation; the signature is checked over the raw body before parsing, deliveries are deduplicated by delivery ID, and pushes to other branches are ignored. The routing lives in the environment revision, not in service environment variables. |
| D29 | Domain check path | Each domain binding has an optional check path (`domain add … --check-path PATH`), defaulting to the `web` process health path. The domain observer and the D17 public verification request each host at its own path, so an alias that serves no health route can still be ready. deploycrate has one path per web process; this is an eregion extension found in a real deployment. |
| D30 | Disk retention | Required for v1.0.0, not alpha.1. A retention policy (server default, per-environment override) and one planner used by a periodic River job and by `eregion prune [--dry-run]`. Defaults: keep the newest 10 releases per environment plus any newer than 14 days, always the serving release and its rollback target; registry artifacts no release references after 14 days; Buildpacks caches above 10 GiB or 80% disk; ownerless labeled containers and dangling images always. Release rows are never deleted; a pruned release records `pruned_at` and a rollback to it refuses clearly. Registry deletion needs a new registry host-step version with deletes enabled plus garbage collection. |
| D31 | Workload repair | As deploycrate's `workload_reconciliation`: on worker start the control plane compares each serving deployment's recorded containers with Docker and queues one restart of the serving release for an environment whose serving containers are missing. A restart never re-runs the release command (D21). |

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
eregion operations list|get|follow|cancel
eregion token create|list|revoke  ·  protect init|status
eregion ssh [node] [--command CMD]  ·  node unlock-ssh # break-glass
eregion upgrade
eregion ui  ·  commands | guide agents | skill install | version
eregion node …                                         # node mode, not for operators
```

Before v1.0.0 final, after alpha.1 (D30): `retention show|set` and
`prune [--dry-run]`.

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
converges its steps; `doctor` reports drift, security, certificates, disk and
versions. `eregion ssh` opens a shell over WireGuard.

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
replaces it. A push to the bound branch runs the same deploy as `eregion deploy`
at the pushed commit; a redelivery returns the original operation.

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
in D14–D15. After v1.0.0 final: additional nodes, the telemetry node, GitHub
App sources, Cloudflare DNS automation, promotion, access modes,
`server init --local` and HA.

**v1 is an alpha: nothing is frozen.** The `eregion/v1` JSON, the handoff and
bundle formats, context files, host step IDs and the database schema may change
between alphas, and a later alpha may require a fresh `server init`. A v1
compatibility contract in vaire's form (formats, JSON, commands, the
`upgrade` + `apply --auto-upgrade-nodes` path) starts at `v1.0.0` final.

An alpha is cut when MANUAL-TESTING's baseline passes on a real server for the
tagged build. "Builds" or "appears in `commands --json`" is never the gate.
Disk retention (D30) is not an alpha.1 gate; it is required before v1.0.0
final and is the main alpha.2 item.

## 8. Current state

Source state 2026-10-26: rows 1–10 are implemented on `master` and pass build,
vet, the darwin build and the unit tests of the changed packages; a read-only
audit traced every MANUAL-TESTING check and its blocking defects are fixed.
**Nothing is qualified:** no real-host run is recorded for this build.
Schema: embedded migrations through 013.

A source re-check on 2026-10-28 (snapshot `a7478c4`) and a real deployment of a
second app (andurel-site, edge-8a7179d: image push, digest deploy, release
command, web×2 + service + worker, Cloudflare-proxied domains) found open
defects at the edges; rows 11–15 close them. The run is evidence, not a
baseline pass: older build, no failed candidate, rollback or zero-downtime
check. Task-level detail for rows 11–15 lives in the temporary
[ALPHA-SPEC](ALPHA-SPEC.md), deleted at the alpha.1 tag.

| Area | What exists | Open before the alpha |
| --- | --- | --- |
| `server init` | vaire flow: preflight, `--answers`, `--password-stdin`, plan with `--confirm <server>`, binary installed once, vault keyring, node_exporter on WireGuard, guarded handoff with `--secrets-out`, `--resume`/`--discard`, `context remove`. SSH on port 22 only. | Real-host qualification. |
| Operator channel | `eregion node rpc` over SSH in WireGuard; token-free contexts and handoff; version lockstep and `--auto-upgrade-nodes`. | Real-host qualification. |
| Host convergence | `plan`, `apply`, `doctor`, `server status`, `ssh`, `node unlock-ssh`. | Real-host qualification. |
| Apps and environments | Revisions with source binding, processes, config and secret names; guided and flag-only `app create`, `env create|update`; `process`, `config`; deployability gate. | Real-host qualification. |
| Build, release, deploy | Deploy from the bound source (public or private git) or a release; build lifecycle; per-process containers with replicas; blue/green switch with public verification; rollback with pinned secret revisions; restart. | Real-host qualification. |
| Domains | Primary and aliases; DNS, route, certificate and public state; serialized atomic Caddy writes; reconciliation and observation. | Real-host qualification. |
| Observe and remove | `app/env status`, `logs`, `operations list`, `app/env delete` per D13. Over node RPC the operator reads, follows and cancels any operation; the public HTTP path stays actor- and grant-scoped. | Real-host qualification. |
| CI and push to deploy | D7 tokens behind `protect init`; per-environment deploy endpoint; per-environment GitHub webhook via `env update --auto-deploy`; `secrets export`/`secret export` behind the factor. | Real-host qualification. |
| Surface | `commands --json` lists only section 5; node-only commands hidden under `eregion node`. | — |
| Release | vaire pipeline: cosign keyless, `install.sh`, edge on push to `master`, stable on `v*` tags, origin `eregion.mbvlabs.com`. | First edge publish; `stable` environment secrets before the first tag. |

## 9. Work list

One row at a time, in order (D16). A row is done when its user-visible flow
works with flags, JSON, failure behavior and regression tests. Real-host checks
are in MANUAL-TESTING. Exception: rows 11–14 touch disjoint behavior and may
run in parallel per ALPHA-SPEC; row 15 follows them.

| # | Outcome | Done when |
| --- | --- | --- |
| 1 | **Node-mode dispatch.** `eregion node <cmd> --json` over SSH/WireGuard calls the application services; contexts and handoff carry no API token; version lockstep and `--auto-upgrade-nodes`; binary installed once. | Separate-install login works; mismatched version refuses before mutation; no token in context or handoff. |
| 2 | **`server init` to vaire's shape.** Preflight, `--answers`, `--password-stdin`, `--confirm <server>`, `--resume`/`--discard`, handoff with stored gate and `--secrets-out`, vault keyring, node_exporter on WireGuard, `context remove`, `secrets export` behind the human factor. | Interrupted-init and lockdown-revert checks pass; JSON/no-TTY never prompts. |
| 3 | **Host convergence.** `plan`, `apply`, `doctor`, `status`, `ssh`, `node unlock-ssh`. | A second `apply` is a no-op; an upgraded node re-runs only changed steps. |
| 4 | **App and environment model.** Migration for source binding, revisions, processes and deployability; `app create`, `env create|update|list|status`, `process list|scale`, `config`, secrets per D18. | Guided and flag-only paths save the same revision; a new environment can deploy without raw JSON. |
| 5 | **Deploy.** `deploy` from source (public or private git) or release, build lifecycle commands, release command, per-process containers with replicas, blue/green per D17, public verification, rollback, restart. | Two deploys and a rollback verified at the public HTTPS URL with no failed requests during the switch; a failed candidate leaves the old release serving; a worker process runs alongside web. |
| 6 | **Domains.** Primary/aliases, DNS check, desired/applied/observed route state, serialized atomic Caddy writes, reconciliation on start and retry. | Failed reload and interrupted change recover; removing one domain leaves others intact. |
| 7 | **Observe and remove.** `app/env status`, `logs`, `operations list`, `app delete`, `env delete` per D13. | Status shows serving release, processes and per-domain state; deletion cleans containers, routes and secrets and keeps history. |
| 8 | **CI and push to deploy.** Per-environment deploy endpoint through Caddy, `token create|list|revoke`, `protect init`; per-environment GitHub webhook deploying pushes to the bound branch (D28), replacing `EREGION_WEBHOOK_SECRET_FILE` and `EREGION_WEBHOOK_ALLOW`. | CI deploy works; expired, revoked and wrong-target tokens fail; `--yes`, JSON and stdin cannot mint a token; a push deploys, a redelivery does not deploy twice, a bad signature or other branch does nothing. |
| 9 | **Surface cleanup.** Remove the D6 surfaces and the resources/backup code (D19); node-only commands hidden; `guide agents`, `skill install`. | `commands --json` lists only the section 5 tree. |
| 10 | **Release pipeline.** D14–D15: edge on push, `1.x`, tags, cosign, install script, channel manifests, Linux/macOS only. | An edge build installs on Linux and macOS and `upgrade` moves between two edge builds. |
| 11 | **Trust boundary (D7, D12).** The factor is verified inside the same request for token create, both exports, `app delete` and `env delete`; dry run never installs a node binary; a node on a release build installs only a cosign-verified release binary. | A direct `vault.export`, `handoff.export` or delete submission without a valid code fails on the node; `--dry-run --auto-upgrade-nodes` leaves the node build unchanged; an unsigned upload to a release-build node is refused. |
| 12 | **Deterministic builds.** Git builds honour `context_path`; the ref is resolved to an exact commit at submission and stored on the build; build, retry and rollback use only that commit. | A monorepo subdirectory builds; a retry of a failed branch build rebuilds the original commit after the branch moved. |
| 13 | **Workloads and probes (D17, D21, D29, D31).** Release-scoped release command; recorded and retried container cleanup; serving-container repair at worker start; per-domain check path; public verification logs host, path and status; the observer covers every domain. | Scale, restart and rollback do not re-run a succeeded release command; a removed serving container returns after a worker restart; an alias with its own check path becomes ready; a leaked container shows in `env status` and is removed later. |
| 14 | **Operator contracts.** Atomic `login` with `--replace`; same-key create retry returns the original operation without writing secrets again; `--context`/`EREGION_CONTEXT`; `logs --json` is one envelope (NDJSON only with `--output ndjson`); `domain add --json` returns an object; `ssh --command`. | Each has a regression test; a failed import leaves an existing context working. |
| 15 | **Guided finish.** Guided `app create`/`env create` follow build and deploy to serving or failed, Ctrl-C detaches (vaire's flow); `server init` ends with vaire's doctor gate (`doctor_failed`, `--resume`, `--skip-doctor`). | Guided create ends at a serving URL or a failed operation with its error; init on a healthy host ends with a clean doctor summary. |
| 16 | **Alpha qualification.** MANUAL-TESTING baseline on a real server for the tagged build. | Recorded pass; then the user authorizes `1.x` and `v1.0.0-alpha.1`. |
| 17 | **Disk retention (D30), alpha.2, required for v1.0.0.** Policy, planner, periodic job, `retention show|set`, `prune [--dry-run]`, registry deletes and GC, `doctor` disk warnings. | A node with 30 releases keeps exactly the policy's set; `prune --dry-run` lists what would go and the bytes; rollback to a pruned release refuses clearly; a second `apply` stays a no-op. |

## Reference index

[Contracts](contracts.md) · [Secrets](secrets.md) · [Routes and DNS](network.md) ·
[Telemetry](telemetry.md) · [TUI design](tui-design.md) ·
[Terminal presentation](workspace.md) · [Upgrades](upgrades.md) ·
[Security notes](security-review.md)
