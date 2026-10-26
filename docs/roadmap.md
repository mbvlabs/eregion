# eregion implementation roadmap

**Status:** Implemented v0.1 vertical slices · 2026-10-20  
**Product direction:** [Initial research](initial_research.md), Sections 4–5  
**Terminal direction:** [TUI design](tui-design.md) · [interactive sketches](design/tui-sketches.html)

## Outcome

An agent can discover, plan, submit, observe, and recover application operations
without a terminal or a browser. A human can use the same contracts through an
exceptional Charm-based terminal interface. Neither client owns execution lifetime.

This is a dependency-ordered plan, not a delivery-date estimate. Checkboxes denote
verified deliverables with local code and tests, not the presence of placeholder
commands and not a production-infrastructure claim. M0.1–M0.4 and the local
implementation of M1–M7 are in this tree. Two cross-node build bullets remain
open, and the fresh-VM, real-Cloudflare, real-telemetry-host, and multi-host
failover gates cannot be proven from this checkout; those are called out where
they occur. Browser sketches use synthetic data.

## Evidence in this tree

Run these from a clean checkout. There are no local Docker launchers or drills;
everything past these tests is exercised by hand on a real test host.

```sh
just check                     # formatting, vet, unit/contract/schema tests
just integration-norace        # PostgreSQL/River/API/process integration
just transport                 # pinned SSH + WireGuard exactly-once scenarios
```

The v0.1 line does not claim fresh-VM support, real Cloudflare behavior, a live
telemetry-node install, or automatic HA/failover. See
[release criteria](release-criteria.md) for the gate-by-gate record.

## Constraints that apply to every milestone

- External PostgreSQL and River; no SQLite, custom job queue, or separate broker.
- Debian and Ubuntu; select supported releases before shipping host mutations.
- Node model: the main node runs the control plane plus application runtime,
  Buildpacks builds, and the container registry by default. Additional nodes take
  explicit roles (application, builder, edge, telemetry). Telemetry never runs on the
  main node. Responsibilities stay separable in code and placement is explicit, so
  work can move off the main node without redesign.
- Agent-first metadata, structured errors/output, no-TTY inputs, and dry-run or
  preview semantics ship with each command. Do not defer them to a polish phase.
- API and CLI reuse application services. UI packages never enqueue work or run SSH
  directly; they issue requests through the same client interfaces.
- Redact at ingestion and output. Untrusted logs cannot inject terminal escapes or
  control sequences. Do not persist plaintext secrets in events or River arguments.
- Remote effects are at-least-once; use idempotency, scoped coordination, and
  reconciliation. River queue concurrency is not a distributed environment lock.
- No bundled ClickHouse. Operation history lives in PostgreSQL and works without any
  telemetry node. Application telemetry runs on a dedicated telemetry node role (see
  M5), never on the control plane.
- Deliver a human-visible improvement alongside each agent-capable vertical slice.

## M0 — Prove one durable operation (research Phase 0)

### M0.1 — Repository and agent contracts

- [x] Initialize the Go module, executable, formatting/lint/test commands, and CI.
- [x] Adapt eru's command registry, metadata validator, output envelope, structured
  errors, and secret redaction. Choose one schema name and document compatibility.
- [x] Add implemented commands only; advertise planned capabilities separately.
- [x] Define operation IDs, actor identity, submission idempotency keys, state
  transitions, event sequence/cursor, and confirmation binding.
- [x] Specify machine modes: one JSON result for reads/submissions, NDJSON for
  event-following, deterministic exit codes, no ANSI or prompts in machine output.
- [x] Extract a small shared client/service boundary; keep transport and rendering
  adapters separate. Do not copy entire frameworks without a demonstrated need.

**Gate:** command metadata and output contract tests pass, including redirected
output, invalid arguments, no-TTY operation, and redaction.

**Delivered:** the `commands`, `help`, `version`, operation, platform, resource,
network, secret, upgrade, bootstrap, and workspace command families; the
`eregion/v1` envelope and JSON Schema; metadata validation; deterministic exit
codes; NDJSON event streaming; and shared client/service boundaries. See
[contracts](contracts.md).

### M0.2 — PostgreSQL, River, and the API

