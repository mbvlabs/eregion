# eregion implementation roadmap

**Status:** Proposed execution plan · 2026-10-16  
**Product direction:** [Initial research](initial_research.md), Sections 4–5  
**Terminal direction:** [TUI design](tui-design.md) · [interactive sketches](design/tui-sketches.html)

## Outcome

An agent can discover, plan, submit, observe, and recover application operations
without a terminal or a browser. A human can use the same contracts through an
exceptional Charm-based terminal interface. Neither client owns execution lifetime.

This is a dependency-ordered plan, not a delivery-date estimate. Checkboxes denote
verified deliverables, not the presence of placeholder commands. M0.1, M0.2, and
M0.3 are implemented: M0.3's local execution boundary, its pinned SSH transport, and
its userspace WireGuard transport all pass the exactly-once scenarios. Later
milestones remain planned. Browser sketches use synthetic data.

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

**Delivered:** local `commands`, `help`, and `version`; `eregion/v1` contracts and
JSON Schema; a future operations service interface and NDJSON serializer, not a
running service. See [contracts](contracts.md) for invariants and deferred behavior.

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

**Delivered:** native pgx/River transactions, explicit migrations, scoped hashed-token
API auth, diagnostic submission/read/events/cancel commands, separate API/worker
entry points, and PostgreSQL/process integration tests. See [setup and limitations](control-plane.md).
No remote host execution, deployment, interactive viewer, or complete HA claim yet.

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

- [ ] Build Bubble Tea v2 + Bubbles v2 + Lip Gloss v2 presentation primitives from
  the approved sketches: header, stage list, event viewport, status/footer.
- [ ] Feed live operation events through a reducer also used by fixture tests.
- [ ] Implement loading, queued, running, stale/reconnecting, failed, and completed
  states before adding dashboards. Show elapsed time and indeterminate work honestly.
- [ ] Support resize, narrow layouts, help, keyboard navigation, plain output,
  no-color, reduced motion, and safe terminal text.
- [ ] Test that leaving the viewer does not cancel the operation and that rendered
  state catches up correctly after replay, duplicate delivery, or connection loss.

**Gate:** the same test operation is usable through JSON/NDJSON, plain text, and a
real interactive terminal. No renderer is required for execution or diagnosis.

## M1 — Bootstrap a control plane (research Phase 1)

**Depends on:** M0 and the database/platform/API decisions below.

- [ ] Resumable first contact, credential handoff, and node-local bootstrap journal.
  Pre-database bootstrap progress cannot depend on the unavailable platform store.
- [ ] Debian/Ubuntu compatibility checks and role-aware package/service steps.
- [ ] Validate external database TLS/permissions, apply migrations safely, install
  the API/worker service, and expose readiness with useful failure reasons.
- [ ] Context login/export, scoped automation credentials, ingress/access policy,
  and commit-confirm for changes that may break operator access.
- [ ] Guided terminal setup with noninteractive flag parity and a plan/diff view.
- [ ] Add a second control plane against the same database in integration tests.

**Gate:** supported fresh-host VM matrix passes bootstrap, interrupted resume, and
idempotent re-apply; the main node installs the application runtime, Buildpacks
build stack, and container registry, and no telemetry stack. Database outage and competing migration behavior are documented and tested.

## M2 — Enroll additional application and builder nodes (research Phase 2)

**Depends on:** M1 and an explicit role/placement policy.

- [ ] Port enrollment, identity verification, capabilities, peer reconciliation,
  private connectivity, and role-specific host configuration.
- [ ] Isolate build credentials, caches, concurrency, and source execution from
  runtime workloads; never accept a hostname as sufficient authorization.
- [ ] Implement nodes list/status/check and operation-backed enrollment/retry.
- [ ] Add a fleet terminal view with role labels, health, freshness, and focused
  node details. Empty and unreachable states are first-class.

**Gate:** application and builder nodes enroll on the supported distro matrix and cannot receive
work outside their declared placement/capability constraints.

## M3 — Build and deploy from CLI or CI (research Phase 3)

**Depends on:** M2; durable remote task recovery must already work.

- [ ] Port apps/environments, revisioned secrets, sources/targets, releases,
  deployment records, desired/applied state, and change history into PostgreSQL.
- [ ] Port River build/release-command/deploy workers, Buildpacks execution on the
  main node or builder nodes, registry/artifact handling, and runtime placement.
- [ ] Implement preflight → candidates → health → traffic switch → verification →
  cleanup, plus rollback and incomplete-operation reconciliation.
- [ ] Add scoped CI deployment triggers, request deduplication, operation polling,
  follow/reconnect, safe retry/cancel, and dry-run plans.
- [ ] Expand the terminal operation view with per-target state, build logs, explicit
  health observations, and a visible statement of which release is serving.

**Gate:** a Go sample app builds and deploys from CLI and CI/API, survives viewer and
worker loss, swaps traffic safely, and rolls back. Duplicate CI requests do not
create duplicate deployments. A failed candidate leaves the serving release intact.

## M4 — Resources and recovery (research Phase 4)

