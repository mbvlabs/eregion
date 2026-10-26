# Working on eregion

## Read first

Read `docs/MASTER.md`, then `docs/IMPLEMENTATION.md`. Use
`docs/MANUAL-TESTING.md` for acceptance and evidence. MASTER is the only product
status and work-list authority; focused references explain mechanisms, not
separate plans.

For terminal/UI work also read `docs/tui-design.md`. Keep it and its interactive
sketches as the design reference; `docs/workspace.md` describes current views.

- Functionality comes from `../deploycrate-ce`; where MASTER is silent,
  deploycrate's behavior is the requirement. Operator flows, terminal and agent
  conventions, and the release model come from `../vaire`. Inspect the relevant
  source, not just their READMEs. Do not copy vaire's database-specific
  architecture wholesale.
- No milestone tree, parallel roadmap or new handoff/spec document by default.
  Update the existing three core docs. Old plans are available in Git history;
  they are not active instructions. Root `scratchpad.md` is an ignored historical
  session log, not current requirements or authorization.

## Product rules

- One `eregion` binary, Go and embedded bash. No web management UI. Never
  Kubernetes, Compose deploys or Windows servers.
- External PostgreSQL and River remain the durable state/execution backend.
  No SQLite, second queue, or competing TOML store for platform entities.
- Operator control is SSH over embedded userspace WireGuard into node mode
  (MASTER D1). The token-bearing HTTP-over-SSH adapter is current transitional
  code; build new commands as node-mode commands, not on that adapter.
- Pre-v1: delete old paths and surfaces; no compatibility aliases or shims.
- Keep human and agent commands on the same application services. Every prompt
  needs a flag/input equivalent; JSON and no-TTY use must never prompt.
- Never put plaintext secrets in operation input, River arguments, events,
  journals or ordinary output. Handoff/export are explicit protected exceptions.
- A submission is accepted, not completed. A domain binding is desired state,
  not proof of an applied route, valid TLS or a reachable application.
- Do not add fleet/HA/backup scope to evade finishing the alpha app workflow.
- Release model (MASTER D14): `master` is edge, `1.x` is the v1 line, alphas are
  `v1.0.0-alpha.N`. Never cut a branch or push a tag without the user.

## Implement and verify

- Work through MASTER's ordered outcomes, completing a user-visible flow rather
  than isolated handlers. Record implementation and manual qualification separately.
- Fix root causes in the release; do not add ad-hoc repair commands as substitutes.
- Add isolated unit/regression tests, build and vet. No new Docker, Incus or
  scratch-VM harnesses. Host qualification is manual on an authorized real server.
- Existing tests can start Docker automatically. Inspect selected tests before
  running them; do not assume `go test ./...` is an offline-only suite.
- Never use an ambient production DSN/context as a test target. Do not change a
  server, database, DNS zone or release without authorization for that target.
- Do not edit applied migrations, erase journals, reset databases or discard user
  changes. No commit, push, tag or release unless the user requests it.
- At handoff, name what changed, what was tested, what was skipped, and the exact
  remaining blocker. Never call the alpha complete until the MANUAL-TESTING
  alpha baseline passes for the recorded build.