- [x] Decide the database driver/transaction interface by inspecting deploycrate's
  `queue/queue.go`, storage abstraction, and transaction boundaries. Reuse River
  patterns, not unreviewed global queue limits or worker counts.
- [x] Add explicit migrations for a minimal operation/event model and River schema.
- [x] Implement a harmless, explicitly named test operation. Domain state and job
  insertion commit together; an injected transaction failure commits neither.
- [x] Add authenticated submission, operation read, and cursor-based event access.
  Define authorization, limits, timeouts, readiness, and graceful shutdown.
- [x] Implement durable progress, bounded retries, and visible terminal failure.
  A client timeout must not silently resubmit an operation with a new identity.
- [x] Introduce API/worker service entry points. Keep their lifecycle separable even
  if the initial deployment uses one supervised process.

**Gate:** a CLI or API client submits once, disconnects, and reconnects to the same
operation. Two worker instances can run; a worker crash does not lose accepted work.

**Delivered:** native pgx/River transactions, explicit migrations, scoped
hashed-token API auth, durable operation/event/step history, diagnostic and
platform submission/read/events/cancel/follow commands, separate API/worker
entry points, readiness/timeouts/graceful shutdown, and PostgreSQL/process
integration tests. See [control-plane setup](control-plane.md). No complete HA
claim is made.

### M0.3 — Node execution and recovery boundary

- [x] Adapt Check/Apply, journal/input hashes, atomic writes, and shell redaction.
- [x] Run the test operation locally through node dispatch. The transport interface
  is deliberately independent of the local implementation.
- [x] Add pinned SSH as a separate transport increment with separate tests. The
  pinned SSH endpoint passes the remote exactly-once scenarios: a lost response, a
  killed worker, a duplicate submission, a cancellation at a step boundary, and a
  node task that outlives its caller.
- [x] Add userspace WireGuard as a separate transport increment with separate
  tests. Pinned SSH inside a userspace WireGuard tunnel passes the same remote
  exactly-once scenarios against a kernel WireGuard node. See
  [the M0.3 WireGuard plan](m0.3-wireguard-plan.md).
- [x] Define durable local node execution identity and attach/status behavior. A
  retried job asks the node before dispatching and never starts a second live effect.
  Remote supervision and reattachment are decided for SSH in
  [the M0.3 SSH plan](m0.3-ssh-plan.md).
- [x] Persist and reconcile completed side effects when the corresponding database
  acknowledgement was interrupted, including tests that fail between effect and
  acknowledgement.
- [x] Define cancellation boundaries independently from detaching a viewer.

**Gate (local):** the local test path reports honest results; duplicate requests and
lost acknowledgements cannot duplicate the test side effect.
**Gate (pinned SSH):** the SSH endpoint passes the same exactly-once scenarios as
the local suite, including a lost response, a killed worker, a duplicate submission,
cancellation at a step boundary, and a node task that outlives its caller.
**Gate (userspace WireGuard):** passed. The SSH endpoint reached through a userspace
WireGuard tunnel passes the same scenarios, with a tunnel lost after dispatch as the
lost response. A tunnel that cannot come up dispatches no work.

**Delivered:** M0.3 is complete. Local node dispatch, durable node execution identity
and status, acknowledgement reconciliation, the pinned SSH transport with detached
node tasks, and the userspace WireGuard dialer it can run through. See
[setup and limitations](control-plane.md). There is no node enrollment, WireGuard key
distribution, multi-node placement, or distributed lease yet.

### M0.4 — First real Charm view

- [x] Build Bubble Tea v2 + Bubbles v2 + Lip Gloss v2 presentation primitives from
  the approved sketches: header, stage list, event viewport, status/footer.
- [x] Feed live operation events through a reducer also used by fixture tests.
- [x] Implement loading, queued, running, stale/reconnecting, failed, and completed
  states before adding dashboards. Show elapsed time and indeterminate work honestly.
- [x] Support resize, narrow layouts, help, keyboard navigation, plain output,
  no-color, reduced motion, and safe terminal text.
- [x] Test that leaving the viewer does not cancel the operation and that rendered
  state catches up correctly after replay, duplicate delivery, or connection loss.

