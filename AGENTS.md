# Working on eregion

## Read first

Read `docs/MASTER.md`, then `docs/IMPLEMENTATION.md`. Use
`docs/MANUAL-TESTING.md` for acceptance and evidence. MASTER owns product vision,
scope and decisions; Linear owns actionable work and execution status in the
[Eregion project](https://linear.app/mbvlabs/project/eregion-2ff1d2e4d199).
Focused references explain mechanisms, not competing work lists.

For terminal/UI work also read `docs/tui-design.md`. Keep it and its interactive
sketches as the design reference; `docs/workspace.md` describes current views.

- Functionality comes from `../deploycrate-ce`; where MASTER is silent,
  deploycrate's behavior is the requirement. Operator flows, terminal and agent
  conventions, and the release model come from `../vaire`. Inspect the relevant
  source, not just their READMEs. Do not copy vaire's database-specific
  architecture wholesale.
- No milestone tree, parallel work list or new handoff/spec document by default.
  Keep product contracts and mechanisms in the existing core docs; track specific
  implementation tasks and qualification blockers in Linear. Old plans are
  available in Git history; they are not active instructions. Root
  `scratchpad.md` is an ignored historical session log, not current requirements
  or authorization.

## Product rules

- One `eregion` binary, Go and embedded bash. No web management UI. Never
  Kubernetes, Compose deploys or Windows servers.
- External PostgreSQL and River remain the durable state/execution backend.
  No SQLite, second queue, or competing TOML store for platform entities.
- Operator control is SSH over embedded userspace WireGuard into node mode
  (MASTER D1). Keep commands on node-mode application services; do not restore a
  token-bearing HTTP-over-SSH operator adapter.
- Pre-v1: delete old paths and surfaces; no compatibility aliases or shims.
  The historical alpha.7 exception does not apply to beta: remove legacy
  handoff/pending-init and submission-capability fallbacks before beta.1.
  Qualify beta on a fresh installation; do not add compatibility paths or
  promise an alpha-to-beta upgrade. Beta.1 publication for the operator's fresh
  test is authorized separately from that still-pending host qualification.
- Keep human and agent commands on the same application services. Every prompt
  needs a flag/input equivalent; JSON and no-TTY use must never prompt.
- Never put plaintext secrets in operation input, River arguments, events,
  journals or ordinary output. Handoff/export are explicit protected exceptions.
- A submission is accepted, not completed. A domain binding is desired state,
  not proof of an applied route, valid TLS or a reachable application.
- Diagnostics are read-only views of real build/deployment/container evidence.
  Remove executable diagnostic containers; do not add a replacement arbitrary
  execution mechanism or a transcript store for them.
- Fresh-server Docker logging defaults to `local`, with bounded rotation
  (`max-size=20m`, `max-file=5`, compression enabled), not unrotated `json-file`.
  Keep the API/worker under systemd. No container migration or compatibility
  work is required before v1.
- Do not add fleet/HA/backup scope to evade finishing the alpha app workflow.
  Durable application data belongs in external services under the current
  recovery policy; managed local volumes are expendable, never implicitly backed
  up by the control database. Replacement-VPS recovery documentation is not an
  RC gate (operator correction, 2026-11-14). No backup product or live recovery
  rehearsal is implied.
- Release model (MASTER D14): `master` is edge, `1.x` is the v1 line, alphas are
  `v1.0.0-alpha.N`. Never cut a branch or push a tag without the user.

## Implement and verify

- Work through the approved scope and its Linear tasks, completing a user-visible
  flow rather than isolated handlers. Repository product decisions constrain the
  implementation. Record implementation and manual qualification separately.
- Fix root causes in the release; do not add ad-hoc repair commands as substitutes.
- Test policy (operator decision, 2026-11-13): no unit tests. Add regressions
  through real PostgreSQL and/or running API/application-service integration.
  Retain helpers only where those integrations need them; do not rename pure
  mock tests as integration tests. Server setup/provisioning and SSH/WireGuard
  host qualification are manual. Keep build, vet and static checks. No new
  Docker, Incus or scratch-VM harnesses.
- Existing tests can start Docker automatically. Inspect selected tests before
  running them; do not assume `go test ./...` is an offline-only suite.
- Never use an ambient production DSN/context as a test target. Do not change a
  server, database, DNS zone or release without authorization for that target.
- Do not edit applied migrations, erase journals, reset databases or discard user
  changes. No commit, push, tag or release unless the user requests it.
- At handoff, name what changed, what was tested, what was skipped, and the exact
  remaining blocker. Never call the alpha complete until the MANUAL-TESTING
  alpha baseline passes for the recorded build.
