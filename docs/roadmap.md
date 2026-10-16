# eregion implementation roadmap

**Status:** Proposed execution plan · 2026-10-16  
**Product direction:** [Initial research](initial_research.md), Sections 4–5  
**Terminal direction:** [TUI design](tui-design.md) · [interactive sketches](design/tui-sketches.html)

## Outcome

An agent can discover, plan, submit, observe, and recover application operations
without a terminal or a browser. A human can use the same contracts through an
exceptional Charm-based terminal interface. Neither client owns execution lifetime.

This is a dependency-ordered plan, not a delivery-date estimate. Checkboxes denote
verified deliverables, not the presence of placeholder commands. M0.1 is implemented;
later milestones remain planned. The browser sketches use synthetic fixtures only.

## Constraints that apply to every milestone

- External PostgreSQL and River; no SQLite, custom job queue, or separate broker.
- Debian and Ubuntu; select supported releases before shipping host mutations.
- Separate control-plane, application, and build responsibilities; role combination
  policy remains explicit, not accidental.
- Agent-first metadata, structured errors/output, no-TTY inputs, and dry-run or
  preview semantics ship with each command. Do not defer them to a polish phase.
- API and CLI reuse application services. UI packages never enqueue work or run SSH
  directly; they issue requests through the same client interfaces.
- Redact at ingestion and output. Untrusted logs cannot inject terminal escapes or
  control sequences. Do not persist plaintext secrets in events or River arguments.
- Remote effects are at-least-once; use idempotency, scoped coordination, and
  reconciliation. River queue concurrency is not a distributed environment lock.
- No bundled ClickHouse. Operation history works without an observability backend.
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

- [ ] Decide the database driver/transaction interface by inspecting deploycrate's
  `queue/queue.go`, storage abstraction, and transaction boundaries. Reuse River
  patterns, not unreviewed global queue limits or worker counts.
- [ ] Add explicit migrations for a minimal operation/event model and River schema.
- [ ] Implement a harmless, explicitly named test operation. Domain state and job
  insertion commit together; an injected transaction failure commits neither.
- [ ] Add authenticated submission, operation read, and cursor-based event access.
  Define authorization, limits, timeouts, readiness, and graceful shutdown.
- [ ] Implement durable progress, bounded retries, and visible terminal failure.
  A client timeout must not silently resubmit an operation with a new identity.
- [ ] Introduce API/worker service entry points. Keep their lifecycle separable even
  if the initial deployment uses one supervised process.

**Gate:** a CLI or API client submits once, disconnects, and reconnects to the same
operation. Two worker instances can run; a worker crash does not lose accepted work.

### M0.3 — Node execution and recovery boundary

- [ ] Adapt Check/Apply, journal/input hashes, atomic writes, and shell redaction.
- [ ] Run the test operation locally through node dispatch first; add pinned SSH
  and userspace WireGuard as separate transport increments with separate tests.
- [ ] Define durable node execution identity and attach/status behavior for remote
  tasks. If SSH or an orchestration worker dies, determine whether the node task
  continues before retrying; never blindly launch a second build/deployment.
- [ ] Persist and reconcile completed side effects when the corresponding database
  acknowledgement was interrupted. Test failure between effect and acknowledgement.
- [ ] Define cancellation boundaries independently from detaching a viewer.

**Gate:** local and remote test paths report honest results; duplicate requests and
lost acknowledgements cannot duplicate the test side effect.

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
idempotent re-apply; no builder stack is installed just because a host is a control
plane. Database outage and competing migration behavior are documented and tested.

## M2 — Enroll application and build servers (research Phase 2)

**Depends on:** M1 and an explicit role/placement policy.

- [ ] Port enrollment, identity verification, capabilities, peer reconciliation,
  private connectivity, and role-specific host configuration.
- [ ] Isolate build credentials, caches, concurrency, and source execution from
  runtime workloads; never accept a hostname as sufficient authorization.
- [ ] Implement nodes list/status/check and operation-backed enrollment/retry.
- [ ] Add a fleet terminal view with role labels, health, freshness, and focused
  node details. Empty and unreachable states are first-class.

**Gate:** both role types enroll on the supported distro matrix and cannot receive
work outside their declared placement/capability constraints.

## M3 — Build and deploy from CLI or CI (research Phase 3)

**Depends on:** M2; durable remote task recovery must already work.

- [ ] Port apps/environments, revisioned secrets, sources/targets, releases,
  deployment records, desired/applied state, and change history into PostgreSQL.
- [ ] Port River build/release-command/deploy workers, Buildpacks execution on build
  servers, registry/artifact handling, and runtime placement.
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

## M5 — Telemetry and network integrations (research Phase 5)

- [ ] External metrics/log/trace integration, with documented collector/backend
  examples and no bundled ClickHouse requirement.
- [ ] CLI log access and optional terminal graphs with source, unit, time window,
  timestamp, and honest stale/no-data/backend-unconfigured states.
- [ ] Cloudflare DNS reconciliation through River, custom routes, and private
  access device/grant operations with auditable authorization.

**Gate:** a documented external observability stack receives data, DNS reconciles,
and private access is enforced. Agent operation diagnosis still works without it.

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

## Decisions to make just in time

These are unresolved, not silently approved by the sketches:

- **Before M0.2:** PostgreSQL versions/TLS/driver; initial auth/scopes; API transport,
  event replay/retention, operation states, and submission idempotency contract.
  Suggested starting API: HTTP JSON plus cursor-based SSE or NDJSON events.
- **Before M0.3:** remote execution supervision/reattachment and ownership fencing;
  distributed conflict scopes. Local `flock` only protects local host mutations.
- **Before M1:** Debian/Ubuntu releases and architectures; external database hosting
  boundary; public/private API exposure; public SSH posture; migration ownership.
- **Before M2:** role combinations/resource-hosting role; build isolation; minimum
  HA deployment and artifact/secret availability.
- **Before M5:** collector packaging, local log access/buffering/retention, external
  data-source interface, and default telemetry collection cost.

## First implementation batch

M0.1 is complete; begin **M0.2** next, keeping commits reviewable. Use synthetic operation
fixtures from the sketches to test output and reducer contracts while PostgreSQL
integration is built. Add the real Charm operation viewer in M0.4, not a separate
demo CLI that grows its own business logic.

Do not port the entire command catalog, deploy real services, or advertise working
deployments in this first batch. Its output is one trustworthy vertical slice.