**Gate:** the same test operation is usable through JSON/NDJSON, plain text, and a
real interactive terminal. No renderer is required for execution or diagnosis.

**Delivered (2026-10-20):** real Charm v2 operation viewer, shared reducer and
poll/replay loop, JSON/NDJSON/plain presenters, accessibility options, fixture and
render tests. A local PTY smoke harness (since removed) passed against a live PostgreSQL/API/worker:
resize down to 18×5, help/navigation, detach with exit 130 without cancellation,
then reconnect/replay to completion. This does not establish later host/fleet/HA gates.

## M1 — Bootstrap a control plane (research Phase 1)

**Depends on:** M0 and the database/platform/API decisions below.

- [x] Resumable first contact, credential handoff, and node-local bootstrap journal.
  Pre-database bootstrap progress cannot depend on the unavailable platform store.
- [x] Debian/Ubuntu compatibility checks and role-aware package/service steps.
- [x] Validate external database TLS/permissions, apply migrations safely, install
  the API/worker service, and expose readiness with useful failure reasons.
- [x] Context login/export, scoped automation credentials, ingress/access policy,
  and commit-confirm for changes that may break operator access. The exposure
  policy is a loopback/TLS listener requirement, access-risk acknowledgement, and
  commit-confirm watchdog; no reverse proxy or firewall is bundled.
- [x] Guided terminal setup with noninteractive flag parity and a plan/diff view.
- [x] Add a second control plane against the same database in integration tests.

**Gate (local):** passed for the control-plane side only: credential-once
handoff, the commit-confirm watchdog script, competing migrations, and two
control planes on one database. Host apply/resume/idempotency is not tested;
simulated-host tests with stubbed systemd/Docker/apt were removed.
**Gate (fresh VM):** unproven. Bootstrap apply has never converged a real host;
no clean Debian/Ubuntu VM install was run here.

**Delivered:** `eregion bootstrap` (local and pinned-SSH remote), resumable
node-local journal, Debian 12/13 and Ubuntu 22.04/24.04 checks, role-specific
packages/services (main/application/builder/telemetry), database preflight and
advisory-locked forward-only migrations, API/worker units, authenticated
readiness, contexts and private credential files, plan-bound scoped automation
credentials with action grants, guided Bubble Tea setup with flag parity, and a
detached commit-confirm watchdog. See [bootstrap](bootstrap.md).

## M2 — Enroll additional application and builder nodes (research Phase 2)

**Depends on:** M1 and an explicit role/placement policy.

- [x] Port enrollment, identity verification, capabilities, peer reconciliation,
  private connectivity, and role-specific host configuration.
- [ ] Isolate build credentials, caches, concurrency, and source execution from
  runtime workloads; never accept a hostname as sufficient authorization.
  Caches, source allowlisting, scrubbed build environments, per-node build slots,
  and identity checks are implemented. Dedicated build credentials and
  cross-node artifact movement are not; a distinct `build_node` is refused
  before `pack` rather than pretending the image moved.
- [x] Implement nodes list/status/check and operation-backed enrollment/retry.
- [x] Add a fleet terminal view with role labels, health, freshness, and focused
  node details. Empty and unreachable states are first-class.

**Gate (local):** passed for placement and identity. Nodes enroll over pinned SSH,
prove a node-generated identity, expose probed capabilities, reconcile WireGuard
peers, and cannot receive work outside their declared role/capability constraints.
**Gate (distro matrix):** unproven. Enrollment is exercised against the Debian 12
fixture, not every supported Debian/Ubuntu release and architecture.

**Delivered:** enrollment over pinned SSH, identity attestation and first-use
pinning, capability probing, role validation (including telemetry and registry),
WireGuard key/address/peer reconciliation, role-specific host configuration,
quarantine on identity swap, `platform nodes list/status/check/enroll/retry/reconcile/register`,
and the keyboard-driven fleet workspace. See [nodes](nodes.md).

## M3 — Build and deploy from CLI or CI (research Phase 3)

**Depends on:** M2; durable remote task recovery must already work.

- [x] Port apps/environments, revisioned secrets, sources/targets, releases,
  deployment records, desired/applied state, and change history into PostgreSQL.