- [ ] PostgreSQL application resources first: credentials, endpoints, volumes,
  placement, connectivity, health, and guarded lifecycle operations.
- [ ] River-backed backup destinations, schedules, execution, verification,
  retention, safety backups, and confirmed restore.
- [ ] Keep the external control-plane database outside ordinary application-resource
  destruction. Document and exercise its separate recovery procedure.
- [ ] Add resource/backup terminal views and destructive plan/confirmation screens.

**Gate:** app-to-database access and backup/verify/restore drills pass; failed restore
has a tested recovery path. Additional engines require explicit scope decisions.

## M5 — Telemetry node and network integrations (research Phase 5)

**Depends on:** M2 node enrollment and role/placement policy.

- [ ] Add a `telemetry` node role, enrolled and managed like other nodes (Check/Apply
  steps, health, upgrades). It is never co-located with the control plane.
- [ ] Install an opinionated, self-hosted stack on that node: OpenTelemetry Collector
  ingest, metrics/log/trace storage with bounded retention, and unmodified Grafana
  with eregion-authored dashboards and alerting defaults.
- [ ] Expose OTLP for applications; run a collector on application/build nodes that
  ships container logs and host/service metrics to the telemetry node over the
  private network with per-node credentials.
- [ ] CLI log access and optional terminal graphs that query the telemetry node
  through a data-source interface, with source, unit, time window, timestamp, and
  honest stale/no-data/no-telemetry-node states.
- [ ] Optional export to an external observability stack instead of, or in addition
  to, the telemetry node.
- [ ] Cloudflare DNS reconciliation through River, custom routes, and private
  access device/grant operations with auditable authorization.

**Gate:** a telemetry node enrolls, receives application OTLP data and node logs/
metrics, serves Grafana dashboards, and the CLI can search/tail logs from it; DNS
reconciles and private access is enforced. Agent operation diagnosis still works
with no telemetry node.

## M6 — Webhooks and expanded terminal workflows (research Phase 6)

- [ ] Signed GitHub webhook ingestion and deduplicated durable submission using the
  same path as CLI/CI. Do not build another queue or deployment implementation.
- [ ] Expand keyboard-driven application/fleet views, guided forms, filters,
  command discovery, and graphs where data is available.

**Gate:** GitHub delivery retries are safe; all interactive actions have documented
noninteractive equivalents. No web UI is required.

## M7 — Upgrades and release readiness (research Phase 7)

- [ ] Signed releases, operator/node upgrade and rollback, coordinated control-plane
  upgrades, compatibility checks, and schema migration policy.
- [ ] Validate the agreed HA scope: process/node loss, shared database outage,
  competing operations, and interrupted remote acknowledgements.
- [ ] Complete recovery drills, agent skill/guide generation, security review,
  installation docs, terminal compatibility tests, and versioned release criteria.

**Gate:** publish only the tested failover guarantees. Multiple worker processes
alone are not proof of an HA platform.

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

These are unresolved, not silently approved by the sketches:

- **Before M0.2:** PostgreSQL versions/TLS/driver; initial auth/scopes; API transport,
  event replay/retention, operation states, and submission idempotency contract.
  Suggested starting API: HTTP JSON plus cursor-based SSE or NDJSON events.
- **Before M0.3:** remote execution supervision/reattachment and ownership fencing;
  distributed conflict scopes. Local `flock` only protects local host mutations.
  The local decisions are recorded in the [M0.3 plan](m0.3-plan.md); the SSH
  decisions are recorded in the [M0.3 SSH plan](m0.3-ssh-plan.md), and the WireGuard
  decisions in the [M0.3 WireGuard plan](m0.3-wireguard-plan.md). Distributed
  conflict scopes across nodes remain open for
  [the SSH/WireGuard increments](ssh-wireguard-next.md).
- **Before M1:** Debian/Ubuntu releases and architectures; external database hosting
  boundary; public/private API exposure; public SSH posture; migration ownership.
- **Before M2:** which roles may combine beyond the main node's defaults (edge role
  scope, resource-hosting role); how builds and the registry move off the main node;
  build isolation from runtime workloads on the main node; minimum
  HA deployment and artifact/secret availability.
- **Before M5:** telemetry storage engines (license and footprint: e.g. VictoriaMetrics
  + VictoriaLogs (Apache 2.0) vs Loki/Tempo/Mimir (AGPLv3)); Grafana AGPLv3 posture
  (ship unmodified upstream) and dashboard licensing; retention and sizing defaults;
  telemetry-node HA/backup expectations; collector packaging and auth; external
  export interface.

## First implementation batch

M0.1, M0.2, and M0.3 are complete. M0.3's pinned SSH transport is tested against a
disposable SSH endpoint, both directly and through a userspace WireGuard tunnel to a
kernel WireGuard node. Next is M0.4's Charm viewer. Use
synthetic operation fixtures from the sketches to test output and reducer contracts.
Add the real Charm operation viewer in M0.4, not a separate demo CLI that grows its
own business logic.

Do not port the entire command catalog, deploy real services, or advertise working
deployments in this first batch. Its output is one trustworthy vertical slice.