- [ ] Port River build/release-command/deploy workers, Buildpacks execution on the
  main node or builder nodes, registry/artifact handling, and runtime placement.
  River workers, directory/git/image sources, release commands, placement, and
  Buildpacks on a co-located or builder node are implemented. The installed
  registry is not yet on the release/deploy path, so a distinct builder node is
  refused instead of pretending a local image is available on the app node.
- [x] Implement preflight → candidates → health → traffic switch → verification →
  cleanup, plus rollback and incomplete-operation reconciliation.
- [x] Add scoped CI deployment triggers, request deduplication, operation polling,
  follow/reconnect, safe retry/cancel, and dry-run plans.
- [x] Expand the terminal operation view with per-target state, build logs, explicit
  health observations, and a visible statement of which release is serving.

**Gate (local CLI/API):** passed on 2026-10-20 with local smoke harnesses (since
removed): real images were built and deployed, submissions deduplicated, the
serving release preserved after a failed candidate, events followed through the
CLI, and a rollback completed. A Buildpacks build of the sample directory source
was deployed and its served body verified.
Worker loss, lost acknowledgements, competing operations, and node loss during a
deploy are covered by PostgreSQL integration tests.
**Gate (CI end-to-end):** partially proven. A signed GitHub delivery is tested
through the mounted mux into the real operation service with duplicate-delivery
deduplication; a push submits a durable release build, not an automatic deploy.

**Delivered:** PostgreSQL apps/environments/releases/deployments/change history,
revisioned encrypted secrets, River build/deploy/rollback workers, Buildpacks and
image sources, preflight, release commands, candidate health, traffic switch,
post-switch verification, durable cleanup, rollback, reconciliation, scoped CI
action grants, webhook dedup, polling/follow, retry/cancel, dry-run plans, and a
per-target Charm operation view. See [platform contract](platform-contract.md).

## M4 — Resources and recovery (research Phase 4)

- [x] PostgreSQL application resources first: credentials, endpoints, volumes,
  placement, connectivity, health, and guarded lifecycle operations.
- [x] River-backed backup destinations, schedules, execution, verification,
  retention, safety backups, and confirmed restore. One explicit filesystem
  backup destination is supported; there is no object-store driver yet.
- [x] Keep the external control-plane database outside ordinary application-resource
  destruction. Document and exercise its separate recovery procedure.
- [x] Add resource/backup terminal views and destructive plan/confirmation screens.

**Gate (local):** passed. Resource creation/health/connectivity, backup/verify,
confirmed restore, failed-restore rollback, safety backup verification, declared
schedules, retention, verify-after, and the separate control-plane dump/restore/
readiness drill all pass against disposable PostgreSQL.
**Gate (deployed app):** connectivity is tested from a real pgx client on the
host, not from a deployed application container. Only PostgreSQL is in scope.

**Delivered:** labelled PostgreSQL containers and volumes with verified host
identity, generated and exported credentials, endpoint/health probes, guarded
destroy, River-backed backups with manifests/SHA-256, schedules, retention,
safety backups, confirmed restore with rollback, a durable restore journal and
recover action, exclusion of the control-plane database, an exercised separate
control-plane recovery procedure, and resource/backup terminal workspaces. See
[resources](resources.md).

## M5 — Telemetry node and network integrations (research Phase 5)

**Depends on:** M2 node enrollment and role/placement policy.

- [x] Add a `telemetry` node role, enrolled and managed like other nodes (Check/Apply
  steps, health, upgrades). It is never co-located with the control plane.
  The role is enrollable, host-prepped with Docker/Compose, refused with
  `control-plane`, and its stack actions use the shared River/node path. Stack
  re-install is the upgrade mechanism; there is no separate telemetry-upgrade
  command.
- [x] Install an opinionated, self-hosted stack on that node: OpenTelemetry Collector
  ingest, metrics/log/trace storage with bounded retention, and unmodified Grafana
  with eregion-authored dashboards and alerting defaults.
- [x] Expose OTLP for applications; run a collector on application/build nodes that
  ships container logs and host/service metrics to the telemetry node over the
  private network with per-node credentials.
- [x] CLI log access and optional terminal graphs that query the telemetry node
  through a data-source interface, with source, unit, time window, timestamp, and
  honest stale/no-data/no-telemetry-node states.
- [x] Optional export to an external observability stack instead of, or in addition
  to, the telemetry node. External-only is implemented for the per-node OTLP
  agent; the stack installer remains additive.
- [x] Cloudflare DNS reconciliation through River, custom routes, and private
  access device/grant operations with auditable authorization. DNS records are
  explicit desired records; private access covers device inventory and
  expiring grants, not device enrollment or a bundled tunnel client.

**Gate (local interface):** passed. The CLI returns source/window/unit/timestamp
and distinct ok/no-data/stale/absent states; agent, install, and credential
actions validate placement and never persist plaintext tokens.
**Gate (real infrastructure):** unproven. No telemetry VM, Docker Compose stack,
Grafana dashboard/alert evaluation, live OTLP delivery, Cloudflare zone, or real
private tunnel was exercised in this checkout.

**Delivered:** telemetry/application/builder role placement, production action
wiring through the per-node resolver, VictoriaMetrics/VictoriaLogs/vmauth/Grafana
rendering with retention and alert provisioning, OTLP receivers, per-node ingest
credentials, a hostmetrics/filelog agent, additive and external-only export,
`logs search/tail`, `metrics query`, workspace graphs, Cloudflare DNS
plan/reconcile with ownership markers and audit, and private-access grants with
audit. See [telemetry](telemetry.md) and [network](network.md).

## M6 — Webhooks and expanded terminal workflows (research Phase 6)

- [x] Signed GitHub webhook ingestion and deduplicated durable submission using the
  same path as CLI/CI. Do not build another queue or deployment implementation.
- [x] Expand keyboard-driven application/fleet views, guided forms, filters,
  command discovery, and graphs where data is available.

**Gate (local):** passed. HMAC-verified deliveries, repository/branch allowlists,
a durable `eregion_webhook_deliveries` reservation that survives operation
retention, duplicate replay returning the original operation, and conflict on a
reused delivery ID with different bytes. The handler submits through the real
`operations.Service`; it does not open a second queue. Interactive forms show
their exact noninteractive CLI equivalent.

**Delivered:** `POST /v1/webhooks/github` with signed, deduplicated pushes mapped
to durable release creation; fleet/app/environment/release/deployment/resource/
backup workspace tabs; filters, details, help, command discovery, and metrics
graphs through the telemetry data source. See [workspace](workspace.md).

## M7 — Upgrades and release readiness (research Phase 7)

- [x] Signed releases, operator/node upgrade and rollback, coordinated control-plane
  upgrades, compatibility checks, and schema migration policy. Coordinated
  control-plane upgrade is an inventory-validated operator plan plus sequential
  commands; it is not autonomous multi-host orchestration.
- [x] Validate the agreed HA scope: process/node loss, shared database outage,
  competing operations, and interrupted remote acknowledgements.
- [x] Complete recovery drills, agent skill/guide generation, security review,
  installation docs, terminal compatibility tests, and versioned release criteria.

**Gate (local/documented HA scope):** passed. Two control planes on one database,
worker/process loss, interrupted acknowledgements, duplicate submissions,
cross-process target locks, a connection-level shared-database outage, node loss
during a deploy, and competing operations are covered by integration tests.
**Gate (multi-host failover):** unproven. A PostgreSQL primary failover, load-
balanced control planes, and a live multi-host drill were not run. Publish only
the tested guarantees; multiple worker processes are not an HA platform.

**Delivered:** Ed25519-signed manifests with strict parsing, artifact size/SHA-256
verification, compatibility and schema-direction checks, atomic single-host
apply/rollback with journal and backup, health-checked automatic restore, a
signed S3 publish workflow, `eregion upgrade check/plan/apply/rollback/verify/guide`,
`eregion guide`, install/security/release-criteria docs, the terminal TERM matrix,
and local upgrade/control-plane/resource drills. See [upgrades](upgrades.md).

## Future direction (not scheduled)

Idea under consideration (2026-10-20), not part of v1 and not a commitment:

- **Cluster wrapper CLI.** Once eregion and eru are stable, a separate wrapper
  provisions infrastructure (servers, private network, firewalls, DNS) with
  OpenTofu/Terraform or provider APIs, then drives eregion and the PostgreSQL tool
  through their machine contracts (JSON/NDJSON, operation IDs, dry-run, idempotency)
  rather than importing their internals.
- **Ownership split.** The provisioning layer owns machines and networks; eregion and
  the PostgreSQL tool own what runs on them. No resource has two sources of truth.
- **Separate binaries.** Keep eregion and the PostgreSQL tool independent (different
  install targets, release cadence, and blast radius). Reduce duplicated code by
  extracting a shared Go module (contracts, output, Check/Apply engine, transport)
  once eregion's adapted copies stabilise, not by merging binaries.
- **Naming.** The wrapper may take the `eru` name, and the PostgreSQL tool would be
  renamed (candidates: Vairë, Mandos). Renaming touches binary names, `/etc/eru`,
  `/var/lib/eru`, lock paths, and schema IDs, so do it early or with a compatibility
  period.

Keep the door open now at no extra cost: expose machine-readable contract versions
for compatibility checks, and keep node enrollment taking existing hosts as input.
eregion never provisions machines itself.

## Decisions to make just in time

Most of these were decided while implementing; the remaining open items are not
silently approved by the sketches:

- **M0.2 (resolved):** PostgreSQL 17 + native pgx/River; scoped hashed bearer
  tokens; HTTP JSON plus cursor-based NDJSON events; the operation/idempotency/
  state contract in [contracts](contracts.md) and [control-plane](control-plane.md).
- **M0.3 (resolved locally/SSH/WireGuard):** supervision, reattachment, and
  ownership fencing are recorded in the [M0.3 plan](m0.3-plan.md), [SSH plan](m0.3-ssh-plan.md),
  and [WireGuard plan](m0.3-wireguard-plan.md). Cross-node distributed conflict
  scopes remain open ([next increments](ssh-wireguard-next.md)).
- **M1 (resolved):** Debian 12/13 and Ubuntu 22.04/24.04, amd64/arm64; external
  PostgreSQL is the only store; non-loopback API requires TLS; migrations use an
  advisory lock and forward-only checksums; bootstrap owns migration execution.
  The fresh-VM matrix itself is still unproven.
- **M2 (partly open):** role combinations are constrained in code (telemetry and
  control-plane cannot be enrolled; builder+application is refused; a distinct
  `build_node` is gated). Still open: how builds and registry artifacts move off
  the main node, dedicated build credentials, and a multi-tenant builder boundary.
- **M5 (mostly resolved):** VictoriaMetrics/VictoriaLogs (Apache 2.0), vmauth
  read gateways, optional Tempo, unmodified upstream Grafana, bounded retention,
  and file-referenced collector credentials. Still open: sizing/HA/backup
  expectations for the telemetry node and external-only stack install (the
  external-only agent path exists).
- **Ingress:** the current policy is loopback/TLS plus commit-confirm and private
  access grants. A bundled reverse proxy/firewall is a scope decision, not a
  hidden default.

## Current status and next evidence

The v0.1 local product is testable end to end: bootstrap a host, enroll nodes,
build and deploy an application, observe/reattach/detach, roll back, back up and
restore, read telemetry, reconcile DNS, revoke access, receive signed webhooks,
and upgrade/roll back the binary. The remaining work is evidence and hardening,
not placeholder commands:

1. Run the fresh-host VM matrix (Debian 12/13 and Ubuntu 22.04/24.04, amd64/arm64)
   for main, application, builder, and telemetry roles. Record G8 evidence.
2. Exercise a real Cloudflare zone/token, private-access tunnel, and telemetry VM
   with live OTLP + Grafana. Record G9 and the M5 real-infrastructure gate.
3. Implement cross-node artifact movement (registry push/pull and build
   credentials) so a distinct builder node can receive builds. Close M2.2/M3.2.
4. Drill PostgreSQL primary failover, load-balanced control planes, and a
   node-loss/upgrade sequence across hosts. Publish only what passes (G10/M7).
5. Add a bundled ingress policy (reverse proxy/firewall) if operators should not
   supply their own; today the API exposure policy is loopback/TLS plus
   commit-confirm.

Do not advertise production HA, fresh-VM support, or a multi-tenant builder
boundary before those runs exist. Browser sketches remain synthetic.
