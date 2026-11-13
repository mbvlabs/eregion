# Manual testing

Acceptance checks for the v1 alpha/beta line and the record of real-server runs.
[MASTER](MASTER.md) owns product scope and decisions (D-numbers below); Linear
tracks execution. This file says what must work on a real server and what has
been observed.

## Current test boundary (operator decision, 2026-11-13)

Automated behavioral coverage is integration-only: real PostgreSQL and/or
running API/application services. All unit tests are removed, including
mock-only HTTP/RPC, parser-only and offline shell fixture tests. Do not rename
those tests to retain them. Database/service tests may fake external host
effects, but their application behavior and persistence must be real.

Server setup/provisioning, SSH/WireGuard host behavior and real deployment-host
qualification are manual on an explicitly authorized server. Automated test
recipes do not start local PostgreSQL or SSH/WireGuard fixture containers.
Build, formatting, vet and actual production release-verification safeguards
remain automated.

The normal and race integration recipes require a disposable database DSN.
CI supplies its isolated PostgreSQL service; an ambient installation DSN is
never an authorized substitute. Historical unit-test counts and shell-harness
results below describe earlier candidates, not the current verification gate.

The local integration-only candidate adds database-backed assertions for
Go-owned defaults, admission atomicity and invalid enum/data rejection across
application/environment creation, domains/resources, deployment records,
retention, vault/provider revisions, audit allocation, cleanup and release
commands. It also exercises authenticated HTTP resource admission against the
real database-backed service and checks corrupted stored resource/secret
snapshots fail closed. Existing worker, migration, recovery and
concurrency integrations remain; the real API/worker executable lifecycle test
is retained, with its explicit Linux/database/public-host-key prerequisites.

Candidate `6ae7b0a` ran in CI `43426626454`. Build/static checks and macOS passed.
The normal PostgreSQL suite completed in 192.894 seconds with 223 top-level
passes, one expected helper skip, and two failures in new node-initialization
and retention-run default assertions. The 21-file Goose migration, partial
history retry and orphan-refusal integrations passed. The race step did not
run because the normal suite failed. This is partial qualification only;
corrections and a complete normal/race rerun are required.

## Unpublished three-channel correction (2026-11-13)

The reviewed local correction routes development/alpha/beta to edge, release
candidates to rc and finals to stable. Focused release/upgrade tests, offline
publication harnesses, temporary-Git ordering tests and conditional-write
adapter tests passed. The pinned AWS CLI passed offline model checks and a
local fake-HTTP conditional-header/412 test. These are not live R2 evidence;
the downloaded CLI's official artifact checksum was not independently verified.

Before publication qualification, verify conditional pointer writes on an
explicitly authorized R2 target. Publish and verify the first corrected
anonymous edge build separately, including historical beta.5's edge upgrade,
before attesting bootstrap completion or allowing a tagged alpha/beta to
replace the pointer. The bootstrap attestation is not set by this work.
Historical beta.5 may reject later tag-signed edge pointers and may fail closed
during a moving manifest/bundle race. No compatibility guarantee, new release
or existing-server upgrade is claimed.

## Unpublished separate Goose history (2026-11-13)

The correction must retain separate, dependency-ordered Goose migrations for
the original schema areas, including vault tables, rather than one baseline.
Each table is created in its final shape without subsequent ALTER migrations.
Published beta.5 remains schema 33. The replacement history has no
application defaults, domain CHECKs, business triggers or policy seeds; Go
supplies initial values and validates references and domain policy. Source
review covered schema parity, write values, read validation, migration refusal
and parent/reference locking. These reviews do not establish PostgreSQL passes.

Qualification must use an explicitly authorized fresh disposable PostgreSQL
target, never an ambient installation DSN. Required evidence includes read-only
planning, refusal of legacy/orphan/River-only schemas without writes, supported
schema resolution, transactional migration retry, concurrent migrators,
cancellation/pool lifetime, Goose/River recovery histories, fresh definition
apply without a DNS-policy row, explicit-value fixtures, and coordinated
pointer/build/diagnostic/deletion/retention races. Preserve stale-attempt,
unknown-effect and restore-owner protections. No existing database migration,
adoption or compatibility path is promised.

The operator authorized a CI-only branch submission. Candidate `e6f5eab` ran
against the existing isolated PostgreSQL 17 service in CI `43416047287`.
Integration completed in 329.793 seconds with 228 top-level passes, one expected
helper skip and one resource-reconciliation failure. Goose fresh/read-only
planning, refusal, history validation, transactional retry, schema selection,
two-connection concurrency/cancellation and the added pointer-locking
regressions passed. This is partial evidence, not an overall suite pass.
Linux also failed an SSH deadline assertion, and macOS failed the conditional
AWS adapter fixture. Corrections and a full CI rerun are required. Only CI ran:
no publication or existing-server/database change occurred.

Candidate `5d3eecc` subsequently passed all CI jobs, including 229 top-level
PostgreSQL passes and one expected helper skip. **That result covers the
incorrect single-file layout only.** The operator then clarified that the
history must use separate migrations. The split requires fresh source,
ordering, pending-version and partial-migration retry verification; the earlier
green CI must not be presented as qualification of the corrected layout.

## Beta.4 incremental acceptance

These checks describe the candidate changes, not a pass record. Publication is
authorized separately from live qualification. No live target is authorized by
this checklist; preserve existing databases, volumes, DNS, credentials and hooks.

| Changed surface | Required candidate evidence |
| --- | --- |
| Read-only diagnosis | Inspect an authorized failed and active operation. Verify recorded source/image identities, historical versus current settings, event continuation, and explicit missing evidence. The command must not upgrade a node, pull an image or change deployment state. No raw build environment values or secrets may appear. |
| Candidate evidence | Observe only the recorded deployment's planned, correctly labelled containers with the exact pinned image. Missing identity or redaction context must withhold logs. Child stderr is observable only when forwarded to container logs. |
| Isolated diagnostic execution | On a separately authorized disposable fixture, verify exact confirmation, historical image/node pinning, immutable image use without pulling, fixed sandbox limits, and a new operation distinct from the original. Verify cancellation, lost-response recovery and cleanup before terminalization; an ambiguous prior execution must never be rerun automatically. Command output is deliberately withheld. |
| TUI deployment history | Open active and terminal deployments, read their linked operation history, return to row details, open help/discovery overlays, and detach without cancellation. |
| TOML automation | Export and reapply an enabled environment without rotating its webhook secret or changing other desired state. Verify omitted fields preserve automation, explicit disable retains the reference, and invalid or archived references refuse without mutation. On an authorized fixture, verify actual master push delivery and PR-branch isolation afterward. |
| Definition boundaries | Inspection snapshots remain non-applicable with safe guidance. Ordinary domain bindings do not claim to reconstruct managed DNS/provider ownership. Bootstrap secrets and resources through their documented separate steps. |
| Shared definition planning | Verify a server-side preview reads provider state even for an unchanged binding, and that changed policy, inventory or provider records invalidate the prior digest. Verify missing secret metadata and unsafe managed cleanup block without mutation. Confirm removals of config, processes, secret references, resource attachments and domains require the explicit target in addition to the plan digest. Existing app/environment reconciliation does not create a target or deploy. These checks are not a qualification pass. |
| Definition DNS reconciliation | On an authorized disposable hostname, verify missing records are created, every matching external record remains unchanged and unowned, and conflicting A/AAAA or CNAME records require approval of the exact displayed plan. Verify owned-plus-external and duplicate matching records, and preserve external records during subsequent binding cleanup. Explicit public origins and configured DNS policy are prerequisites; credentials alone do not enable management. |
| Definition apply recovery | Once the complete lifecycle is enabled, interrupt admission and a provider response; verify replay returns the same parent and resumes the same child without duplicate DNS creates. Verify cancellation and exhausted-job recovery cannot finish the parent while a child can still execute. Confirm competing ordinary mutations are refused while the run is active, primary promotion precedes old-primary removal, and apply neither changes the serving release nor queues a deployment. CNAME replacement has a disclosed resolution gap, not an atomic provider switch. |
| Public build targets | Import/export uses `targets`; CLI uses `--target`. Removed public names are rejected. Optional target mappings retain their documented Go-buildpack semantics; selecting another builder does not imply its runtime qualification. |
| Secrets handoff filename | At an authorized human handoff, verify the suggested name is `<workspace>--<node>-secrets.md`; explicit paths, owner-only permissions and overwrite refusal remain unchanged. Do not capture the record's contents as evidence. |

The new PostgreSQL automation admission/archival regressions require an explicitly
authorized disposable database. Compilation without that target is not evidence
that the database tests passed. Offline build, vet and selected regressions do
not establish live sandbox or deployment acceptance.

The reported Rails Docker/containerd export failure, worker-lease failures,
preview/admission discrepancies and stale observations remain separate findings
until a cause-specific fix is verified; publication alone does not close them.

## 2026-11-12 — beta.4 Rails Buildpacks export investigation

Operator-supplied evidence on `thelab/main`, Docker 29.1.3 with the containerd
image store, follows the beta.4 failure recorded in the Rails fixture handoff.
This is export diagnosis, not a successful Buildpacks deployment. Execution is
tracked in LABS-357.

- **Fail:** platform-filtered save of `heroku/heroku:24` reports no suitable
  export target, while platform inspection selects its amd64 child and
  unfiltered save returns successfully.
- **Incomplete export:** the operator's streamed unfiltered archive listing
  contains the index, amd64 manifest and attestation-related blobs, but lacks
  the exact amd64 manifest's required config and both compressed layer blobs.
  Those requirements were checked against the immutable public registry
  manifest. Successful unfiltered save is not proof of a complete image.
- **Failed repair attempt:** with explicit operator authorization, pulling
  `heroku/heroku@sha256:9de81d37c1fc00db948739da7ad3523d42c84c9fa27f1685fca0b0ff006081c7`
  for `linux/amd64` succeeded. Filtered save still failed for both the original
  tag and that exact child reference. Neither pulling nor child substitution
  is a verified remedy.
- **Not run:** another application deployment, Rails database release command,
  candidate health and public HTTPS qualification after a fix. No image
  deletion, daemon restart or storage-backend change was performed in this
  investigation. No new production health observation is inferred from these
  image-only commands.

The archive is demonstrably incomplete; the internal cause of the missing
export content remains unverified. Keep a source-level alternative and its
offline checks separate from authorized real-host acceptance.

## 2026-11-12 — registry-backed Buildpacks publish candidate

This candidate replaces the failing local-daemon export path for Git
Buildpacks releases with `pack --publish` into the existing private registry.
It has offline, unit and source-level evidence only. The build, deployment,
Rails release command, candidate health, public HTTPS response, retention
execution and fresh-server installation are **not run** here; the beta.4
investigation above remains the operator's failure evidence.

| Changed surface | Required candidate evidence |
| --- | --- |
| Published Git build | On the authorized fresh beta.5 server, build a Git application (including the Rails/Heroku builder fixture) with Docker 29 containerd storage. Verify pack publishes the application image to the private registry, uses the transient owned bridge and registry container without host networking or a daemon insecure-registry change, and does not perform the failing local run-image export. Verify the build node architecture is explicit for both the default Paketo profile and an explicit builder. |
| Immutable release identity | Verify the ready release records the canonical configured-loopback `repository@sha256:<manifest>` in `image`, the Docker config digest in `image_id`, and the exact Git commit. A local tag alone must not pass the Pack step's `Check`; a pre-existing unrelated tag must not qualify. |
| Ready-image recovery | Remove only the local Docker materialization of a ready Git release, then deploy and roll back to it. Verify the node pulls the recorded canonical digest and the resulting config ID equals the recorded `image_id`; a deliberate mismatch must fail without rewriting the release or accepting a mutable tag. |
| Promotion | Promote a registry-backed staging release and verify production copies both the canonical reference and config ID; no rebuild occurs. |
| Cancellation and crash recovery | Cancel a published build and interrupt one between registry publication and the local completion tag. Verify lifecycle containers and only the owned network's registry attachment are cleaned, the shared registry stays running, a stale owned network is removed on retry, and the recovered Pack step returns the exact commit and canonical digest. |
| Retention boundaries | With the existing D30 planner, verify protected serving and rollback manifests remain when Docker `RepoDigests`/local image metadata are absent or a tag moves, and an eligible unprotected artifact is removable ordinarily. Confirm no blanket prune or immediate removal of retained artifacts is introduced. |

Publication is not evidence that this table passed. The fresh target,
database, deployment and retention execution require separate authorization;
the unchanged live target must not be used for this candidate.

## Beta.5 app repository and explicit artifact acceptance

This section describes the beta.5 candidate contract; it is **not** a beta.5
pass record and no new live target is authorized here. The operator-supplied
beta.4 report at
`/home/mbv/work/eregion-beta-one/deploy/beta4-test-results.md` remains the
reference for the earlier operator evidence: supported definition plan/apply
with old/new diff, the harmless staging marker deploy, definition-driven PR DNS
and isolated PR deployment, deferred secret-consuming resource bootstrap,
automation export/reapply without secret values, and refreshed TLS/public
observations. That report also records the unresolved node-task retry
correlation behind an unrelated build and notes that a fresh master-push
webhook delivery was not retested. It does **not** establish the historical
worker-lease root cause, and this candidate does not claim that cause fixed.

| Changed surface | Required candidate evidence |
| --- | --- |
| App repository | On a fresh beta.5 server, create an app with `--image-repository mbvlabs/<app>` and without a Git source. Verify the returned/exported repository is the actual durable value, an omitted value defaults to the app slug, a second app cannot claim the same path, and no environment create/update or TOML override can change it. |
| Admission composition | Exercise private `--image-tag`/`--image-digest` submissions through the zero-worker node API and the SSH node app service. Verify both load the strict managed loopback registry endpoint even with no River worker, classify a full reference to that endpoint as private rather than external, and fail app creation/canonical overflow closed when the managed endpoint is missing. Offline discovery remains available without registry configuration. Verify a fresh bootstrap writes full node name/state fields to node.json after the binary and before the first API/worker start, so the first worker start succeeds without a retry. |
| Git-less app and deploy | Create an image-only app without `--deploy`; verify no first deploy is queued. Attempt an explicit create-and-deploy without Git/artifact and verify it fails before any app/environment/release row or vault write. Run `deploy` with no selector and verify the clear artifact-selection guidance rather than a latest/newest/serving guess. |
| Selector admission | Push a local image with `eregion image push --app <app>` and verify the node resolves the durable app repository. Deploy `--image-tag`, then repeat with `--image-digest` and a fully qualified `--image`. Verify selector flags are mutually exclusive, no implicit latest exists, a full reference to this private registry with another repository is refused, and an external public digest from a different repository is admitted and pulled anonymously by canonical manifest. |
| Upload identity fence | Delete/recreate the app (or same-name app with a new ID) while an upload is paused before payload read. Verify the upload refuses before archive bytes or Docker load/push, a paused push blocks app and last-environment final metadata deletion, and the reverse lock order leaves no deletion rows. Contend each lock with no payload read; after an error/cancel, verify both locks release only after the helper stops. |
| Physical registry-host binding | On first fresh workspace assignment, verify the singleton records the main node name, public SSH host-key type/SHA256 fingerprint and canonical node state directory atomically. Replay with identical values succeeds; a different name, fingerprint, state directory or second host refuses. A pre-binding workspace stays unverified and upload/final deletion refuse without backfill. Cross-host tests run only with an authorized disposable DSN and no Docker. |
| External metadata safety | Resolve a public reference through an anonymous token realm. Verify HTTP downgrade, redirects to loopback/link-local/private hosts, localhost/private-IP realms, DNS answers that include private addresses, userinfo realms, and environment proxies are refused before metadata use. Offline tests only; no external network call. |
| Pull credential isolation | Set a sentinel `HOME`, `DOCKER_CONFIG` and credential helper on the node user. Materialize an admitted artifact and verify the child Docker process receives an explicit private empty config and never reads the ambient config/helper. A missing isolated config fails closed instead of falling back. |
| Canonical repository bound | Create an app repository at the maximum private-path length with the actual configured registry endpoint. Verify creation is refused before any app row when the canonical `endpoint/repository@sha256:…` reference would exceed the 255-character release image bound, and a boundary-fitting repository succeeds. |
| Pinned replay | Move the tag after acceptance and deploy/replay the same idempotency key during a registry outage. Verify the replay returns the original operation without registry I/O, the release keeps the originally admitted manifest and config digest, and a new key resolves the new tag. Recheck app/repository/environment placement after resolution; a concurrent placement change must conflict without a release row. |
| Preview | Run `deploy --image-tag --dry-run --json`. Verify the app repository and selector are shown, availability is explicitly unverified, known runtime/process/domain/DNS/deployability blockers still appear, and no registry read, acceptance, vault write or artifact reservation occurs. Replay with the recorded key still fences the hash early. |
| Worker identity | Delete only the local materialization of an admitted artifact, then deploy/restart it. Verify the worker pulls the pinned canonical digest, checks the config digest against the admitted `image_id`, then marks the release ready; a config-digest mismatch fails with the old release still serving and no tag fallback, rebuild or mirror. |
| Retained foundation | Recheck the reviewed registry publish/bridge endpoint identity and node-effect ownership fences. In particular, a ready-image `docker pull` observation remains a node effect whose LIVE or UNKNOWN status retains deployment ownership until safely observed. |
| Removed legacy admission | Verify `platform.release.create` is not a registered action and no arbitrary destination or pull-toggle route can create a weaker artifact path. Internal build retry still works. |
| Retention | Verify all environments sharing an app repository protect accepted pending/serving/rollback manifest digests and moved tags under the unchanged D30 policy, with exact repository parsing and no external-registry deletion. |
| Definition and export | Apply a schema-2 definition with a Git `[source]`, an empty/absent source, and root `image_repository`. Verify schema-1 documents and `source.kind`/`source.image` are rejected without aliases, an omitted or empty source clears an existing Git recipe, export omits a Git-less source, enabled automation without Git is refused rather than silently retaining an enabled webhook, `app export` writes the actual repository, and environment apply cannot change it. |
| Git-less CLI default | Create an image-only app via flags, definition file and interactive/non-TTY paths without `--no-deploy`. Verify no first deploy is requested, while an explicit API `deploy: true` without Git/artifact still fails before any partial commit. A missing app inventory lookup for export must fail closed, never fabricate a default-slug repository. |

**Pre-RC definition version decision (2026-11-13):** published
`v1.0.0-beta.5` remains the immutable historical record and used definition
version 2. The current pre-RC source returns the marker to version 1 for the
next prerelease: it accepts version-1 documents only, rejects version 2 as
unsupported, and still rejects `source.kind`/`source.image` as unknown fields
under version 1. The approved app repository, optional Git source and
no-environment-image-mode model is unchanged. The beta.5 row above keeps its
published criteria; current definition/export checks use version 1.

The beta.5 PostgreSQL admission/replay/placement regressions are gated on an
explicitly authorized disposable DSN. Local verification compiled them without
running them. The CI attempt below used its isolated PostgreSQL service; it did
not establish a complete integration pass.

### Beta.5 source CI attempt

The operator authorized committing and pushing the reviewed candidate and
publishing beta.5 only after the release gates pass. Source
`b9d09df05594b5b9643520200a46a7b70761b1c1` passed Linux checks, race tests,
build/discovery and macOS CI in run `43305307319`. Its PostgreSQL 17 integration
job failed artifact-admission assertions and then timed out during build-slot
test cleanup. No complete SQL integration pass is claimed. Edge publication
`43305307263` succeeded; beta.5 was not tagged or published at this gate.

Earlier local verification mistakenly selected Docker-backed SSH/WireGuard
fixtures despite the local test restriction. Their caller-cancellation
assertions failed against the new explicit cancellation classification. The
assertions were corrected without rerunning those fixtures locally. A subsequent
read-only inventory of the local system Docker socket found no remaining
`eregion-ssh-fixture-*` containers; cached fixture images and temporary key
directories were not removed or claimed clean. This incident is not live
application qualification, and no application or production resource cleanup
was performed.

## Alpha.1 qualification

The operator explicitly confirmed that all alpha.1 testing is done, including
the disk-retention gate. Alpha.1 is recorded as qualified by that confirmation,
not by a reconstructed baseline: no build, date, operation, individual
observation or pass result is invented or backfilled. The run records below are
the evidence retained in this checkout; a **partial** or **not run** entry in a
historical record is an evidence gap for that run, not an open alpha.1 blocker.

Alpha.2 acceptance is **incremental**: it is qualified against its own
acceptance criteria on the explicitly authorized existing main node, disposable
fixture app, authorized hostname and authorized GitHub repository. A complete
fresh-server baseline is not required for each alpha; a fresh-server
v1.0.0-final rehearsal remains a separate final decision. Alpha.1 stays
operator-qualified. macOS acceptance (R10) remains required before v1.0.0 final
and was not an alpha.1 gate. No evidence is reconstructed or backfilled.

## Evidence rules

- Record the build/commit, date, operator OS/arch, server provider, OS/arch and
  topology, and the outcome. A run from a checkout is not a published-binary run.
- Use **pass**, **fail**, **blocked** (missing implementation) or **not run**.
  A skipped or absent check is never a pass.
- Explicit operator confirmation may close an alpha qualification. That records
  the status; it does not turn an unrecorded check into a pass and does not
  authorize invented evidence.
- A submission is acceptance only. Keep the operation ID, wait for its terminal
  state, inspect serving and route state, and check the external response.
- Never store tokens, DSNs, passwords, handoff plaintext or passphrases, or
  secret values in evidence. IDs, versions, sanitized errors and response
  markers suffice.
- An existing server is not disposable. Get authorization before mutating a
  host, DNS, a database or a release.

## Alpha.7 incremental acceptance

This matrix describes the alpha candidate and its recorded transition behavior,
not beta command syntax. Beta removes the public assignment transition and the
old local formats; use the beta fresh-install boundary below for new testing.

These are acceptance criteria, **not a pass record** or a parallel work list.
Linear tracks execution and blockers. Reuse alpha.6's recorded evidence for
unchanged behavior; do not repeat a full baseline by default. Record the exact
candidate version/commit and expand this matrix when shared-service changes
affect more than the named surfaces. No live target is authorized by this list.

| Changed surface | Required candidate evidence |
| --- | --- |
| Cloudflare prompt order | Wrong/missing workspace/store fails before zone/token input; machine mode still refuses before token entry. Correct workspace reaches the human flow; do not rotate a credential solely to retest prompt ordering. |
| Explicit gh-assisted webhook automation | JSON/no-TTY enable and rotation require `--github-webhook`; missing gh/auth/permission refuses before secret/vault/desired-state mutation. Dry-run and same-key replay do not rotate or mutate hooks. Real authorized fixture: signed push succeeds, duplicate is suppressed, other branch ignored, old HMAC rejected after rotation. No plaintext in output/argv/events. Partial hook failure reports activation separately and never prints the secret. |
| Portable workspace identity and selection | Workspace is the only named target; node remains distinct. `--workspace` selects verified identity, never a private historical alias; old context commands/flags are absent and obsolete environment settings cannot silently retarget. List/show/use/remove and JSON expose workspaces, not named profiles. Selection and explicit identity initialization (`--assign-workspace`) are distinct. Refreshed export/login across a second machine/store preserves ID and node. Conflicting/ambiguous identities refuse safely; verified replacement preserves unrelated concurrent updates and existing credentials/pins on failure. |
| Agent discovery and human presentation | No-argument overview, help, metadata checks and search work offline with no saved workspace, including JSON/no-TTY/no-ANSI. Hidden node commands stay hidden. Login, setup and TUI identify the workspace without a duplicate context/profile identity. Changed TUI discovery supports keyboard search/help and exact command equivalents without generic unsafe execution. Exercise narrow/wide resize, no-color/plain, detach and terminal restoration. |
| HTTPS telemetry integration | Proper HTTPS metrics and discovery URLs on the node hostname are usable by an external collector. Both require authentication; raw exporters remain private. Generated Prometheus/Alloy configuration and discovered targets use reachable URLs. Verify anonymous denial, authenticated metrics/discovery success, correct generated targets and unchanged application routes. A current snapshot or WireGuard-only address does not satisfy this row. Reuse working credentials where possible; no collector/dashboard deployment is implied. |
| Readiness | Stage/acquisition/query timings and safe pool snapshots distinguish slow acquisition from query work. Deterministic offline cancellation/resource-release regressions pass. Authorized candidate idle/representative-workload observations must establish a cause-specific fix; a shorter handler budget or healthy subsequent probes alone do not resolve LABS-346. |
| Candidate upgrade | Verify signed artifacts; CLI-only alpha.6 upgrade; mismatch dry-run changes nothing; real node converge reaches expected schema/River; repeat apply is a no-op; doctor has no unexplained failure/warning; existing production/staging retain HTTPS version/environment/secret-match markers. |

Supporting offline checks include inspected isolated unit/regression/race tests,
build, vet and Darwin cross-build. Disposable-DB tests need an explicitly
authorized isolated database; never use an ambient DSN or an automatically
started Docker fixture without authorization. Cross-build/CI is not Mac host
acceptance. Record failures, not-run checks and build provenance separately.

Alpha.7 acceptance requires the operator's explicit acceptance of its candidate
delta evidence. An unexplained readiness timeout remains blocking. Publication,
live mutation and beta promotion each need separate authorization.

The 2026-11-10 signed-edge and fixture-webhook records below cover parts of the
candidate-upgrade, webhook, workspace, discovery and TUI rows for
`edge-079363faf394` on `thelab`/`main`. They are delta evidence, not alpha.7
acceptance or a readiness fix.

### Beta.1, RC and final gates

**Publication versus qualification (operator authorization, 2026-11-10):** the
operator requests a reviewed, signed `v1.0.0-beta.1` prerelease for a fresh test
tomorrow. Publishing that test artifact is authorized; it does not mark the
acceptance gates below as passed. Record pending fresh-host, provider adverse
cases and readiness findings in the release handoff. No reset or repurposing of
the existing alpha node/database, or use of an ambient database, is implied.

- **Beta.1:** alpha.7 delta acceptance plus authorized real multi-workspace/server
  SSH-pin qualification in a single store (shared WireGuard management IP,
  alternating workspaces, wrong-key refusal without damaged saved access). Complete
  Cloudflare provider failure/interruption, multi-binding deletion and actual
  foreign-record **apply** refusal against disposable authorized bindings.
  Preview refusal is not apply refusal. Preserve foreign resources and serving
  apps, retain retryable ledger evidence, and distinguish partial effects from
  successful cleanup. No automatic alpha → beta promotion.
  The operator additionally requires a fresh beta installation: remove alpha.7's
  legacy bundle/unfinished-setup fallbacks before beta.1, then qualify bootstrap
  and the main app workflow on a separately authorized fresh server and dedicated
  control database. Do not carry alpha state forward as proof of fresh-install
  acceptance, and do not erase the existing alpha server or database.
- **Before RC:** review replacement-VPS recovery documentation against the same
  surviving control DB/external services. Inventory protected vault keys,
  pinned node/transport identity, placement/in-flight work, exact artifacts,
  routing and external-service prerequisites. State unsupported replacement
  mechanisms and expendable local-volume policy explicitly. Documentation-only
  is the agreed gate; no live rehearsal or backup product is implied.
- **Before final:** actual macOS install/upgrade acceptance remains required.
  A separate fresh-server final rehearsal remains an operator decision.

Historical alpha.6 evidence below remains valid for its recorded builds and
unchanged behavior. It does not establish these new candidate checks as passed.

## 2026-11-10 — beta.1 source verification for fresh testing

The operator authorized publishing `v1.0.0-beta.1` for a fresh test tomorrow,
separately from qualification. Existing alpha hosts, databases, operator access
files, services and the live Grafana dashboard were not changed by this work.

The beta cleanup uses `workspaces.json` and current workspace-named private
state, with no old-filename/schema fallback. Identityless handoff import,
old unfinished setup, persisted unconfigured access, public workspace-assignment
upgrade input and optional atomic-submission capability fallbacks were removed.
Fresh setup verifies identity/readiness over its pinned pending route before
publishing completed access. Exact publication replay is fenced against changed
identity/routes. New setup refuses occupied credential/pending paths; unsupported
pending state cannot authorize deletion of old credentials. Missing workspace
schema is an error except for the precise migration prerequisite during fresh
planning/preflight; migration still precedes assignment and runtime checking.

The exporter produces verified workspace/node identity through
`eregion_node_info`; it does not alter OS hostname metadata. Step v3 covers
producer application, no-op, drift and error preservation. A documented Grafana
display join passed an offline promtool query test, not a live dashboard test.
The ready RPC no longer repeats its successful startup readiness check; this
bounded redundancy fix does not establish LABS-346's historical HTTP cause.

Independent source review cleared setup trust/ownership, store publication and
schema discrimination, login, atomic receipts and telemetry production. A final
semantic audit classified remaining context strings as Go cancellation/build
contexts, unrelated technical uses, historical evidence, or intentional negative
tests and fail-closed obsolete-input refusals—not an operator context feature.
Two stale assignment remnants found by that audit were removed rather than
retained as an unused alpha path.

Independent frozen-tree verification passed:

- Full inspected offline suite: **2,027 passed / 199 skipped / 0 failed** test
  events, including subtests; top-level **1,213 passed / 171 skipped**. Skipped
  events were database integration and guarded node-task helpers. Docker-backed
  fixture cases were explicitly excluded before execution.
- Focused race suite across eight affected packages: **1,508 passed / 0 skipped /
  0 failed**, **833 top-level** cases and zero race reports.
- Both real pinned-Caddy tests passed with **zero skips**. Build, vet, example
  checks, formatting and Darwin amd64/arm64 cross-builds passed. Cross-build is
  not Mac host acceptance.
- Offline metadata checked **115** commands, with **100** visible commands.
  Synthetic old filename data remained unchanged and was not adopted; old JSON
  fields, missing stored identity and removed assignment input refused safely.

The independent **630-file** before/after production manifest matched:
`55405a65e23a632183336f97c11aa88d89cbb2e1863d6a54edd9401308fc2cca`.
After that batch, the unused assignment helper and stale status hint were
removed and presentation regression coverage extended. Focused race checks,
build and vet passed; the final 630-file manifest is
`d9236dd3e136275f1d61a7b7e637e068095e83c0861fe8fd7c23dc8ef55b6172`.
The original baseline manifests were retained without overwriting them.

A final strict-store review reproduced a pre-existing parser defect: a valid
JSON object followed by another value or garbage was accepted. The loader now
requires end-of-file after the first object, matching pending-state validation.
The new regression failed before the fix, passes afterward, accepts trailing
whitespace and proves input bytes remain unchanged. Ten normal and ten race
repetitions of workspace-store tests, build and vet passed; independent review
cleared the two-file correction. The updated final 630-file manifest is
`4ae7b213d15f93fa610726c817369b0cd4c0e8767335eb9a2b0a55a5a02610d8`.
Evidence: `.local/qualification/beta-store-json/`. Release CI must cover the
corrected commit, not just the preceding candidate.

CI for `a839215` passed Linux checks but found one stale pre-migration assertion
and two Linux-only default-config fixture assumptions on macOS. The tests now
require the missing-schema sentinel and prove the read creates no table; default
path fixtures isolate both HOME and XDG and use the native OS config directory.
Production behavior and refusal/preservation checks were not relaxed. Ten normal
and ten race repetitions passed locally; the DB case remained explicitly skipped
without a DSN. Independent review cleared the corrections. The updated 630-file
manifest is `40e159984e49d7a8200b9e47cb934c6a24c5bfc12c69176be7fb2b2274526635`.
Evidence: `.local/qualification/beta-ci-fixtures/` and the preserved failed CI
artifact record. Actual macOS and PostgreSQL 17 CI must pass before tagging.

Evidence: `.local/qualification/beta-independent/`, `beta-final-cleanup/`,
`beta-final-naming/` and the scoped workspace/bootstrap/submission/telemetry
evidence directories. User-owned untracked qualification code and `specs/`
were excluded, not inspected or edited. No local Docker or ambient/test database
was used. The six workspace and eight platform beta-related DB cases, the
adjacent target-lock case and broader integration suite require disposable CI
PostgreSQL evidence. Signed publication is a separate release gate. Fresh-host,
provider adverse-case, multi-workspace SSH-pin and actual Mac host qualification
remain pending; LABS-346 is not closed and no acceptance is invented.

### Beta.1 initial publication and immutable completion evidence

The exact `6316199b3f7502347bb25cc824dcf132794a0e20` candidate passed source CI
`42900238079`, edge publication `42900237969`, line CI `42902575348` and tag CI
`42902579682`, including PostgreSQL 17 and macOS. `1.x` was fast-forwarded and
annotated tag `v1.0.0-beta.1` points to that same commit. No tag was moved.

Tagged publication `42902579661` failed `CompleteMultipartUpload` with
`InvalidPart` for Linux/amd64. The manifest, its signature, installer, binary
sidecars and other three binaries were already present; the incomplete release
was not advertised as ready. The underlying provider cause is not established.
Future publishing now avoids that multipart path, uses conditional single PUTs
with readback verification and publishes the manifest last. Eight offline
script suites, ShellCheck, actionlint and independent review cleared the changes.

A clean tagged-source rebuild using the original Go 1.27.1 script reproduced all
four binary hashes and the original manifest byte-for-byte. The reproduced
Linux/amd64 bytes verified against the already-published original signature:
`db99c46051d5deffe22977f7d5723c89df6e46d7f926abd8e0e5ca0d87c21f3b`,
**41,922,720 bytes**, version `1.0.0-beta.1`, commit `6316199b3f75`, dirty=false.
The authoritative manifest SHA-256 is
`35adc53f1d98d2dbc087a82bd3a54c1efc955ed481a62dbb1d50e8db47c31ec5`.
Original manifest and all binary signatures were authenticated under the exact
original tag workflow identity, not re-signed by recovery.

The other three original binaries report dirty metadata from unignored generated
output between builds, not a source discrepancy: all hashes reproduced exactly.
This caveat is retained explicitly; future builds now isolate output and check
clean metadata and exact source revision per binary. Preserving original signed
bytes takes precedence over changing those immutable metadata flags.

Guarded completion is limited to creating the one missing key with the original
authenticated bytes. It requires actual new-prefix upload verification and an
authenticated create-only workflow, followed by independent public verification;
read-only reproduction is not completion or fresh-install acceptance. Execution
status and final publication evidence are tracked in LABS-352.
Evidence: `.local/qualification/1.0.0-beta.1-artifacts/`,
`beta1-reproduction/` and `release-upload/`. The initial failure and subsequent
proof manifests are retained separately, without overwriting their records.

## 2026-11-10 — alpha.7 local verification and signed-edge live upgrade

### Working-tree local verification

**This working-tree pass was local verification, not publication or host
acceptance.** Source was the dirty `master` checkout at
`c9f0a12f3ce3f7f0ccbf68051187aef36ef36108`, including the new untracked
implementation files and embedded guide. Before/after source
manifests match:

```text
4ee5addccfd22864eeb419c9118766eaaf58109b7a3e23dbfc7e7fe7f90831e6
```

Evidence root: `/tmp/opencode/eregion-final-verify.20261110-113148`.
`evidence/source-before.sha256` and `source-after.sha256` cover source, tests,
schema, embedded guide and build inputs, including untracked source; ordinary
repository docs are excluded so evidence updates do not change the tested-code
identity. HEAD and branch did not change. The pre-existing Cloudflare test,
metrics qualification test and user-owned spec retain their original hashes;
the spec's contents were not used as requirements.

| Check | Observed result |
| --- | --- |
| Inspected offline unit/regression suite | **1,121 pass / 9 expected skip / 0 fail**. Docker SSH/WireGuard/bootstrap fixtures and live metrics qualification were explicitly excluded; all ambient database/context/credential settings were neutralized. |
| Same offline suite with race detector | **1,121 pass / 9 expected skip / 0 fail**, no race report. |
| Build/vet | CGO-disabled Linux build and binary, `go vet ./...`, example test/vet passed. Formatting and `git diff --check` clean. |
| Darwin cross-build | amd64 and arm64 passed; artifacts identified as Mach-O for the intended architectures. This is not Mac host acceptance. |
| Authorized disposable PostgreSQL 18, schema 028 | **18/18 pass**, exact allowlist in `evidence/schema028-allowlist.txt`: six workspace cases, migration test, seven platform webhook admission/worker cases and four delivery/fresh-secret regressions. |
| Workspace concurrency/migration race repeats | Four selected cases, `-race -count=3`: **12/12 pass**. |
| Shared admission/delivery race repeats | Five selected cases, `-race -count=3`: **15/15 pass**. |
| Offline binary smoke | Nine commands passed with valid JSON/no ANSI where applicable: version, root JSON/plain, catalog, search, metadata check, command help and apply flag help. **115 commands** passed explicit metadata validation. Missing test context store remained absent. No init/apply/upgrade/RPC mutation was invoked. |

Expected offline skips were six DB-gated workspace cases (run separately on the
authorized fixture), two node-task helper/runner cases, and the pinned-Caddy
validation case with `EREGION_CADDY_BIN` unset. These are skips, not offline
passes. Excluded Docker/live fixtures are not counted as passed or qualified.

`TestIntegrationWebhookAdmissionAndDeletionLockOrder` was not in the 18-case
non-race allowlist, but passed all three executions in
`logs/shared-admission-race-count3.log` (lines 9–10, 19–20, 29–30). It was not
silently skipped or unnecessarily rerun; the clarification is retained in
`evidence/missing-lock-regression.txt`.

The operator authorized a new disposable local PostgreSQL 18 target and private
package extraction only. The fixture used a loopback-only listener and private
data/module directories, never an ambient DSN or production context. The
PostgreSQL 18.6 package matched its apt metadata SHA256; no system package was
installed and no existing service was changed. Fixture databases, listener,
postmaster, data directory and extracted package stage were removed after the
run. Sanitized package/cluster facts are retained under `evidence/pg18-package/`
and `evidence/pg18-cluster/`. No Docker was used.

Earlier targeted webhook evidence is retained at
`/tmp/opencode/eregion-webhook-phase.20261110-111609` and
`/tmp/opencode/eregion-webhook-rerun.20261110-111930`. The initial run found a
test querying a nonexistent `message` column; the assertion was corrected to
scan the actual complete `event` JSON. Its formerly unexecuted assertions then
passed, with concurrency/rollback/lock-order repeats and race runs. This was a
test/schema mismatch, not a production mutation or an unexplained discarded
failure. The final schema-028 allowlist above includes the corrected case.

**Not run in this working-tree pass:** live server init, real second-machine
workspace handoff/login, actual multi-server/shared-IP SSH-pin qualification,
live gh/GitHub hook enable/rotation/push checks, changed TUI terminal
acceptance, Cloudflare adverse cases, and actual macOS installation/upgrade.
No signed alpha.7 artifact was published or installed *in this pass*; the
signed-edge record below later published and installed the candidate. Existing
alpha.6 evidence remains the unchanged-behavior baseline, not a pass for these
changed candidate flows.

**Status at this local stage:** LABS-346's intermittent live readiness timeout
had no established cause-specific fix. The four-second HTTP budget and safe
stage/pool diagnostics are containment and triage, not root-cause
qualification. The signed-edge run below did not establish a cause either.
Alpha.7 acceptance and beta promotion remain unclaimed; explicit operator
acceptance and the remaining unqualified gates are still required.

### Signed-edge publication and live upgrade (`edge-079363faf394`)

**Signed publication and artifact verification — pass.** Candidate
`079363faf39450da1410e587f8c6123a4bc7cbfa` (`edge-079363faf394`) was verified at
the exact commit. CI `42816902095` (`check`, `portable-cli (macos-latest)` and
PostgreSQL integration job `129832169184`) and Publish edge `42816901789` both
succeeded. The commit is test-only: six function renames in
`internal/controlplane/workspace_integration_test.go`, no runtime change. The
exact-commit PostgreSQL 17.11 integration command
`go test -race -count=1 ./internal/controlplane -run TestIntegration -timeout
600s` passed (`ok`, 279.703s). CI does not pass `-v`, so its log shows the
package result, not individual test lines; selector coverage and the configured
database fixture establish that the six workspace cases executed. The immutable
manifest returned HTTP 200 with immutable caching and reported schema 1, channel
`edge`, version `edge-079363faf394`, and four valid artifact URLs and SHA-256
fields. The manifest and all four binaries verified with cosign against
`https://github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master`
and issuer `https://token.actions.githubusercontent.com` (six `Verified OK`
results, including the edge pointer). Each downloaded binary matched both the
manifest and its published sidecar SHA-256:

| Platform | SHA-256 |
| --- | --- |
| linux/amd64 | `824e3a1040d57026f6a6bed64a1aeb298d930e1c91d4d8b3979f41c3ea0c5e77` |
| linux/arm64 | `7275cce04daa57c6cbaff73ff029ea2ee69d17d26d5f35517b7909b5daaad51a` |
| darwin/amd64 | `4ac5190cdafd1b6cd78c8239a2f707e729af8082d322f3a0f9080b2b65bb81f7` |
| darwin/arm64 | `b29749927166466432d9a2a3cadc7f67064b9d0c46ab8156fa3414d16d6bff0c` |

The linux/amd64 binary is 41,787,552 bytes and reports `edge-079363faf394`,
channel `edge`, commit `079363faf394`, `dirty=false`, go1.27.1. Its offline
isolated-HOME smoke passed nine commands with rc 0 and no ANSI, without a
context store, RPC, install, apply or upgrade; `commands --check-meta --json`
reported 115 checked commands and valid metadata. Root, `1.x` and `rc`
manifests returned HTTP 404, the edge channel manifest was byte-identical to
the immutable version manifest, and no stable pointer moved. Evidence:
`/tmp/opencode/edge-079363faf394-verify.20261110-123455/`.

**Authorized live target.** Context `thelab`, node `main`, route
`mbv@10.78.0.1:22` over WireGuard via `46.224.129.96:51820`, 2026-11-10
12:59–13:09 UTC. Every CLI call used the isolated
`EREGION_CONTEXT_FILE=/home/mbv/.config/eregion-fresh-test/contexts.json`;
node calls used `--context thelab`. Queued/running/cancelling operations were
zero before the CLI upgrade and again immediately before apply.

**CLI upgrade and lockstep — pass.** `upgrade` planned `1.0.0-alpha.6` (stable)
to `edge-079363faf394`, `changed=true`, `nodes="not upgraded"`. The real
upgrade exited 0, kept `eregion.prev` at alpha.6 / `b8211a2c44f6`, and installed
the exact linux/amd64 SHA-256 above. With the new CLI against the old node,
`apply --workspace thelab --auto-upgrade-nodes --dry-run` refused with exit 5
`node_version_mismatch` (client `edge-079363faf394`, node `1.0.0-alpha.6`) and
did not install the node; the old CLI then confirmed the node unchanged at
alpha.6, schema 27/27 and River 7/7.

**Node converge, migration and workspace — pass.** Real apply
`op_03d29b225ee072f8ba8d373fa1e604b3` exited 0 with **5 applied / 17 skipped**
of 22 steps, generation 6, completed 2026-11-10T13:00:33.046279798Z. It executed
`host-database-v1` (schema 27/28), `host-workspace-v1`,
`host-api-service-v1`, `host-worker-service-v1` and `host-readiness-v1`. The
first readiness probe in that apply hit loopback connection refused while the
API restarted; the apply still completed successfully. Workspace configured
true as **thelab** / `ws_779004d145462c0fd7f48868`, and local context metadata
refreshed to the same name and ID. `server status` reported CLI and node both
`edge-079363faf394`, node commit `079363faf394`, schema **28/28**, River
**7/7**, workspace `thelab` and all six services true. API and worker were
active/running with `NRestarts=0`.

**Post-upgrade convergence — pass.** `doctor` was **23 pass / 0 warn / 0 fail /
0 skip**. A second real apply `op_53b59db7479547a5b30bdd2de551af7d` was
**0 applied / 22 skipped** with no executed steps. Before and after the
readiness window, production
`https://eregion-test1.mbvlabs.com/qualification` and staging
`https://eregion-test2.mbvlabs.com/qualification` both returned HTTP 200 with
`ssl_verify_result=0`, preserving production v1/production/secret match and
staging v3/staging/secret match. Final queued/running/cancelling counts were
zero.

**Readiness diagnosis — still unresolved.** All bodies were discarded; only
status and timing were retained. In the bounded exercise, the four valid
readiness samples returned HTTP 200 in 0.319–0.598s, with no non-200, timeout
or operator-read failure; they do not establish a fix. Two earlier overlap
attempts were invalid because a shell command-substitution bug delayed the
operator read until after the request completed (0.684s and 0.424s, both 200).
They are not overlap samples, but they mean the bounded exercise sent six
readiness requests, over the specified maximum of four; they were harness
failures, not retries of a slow or failed result. At 13:07:10.713Z, after
convergence, the new candidate emitted a safe `control-plane readiness slow`
summary on CLI stderr during a read-only operator RPC
(`operations list --state running`): total 1454 ms, stages
`schema_table=239`, `schema_migrations=203`, `river_table=207`,
`river_versions=804`, 6 acquires / 6 queries, query 1454 ms, pool max 12, idle
1, query_errors 0. This came from the CLI's node-side `NodeApp` short-lived
control-plane pool, not the API pool, and is **not** a reproduction of the API
pool timeout; no saturation cause is claimed. The
fail-closed API journal whitelist found exactly one failed summary at
13:00:29.063Z during the service-start window: `control-plane readiness failed`,
total 660 ms, failed stage `river_versions` (stages 139/130/135/255 ms),
query_errors 1. Its cause is unknown and no PID or pool-class evidence was
retained; the service became healthy afterward. The same whitelist found zero
slow summaries in `eregion-api.service` since 13:00:29Z; the 1454 ms summary
came through normal CLI stderr, not a raw journal read. No general or raw
journal, environment, DSN, SQL or driver error was read or emitted, and no
apply, restart, pool/timeout change or journal dump was used as a repair.
Startup and steady-state evidence are reported separately.

**Timestamp correction.** The apparent start/end reversals in the readiness
samples are explained by a local harness formatting bug, not by an established
wall-clock step. The local harness uses uutils `date` 0.8.0,
whose `%3N` drops leading fractional zeros; local reproductions prove the local
public-readyz reversal is a formatting bug. The remote node implementation was
not rechecked, so the same explanation there is only numerically consistent,
not directly code-proven. No clock/NTP step is established or implied. Raw
timing files are preserved and hashed in `raw-timing-sha256.txt`;
`ERRATUM-timestamp-format.md` and `corrected-timestamps.txt` in the same live
evidence root record the correction, and the live summary was updated. Other
findings above are unchanged, and no further target calls were made.

**Safety and remaining gates.** No tag, beta promotion, fresh-server init,
app/environment/resource/domain deletion, DNS mutation, secret read or export,
GitHub push or hook rotation was performed. No direct database connection,
migration reset, source edit, commit or push occurred; raw HTTP bodies were
discarded and the evidence directory is mode 0700 with mode-0600 files. The new
tunnel daemon was left running. Evidence:
`/tmp/opencode/alpha7-live-qual.preflight/live-20261110-125853/`. This run
covers the candidate-upgrade row and the unchanged production/staging markers
for this delta only. Still unqualified from that run: protected handoff/login,
changed TUI terminal acceptance, real second-machine workspace handoff/login,
multi-server/shared-IP SSH-pin qualification, Cloudflare adverse cases, live
server init and macOS installation/upgrade. The later 14:01–14:15 fixture-webhook
run below supplies live hook rotation/push evidence for `079`; the earlier phase
performed none. The persistent API-pool
timeout LABS-346 remains unreproduced and unresolved; no cause-specific fix is
claimed. Alpha.7 is not complete, no tag or beta promotion is implied, and
explicit operator acceptance of this delta evidence is still required.

### Readiness attribution follow-up — local verification

The follow-up adds fixed failure classification, independent context state,
runtime/startup/request attribution and bounded per-query timing to slow or
failed readiness summaries. It does not change timeout budgets, pool sizes,
readiness decisions or logging thresholds, and it is not a cause-specific fix.
Independent review found no actionable defects.

Targeted offline verification passed 36 top-level tests (94 including subtests),
both normally and with the race detector, with no failures or skips. Build and
vet passed. The tracked Go source manifest covered 556 files and matched before
and after testing: `c53bd8e7253ce754c6016819bc44f5a3309cc50a069d13d9bc7b59ec10f4bc3b`.
An earlier independent workflow pass covered another 107 top-level tests
(191 including subtests), normally and with the race detector, for webhook,
terminal-model and agent-discovery behavior. Their affected sources remained
unchanged during final verification. No external database or Docker was used;
the readiness regression tests use an in-process loopback PostgreSQL wire fake.
The user-owned untracked qualification test was excluded without reading or
modifying it. Full unrestricted test-suite execution was deliberately skipped
because some existing tests can start Docker or use an integration database.

Evidence: `/tmp/opencode/offline-alpha7-final-followup.summary.md` and
`/tmp/opencode/offline-alpha7-followup.summary.md`. These are local implementation
checks, not signed publication or host qualification of this follow-up. The
`6514486f6cc1` (`edge-6514486f6cc1`) diagnostic candidate's artifact handoff and
host upgrade remain a separate pending qualification; it was not installed or
validated here, and this local record is not blanket alpha.7 acceptance.
LABS-346 remains open pending classified live evidence and a verified correction.

### Fixture webhook, workspace and discovery live qualification (`edge-079363faf394`)

**Scope and target.** 2026-11-10, approximately 14:01–14:15 UTC. Context
`thelab`, node `main`, `46.224.129.96`; explicit
`EREGION_CONTEXT_FILE=/home/mbv/.config/eregion-fresh-test/contexts.json`, and
every node call used `/home/mbv/.local/bin/eregion --context thelab`. Installed
CLI and node were `edge-079363faf394`, commit `079363faf394`, `dirty=false`;
schema 28/28, River 7/7, workspace `thelab`
(`ws_779004d145462c0fd7f48868`). Newer
`6514486f6cc14b1cf256d037ee7c1ecc5e6455e4` (`edge-6514486f6cc1`) was not
installed; no upgrade or artifact download occurred.

**Workspace identity and dry-run plans.** `server status` and `context show` both
reported workspace `thelab`/`ws_779004d145462c0fd7f48868`; CLI and node were both
`edge079`; all six services were true. Valid-name
`apply --workspace thelab --dry-run` returned rc 0, `would_change=0`,
`ready=true`, and 22/22 steps complete. Wrong-name
`apply --workspace lab --dry-run` also returned rc 0 with `ok=true`, but
`would_change=1` and plan step `host-workspace-v1` incomplete with error
`the workspace identity is already configured as thelab`; it made no mutation.
Workspace, services, health and active operations were unchanged. This is
recorded as a plan-level observation: the conflict appears as an incomplete
step, **not** an exit-code refusal, and no mutation occurred; it is not claimed
as a top-level conflict. Final queued/running/cancelling were 0/0/0.

**Agent discovery, help, search and no-TTY.** Non-TTY was confirmed
(`stdin_tty=no`). Root `eregion --json` returned rc 0, valid `eregion/v1`, 100
visible commands, version `edge079`; root plain returned rc 0 without ANSI.
`commands --json` returned rc 0, valid JSON and 100 visible commands, with no
hidden `node` path; `commands --check-meta --json` returned rc 0 with
`checked=115`, `ok=true`; `commands --search workspace --json` returned rc 0
with two matching visible commands; `help apply --json` returned rc 0 with valid
flag/metadata JSON. All discovery output was ANSI-free. The existing tunnel
daemon pid `2880221` and handshake `2026-11-10T13:54:07Z` were unchanged; these
offline commands made no node contact and started no daemon.

**Cloudflare prompt order.** `domain cloudflare configure --context thelab` with
no TTY returned rc 2 `input_required` ("needs a human at an interactive
terminal") without a token prompt; the same with `--json` returned rc 2
structured `input_required` with empty stderr and no token prompt. PTY
`--context does-not-exist` returned rc 3 `not_found` before any zone/token
prompt; the captured typescript contained no `Cloudflare zone ID`, `Cloudflare
zone name`, or `Cloudflare API token` prompt. `domain cloudflare status
--context does-not-exist --json` returned rc 3 `not_found` with no provider
call. No Cloudflare credential was created, rotated, read or exported. The
correct-context human token flow was intentionally not run; credential
entry/rotation is not needed merely to repeat prompt-order testing, and the
ordering evidence is limited to these pre-token refusals. That correct-context
human step is not claimed.

**TUI via PTY.** No-TTY `ui --json` returned rc 2 `invalid_output_mode` ("the
interactive workspace cannot produce machine output") and launched no TUI. PTY
launch `ui --tab apps --ascii --no-color --reduced-motion` followed only by `q`
returned rc 0; it rendered `Apps`, `alpha-five`, `thelab` and `Fleet`, and no
action key was sent. No residual `eregion ui` process remained. The transcript
contained alternate-screen exit (`?1049l`), cursor-show (`?25h`) and
bracketed-paste-off (`?2004l`) sequences; ANSI is expected in a full-screen TUI.
The TUI is mutating and has no read-only mode, so this was a limited
inventory/quit smoke. The escape output demonstrates terminal-mode restoration
events, not full human visual acceptance: wide/narrow resize, real-terminal
visual restoration and no-color/ASCII review remain human checks.

**GitHub fixture preflight.** App `alpha-five`; staging source
`https://github.com/mbvlabs/alpha-five-eregion` ref `master`,
`auto_deploy=true`; production used the same repository/ref with
`auto_deploy=false`. Hook `709788240` was active for `push` events, content type
`json`, and its URL matched the node ingress host and expected
app/environment webhook route; the full URL is retained only as a SHA-256 hash
in safe evidence. URL, active state and events were unchanged through rotation.
Checkout `~/work/alpha-five-eregion` was clean `master` at
`6fceddac583aac6603682a5bcb1a8b7a98b8fdb4`, with expected origin and no
credential URL; `gh` auth worked (account `MBvisti`), the repository was private
with default `master`. Initial active staging deploy operations were zero; GitHub
repository/hook reads succeeded.

**Old-HMAC gate and rotation.** Existing successful delivery
`3853233692285599744` was a `push` with status `OK`, HTTP 202 and matching
repository/ref `master`. Its signature was extracted into a 0700/0600 replay
area; no signature, header, Authorization value or raw payload was printed.
Only the compact JSON **without LF** candidate was sent: exact HTTP **200** with
message `pong` (`exact_200_pong=true`); the LF candidate was not sent. This
proves the old HMAC keypair validated the old body before rotation without a
deployment or secret export. Dry-run `env update alpha-five staging
--rotate-webhook-secret --github-webhook --dry-run --json` returned rc 0,
`changed=[webhook_secret_rotation]`, no operation; hook `updated_at` and webhook
secret revision 1 were unchanged. Real rotation
`op_03d3a256e57d897be8645209f475e949` succeeded with
`webhook_secret_activated=true`. GitHub hook `709788240` reported `updated`,
`updated_at=2026-11-10T14:03:15Z`, with URL/active/`push`/`json` unchanged;
webhook secret revision moved **1 → 2** and became active. The rotation emitted
a natural safe `control-plane readiness slow` summary at 4054 ms; no journal was
read. Replaying the exact old pair after rotation returned exact HTTP **401**
with message `invalid GitHub signature` (`exact_401_invalid_signature=true`). No
second rotation or hook update was performed. The original rotation CLI
submission was replayed once after rotation and before any `651` upgrade, with
the same command, flags and idempotency key
`alpha7-wh-rotate-20261110-140210`; it returned rc 0, the same operation
`op_03d3a256e57d897be8645209f475e949` and `webhook_replay=true`, with
`webhook_rotated`, `webhook_secret_activated` and `github_webhook` absent. Its
message was `Idempotent replay: the original operation is returned; the GitHub
webhook was NOT retried or verified.` Webhook secret revision stayed **2 → 2**
active and hook `updated_at` stayed `2026-11-10T14:03:15Z`
(active/`push`/`json`); there was no GitHub mutation or new operation. Raw output
was deleted; safe fields remain in `68-replay-safe.txt` and
`68-replay-safe.json`. This is the replay proof, not an inference from the
absence of a second rotation. Replay material and raw delivery/hook files were
shredded; only hashes/booleans remain, and no webhook signature header value or
raw request header remains in evidence.

**Fresh push, redelivery and other-branch test.** Empty commit
`test(qualification): verify rotated webhook` produced
`3f669fe3a92861cac28494d04d13df2b0dee8c3d` and was pushed normally to `master`
with no force or rewrite. Delivery `3853271865590022144` was a `push` with HTTP
**202**, and repository/ref `master` plus `after=3f669fe…` matched. Node
deployment `op_6b6d68c4cd75dc7475bc2997a106d532` succeeded as actor `webhook`
(`github:mbvlabs/alpha-five-eregion`), target `env/alpha-five/staging`. Staging
ran release `rel_ccc92b340d2396c39b3583cbf9c73baf` at commit `3f669fe3…`, ready
and healthy. Production stayed at release
`rel_7e5930ed6f4595943f87f341e14df402`, commit `5022dc2d…`, healthy and
unchanged. After the push/redelivery, production returned the
v1/production/secret-match markers and staging the v3/staging/secret-match
markers, both HTTP 200 with TLS verify 0.

A single redelivery POST returned HTTP 202; its row had the same GUID
`cc9f5f36-dd0f-11f1-9413-c30b0f960ebb`, delivered
`2026-11-10T14:09:10.235Z`, status 202. Operation verification found only the
original `op_6b6…` deployment and no extra operation. A first `jq` check
compared only seconds and falsely reported zero; a corrected `startswith` filter
reported `deploy_ops_after_push=1` and `extra_deploy_ops=0`. Temporary branch
`alpha7-qualification-20261110-1413` was unused, pushed once, then deleted
normally; its delivery `3853272652856705024` returned HTTP **200** with `push is
not to the bound branch`, matching the temporary-branch repository/ref and
`after=3f669fe…`, and produced no extra deployment operation. Remote and local
branch were removed. Final checkout was clean `master` at
`3f669fe3a92861cac28494d04d13df2b0dee8c3d`, local equal to remote, with no
`alpha7-qualification-*` branch.

**Final state.** CLI/node `edge-079363faf394`, node commit `079363faf394`,
schema 28/28, River 7/7, workspace `thelab`, all six services true; production
v1/production/secret markers; staging v3/staging/secret markers;
queued/running/cancelling 0/0/0; no unhealthy app or active operation observed.

**Natural readiness diagnostics.** Across ordinary operator reads in this phase,
29 unique safe `control-plane readiness slow` summaries were emitted naturally
(including three during the same-key rotation replay), min **1000 ms**, max
**7851 ms**. These were existing sanitized CLI stderr
summaries from NodeApp RPC reads; no readiness probes were added, no general/raw
journal was read and no packet/traffic capture was performed. The `079`
instrumentation lacks the newer runtime/phase/error_class/context_state fields,
and the expanded fail-closed journal whitelist remains pending the `6514`
handoff/source review. This is not a reproduction of the persistent API-pool
timeout and not a fix: the intermittent readiness issue is active in this
build, and no cause-specific correction is claimed.

**Safety and remaining gates.** No upgrade to `6514`, no unverified binary
install, no tag/beta, no fresh-server init, no DNS/provider mutation, no GitHub
push to any other repository, no app/environment/resource/domain deletion, no
secret read/export, no human-factor bypass, no direct database/shared-DSN test
and no journal read. The main checkout was not edited, committed, pushed or
tagged by this phase; the pre-existing `docs/IMPLEMENTATION.md` modification and
untracked `internal/cli/metrics_qualification_test.go` and `specs/` were not
touched. The fixture repository received only the authorized empty commit and
temporary branch. Replay material was shredded; safe hashes/booleans remain, and
the evidence directory is mode 0700 with files 0600. The owned tunnel daemon pid
`2880221` (`edge079`) was left running; no user daemon or service was killed.

This run supplies the live hook rotation/push evidence for `079` that the
earlier 12:59–13:09 delta record left unqualified; it does not broaden that to
another build or scope. Still unqualified: the `6514486f6cc1` artifact handoff
(not received), installation and host upgrade as a separate diagnostic
qualification (its new readiness diagnostics and filter expansion were not
installed or validated); the correct-context Cloudflare human token flow;
protected handoff/login at a full TTY and real second-machine workspace login;
full TUI visual acceptance, including real-terminal restoration and wide/narrow
resize; multi-server/shared-IP SSH-pin qualification; Cloudflare
adverse/provider-interruption cases; live server init; and macOS
installation/upgrade. The `079` intermittent readiness slow remains
active and unresolved, with no cause-specific fix. Alpha.7 acceptance, beta
promotion and a clean readiness result are not claimed.

Evidence: `/tmp/opencode/alpha7-qual-20261110-135243/` (0700, files 0600). Key
files include `55-replay-safe-evidence.txt`, `55-hook-before-safe.json`,
`55-hook-after-safe.json`, `55-secret-before-safe.json`,
`55-secret-after-safe.json`, `55-rotate-dryrun-safe.json`,
`55-rotate-real-safe.json`, `55-operation-safe.json`,
`68-replay-safe.txt`, `68-replay-safe.json`,
`67-redelivery-safe-evidence.txt`, `70-branch-safe-evidence.txt`,
`65-staging-safe.json`, `65-production-safe.json`, `65-deploy-op-safe.json`,
`80-build.json`, `80-server-status.json`, `80-final-qualification.txt`,
`81-natural-readiness-summaries.txt` and `90-tui-safe-evidence.txt`.

### Diagnostic candidate publication and live upgrade (`edge-6514486f6cc1`)

Candidate `6514486f6cc14b1cf256d037ee7c1ecc5e6455e4` passed CI
`42831080539`, including PostgreSQL integration and macOS checks. Publication
`42831080477` first failed fetching the GitHub Actions OIDC signing identity,
before upload. One retry of the same tested commit succeeded; the immutable
manifest and all four binaries passed pinned-identity cosign and SHA-256
verification. The linux/amd64 binary SHA-256 is
`667461d7a24f77a15ba30305c4e576a0edfb4148eb340caa96d075dcfc314f2b`.
No unsigned build was installed. Publication evidence:
`/tmp/opencode/edge-6514486f6cc1-verify.20261110-140858/`.

On 2026-11-10, 14:34–14:45 UTC, the CLI and `thelab`/`main` were upgraded to
that exact candidate using the same explicit isolated context store. Previous
CLI `edge-079363faf394` was preserved. Apply
`op_b35268cbecd8ac116570d195ea9d0e93` completed with **3 applied / 19 skipped**:
API, worker and readiness converged; database/workspace steps were skipped.
Schema **28/28**, River **7/7** and the workspace identity remained unchanged.
Doctor passed **23/0/0**; all six services were healthy, API/worker active with
`NRestarts=0`, and active operations were **0/0/0**. Verified HTTPS responses
preserved production v1/production/secret match and staging v3/staging/secret
match. No GitHub or DNS mutation occurred in this upgrade phase.

**New diagnostic attribution is live.** A startup API slow summary reported
1087 ms with `runtime=api`, `phase=startup`, active context and bounded query
ordinals. One allowed loopback API readiness request returned HTTP 200 in
1.938788 seconds. Its correlated summary reported `runtime=api`,
`phase=request`, active context, 1937 ms total, 1759 ms query time, 178 ms
acquisition time, six queries and zero query errors. Both messages were tied to
the same PID, service invocation and boot. This establishes a query-dominated
**API request slowdown**, not the historical five-second timeout or its cause.
No failure occurred, so live failure-class/failed-stage fields remain
unobserved. No additional probes, timeout/pool changes or repair restarts were
used. LABS-346 remains unresolved.

The node lacked `jq`; that extraction path failed closed. A locally tested
on-node awk whitelist reconstructed only validated new-format diagnostic
fields and selected process/journal metadata before SSH output. No raw journal
message, SQL, error text, DSN or credential was emitted.

**Prompt-order pass, cancellation defect found.** A correct-context PTY launch
of Cloudflare configure reached the zone-ID prompt, then was cancelled before
any zone or token entry. No provider request or credential change occurred and
no process remained. Ctrl-C nevertheless exited **70 / internal_error** instead
of reporting cancellation; this is an open defect, not a clean-cancellation
pass. Full human visual TUI and protected handoff/login acceptance remain
unclaimed. Evidence:
`/tmp/opencode/alpha7-651-live-20261110-143421/LIVE-SUMMARY.md`.

### Prompt cancellation and redundant-query follow-up — local verification

The prompt fix routes the shared typed interruption through the existing
`cancelled` / exit **130** contract while preserving sentinel matching, EOF and
genuine fault classifications. The readiness improvement removes only the
outer River-table existence lookup: the initialized path changes **6 → 5**
queries, while absent-table bootstrap remains **2 → 2**. River retains its own
existence/legacy-column handling; application checksum/gap validation and newer
River refusal remain fail closed. This is not a demonstrated LABS-346 fix.

Independent review cleared both changes. Offline verification passed **158
top-level / 275 including subtests**, normally and with the race detector, with
zero failures or skips. Build and vet passed. The source manifest covered 557
Go files, including the authorized new cancellation regression test, and was
unchanged before/after: `e9ba04b239dbfe80458387a0280381859e4177678eea43adab5252f59cdfa765`.
Coverage includes prompt classification/presentation, Cloudflare interruption
before token/backend activity, unchanged no-TTY gates, River absent/current/
legacy/newer/error/cancellation paths, webhook boundaries, discovery and
terminal-model behavior. Unrestricted Docker/DB-capable suites were not run;
the isolated readiness fake uses only ephemeral loopback connections. Existing
user-owned untracked files were excluded without reading or changing them.
Evidence: `/tmp/opencode/offline-alpha7-final2.summary.md`.

These were local implementation checks. The signed publication and actual PTY
cancellation evidence follows below.

### Final signed-edge qualification (`edge-b04777ce2218`)

Candidate `b04777ce2218f3b544d6db96832cad0e975ccc56` passed CI
`42841616664` (check, PostgreSQL 17 integration and macOS) and publication
`42841616638`, both on attempt one. Its immutable manifest and all four platform
binaries passed pinned-workflow/issuer cosign verification and matched both
manifest and sidecar SHA-256 values. The installed linux/amd64 binary is
41,795,744 bytes with SHA-256
`e65989c57b30d6d3b02f083b851bac99d2076b30d89650b4d00799d52d167c30`.
Command metadata checked 115 commands; nine isolated offline smoke commands
passed. No unsigned or moving-pointer substitute was installed.

On 2026-11-10, 15:22–15:28 UTC, the CLI and `thelab`/`main` were upgraded to
that exact candidate using the explicit isolated context store. The previous
651 CLI was preserved. Apply `op_371b169cd2470cd6e29c2d845d7a98c8`
completed with **2 applied / 20 skipped**, generation 9: only API and worker
service steps executed. Schema **28/28**, River **7/7** and workspace
`thelab` / `ws_779004d145462c0fd7f48868` were unchanged. Doctor passed
**23/0/0**; all six services were healthy, API/worker active with
`NRestarts=0`, and active operations were **0/0/0**. Verified HTTPS responses
preserved production v1/production/secret match and staging v3/staging/secret
match. No repeated apply or repair command was used.

**Prompt-cancellation fix — pass.** The correct-context Cloudflare configure
flow reached the zone-ID prompt; Ctrl-C before any entry exited **130 /
cancelled**, not 70/internal_error. No process remained. No zone or token was
entered, and no provider request or credential change occurred. LABS-348 is qualified.
This does not repeat or replace the protected credential setup flow.

**Readiness limit.** Normal apply skipped the readiness step, and a bounded
whitelist read of existing API messages since startup found zero slow/failed
summaries. No extra probe or synthetic load was added. Thus the five-query live
API path was not observed in this phase; its count/semantic proof is offline and
CI evidence. The three-stage diagnostic parser was separately checked to accept
the new bounded format and reject the old four-stage and extra-field forms.
LABS-346 remains open: neither the performance change nor absence of a new
message establishes the historical timeout's cause or a verified fix.

Current evidence is retained inside the repository, ignored by Git and
owner-only:

- `.local/qualification/edge-b04777ce2218-artifacts/` — 69 verified manifest
  entries; manifest SHA-256
  `c4d776f31c445485939e42cc14cb4c30d29d6bb8eff9bebaa81f849213094d46`.
- `.local/qualification/edge-b04777ce2218-live/` — 75 verified manifest entries;
  manifest SHA-256
  `a273442688e0152112cc343b823e8ad6ef40dd2425f444d27ff1f62b52eaafa7`.

**Operator follow-up (2026-11-10).** The operator ran the installed TUI and
reported a normal detach from Envs, with no operations submitted. The operator
also completed the authenticator-gated handoff export: the CLI reported writing
the new handoff file and copying the record to the clipboard. This qualifies
the protected export flow and clean TUI detach; it does not establish visual
resize behavior or successful import on another machine. No authentication
code, passphrase or bundle content is retained in this record, and the agent
did not open the exported file.

**Second-machine login — pass (operator-reported).** On a separate Linux/amd64
machine, the operator installed the immutable `edge-b04777ce2218` installer.
`version` reported the exact commit, edge channel and `dirty=false`. Interactive
login with the refreshed protected handoff succeeded and selected context
`thelab`, node `main`, the expected WireGuard SSH route and workspace
`thelab` / `ws_779004d145462c0fd7f48868`. The transcript confirms portable
identity and successful login; the agent did not decrypt or retain the pasted
bundle or its passphrase. The follow-up `doctor` command from that machine
passed **23 pass / 0 warn / 0 fail / 0 skip**, confirming ordinary operator
access after import. The operator also identified a public terminology defect:
workspace selection must use `--workspace`, not `--context`, and the login card
must not duplicate the installation under a context label. Those presentation
and command-contract corrections are separate from the successful access check.

Full human TUI visual/resize acceptance remains unqualified. Actual macOS
installation/upgrade and fresh beta/provider/multi-server gates remain separate.
No alpha.7 tag, beta promotion or overall acceptance is implied.

### Telemetry verification and external-integration correction (2026-11-10)

A read-only b047 check at approximately 15:49 UTC found the existing exporter
and authenticated private endpoint configured and healthy. One fresh snapshot
had no missing values/issues; both services were active with zero restarts.
Listeners were loopback `127.0.0.1:9100` and WireGuard `10.78.0.1:9101`, with
no public telemetry listener. Existing doctor evidence included anonymous 401
denial; authenticated scraping was not re-probed. No configuration, service,
credential or protected handoff changed. Retained evidence:
`.local/qualification/telemetry-b047-20261110-154829/`, including the scope
erratum alongside the unchanged original evidence.

**This does not qualify the operator's external telemetry integration.** The
operator requires a proper authenticated HTTPS URL as in vaire; the private
WireGuard URL alone cannot satisfy that requirement. LABS-350 tracks the actual
endpoint, discovery and collector-configuration correction. Do not call that
flow complete from the healthy exporter/snapshot check. The future optional
telemetry-node concept remains in MASTER; no current managed collector or
dashboard installation is implied.

### Workspace-only targeting and HTTPS telemetry — integrated local verification

The corrected operator contract uses one named target, workspace, throughout
selection, management, setup/login presentation and machine output. Credentials
remain in their existing private locations. Initialization and target selection
have distinct flags; historical storage keys are not accepted as workspace
identities. Independent review covered SSH/tunnel dispatch, interrupted setup,
explicit stores, conservative local cleanup and verified atomic login
publication. Identified defects were corrected before integration. Failed
replacement tests now seed existing verified access and compare store, key and
pin bytes, including the complete credential-directory set after failure.

The HTTPS telemetry correction uses two exact paths on the configured hostname
and loopback-only exporter/authentication backends. It preserves the existing
credential during managed convergence, validates complete managed configuration,
serializes ingress publication, and provides complete bounded recovery even when
the initiating context is cancelled. External discovery and generated scrape
configuration use authenticated HTTPS, not listener or WireGuard addresses.
Independent review findings were corrected and re-reviewed; no live deployment
or authenticated external scrape is claimed by these local checks.

The integrated affected package suites passed normal/race testing, build, vet
and Darwin amd64/arm64 cross-builds. Independent targeted verification passed
**49 normal / 50 race test cases**, with zero failures or skips, metadata checks
for **115 commands** and **100 visible commands**, and both real pinned-Caddy
gateway validation/public-root adaptation checks. Two final presentation fixes
removed setup's duplicate Context answer row and corrected the resume command;
their focused normal/race tests, build, vet and metadata check passed.

The final **627-file** production-asset manifest, including embedded guide,
scripts and SQL, matched before/after the focused final checks:
`9c11d890a563fa54196f802e4944f953295b0e8494c0adb77e887d725f9bc731`.
The preceding independent integrated baseline was
`2c4b2092aea6d751554c41bfd687d78a9906c9b91c9a4af190725b8e856e9bd0`;
the retained manifest-provenance note distinguishes the two snapshots.
Evidence is retained under `.local/qualification/workspace-only/`,
`workspace-independent/`, `https-telemetry-track/`, `integrated-workspace-https/`,
`integrated-final/` and `final-wording/`. The isolated telemetry source patch and
84-entry evidence archive are retained; the implementation worktree was removed.

Signed CI/publication and exact-candidate host/external-collector qualification
are separate gates. Prepared verified Prometheus tooling is a test client, not
a managed telemetry-node deployment. LABS-346's historical timeout remains open.

**CI portability correction.** The first combined candidate `96c10c6` passed
Linux check/integration and publication, but macOS CI failed a unique credential
directory cleanup assertion. The conservative production policy retains paths
with symlink ancestors; the fixture had not canonicalized macOS's temporary-root
alias. Only the tests were changed: use physical roots for the happy paths and
explicitly verify that an aliased store root still retains its pin bytes.
Independent review cleared the correction; four cases passed ten normal and ten
race repetitions locally. No production deletion policy was relaxed or platform
test skipped. The replacement candidate still requires actual macOS CI success
before deployment. Evidence: `.local/qualification/macos-cleanup-portability/`;
updated 627-file manifest
`b2b4b8628c4d5104bde2cc24f3ada60514e0de717938ceefa27b9ceb2843edbc`.

### Workspace-only targeting and public HTTPS telemetry — live qualification (2026-11-10)

Exact signed candidate `edge-e6abe9577a04` (commit
`e6abe9577a04d09abd94dc877c84c7e22aaf71a7`) passed CI `42878869663`,
including macOS, and publication `42878869544` on attempt one. The immutable
manifest and all four binaries passed pinned-workflow signature and checksum
verification. Installed Linux/amd64 SHA-256:
`ebc755b8fbd3d342c0cadba82925d704e42670700bd54ff0a83d432ee523d2bb`.

The initial local upgrade failed before installation because b047's downloaded
binary version probe inherited obsolete context environment settings. Offline
differential checks reproduced the refusal with dummy paths and passed without
those settings. One corrected normal local-only upgrade with selectors unset
succeeded; no manual binary replacement or release-verification bypass occurred.
The prior b047 binary is retained as `.prev`.

On the authorized `thelab` node, apply
`op_613553c7e40df2806e882b877897db0b` completed with **5 applied / 17 skipped**,
including managed ingress and telemetry convergence. CLI and node now report
`edge-e6abe9577a04`; workspace identity remains
`thelab` / `ws_779004d145462c0fd7f48868`, schema **28/28**, River **7/7**.
Doctor passed **23/0/0**, all six services were healthy, queues **0/0/0**.
Default and explicit workspace selection matched; a wrong workspace refused
without fallback. Removed context flags/commands and obsolete environment
settings refused explicitly. Workspace-selected SSH dispatch passed.

Verified public URLs, with normal TLS verification and basic-auth user `metrics`:

- `https://eregrion-alpha5.mbvlabs.com/metrics/node`
- `https://eregrion-alpha5.mbvlabs.com/metrics/targets.json`

Both paths denied anonymous and incorrect authentication with **401**; unrelated
and suffix paths returned **404**. Exporter and gateway listened only on
`127.0.0.1:9100/9101`; bounded public-origin probes could not reach those ports.
Normal `metrics config --format prometheus --out` generated an owner-only file
for a temporary native Prometheus 3.16.0 client. Promtool validation passed;
the collector discovered the public HTTPS target and reported **health=up**,
empty scrape error and **up=1**. The wrapper's final jq assertion initially
misinterpreted the captured JSON shape; the original failure and corrected
safe proof are both retained. No protected config contents were read by the
agent. The owned collector exited and its config and TSDB were removed.

The existing telemetry credential was not rotated: mode, inode, size and mtime
were unchanged across apply, collection and repeat apply; successful collection
used that existing credential. Public readiness remained **200** and production
v1/staging v3 retained their environment and secret-match markers. Acceptance
repeat apply `op_31cafe6e38745802db3a554f2fd3b6f6` completed with
**0 applied / 22 skipped**. No reset, foreign-resource cleanup, DNS/provider
change or permanent collector/dashboard deployment occurred.

Evidence: `.local/qualification/edge-e6abe9577a04-artifacts/` and
`.local/qualification/edge-e6abe9577a04-live/`, including the initial failure,
offline differential proof, raw safe collector results and cleanup records.
The live 161-entry evidence manifest passed checksum verification.
This qualifies the changed workspace/telemetry flows, not the entire alpha:
LABS-346 remains unresolved, and no alpha tag or beta promotion is implied.

### Evidence retention correction (2026-11-10)

Earlier qualification phases incorrectly retained detailed evidence under
`/tmp/opencode`. By approximately 15:20 UTC, named alpha.7/079 and 651 evidence
directories were no longer present. The cause is unknown; inspection found no
repository test cleanup targeting those directories. Do not treat the old
temporary links above as currently available archives or reconstruct missing
raw logs from the summaries. Recorded outcomes and hashes remain in Git, but
the missing detailed evidence limits retrospective re-verification.

Retained qualification evidence now belongs inside this repository under the
already ignored, owner-only `.local/qualification/` directory. The final b047
artifact verification is preserved at
`.local/qualification/edge-b04777ce2218-artifacts/`; its SHA-256 manifest is
`c4d776f31c445485939e42cc14cb4c30d29d6bb8eff9bebaa81f849213094d46`,
and all 69 entries verified after copying. Current signed artifact URLs and
GitHub workflow records are independent sources; they do not replace missing
historical live evidence.

## Historical run evidence

### 2026-11-06 measured GC and post-GC deployment (`edge-527592419f06`)

On authorized test node `main`, prune `op_91ba5356aa14c019b8fb7f5138d31e3c`
succeeded: the five removed `retention905` registry manifest identities exactly
match its immediate preview, with no other removed items, followed by the GC
completion event. The operator transcribed interactive sudo `du -sb` readings
of 3,517,816 before and 3,303,852 after, a 213,964-byte decrease; the actual
sudo output was not independently captured. The planner's per-artifact sum was
12,046,645 bytes, which includes shared blobs, so the stricter stated size
criterion is not established. Registry service was active and `/v2/` returned
200. The image was built locally before GC but first pushed after it: digest
`sha256:97711214ab6a5cb05aef02bbb51434df3c11d229e2bf9cfc254c70beaa877483`.
Environment update `op_12c38716a2af924d24918ce88df3a197` and deploy
`op_8867de41ec94dfca732d7b6a75d1cde6` succeeded, serving
`rel_c5403a89567eed250689b3192dcb2347`; its new public marker and the four
existing URLs returned 200. Doctor was 20/0/0. Evidence is in the private
`906-gc-human-*` and `910-upgrade-gc-*` files under the fresh-session evidence
directory. GC success and post-GC push/deploy are observed, but stale collector,
running collector, collection failure and restart failure variants were not run.

The local isolated `go test ./internal/release ./internal/upgrade` passed; this
is not host evidence of tampered signed-manifest or artifact rejection. Those
same-build host variants were not run to avoid changing the live install; an
actual forward upgrade requires a newer signed edge. This run alone did not
complete the alpha baseline; alpha.1 is now qualified by the operator
confirmation above, not by reconstructed evidence.

### Release-channel publication (2026-11-06)

With explicit authorization, `v1.0.0-alpha.1` was moved from `5275924` to
`37d4f19bee053c3e51b6c88e6d01bf099de3c0bd` after `1.x` was fast-forwarded.
Tag workflow 42220478211 passed in the edge environment; its immutable-prefix
guard passed, the stable job and RC promotion were skipped. Cosign verified
the snapshot manifest and each of the four platform binaries against the exact
tag workflow identity, and each binary matched the manifest SHA-256. Root,
1.x and RC manifests remained HTTP 404; the edge manifest was byte-identical
before and after the tag workflow. Edge workflow 42219795190 passed. CLI and
main were upgraded to `edge-37d4f19bee05`: schema 21/21, River 7/7, doctor
20/0/0, second apply 0/20, four serving URLs HTTP 200. The credentials step was
skipped; authenticated CI-token behavior was not retested. Evidence is in
`950-*` under the fresh-session evidence directory. This publication record is
not the alpha baseline pass; alpha.1 qualification rests on the operator
confirmation above.

### 2026-11-08 alpha.4 candidate publication (`edge-f1b5440c146b`)

Candidate `f1b5440c146bcd605b4ebb4aec978d0b64eb96f1` published as
`edge-f1b5440c146b`. Publish edge `42518693443` and CI `42518693394` both
completed success. The signed edge manifest and each of the four platform
binaries verified with cosign against
`https://github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master`
and issuer `https://token.actions.githubusercontent.com` (six `Verified OK`
results). The manifest hashes were:

| Platform | SHA-256 |
| --- | --- |
| linux/amd64 | `580cdf6c24e91c9af0d91bbe3006cad33e7b52a66e84e6b3f43196a8eec3444a` |
| linux/arm64 | `ec01fb8d813ac3055f389de65d9ac6746d40d959a5b818b27b128bcf08c8c3f9` |
| darwin/amd64 | `524554e7d2d05d3eb8dfc33bb27b9680f501e00d87197777d8d4956b79ed8fda` |
| darwin/arm64 | `ecdcfe0e1685fd45d5ddbb7eef55bdd95449c802ff90ed50aeb5cb70f13734f8` |

The downloaded linux/amd64 binary reports version `edge-f1b5440c146b`, channel
`edge`, commit `f1b5440c146b`, `dirty=false`, go1.27.1, and `commands --json`
exits 0. Root, `1.x` and `rc` manifests returned HTTP 404 at publication time;
no stable pointer changed. This is an immutable edge publication, not an alpha
tag or release cut.

**Host application on the authorized `main`.** CLI `upgrade --channel edge
--yes` moved `/home/mbv/.local/bin/eregion` from `edge-fa33befb675d` to
`edge-f1b5440c146b` (previous binary kept as `eregion.prev`). Before the node
apply, `operations list --state queued`, `--state running` and `--state
cancelling` each returned zero. `apply --auto-upgrade-nodes` succeeded as
`op_ae5673fc2d52cd61c867489aac65c849`: 4 applied / 16 skipped. The executed
steps were `host-database-v1` (migration 026, schema 25/26 to **26/26**, River
7/7), `host-api-service-v1`, `host-worker-service-v1` and
`host-node-exporter-v2`. Migration 026's fail-closed cutover therefore passed
with no in-flight deployment lifecycle operation; no operation was cancelled,
no SQL repair was used and no legacy owner was backfilled. A second real apply
`op_df0185c4f7be369426011590fc7dd00e` was **0 applied / 20 skipped**; a dry run
reported 20 complete steps and no pending step. `server status` reports both
sides `edge-f1b5440c146b`, node commit `f1b5440c146b`, schema **26/26**, River
7/7 and all services active. `doctor` is **22 pass / 0 warn / 0 fail**, adding
the `telemetry.local` row. No Cloudflare token, DNS record, unrelated app,
domain or secret was touched. The authorized hostname was reused through its
existing proxied record and returned to unbound/525 after the promotion work;
no new DNS record was created.

### 2026-11-09 alpha.5 telemetry endpoint host qualification (`edge-2767500209f1`)

Candidate `2767500209f1fe264deef02a0e3819b70f887fa0` published as
`edge-2767500209f1`. Publish edge `42611197118` and CI `42611197237` both
completed success (CI `check`, `integration` and `portable-cli` jobs all
successful). The signed edge manifest and all four platform binaries verified
with cosign against
`https://github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master`
and issuer `https://token.actions.githubusercontent.com`; every verification
reported `Verified OK` and every downloaded binary matched its manifest hash:

| Platform | SHA-256 |
| --- | --- |
| linux/amd64 | `2be86705a94497d12bcf152c73746654460fae00c1d63fc1357da47eab3345d2` |
| linux/arm64 | `aec180e59107b069ff714b629d971882f52775e5eb58cfebfe2816ba79e698e4` |
| darwin/amd64 | `4fb0fbfb0ad41f0cb700ab29a24b7ac59bb9f5f5ae37443562a4bc4a9c69b662` |
| darwin/arm64 | `e27ee2bdba5d3aede4e66008f9e1135429b1b6485141121c7fa6f58541df7900` |

The downloaded linux/amd64 binary reports version `edge-2767500209f1`, channel
`edge`, commit `2767500209f1`, `dirty=false`, and `commands --json` exits 0.
Root, `1.x` and `rc` manifests returned HTTP 404; the edge manifest returned
200 and no stable pointer was moved. This is an immutable edge publication and
targeted host qualification, not an alpha tag or release cut.

**Host application on the authorized `main`.** CLI `upgrade --channel edge
--version edge-2767500209f1 --yes` moved `/home/mbv/.local/bin/eregion` from
`edge-f1b5440c146b` to `edge-2767500209f1` and kept `eregion.prev`. Before the
node apply, `operations list --state queued`, `--state running` and
`--state cancelling` each returned zero. `apply --auto-upgrade-nodes` succeeded
as `op_591b1885bcaf69397d1e1313917e4bcc`: 4 applied / 17 skipped of 21 steps.
The executed steps were `host-api-service-v1`, `host-worker-service-v1`,
`host-node-exporter-v2` and `host-telemetry-v1`; schema stayed **26/26**, River
7/7, and all services were active after the apply. No app, domain, DNS record or
Cloudflare token was touched.

The host telemetry run is recorded in the alpha.5 acceptance section below.
Briefly, `node_exporter` became loopback-only on `127.0.0.1:9100`; the dedicated
`eregion-telemetry.service` bound `10.78.0.1:9101`; two live rotations were
performed; unauthenticated requests returned 401; authenticated `/targets.json`
and `/node` requests returned 200 through the operator's WireGuard tunnel; the
generated Prometheus config passed `promtool check config`; public port 9101 was
unreachable; `doctor` reached **23 pass / 0 warn / 0 fail**; repeated applies
before and after the second rotation were **0 applied / 21 skipped** with the
telemetry Caddyfile, unit and process unchanged. The four existing serving URLs
stayed HTTP 200. The first rotation
created the only credential because the prior edge had no telemetry Caddyfile or
local credential; the second rotation verified that the previous password gets
401 and the new password gets 200. No password or bcrypt hash was printed. The
WireGuard stop/start recovery and node_exporter-stop failure rows were not run
because this run was instructed not to take destructive stop-WG/exporter
actions; the hostname-less control-plane and handoff/export variants were also
not run.

### 2026-11-06 alpha.2 definitions and resources on `edge-ef4a6f879f45`

Authorized target: existing context/node `main` (`exploration`, Ubuntu 26.04
amd64), external control database on `thelab` through Funnel. Operator CLI
Linux/amd64. No existing app, database, DNS record or GitHub repository was
mutated; no tag, branch, commit or push was created.

**Release and upgrade.** Edge workflow 42242438039 completed successfully for
`ef4a6f879f45cbbd6a2d447705dbcfc8a617708d`. The edge manifest resolved to
`edge-ef4a6f879f45`; `eregion upgrade --channel edge --version
edge-ef4a6f879f45 --dry-run --json` reported that move and `--yes` replaced
`/home/mbv/.local/bin/eregion`, keeping `eregion.prev`. On the node, `eregion
--context main apply --auto-upgrade-nodes --json` produced
`op_25095a2dcb1b069a00db3e60b4b6f4ed`: 4 applied, 16 skipped, migrations
022–023 applied, then `server status` reported CLI/node `edge-ef4a6f879f45`,
**schema 23/23, River 7/7**, and all five services active. A following `apply
--dry-run --json` was 0 of 20 (generation 9). Before and after the upgrade,
eleven consecutive `doctor --json` runs were **20 pass / 0 warn / 0 fail**;
the earlier intermittent doctor warning was not reproduced while the control
database was reachable.

**Doctor `host.retention` warning diagnosis.** At 2026-11-06T15:50:19Z the
control database became unreachable from `main` and `doctor` changed to
`drift.pending` fail, `version.schema` fail, `host.retention` warn ("cannot read
the retention runs") and `version.river` warn. The node journal then reported
`failed to write startup message: write failed: EOF` for every resolved
`thelab.tailbbed89.ts.net:10000` address. `thelab`'s own Tailscale status showed
the backend Running but `Self.Online=false`; its local Funnel listener existed
and `openssl s_client` against port 10000 verified the
`thelab.tailbbed89.ts.net` certificate with return code 0. The `host.retention`
row is therefore the generic "retention runs could not be read" warning caused
by the control-database/Funnel outage, not evidence of a retention-data or
retention-policy defect. The four existing public URLs stayed HTTP 200 during
the outage. Tailscale was not restarted by the agent; it recovered on its own.

**Definitions.** `app create --from … --dry-run --json` refused an unknown key,
a `[secrets]` value table and a resource without an exec health command with
`validation_failed` before mutation; the rejected secret value was not echoed.
`app create --from create.toml --no-deploy --json` created
`a2defs1106/production` as `op_0267f357f7b4c9f49e5f33e448d2a519` with revision
1, merged shared `SHARED=shared` and `[environments.production]` `OVERRIDE=prod`,
and kept the source image `nginx:1.27-alpine`. `app export` was deterministic
and emitted source, runtime, processes, config, `secret_names`, domains and
resources only; reimporting that export with `env update --from … --dry-run
--json` returned `changed: []`. `secret set DATABASE_PASSWORD` from a generated
0600 file wrote vault revision 1 and attached the name as
`op_d7cf21f1ebaa4e81c6ff571f01c5a896`; output carried only name, revision, hash
and key metadata.

**Resources.** `env update --from update-resources.toml --dry-run --json`
previewed `resources.cache: added` without mutation (the `db` declaration used
`attach=false`). The real update `op_4ee1361ab255783580024cc7c350993f` committed
revision 3 with `resources: ["cache"]` and submitted resource operations
`op_c1dab79460fef320badab3a0321ea4cc` (cache) and
`op_9893a1c7537359040eab2d68a54b0083` (db). Both failed after eight attempts in
`platform-resource-image-…-v1`:

```text
stderr: template parsing error: template: :1:15: executing "" at <.RepoDigests>: wrong type for value; expected []string; got []interface {}
```

The commands at `internal/controlplane/resource.go:461` use `docker image
inspect --format '{{.Id}}|{{join .RepoDigests ","}}'`; Docker client 29.1.3
exposes `RepoDigests` to that template as `[]interface{}`, so `join` fails.
On the node, `--format '{{.Id}}|{{json .RepoDigests}}'` succeeded and printed
the image ID and digest list, confirming the command is the defect rather than
an absent image. Both resource rows were `degraded` with
`error_code=unavailable`; no resource container or volume was created. The
private network `er-env_83bd5eea9dbab1daca6632d430fbc6fa` carried
`io.eregion.app=a2defs1106` and `io.eregion.environment=production` and was
removed after inspection. The pulled `redis:7-alpine` (image ID `c5d34a98396a`)
and `postgres:17-alpine` (`3540c0362c80`) were removed after the run; no
container referenced them.

**Second defect: file removal of the last resource is not applied.** After the
control database recovered, `env update --from update-no-resources.toml
--dry-run --json` reported `resources.cache: removed`, and the real command
without `--confirm` correctly refused with `confirmation_required`. With
`--confirm a2defs1106/production` it accepted
`op_1df96ad438f6ee010308febfc7be850b` (`succeeded`), but revision 4 still
listed `resources: ["cache"]`; the operation's patch carried `"resources":null`.
The file-removal path builds a nil slice when the last resource is dropped
(`append([]string(nil), desired.Resources...)`), JSON encodes the non-nil
pointer's nil slice as `null`, and the node treats a null pointer as "omit and
preserve" rather than "replace with empty". `resource detach` then created
revision 5 with no resources as `op_89ed0a5e2b3d823a2da13469ab7b4f4e`, so the
resource action itself works. The same defect was then confirmed for the last
secret name: `env update --from` reported `config.OVERRIDE: unset` and
`secret_names.DATABASE_PASSWORD: removed`, and `op_766d98cb801e385265912a4ac3067edd`
(succeeded) actually removed the config key but revision 6 still listed
`DATABASE_PASSWORD`. A non-empty `config` map applies correctly; a nil slice for
`secret_names` or `resources` does not clear the existing value.

**Export/status with resources.** Exporting revision 3 included both `[[resources]]`
blocks, `attach = false` for `db`, `DATABASE_PASSWORD` only as a secret name and
no value. `env status` reported deployability
`missing: ["artifact", "domain", "resource:cache"]`, so the unavailable
resource is reflected in the deployability gate. After detach, export listed
both destroyed resources with `attach = false`, and reimporting that export was
a no-op.

**Recovery and cleanup.** The control-database outage recovered on its own:
`server status` returned schema 23/23 again, a second real `apply` was
`op_dea65f9e3139fba03629115fa1cae07d` with 0 applied / 20 skipped, and the first
`doctor` after recovery was 19 pass / 1 warn (`host.retention` still could not
read the runs), followed by three consecutive 20/0/0 runs. That single
post-outage warning reproduces the earlier intermittent symptom and confirms
it tracks a transient control-database read failure rather than retention data.
Both degraded resources were destroyed with exact confirmation: cache
`op_25bbbeed0f4ca36399a55ac82817c832`, db
`op_0829f6c2b1d04f905635ceff9ac7727e`; a further
`resource destroy cache --delete-data` (`op_03fd23841b5830d51506a464be1e06db`)
succeeded with no owned volumes and left a separately created foreign volume
`a2-foreign-1106` untouched (not an ownership-refusal test); that foreign
volume was then removed. Both resource rows are
`destroyed`, with no fixture container, volume or network left.

`app delete a2defs1106 --dry-run` and `env delete a2defs1106 production
--dry-run` previewed one environment, one secret namespace, zero releases,
deployments, builds and domains, with the last-environment warning. Actual
deletion needs the D7 human factor at a terminal, so it was not performed;
`a2defs1106/production` remains with revision 6, the still-attached
`DATABASE_PASSWORD` name and the destroyed resource identities. No DNS or
GitHub mutation was performed.
Resource data retention remains unqualified because no resource container or
volume could be created; the resource apply defect, not host capacity, is the
alpha.2 blocker on this build. Raw command output and definition files are
under `/tmp/opencode/alpha2-defs/`; the generated secret-value file was removed
and no secret value is recorded there.

### 2026-11-06 alpha.2 retest on `edge-ba175353f00a`

Authorized target: same `main` context/node. Commit `ba17535` (edge run
42250484640, `success`) published `edge-ba175353f00a`; the edge manifest
matched that version and the expected `ba175353f00a` prefix. CLI and node were
upgraded from `edge-ef4a6f879f45`: node apply
`op_3c4bebbf8dd7dc6b80bdd3439b24c66c` applied 3 steps and skipped 17, schema
stayed **23/23, River 7/7**; a second apply
`op_3d5f33c2aff06f9dded7820451693ddd` was 0 of 20; initial `doctor` was 20/0/0.

**Docker 29 resource provisioning.** The previously failing image-inspect step
now passes. `resource apply a2defs1106 production cache` with
`redis:7-alpine` succeeded as `op_9b733fa42899a21ea588202e1a6fd969`; status was
`ready`/`healthy`, image ID `sha256:c5d34a98396a…` and volume
`er-res-18bea038bf92f11da1420cc577d221c4-cache-data`. A file-based update
`op_ef31e9cbf700a70226bd62a5bc4fdd1c` accepted revision 11 with secret name
`DATABASE_PASSWORD` and resource `cache`, then provisioned cache and db as
`op_e72ab30ac09ed1b8e493c78d21f197d6` and
`op_82678348f7c0b6040a5f4ba6d026e881`; both succeeded and reported `ready` with
healthy exec checks. The db container reported `POSTGRES_PASSWORD` as set
without its value and the `io.eregion.*` ownership labels.

**Empty file removals.** With revision 11 holding a secret name and an attached
resource, `env update --from` a file with `secret_names = []` and no resources
previewed `secret_names.DATABASE_PASSWORD: removed` and `resources.cache:
removed`, with the patch carrying explicit `[]` for both. The confirmed update
`op_a71963be8e1b0534253233851cf08bb0` created revision 12 with
`secret_names: []` and no resources, so both prior defects are fixed on the
host.

**Lifecycle and retention.** `resource attach cache` created revision 7 as
`op_c00fc0a9c4d6b7a20d7d058c0e9a57fc`; db attach/detach created revisions 9 and
10 as `op_16f52d08d1f9088ed8552a6df41cbb10` and
`op_119994c874076bab49ced971683e66ce`. Redis data written as
`alpha2:key=ba17535-retained` and flushed with `redis-cli SAVE` survived a
default `resource destroy` (`op_ce23ea8eb8ad27be6152eea687b332ec`) and re-apply
(`op_dda1e9aca1f10799431e1a11f0f50fdd`) under the same volume. A first write
without `SAVE` was lost on forced removal, which is Redis' own durability
behaviour, not a product defect; the saved write persisted. A PostgreSQL
marker row survived default destroy `op_60cd328490fee0880220fcfece3c6d16` and
re-apply `op_6169837233808600bea4b18297d8bc7c`.

**Cleanup.** `resource destroy cache --delete-data`
(`op_4b1674f75b45ae6a640700c1d549d73c`) and the db equivalent
(`op_a00b454e157341cb93927872996a00a8`) succeeded. A separately created foreign
volume `a2-foreign-1106b` was untouched by delete-data and was removed by hand;
the fixture network `er-env_83bd5eea9dbab1daca6632d430fbc6fa` was removed after
confirming its ownership labels. No fixture container, volume or network
remains, and the pulled Redis/Postgres images were removed; both resource rows
are `destroyed`. `a2defs1106/production` remains at
revision 12 with no secret name or attached resource. `app delete --confirm
a2defs1106 --json` correctly refused with `input_required` and the app remains,
so a human-terminal factor-gated delete is still required.

**Doctor warning.** `host.retention` remained intermittent after the retest:
runs at 16:47 were 20/0/0 and then 19/1 with “cannot read the retention runs”
while `version.schema` stayed pass; the next runs returned to 20/0/0. The same
pattern occurred after the earlier control-database outage. The retention
check opens its own pool and shares one 5-second deadline across the policy and
two run queries, and the underlying error is not surfaced; this is consistent
with a transient connection/read over the Funnel, not a retention-data fault.
No retention data or policy was changed.

The four existing public URLs stayed HTTP 200. No DNS, GitHub, existing app or
existing resource was mutated; no commit, push, tag or branch was created.
Raw output and definition files are under `/tmp/opencode/ba17535-*` and
`/tmp/opencode/alpha2-defs/`; no secret value is recorded.

### 2026-11-06 alpha.2 deploy gate and resource consumption (`edge-ba175353f00a`)

After the fix retest, the same `a2defs1106/production` fixture was prepared for
a real deploy without DNS or GitHub changes. A small static Go application was
built from local source under `/tmp/opencode/alpha2-app` with no external
modules: `/healthz` returns `ok`, and `/` reads a Redis key over the managed
resource's private network. The image was built for `linux/amd64`
(`sha256:2c7b048fe342b68a54ab3065185512619a385d76991776a7d6fff1756d1f970f`)
and pushed with `eregion image push` as
`127.0.0.1:5000/a2defs1106@sha256:2c7b048fe342b68a54ab3065185512619a385d76991776a7d6fff1756d1f970f`
(2,524,160 bytes).

Revision 14 (`op_e0410dda297832a5e982582cdf088535`) bound that image, the web
process on 8080, health `/healthz`, and plain config `REDIS_ADDR=cache:6379`
and `REDIS_KEY=alpha2:key`. The Redis resource was applied
(`op_f7ecdfcd8c1e8ad5d2ae1c7243f07e33`) and attached
(`op_2f1a515c837a1da5692a1edb50e0bef4`) as revision 14. `env status` reported
`missing: ["artifact", "domain"]` and did **not** list `resource:cache`, so the
ready resource gate passed.

`deploy --dry-run --json` returned the blue/green plan. The real
`deploy a2defs1106 production --json` then refused before any operation:

```text
error [precondition_failed]: environment is not deployable: domain
hint: Fix the listed desired state before deploying; eregion env status shows the same report.
details: {"missing":["domain"]}
```

No operation was accepted, no container was started and no Caddy route was
written. A successful public deploy requires a disposable fully-qualified
hostname resolving to `main`'s public IPv4 `46.224.129.96` (directly or through
a Cloudflare-proxied record whose origin is `main`). No spare existing app
hostname is available; `git-test1`–`git-test5` are bound to existing apps and
must not be changed. No DNS record was created or modified.

Because the public check could not be reached without that domain, the app's
resource consumption was exercised directly on the private environment network
with the same image, network and alias a deployment uses. Redis key
`alpha2:key` was set to `ba17535-app`; a temporary
`a2defs1106-app-test` container joined
`er-env_83bd5eea9dbab1daca6632d430fbc6fa` with
`REDIS_ADDR=cache:6379`, `REDIS_KEY=alpha2:key` and `--network-alias web`.
From the node, `/healthz` returned `ok` and `/` returned
`a2defs1106 app; alpha2:key=ba17535-app`, proving the app image consumes the
managed Redis over the private network. The temporary container was removed;
this is not a deploy-pipeline serving result.

Cleanup then detached the resource (`op_e605e5bef70e886022dd1ebeb4442fcf`),
destroyed it with `--delete-data`
(`op_f263b93ae264137eb5f0bd9e4b90bab4`), removed the network and deleted the
temporary app image locally and from the registry (manifest delete returned
`202`). The environment was returned to revision 16
(`op_c6f9cfe8596e7e0bf86af280d79b0119`) with the public `nginx:1.27-alpine`
image, `SHARED=shared`, no secret names and no resources. No fixture container,
volume, network or image remains; both resource rows are `destroyed`. The app
row remains only because real deletion needs the human TOTP factor. The four
existing public URLs stayed HTTP 200 and `doctor` was 20/0/0.

**Blocker at that point:** the remaining deploy-gating/consumption acceptance
needed an authorized disposable hostname for `a2defs1106` resolving to
`46.224.129.96` (or a proxied equivalent). It was later authorized as
`eregion-alpha-two.mbvlabs.com`; see the serving-deploy record below.

### 2026-11-06 alpha.2 serving deploy, resource consumption and webhook (`edge-ba175353f00a`)

DNS for the authorized `eregion-alpha-two.mbvlabs.com` already resolved to
Cloudflare addresses (`104.21.27.128`, `172.67.142.154` and IPv6) before this
run; no DNS record was created or changed by the agent. Direct port 443 to
`46.224.129.96` with that SNI initially returned a TLS internal error and
Cloudflare returned `525`, consistent with main being the origin but having no
route or certificate yet.

**Unhealthy resource gate.** Revision 17 bound the local app image from
`a2defs1106-app:alpha2` (`127.0.0.1:5000/a2defs1106@sha256:2c7b048f…`), the
configured `eregion-alpha-two.mbvlabs.com` primary domain, and a Redis resource
whose exec health was deliberately `["false"]`. The resource apply
`op_f4a999224394e1a50b012b0d0c9bb5c2` failed and left the row degraded.
`deploy a2defs1106 production --json` refused before submitting work:

```text
error [precondition_failed]: environment is not deployable: resource:cache
details: {"missing":["resource:cache"]}
```

**Healthy resource and serving deployment.** Re-applying Redis with
`redis-cli ping` succeeded as `op_9f72ef1048b5016a9f71db9f884c1a44`; the
resource was healthy and attached. The deploy `op_a8229010e7641e5a2fa433cf542b6e86`
succeeded: candidate health passed at `http://127.0.0.1:32908/healthz`, Caddy
switched release `rel_08ca95568fe2fb174f926b8c6ef7b2b3` across one web replica,
and public verification initially hit the same TLS error then returned HTTP
200. Caddy's certificate and the domain observer then reached route `applied`,
certificate `valid` (expires 2027-02-04) and public `ok`/200. From the public
HTTPS hostname:

```text
https://eregion-alpha-two.mbvlabs.com/healthz → 200, "ok"
https://eregion-alpha-two.mbvlabs.com/         → 200, "a2defs1106 app; alpha2:key=alpha2-serving"
```

The response marker proves the serving application read the managed Redis over
the private environment network. `env status` reported
`serving_release_id=rel_08ca95568fe2fb174f926b8c6ef7b2b3`,
`health_state=healthy`, and deployability `dns=proxied, missing=[]`.

**Failed candidate isolation.** A later cleanup deploy of the public
`nginx:1.27-alpine` image with the wrong container port 80
(`op_24e22c64db1a13508abb6fa485a0a480`) failed candidate health and left the
previous release serving; no serving pointer moved. A corrected revision with
container port 80 deployed successfully as
`op_29a00bceee740a9c117f19613b10f29e`, release
`rel_de6d93651d6e26bde3900c5e90fd2377`, and the hostname then served nginx
HTTP 200. This is also a real failed-candidate/old-release-kept observation.

**Webhook.** With the fixture switched to the authorized private
`https://github.com/mbvlabs/eregion-hello.git` source, branch `main`,
`--auto-deploy --github-webhook` created hook id `708267694` at
`https://db2.mbvlabs.com/v1/webhooks/github/a2defs1106/production`
(`status=created`) without printing the secret. A second
`--auto-deploy --github-webhook --json` reported the same id and
`status=unchanged`, so no duplicate was created. `--rotate-webhook-secret
--github-webhook` PATCHed the same hook (updated timestamp) and advanced
`EREGION_WEBHOOK_SECRET` to revision 2; a following check again returned
`unchanged`. The existing `githello/production` hook was not touched. The
manual fallback's already-enabled path returned the URL and secret name without
a secret value. At that point a real push delivery had not been sent because
commits/pushes were outside the run's authorization, and one-time manual secret
display was not captured; both were later authorized and passed in the
end-to-end webhook record below.

**Cleanup and residual state.** Auto-deploy was disabled
(`op_25c7fb39e51e58a89c2d0c6675973427`), hook `708267694` was deleted through
`gh`, and the repository now has only its pre-existing `githello` hook.
`a2defs1106/production` was returned to revision 21 with the public nginx
image, `SHARED=shared`, no secret names, no resources and no auto-deploy; the
domain remains. The Redis resource was destroyed with `--delete-data`
(`op_e733d0973e02d3167679a3ca2100f9f6`); no resource container or volume
remains. At that point the environment network and the serving nginx container
`a2defs1106-production-web-1-fd4729b3d716` remained because they were owned by
the serving fixture; the primary domain cannot be removed while it is the only
binding (`domain remove` refused with `precondition_failed`, “the primary
domain cannot be removed”), and DNS at Cloudflare was pre-existing and was not
changed.

**Confirmed cleanup after user TOTP delete.** The user ran the factor-gated
`app delete` as `op_90e6c3530ef81a575bba1542a70cda0b` (13 events; succeeded).
Its workload step removed the managed containers and its completion event
recorded that the app and every environment were deleted after workload and
route cleanup. Read-only verification after deletion:

- `app list` no longer contains `a2defs1106`; `app status` and `env status`
  return `not_found`.
- `resource list --app a2defs1106`, `domain list --app a2defs1106`,
  `release list a2defs1106 production` and `secret list --app a2defs1106
  --environment production` are all empty.
- `operations list --app a2defs1106` still shows the deletion and earlier
  operation history.
- Docker has no `a2defs1106` container, network or volume; the only registry
  residue is the non-platform image
  `127.0.0.1:5000/a2defs1106:alpha2`.
- `https://eregion-alpha-two.mbvlabs.com/` now returns Cloudflare `525`
  because Caddy no longer has a route or certificate for it. The pre-existing
  Cloudflare DNS record still resolves to the proxy addresses and was not
  changed.
- The four pre-existing public URLs return HTTP 200 and `doctor` is 20/0/0 on
  the final run after the known intermittent retention read warning.

The remaining alpha.2 webhook qualification gaps are a real push-triggered
delivery and one-time manual secret display; the `gh`-assisted create,
unchanged/no-duplicate and rotation checks passed and the fixture hook was
deleted.

### 2026-11-06 alpha.2 end-to-end GitHub push delivery (`edge-ba175353f00a`)

After the earlier fixture deletion, the user authorized recreating it and
pushing a test commit to `mbvlabs/eregion-hello`. Repository inspection first:
default branch `main` at `59017d6`, no GitHub Actions workflows, no deployments,
and one pre-existing hook for `githello/production` (id `707317891`). The test
used a separate branch `alpha2-webhook-20261106`, created from `main`; `main`
was never pushed or changed.

The branch commit `c768e75` added `apps/web/alpha2_marker.go` and a `GET /marker`
handler returning `alpha2-webhook-20261106`; the existing tests passed. The
branch was pushed. `a2defs1106/production` was recreated from that branch with
`context_path=apps/web`, `GIT_READ_TOKEN` (the authenticated `gh` token was
written through a 0600 temporary file and never printed), `APP_VERSION=
alpha2-webhook`, `FAIL_HEALTH=0`, primary domain `eregion-alpha-two.mbvlabs.com`
with check path `/marker`, and no first deploy. `--auto-deploy --github-webhook`
created hook id **708277604** for
`https://db2.mbvlabs.com/v1/webhooks/github/a2defs1106/production`; the
pre-existing `githello` hook was left untouched.

**Real delivery (gh-assisted setup).** Empty commit `f6bbfa7` was pushed. GitHub
recorded delivery GUID `2a29a758-da0c-11f1-9784-272044788f84` for event `push`
at 2026-11-06T18:02:03Z with status `OK`, HTTP **202**. It admitted operation
`op_1380428ab35058512af820f5d7807a61` with actor
`webhook:github:mbvlabs/eregion-hello`; the build cloned `HEAD is now at
f6bbfa7 test: trigger alpha2 webhook delivery`, produced release
`rel_29c45e28eafd36da697c6b67e3906fa8`, passed candidate health, published the
Caddy route, and the public check `https://eregion-alpha-two.mbvlabs.com/marker`
returned 200. The operation succeeded. HTTPS after the switch returned
`alpha2-webhook-20261106` from `/marker` and
`eregion sample: alpha2-webhook src3` from `/`.

**Manual one-time secret setup.** To exercise the manual path rather than the
`gh` helper, `--rotate-webhook-secret` was run in a PTY and the
`Create the GitHub webhook with gh?` prompt was declined. The one-time secret
was written only to a mode-0600 temporary typescript, extracted to another
mode-0600 file, and sent to the existing hook through `gh api --input -` on
stdin; it was never printed in ordinary output or logs and both temporary
files were shredded. The hook was PATCHed successfully and
`EREGION_WEBHOOK_SECRET` advanced to revision 3. Empty commit `c4507c1` was
pushed; GitHub delivery GUID
`c68a318a-da0c-11f1-81ba-3eb292c14f72` recorded status `OK`/HTTP **202** at
18:06:28Z. Operation `op_b4dc5f8b225d318510c10293aff9ebaf` (actor webhook)
cloned `HEAD is now at c4507c1 test: verify manual alpha2 webhook secret`,
built release `rel_df70f2bf15a82d6cc0204635460c5e92`, passed the public
`/marker` check, and succeeded. The manual secret therefore authenticates real
deliveries.

**Missing-`gh` fallback.** With `gh` removed from `PATH`, the same
`--auto-deploy --github-webhook --json` returned
`github_webhook.status=skipped`, message `the GitHub CLI (gh) is not installed`,
and the manual webhook URL; JSON did not prompt. The missing-admin-permission
variant was not exercised at that point; a later simulated permission response
covered it.

**Cleanup.** Auto-deploy was disabled
(`op_7e56873f9ef33922e9c8d96df63d14a8`), hook `708277604` was deleted through
`gh`, and the repository again has only its pre-existing `githello` hook. The
remote and local test branch `alpha2-webhook-20261106` were deleted; `main`
remains at `59017d6`. The fixture source ref was returned to `main`
(`op_56e5cdb20b6fc931ccaa95b56bafd6e7`), auto-deploy is off, and
`EREGION_WEBHOOK_SECRET` is no longer named by the revision.

**Confirmed final cleanup (user TOTP delete).** At the handoff above,
`a2defs1106/production` still served `rel_df70f2bf15a82d6cc0204635460c5e92`
(commit `c4507c1`) and retained its container, environment network, primary
domain and secret namespaces. The user then ran the factor-gated app deletion
as `op_362bea6789b9617855c43b37a093e16e`; it succeeded with 13 events and
recorded that the app and every environment were deleted after workload and
route cleanup.

Read-only verification after deletion:

- `app list` no longer contains `a2defs1106`; `app status` and `env status`
  return `not_found`.
- `resource list --app a2defs1106`, `domain list --app a2defs1106`,
  `release list a2defs1106 production` and `secret list --app a2defs1106
  --environment production` are empty.
- `operations list --app a2defs1106` retains the deletion and earlier
  operation history.
- Docker has no `a2defs1106` container, network or volume. Reference checks
  found no container using the three identified fixture images. The registry
  manifest `127.0.0.1:5000/a2defs1106@sha256:2c7b048f…` was deleted (HTTP 202),
  and local tags `127.0.0.1:5000/a2defs1106:alpha2`,
  `eregion/a2defs1106-production:1380428ab350` and
  `eregion/a2defs1106-production:b4dc5f8b225d` were removed; no `a2defs1106`
  image remains.
- `https://eregion-alpha-two.mbvlabs.com/` returns Cloudflare `525` because
  Caddy no longer has a route or certificate. The pre-existing Cloudflare DNS
  record still resolves and was not changed.
- DNS cleanup is blocked: no Cloudflare CLI, configuration or authorized API
  access was available without reading a secret. The exact record is
  `eregion-alpha-two.mbvlabs.com` in zone `mbvlabs.com`; it currently resolves
  to proxied Cloudflare addresses. Remove that record in the Cloudflare
  dashboard or with a scoped API token
  (`GET /zones?name=mbvlabs.com`, then
  `GET`/`DELETE /zones/{zone_id}/dns_records?name=eregion-alpha-two.mbvlabs.com`).
  Do not remove any other record.
- The test repository has only its pre-existing `githello` hook and its
  original branches; `main` is unchanged at `59017d6`.
- Four pre-existing public URLs return HTTP 200.

**Remaining alpha.2 evidence boundaries at that point.** The real
push-delivery and manual-secret webhook gaps were closed. Not yet recorded
then:

- A direct file-vs-flags desired-state comparison and distinct staging/production
  overrides in one file.
- A stale-plan refusal and the `env update --from` domain-removal
  `--confirm-domain` path.
- App consumption of managed PostgreSQL through vault secret names, and
  managed-resource data persistence across application restart/redeploy.
- An explicit external-dependency-without-managed-resource variant, and the
  app/environment-deletion-after-resource-destroy volume ledger outcome.
- A real non-admin `gh` account for the permission fallback.

The later remaining-case record covers file-vs-flags/staging overrides,
stale-plan refusal, PostgreSQL app consumption and restart/redeploy
persistence, a simulated missing-admin response, and the post-destroy deletion
volume outcome (the retained volume persisted untracked and was then removed).
Fresh baseline, `--confirm-domain`, explicit external dependency, a real
non-admin account and DNS record cleanup remain open.

The known intermittent `host.retention` read warning is not a retention-data
fault. Non-destructive checks on existing apps passed: `app export img` and its
dry-run reimport reported no changes; `app export githello` correctly refused
because auto-deploy is not representable; `commands --json` contains the alpha.2
surface; `resource list` is empty; the four existing URLs return 200; `doctor`
returned 20/0/0 after the known 19/1 retention-read warning.

### 2026-11-06 alpha.2 remaining-case pass on `edge-ba175353f00a`

**File versus flags and overrides.** The disposable `a2defs1106` app was
recreated from `pg-create.toml`: shared image source, process, health and
runtime; distinct `[environments.production]` overrides (DB config, secret name,
authorized domain and later the `db` resource) and `[environments.staging]`
overrides (`APP_VERSION=staging`, `APP_BUILD=staging`, no domain/resource).
Production was created as `op_f302032a9cb5575118714553aedc71b7`; staging from
the same definition as `op_37897e295e2910f552ab88319a2ea428`; `flags` was
created flag-only as `op_3abfe963f8a96454e87a82266a24c7df` with the same visible
state as staging. `app export` of staging and flags was identical after
normalising only the environment name (`diff` produced zero lines), and
`env update flags --from` that export previewed `changed: []`. Production and
staging revisions held the expected different configs.

**Managed PostgreSQL consumption and persistence.** The app image connects to
the managed `db` resource using `PGHOST=db`, `PGUSER=postgres`,
`PGDATABASE=alpha2`, `PGSSLMODE=disable`, and the vault secret name
`POSTGRES_PASSWORD`. The PostgreSQL resource was applied as
`op_2430a37a4b9b00cfd07082e7b35ed2a3` with `pg_isready` health and a mounted
`db-data` volume.

A first deploy (`op_0e2c35b672e4891668f4664276b17ba3`) failed the public `/db`
check with 502; PostgreSQL logged repeated password-authentication failures.
This was a test-app defect: the app passed `POSTGRES_PASSWORD` to the container
but `pgx` read `PGPASSWORD`. The test image was corrected to copy
`POSTGRES_PASSWORD` into `PGPASSWORD`, pushed as digest
`sha256:50c05454…` (`pgv2`), and revision 3 bound it. Deploy
`op_8fbe3cd57b1f574919d7129b3b3218d0` succeeded with `/db` returning
`pg-persist`, `/build` returning `v1`, and `/healthz` returning `ok`; the app
container showed `POSTGRES_PASSWORD=<set>` and the expected PG variables.

`restart` (`op_31a636774fab00bfeced11078541e86c`) succeeded and `/db` still
returned `pg-persist`. `config set APP_BUILD=v2`
(`op_3639955b4a9d2465ce48407363ac6ac4`) followed by deploy
`op_a6e1c1d21e6caeffe1e29628bf7825f5` succeeded; `/build` returned `v2` while
`/db` still returned `pg-persist`. This is app consumption of managed
PostgreSQL with the same secret name and persistence across restart and
redeploy.

**Stale plan.** With the `flags` environment at revision 2, a direct
`eregion node rpc` `operations.submit` carried the old revision 1
(`erv_8ce39d21…`) and the same config. The node refused with `conflict`: “the
environment changed since the definition was read”; no revision was written
(the environment remained revision 2 at that point). The flag-only path has no
user-facing expected-revision option, so this guard was exercised through the
same node request the CLI uses.

**Missing-admin `gh` fallback.** A temporary `gh` shim returned the real
`auth status` success but `false` for `permissions.admin`; no repository
permission or credential was changed. The `flags` environment had a temporary
gh-assisted hook (`708290796`). With the shim on `PATH`,
`env update flags --auto-deploy --github-webhook --json` returned
`github_webhook.status=skipped` with “the gh account does not have repository
admin permission for mbvlabs/eregion-hello” and the manual webhook URL, without
changing the hook. The hook was then deleted and auto-deploy disabled
(`op_f12a90fa9d098442647ff04567f2bccd`). This is a simulated permission
response, not a second real GitHub account.

**Blocked cases.** `--confirm-domain` was not exercised: the only authorized
hostname is the production primary, and `domain remove` correctly refused to
remove the last primary domain. A second authorized disposable hostname is
needed for an alias-removal test. The explicit external-dependency-without-a-
managed-resource case was not run because no authorized external dependency
endpoint exists and new server/database targets are prohibited. Under the
updated incremental policy, a fresh-server alpha.2 baseline is not required;
no fresh-server run was made.

**Post-destroy state and cleanup.** After the Postgres tests the environment
was moved to public nginx for cleanup
(`op_b4e6adc5679765b5026a37c541f08ae5`, deploy
`op_4847dbdc1d9359677396db1c7e0ff6b8`), and the `db` resource was destroyed
with the default retain-data path (`op_48ade4262610759fa1baaf453f56b507`).
The owned volume
`er-res-2d99b1ef6d8ddc69b01e2271e5a071a2-db-data` remains with its
`io.eregion.*` labels; the resource row is `destroyed`. The three identified
app images/registry manifests were deleted after reference checks, with
registry deletes returning 202 and no containers referencing them. The test
hook and branch are gone; only the pre-existing `githello` hook and original
branches remain.

**Confirmed app deletion and volume-ledger outcome.** At that point
`a2defs1106` had production (nginx, serving the authorized domain), staging and
flags environments; the container/network, primary domain and the retained
PostgreSQL volume remained. The user then completed the TOTP-gated app deletion
as `op_c113f640932b31df52c4a92117f3d105` (13 events, succeeded), which removed
the workload containers and routes before platform rows.

Read-only verification after deletion: `app status` and `env status` returned
`not_found`; app/environment/resource/domain/release/secret lists were empty;
Docker had no `a2defs1106` container, network or image. The retained volume
`er-res-2d99b1ef6d8ddc69b01e2271e5a071a2-db-data` **did persist**, with labels
`io.eregion.app=a2defs1106`, `io.eregion.environment=production`,
`io.eregion.kind=resource`, `io.eregion.managed=true`,
`io.eregion.resource=db` and `io.eregion.resource-id=res_2d99b1ef…`; no
container referenced it. This confirms the documented post-destroy
app-deletion volume-ledger outcome: deleting the app does not delete a
default-retained resource volume.

The authorized test-owned volume was then removed explicitly with
`docker volume rm er-res-2d99b1ef6d8ddc69b01e2271e5a071a2-db-data`; it is gone
and no fixture container, network, volume or image remains. The app,
environments and domain binding are gone; `https://eregion-alpha-two.mbvlabs.com/`
returns Cloudflare `525` because Caddy has no route/certificate. The
pre-existing Cloudflare DNS record still resolves and was not changed. Only the
pre-existing `githello` hook and original repository branches remain. The four
pre-existing URLs return HTTP 200 and `doctor` is 20/0/0.

### 2026-11-06 alpha.2 audited pass halted by policy change

The operator changed the qualification policy during this audited pass: no
fresh-server baseline is required for every alpha, alpha.1 stays
operator-qualified, alpha.2 is qualified incrementally against its own
acceptance criteria, and macOS acceptance (R10) remains required before
v1.0.0 final. The policy is codified in MASTER section 7 and at the top of this
document. The operator then directed an immediate stop to new mutating tests.

Completed before the stop:

- **Guided file-free create — pass.** The fixture app `a2defs1106/production`
  was created through the guided plan and confirmation path without a
  definition file (`op_29b5ffe04dbda9e724960853e8aef430`), with the authorized
  `eregion-alpha-two.mbvlabs.com` primary domain and image
  `sha256:b472757e…`.
- **Failed-health provisioning retry — pass.** A PostgreSQL resource apply with
  exec health deliberately set to `false` failed and left the resource
  degraded (`op_8ef5af49dd1538e5d130801f8400a8df`), with volume
  `er-res-bbbb6c658dff7163ecb31da66ff4d6e9-db-data`. The retry with
  `pg_isready -U postgres` succeeded (`op_8c6283fba697d32cfd66791073895178`),
  state `ready`/`healthy`, and reused the same volume identity. The resource
  was not attached to a revision and no deployment was started.

No further audited cases were started after the stop directive. In
particular, the following were not executed in this pass: file-driven alias
removal with `--confirm-domain`; data retention after detach/declaration
removal on this fixture; destroy-active-dependent refusal; explicit
`--delete-data` refusal of a foreign volume; external dependency without a
managed resource; unhealthy-dependency deploy refusal with an existing serving
release; failed `gh` setup then successful retry; and deploy/webhook no implicit
definition. Earlier records cover some of these on prior fixtures; this pass
does not claim them.

**Cleanup.** The ready-but-unattached PostgreSQL resource was destroyed with
`--delete-data` as `op_40b47b8f8d0d242c802c4d6509bb450e`; the fixture network
`er-env_edb8b65f3647c29181255c4feec19a00` was removed; registry manifest
`127.0.0.1:5000/a2defs1106@sha256:b472757e…` was deleted (HTTP 202); local
`a2defs1106-pgapp:v3` and registry `a2defs1106:pgv3` tags were removed. No
`a2defs1106` container, network, volume or image remains.

Residual: `a2defs1106/production` still has revision 1 (image source, config and
`POSTGRES_PASSWORD` name), the primary domain binding
`eregion-alpha-two.mbvlabs.com` (route pending with no serving upstream), no
serving release, and no containers. The app/environment/domain/secret rows
remain because deletion needs the D7 human factor:

```sh
eregion --context main app delete a2defs1106 --confirm a2defs1106
```

The Cloudflare DNS record remains pre-existing and unchanged; it was not
removed because no safe Cloudflare access is available. Only the pre-existing
`githello` hook and original repository branches exist.

### Release-channel source integration (2026-11-06, before publication)

The existing `v1.0.0-alpha.1` tag predates this change; its release publication
failed because a stable environment secret was malformed. At this point the tag
was still untouched and unpublished; the later publication is recorded above.
The source model has combined development/edge, opt-in RC, finals-only stable,
and exact alpha/beta snapshots without moving channels. An RC client checks its
stable major line for a newer final; publishing a final does not move the RC
pointer. Channel publication retains the existing two-object manifest/signature
race and partial-upload retry risk; signing tooling is unchanged. This source
integration is not a published-build or alpha baseline qualification.

### 2026-11-07 — alpha.3 resolver host qualification (`edge-9e71713eeff9`)

The manifest protocol remains **schema 1**, compatible with the published
alpha.2 client. Manifest `channel` remains the signing family: every tagged
artifact, including alpha/beta and RC, says `stable`; edge says `edge`. Alpha
and beta are immutable snapshots built through the build script's `snapshot`
channel with no pointer; RC uses the rc pointer; stable and major-line pointers
accept only final versions. The build script, pointer installer, upload guard and
promotion code reject prereleases independently, and the stable publish job
accepts only a final tag whose commit is on `origin/<major>.x`. This is a
safety guard, not a new protocol or stable promotion claim.

The alpha.3 source also adds the shared read-only container resolver for bare
process/release commands. It is now host-qualified on `edge-9e71713eeff9` for
the plain-Docker and Buildpacks paths below. Alpha.3 is now tagged and its
snapshot published; the tag itself has not been installed or manually
host-tested, and the tag/snapshot checks are recorded below.

Candidate `9e71713eeff9e2534452a913b54683e9a45e629c` published as
`edge-9e71713eeff9`: Publish edge `42387096819` and CI `42387096776` both passed
via `gh run watch`. The signed manifest verified with the pinned edge identity;
linux/amd64 SHA256 was
`164725d24df9582553624fdf2c51ff3c9cf9a5abac1a0cceb440a92dd92595e0`. CLI and
`main` upgraded; node apply `op_162c42a9c556258cd7123fd40b06965b`, schema
stayed **24/24**, River 7/7, `doctor` 20/0/0 and `apply --dry-run` 0 of 20.

**Plain Docker image bare commands — pass.** Disposable unprotected
`a3-bare-docker` was created from prebuilt image digest
`sha256:f06b2be7670c934543431fe3bbe3c4752821aaaa07b803652ab9b5ca6763df1e`
(`op_1303b7c1ee6d711e36d6921f28c5f569`) with domain
`eregion-alpha-two.mbvlabs.com`, release command `migrate`, web process `web`,
service process `migrate service` on 8081, and worker process
`migrate worker`. Config `PATH=/usr/bin:/bin` forced the cwd fallback.

Deploy `op_1923ede54d8af0b74aaa6228bfdbe3c8` succeeded; serving release
`rel_87a9912d61f0b0d7341e37dfc4e3af78`. The release command output was
`A3MIGRATE mode=release path=/usr/bin:/bin cwd=/app exe=/app/migrate
argv0=migrate` followed by `release-ok`. Service/worker logs showed the same
effective PATH/cwd/target and `service-start`/heartbeat markers. Public
`https://eregion-alpha-two.mbvlabs.com/` returned `plain-web`; `/healthz`
returned `ok`. The environment was deleted with `op_1c18c691da5c6b8608d0c61595ab9c24`.

**Buildpacks image bare commands — pass.** The fixture branch
`alpha3-bare-commands` (commit `8258f22`) names its root module binary
`migrate`; the Buildpacks image exposes it in
`/layers/paketo-buildpacks_go-build/targets/bin`. Disposable unprotected
`a3-bare-buildpacks` used the authorized private repo/ref, release command
`migrate`, web `migrate web`, service `migrate service` on 8081 and worker
`migrate worker` (create `op_123bd0923195fd03d09d3c446a0d4241`). Deploy
`op_47192b397f65edec2b11d27a6ea7d3a7` succeeded; serving release
`rel_787722bb7bcbcd900f5c378ac5b006b8`.

The release command output was `A3BP migrate mode=release
path=/layers/paketo-buildpacks_go-build/targets/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
cwd=/workspace exe=/layers/paketo-buildpacks_go-build/targets/bin/migrate
argv0=migrate` followed by `release-ok version=alpha3-bp`. Service, worker and
web logs showed the same launch-layer PATH and target; worker emitted
heartbeats. Public `/` returned `eregion sample: alpha3-bp`; `/healthz` returned
`ok`. Environment delete `op_e61dc00ae8040c05b7165aac13595538` succeeded.

The first Buildpacks fixture attempt used `BP_GO_TARGETS` with a second package;
pack selected a non-Go detection group and failed before building
(`op_c22b5eca7f1e685ce979a8148b9dfea7`). This was a fixture configuration
failure, not an eregion runtime defect; the corrected single-binary fixture
above passed.

**Cleanup.** Both test environments, their routes and secrets were removed; the
fixture branch was deleted locally and remotely, `main` restored, the plain
registry tag deleted (HTTP 202), and the plain/Buildpacks local images removed.
`a2defs1106` retains only production, staging and protected `prot-audit`; no
test container/volume remains. The designated hostname returns 525; the four
pre-existing URLs return 200.

Offline checks for this source change are
`go test ./cmd/eregion ./internal/containerresolver ./internal/platform
./internal/release ./internal/upgrade ./internal/cli`,
`scripts/test-install-pins.sh`, `scripts/test-install-routing.sh`,
`scripts/test-release-channels.sh`, `scripts/test-release-environments.sh`,
`scripts/test-sign-release.sh` and
`scripts/test-upload-guard.sh`; no tag, upload, bucket write, Docker, VM or
database was used in that source-only pass. This host run is a targeted
qualification, not a repeated full manual acceptance. Alpha.3 tag publication
and installer checks are recorded below; the tag was not installed on the
authorized node. No TOTP, secret value or unrelated app was touched.

### 2026-11-07 — alpha.3 tag publication (succeeded; signed snapshot verified)

PR #2 (`feat: deliver alpha.3 release`, merged 2026-11-07T16:13:59Z)
squash-merged the tested source onto `1.x` as
`b9fa7b962fa1520dde8a9acec4c0823389b1633c`. Its tree
`46c7190129a5b65832aa2cdea6e5fcc451e3d1ca` is identical to `fa996d8` and
differs from host-tested candidate `9e71713` only in `docs/MASTER.md` and
`docs/MANUAL-TESTING.md`. The annotated tag object
`9517303a3f5904151018f985a26c8d05fcb5ddc6` (tagger 2026-11-07T16:14:05Z)
points at that commit and is not GPG-signed.

Publish stable `42390263289` (event push, head `b9fa7b9`) completed **success**
via `gh run watch` (exit 0); `classify` and `publish_prerelease` passed and the
finals-only `publish` job was skipped. CI `42390263242` also passed via
`gh run watch` (exit 0). The upload step wrote only the fifteen
`releases/1.0.0-alpha.3/` files; the RC promotion step was skipped.

The immutable snapshot at
`https://eregion.mbvlabs.com/releases/1.0.0-alpha.3/` returns 200 for the
manifest, signature, installer, four binaries, four `.sha256` files and four
binary signatures. The manifest reports `schemaVersion: 1`, `channel: stable`,
`version: 1.0.0-alpha.3`, and the exact release base URL. Responses carry
`Cache-Control: public,max-age=31536000,immutable`. Cosign verified the manifest
and each binary against
`https://github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-alpha.3`
and issuer `https://token.actions.githubusercontent.com`; all five returned
`Verified OK`. Each binary and `.sha256` matched:

| Platform | SHA-256 |
| --- | --- |
| linux/amd64 | `58a91dd99ca000395abba253eb5b8529c4a40a516cc4f1035b09de159bb8ad48` |
| linux/arm64 | `83d703eab5d8ebcaaa1c8a99de09f73c3b8fcd12aab420f1bc7d691599b02b91` |
| darwin/amd64 | `81f71e184fdff71d64da029ac2efd852e4edc3149cac3ea3e3d387d04ed5c32a` |
| darwin/arm64 | `ca688837a52906d46e88688eb915f6b21fc06e9a18360b9981a5ef2f5e3e3e1e` |

The linux/amd64 binary reported `version=1.0.0-alpha.3`, `channel=stable`,
`commit=b9fa7b962fa1`, `dirty=false`.

The snapshot installer has `snapshot_version="1.0.0-alpha.3"`, resolves the
version manifest under `/releases/1.0.0-alpha.3/`, is syntactically valid
(`bash -n`), and refused a mismatched `--version 1.0.0-alpha.2`. A local
installer run verified the manifest and binary signatures/hash and installed
the snapshot; it was not on the authorized node, and the prior
`edge-9e71713eeff9` CLI was restored afterward.

**Caveats.** The tag annotation is unsigned; authenticity rests on the cosign
bundles. `install.sh` is version-pinned but is not one of the cosign-signed
files. Root, `1.x` and RC manifests remain HTTP 404, so no stable pointer
moved. The edge pointer is `edge-fa996d809378`, produced by the master merge,
not by this tag workflow. The tag itself was not installed or manually
host-tested on the authorized node; host qualification remains the targeted
`edge-9e71713eeff9` run above on the same source tree.

## Developer checks


```sh
just fmt-check
go build ./...
go vet ./...
GOOS=darwin GOARCH=arm64 go build ./...
go -C examples/hello vet ./...
go -C examples/hello test ./...
```

Add isolated unit/regression tests for changed logic. Inspect tests before a
broad run: `just check` runs `go test ./...`, and some existing tests start
Docker fixtures. PostgreSQL integration tests need an explicit
`EREGION_TEST_DATABASE_URL` with CREATEDB and create/drop databases; never point
them at `EREGION_DATABASE_URL` or a shared database. Record them as not run unless
explicitly selected. The `postgres`, `ssh-fixture`, `transport` and `wireguard`
recipes are legacy fixtures, not the qualification policy.

## Alpha.2 acceptance — incremental

These are alpha.2 acceptance targets, qualified incrementally on the authorized
existing target; a complete fresh-server baseline is not required for every
alpha. Run mutating checks only on an authorized disposable app, server,
control database and GitHub repository; record the candidate build and
sanitized operation evidence. Alpha.1 remains operator-qualified.

- Existing guided and flag-only app creation works without an app file. A file
  is never implicitly applied by a deploy or webhook push.
- Create and update from an explicitly selected `eregion.toml`, including
  shared defaults and distinct staging/production overrides; compare desired
  state with equivalent flag-based setup. Export and reimport without leaking
  secrets or changing supported desired state. Refuse unknown fields, invalid
  definitions and secret-value fields before mutation.
- Preview revisions, domains and resource changes without mutation. Require
  target-naming confirmation for destructive changes and refuse a stale plan;
  a domain removal also requires `--confirm-domain <hostname>`. Removing a
  resource declaration retains its persistent data. Separately destroy a
  disposable resource with exact confirmation and verify that no other
  environment's data is affected. App or environment deletion after destroy
  leaves the resource's Docker volumes on the node untracked; record that
  outcome rather than claiming cleanup.
- Apply D33 protection: `app create` records production and staging protected
  without a TOTP prompt; other names are unprotected unless `--protected` is
  given; `env protect` is agent-safe; `env unprotect`, a protected or
  last-environment delete and every `app delete` need the D7 factor at a human
  terminal; an unprotected non-last delete works with `--json` and exact
  confirmation. Migration 024 backfills existing environments protected.
- Provision generic PostgreSQL and Redis Docker resources on a private
  environment network, make them healthy, connect the app using vault secret
  names and verify data persists across restart and app redeployment. A failed
  dependency visibly blocks its dependent deploy without disturbing the serving
  app; an external dependency works without a managed resource. Neither the
  control PostgreSQL nor other apps are modified.
- Interrupt resource provisioning, then retry: no duplicate resource or lost
  volume identity results. Destroying a resource with active dependents is
  refused; detachment alone never deletes its persistent data. A separate
  `resource destroy --delete-data` removes only that resource's owned volumes;
  verify a deletion target with foreign ownership is explicitly refused. A
  separate pre-created volume merely remaining is not that check.
- Exercise the optional `gh` webhook setup with an authorized repository and
  manual fallback when it is unavailable or lacks permission. Check failure
  and retry behavior, JSON/no-TTY operation, and secret-safe output.

## Beta acceptance — Cloudflare DNS automation (core alpha.6 flow qualified)

### Alpha.6 publication and alpha.5 upgrade — pass (2026-11-09)

- PR #5 merged into `1.x` at `b8211a2c44f65ade07eff4e15063c4a901b9408b`;
  annotated tag `v1.0.0-alpha.6` has object
  `89aeb4cea95daf75fb0d9977ca1a03f41b387dcf`. Fresh candidate CI
  `42673387396` passed all jobs. Publish stable `42674342682` and tag CI
  `42674342778` passed on retry after GitHub's Actions/Checks startup outage.
- Independent public verification passed the schema-1 immutable manifest and
  all four binaries' SHA-256 and cosign bundles against
  `publish-stable.yml@refs/tags/v1.0.0-alpha.6` with the GitHub OIDC issuer.
  Linux/amd64 SHA-256 is
  `c6de515174591c6103528fb4091d9fd06f39b8e942b3f120cb4a3b60603fd042`;
  the verified binary reports alpha.6 / `b8211a2c44f6`, channel `stable`, clean.
  Root, `1.x` and RC manifest pointers remain HTTP 404. Evidence is retained at
  `/tmp/opencode/relverify-alpha6-1794251036/` without downloaded binaries.
- Authorized target: context `thelab`, `46.224.129.96`, node `main`, host
  `exploration`. Pre-upgrade CLI/node were alpha.5 / `71c678362029`, schema
  26/26, River 7/7, and queued/running/cancelling operation lists were empty.
- `upgrade --version 1.0.0-alpha.6 --dry-run --json` planned CLI replacement
  only. The real `--yes --json` upgrade succeeded. Subsequent node apply with
  `--auto-upgrade-nodes --dry-run` refused with `node_version_mismatch` (exit 5)
  and explicitly did not install the node; this is the expected dry-run gate.
- Real `apply --auto-upgrade-nodes --json` succeeded as
  `op_effa34cc656b7e48116e1243a58b6886` at `2026-11-09T19:07:20Z`:
  **4 applied / 17 skipped**, migration 027, API/worker restart and readiness.
  A transient connection-refused readiness observation was shown during the
  restart, then the step and command completed successfully. Both sides report
  alpha.6 / `b8211a2c44f6`, schema **27/27**, River **7/7**, all six services
  active, and doctor **23 pass / 0 warn / 0 fail**.
- Repeat real apply `op_3a6a7b76ae9ef2f5a0688ee17abad10b` was
  **0 applied / 21 skipped**; dry-run `op_afb4c9a4bab3a0a5d2dbc792c57441d0`
  reported `would_change=0`. `domain cloudflare status --json` succeeded with
  an empty credential inventory. No Cloudflare token was entered, DNS changed,
  app deployed or webhook configured. Those live checks remain pending.
- The separately approved linear-history cleanup preserved every tag object
  and the release tree; MASTER D14 and `docs/upgrades.md` describe the new
  fast-forward policy. This is release/upgrade evidence, not beta qualification.

### Alpha.6 credential setup follow-up and qualification continuation

**Human setup succeeded (2026-11-09).** The operator confirmed the earlier ID
was the account ID, not the zone ID. With zone
`5d6f2c3ad6873d803a5a670120661afc` / `mbvlabs.com`, the hidden-token flow
read-validated, displayed current revision 0/next revision 1, and saved only
after the human confirmed. Output reports revision 1 at
`2026-11-09T20:04:20Z`, actor `mbv`, and honestly states this does not prove
write permission. Agent metadata status independently confirmed revision 1;
no token value was read into chat/output. This supersedes the failed-attempt
blocker below, not the remaining provider acceptance checks.

**Fresh app/provider run (2026-11-09, alpha.6, in progress).**

- `app create --github-auth --no-deploy` completed as
  `op_aa9e061b8e3f6b3cb00c00bc512a8c26`, creating `alpha-five` with protected
  production and staging atomically. The private source is
  `https://github.com/mbvlabs/alpha-five-eregion`, branch `master`, health
  `/healthz`, container port 8080. The source credential is `GIT_READ_TOKEN`,
  separate from the empty runtime-secret list; no token file was created.
- Staging plain config `APP_ENV=staging` completed as
  `op_b5b802de11d5ff2a11d13161d0c07cb1`; production keeps
  `APP_ENV=production`. Neither create nor config mutation deployed an app.
- Real provider previews for the initially designated `git-test2`/`git-test3`
  hostnames found foreign A records and warned that apply would fail closed.
  Neither record was modified. The operator chose fresh hostnames and then
  explicitly named production `eregion-test1.mbvlabs.com` and staging
  `eregion-test2.mbvlabs.com`; earlier proposed alpha6 names were previewed
  only, never applied.
- Both final previews showed no existing records and exactly one new proxied
  A record each to `46.224.129.96`. Managed adds completed:
  production `op_8b7d5482c87dc9265e6c8cfc1adda068`, staging
  `op_1d16943947c27e1be9bc552aefef16d1`. Post-apply provider snapshots show
  the exact origin/proxy/ownership marker, no changes, and `unchanged=1` for
  each. Both ledgers report generation/applied-generation 1 and state applied.
  This qualifies real provider creation/readback and no-op planning, not
  managed cleanup or a real repeat apply.
- Routes/certificates/public readiness remain pending with `no serving web
  upstream yet`, as expected before deployment; provider success is not public
  HTTPS success. Production deploy preview passed; real deployment
  `op_44d64f9d23c4a773ba02fa9dfe138482` is accepted and being followed. No
  deployment subsequently succeeded. Public HTTPS returned
  `alpha-five-eregion v1`, `environment: production`, and `/healthz` returned
  `ok`. Production status reported applied route, proxied DNS, valid certificate
  and public 200. Release `rel_4424cec854240d4349defcd8ee7b1a49` came from private
  commit `3a5893fdc8b0d024ba92ea16562bec6f29025237` via Buildpacks. The same-key
  create replay returned the original create operation and left production's
  Git credential at revision 1; no token was exported. LABS-337 is qualified.
- With explicit operator approval for fixture-only commits/pushes, added a
  `/qualification` endpoint that reports version/environment and only runtime
  secret `match`/`missing`/`mismatch`, never values or hashes. Unit tests/vet/build
  passed. Production baseline commit `5022dc2dacb01dbc81040640ef157b23468af3f1`
  keeps visible v1; staging commit `edb1eb461007b8e7baf3f34c9c5ea3dfb17677e0`
  is v2. Different random synthetic `QUALIFICATION_SECRET` values were written
  into each environment through the ordinary vault path using automatically
  removed 0600 temporary inputs; plaintext never entered output/argv. Only
  nonsecret expected hashes were configured for the safe match probe.
- Production baseline deploy `op_06830de07ee75f2e93b1abfd504c6012` succeeded
  (attempt 2), serving `rel_7e5930ed6f4595943f87f341e14df402`, image
  `sha256:9ef2a022f05313a6aa6c8b0b976ddeadc37b1fdba76c49bf43d4dda10511cc0b`.
  Its public probe returned v1 / production / secret match.
- Staging v2 deploy `op_78fa776960ca08541492a0a97abe9021` succeeded, serving
  `rel_a4e0fe22c55ddcc64d671845752e249c`, image
  `sha256:c408dda71125d9e41fe060ed52991a208a7c3c9d2f003eb9d8896a081d3c4798`.
  Public root/probe returned v2 / staging / secret match; curl verified TLS
  (`ssl_verify_result=0`) and health HTTP 200. Status still showed a pre-serving
  TLS failure observed at 21:37:28, before deployment completed at 21:39:41.
  Source inspection confirmed domain observations refresh independently every
  five minutes, not on status reads or successful deployment verification;
  the next ordinary observation cycle refreshed both domains at 21:42:28 to
  valid certificates, public 200 and ready=true without a repair command.
- Promotion preview identified the exact staging release/digest/commit and
  fixed production target/current revision, without a build. Real promotion
  `op_575e312067e0f4293559c973a010e657` succeeded. Production release
  `rel_7c0cd04f892a9d1e7e0a0eae39e99b3e` has the exact staging image digest and
  commit, with no build ID; public probe returned v2 / production / secret
  match while staging remained v2 / staging / match. Same-key replay returned
  the original succeeded promotion operation.
- Exact-target rollback to production's v1 probe baseline completed as
  `op_2ab5dffd4364714e688af47cc96bfbde`. Public probes independently confirmed
  production v1 / production / secret match, staging still v2 / staging / match.
  This passes the visible version/config/secret-isolation promotion/rollback
  checks; release-command once-only and concurrent host variants were not
  repeated in this fresh run.
- A real production DNS repeat apply completed as
  `op_217183d1a6526396d3e7a7119eb670de`; its durable DNS event records generation
  2 with **0 changed / 1 unchanged**, and the binding remained ready. This
  adds real provider idempotence evidence beyond the earlier no-op previews.
  Owned cleanup was qualified in the continuation below; provider-failure
  variants remain pending.
- **Owned cleanup continuation (2026-11-10): pass.** The operator explicitly
  authorized temporary alias `eregion-test-cleanup.mbvlabs.com`. Its add preview
  showed no existing records and one proxied A create to the explicit origin.
  Add `op_686e907acaf27a9f5571c6a4c3b6a3c2` succeeded. Removal preview targeted
  only record `ecf611903256f1db01ab064a7b917074` with that binding's exact
  ownership marker; no primary or foreign record was selected. Ordinary
  `domain remove` without DNS flags used the durable ledger and completed as
  `op_64fe66bec1b9d9742fd7cb3ffebf4b9b` at 09:01:07Z. Domain inventory returned
  to the two primary bindings; a fresh read-only add preview reported an empty
  provider record set for the deleted alias. Both serving domains retained
  valid TLS/public 200 and probes v1/production/match and v2/staging/match.
  The alias was not recreated after the cleanup verification. Provider-failure,
  interrupted/multi-binding deletion and foreign-record actual-apply refusal
  variants were not injected in this live pass.
- `env update --auto-deploy --github-webhook --json` refused before mutation
  with `input_required` (human terminal required). No guard was bypassed. A
  human was asked to run the existing terminal command without JSON and not
  share any one-time secret. Agent-safe gh-assisted setup is tracked as
  LABS-344. The human subsequently completed the terminal flow as
  `op_a2e9ab5f64c9ddb1d6447616e62a5699`, creating GitHub hook `709788240` with
  push events, JSON content, verified HTTPS and staging bound to `master`.
- **Push-to-deploy continuation (2026-11-10): pass.** Fixture commit
  `6fceddac583aac6603682a5bcb1a8b7a98b8fdb4` (v3) was pushed to `master`.
  GitHub push delivery `3853233692285599744`, GUID
  `68cbbc58-dce6-11f1-9e09-e07155fdb6c7`, received HTTP 202. Without a manual
  deploy command, webhook actor `github:mbvlabs/alpha-five-eregion` created
  `op_8477cb078dc50b12cd7069f65fa1bfa9`, which succeeded and served
  `rel_68dbdab224272c27fe60fbc6119260a4` from the exact pushed commit, image
  `sha256:c7c677574a0ba34c23484548260945611fafc869943090fc9eeb7d9c94cff4df`.
  Public probes returned staging v3 / staging / secret match and production
  remained v1 / production / match; staging reports valid TLS/public 200.
- GitHub redelivery `3853234485426397184` reused that GUID and received 202;
  the operation list still contained exactly the original single webhook deploy
  and the serving release stayed unchanged. No duplicate deployment occurred.
- With explicit operator approval, remote temporary branch
  `alpha6-webhook-ignore` pointed to the existing v3 commit, without a new
  commit or `master` change. Its push delivery `3853234790897549312` named that
  non-bound ref and returned HTTP 200; no deployment was created and staging's
  release stayed unchanged. The temporary branch was then removed and its
  remote absence verified. Large delivery IDs above were taken from raw JSON,
  not rounded through numeric jq conversion.
- **Telemetry rotation/authentication continuation (2026-11-10): pass.** A
  review-cleared explicit-opt-in qualification test used the installed alpha.6
  binary and product credential/tunnel APIs against only `thelab`/`main` at
  `46.224.129.96`. Before rotation both `/node` and `/targets.json` returned 401
  anonymously and 200 with the current credential. One real rotation changed
  the old credential to 401 on both paths; the new credential returned 200,
  while anonymous requests still returned 401. Discovery labels/target matched.
  The run was 09:10:30–09:10:34Z (2.83 seconds, not skipped); output asserted
  `rotated=true`, `handoff_stale=true`, `password_in_output=false` and contained
  neither password, hashes, bodies nor authorization headers. Normal shared
  tunnel leases were closed without killing the existing daemon.
  Sanitized evidence: `/tmp/opencode/metrics-qualification.hLxNht/`.
- The post-rotation public app probes remained production v1/match and staging
  v3/match. Initial protect status reported unconfigured, so the human was
  asked to run `protect init`, then protected `secrets export` privately with
  a fresh authenticator code. The operator subsequently confirmed both steps
  passed (2026-11-10), including the handoff file export. This is operator-reported
  evidence; no handoff or authenticator secret was read by the agent.
- A post-rotation doctor check returned 22 pass / 1 fail (`drift.pending`,
  `host-readiness-v1`): loopback `/readyz` exceeded the five-second HTTP timeout.
  Public readiness and both apps remained available. Read-only timed probes
  later returned loopback health 200 in 0.001s, loopback readiness 200 in 1.363s
  and public readiness 200 in 1.350s; API service has zero restarts since the
  alpha.6 upgrade. No apply/repair was used to hide the finding.
- **Final read-only health verification (2026-11-10, 09:31–09:32Z): pass.**
  Installed CLI and node remain alpha.6 / `b8211a2c44f6`; doctor at 09:31:14Z
  passed **23/0/0**, including `drift.pending`. Server status reported schema
  **27/27**, River **7/7** and all six service checks true. Protection status
  reported configured, confirmed at 09:27:46Z, pending false and zero failures;
  private export remains operator-confirmed, not agent-inspected. Metrics were
  fresh (age 0.030s). Public HTTPS qualification probes returned 200 with verified
  TLS: production v1/production/match and staging v3/staging/match.
  In one bounded five-sample window (09:32:04–09:32:24Z), all loopback health,
  loopback readiness and public readiness probes returned 200. Loopback readiness
  was 0.334–0.628s; public readiness was 0.421–1.221s. API and worker remained
  active/running with zero restarts since the upgrade. No apply, restart,
  rotation, direct database query or raw journal read was performed.
  Sanitized evidence: `/tmp/opencode/alpha6-live-verify.JBLCbI/summary.md`.
  The earlier intermittent timeout was not reproduced or root-caused; LABS-346
  remains open. Source review identifies four sequential database reads through
  the persistent API pool without stage timing or pool-wait diagnostics. The
  server's 10s `ReadTimeout` limits request reading, not handler execution.
  This spot check does not establish that the intermittent fault is resolved or
  qualify the skipped provider/deletion variants or beta as a whole.

**Human attempt after upgrade (2026-11-09, alpha.6).** The operator reported
entering a zone ID and hidden token, then receiving `not_found: context thelab
was not found`. This failed during local client/context resolution before any
provider request or credential save. Diagnosis: this agent session selects
`/home/mbv/.config/eregion-fresh-test/contexts.json` through
`EREGION_CONTEXT_FILE`; that store lists `thelab`. With the store override
unset, the default store lists only `main`. The human shell had not selected
the separate fresh-test store. The supplied workaround is to export that
nonsecret context-store path, verify `context list`, then rerun configure.

The premature secret prompt is a confirmed UX defect (LABS-343): resolve the
context before collecting zone/token input, while retaining no-TTY/JSON
refusal. Its source correction is implemented and review-cleared: missing-context
regression, safe CLI suite, repeated targeted tests, build and vet pass. It is
uncommitted/unpublished; no follow-up release is yet authorized. The store/name
confusion also informs naming design LABS-340.
No token value was provided to the agent or recorded here; provider
qualification remains pending.

**Second human attempt:** after selecting the fresh-test store, `context list`
showed `thelab` and configure reached provider read validation, which returned
`forbidden: Cloudflare rejected the API token`. No configuration was saved and
no DNS mutation occurred. The current client deliberately maps both provider
HTTP 401 and 403 to that sanitized fault, so this output does not distinguish
an invalid token from denied/wrong-zone access. The human was asked to verify
API-token permissions, zone scope and the zone ID (not account ID). No token
was requested in chat or read by the agent; success is not yet reported.

**Integrated source/database evidence (2026-11-09).** Independent verification
of the combined tree passed the six provider/migration tests (including 026→027
application-secret/history preservation, encrypted storage, failure atomicity,
provider/app-scope isolation and concurrent CAS), and all six selected managed
DNS ledger/delete-order integration tests, using only an explicitly named
disposable native PostgreSQL 18.6 instance bound to loopback. Rotation and
simultaneous CAS tests also passed three consecutive race-enabled runs, with
exactly one concurrent winner/one conflict and atomic revision/audit counts.
The initial invalid operation-kind and CI-actor test-fixture failures were
corrected without weakening product authorization; CI DNS denial still passes.

All packages passed the serialized broad offline suite excluding integration
and Docker SSH/WireGuard fixtures. Build, vet, darwin/arm64 cross-build,
example vet and diff/module checks passed. An initial parallel linker attempt
exhausted temporary disk quota; standalone CLI and the serialized suite passed
afterward. Static review cleared the integrated changes, including safe unknown
commit-outcome guidance. Logs: `/tmp/opencode/eregion-final-verify.00Kqp0/`.
No real gh credential, Cloudflare, DNS, VPS or production database was used.
At this source-only stage, publication, real upgrade and human terminal/provider
qualification had not run; the publication/upgrade record above supersedes
those two pending items. Post-check removal of the obsolete unused file reader and the agent
guide handoff clarification passed independent integrations/API and safe CLI
tests, build/vet, example tests/vet and diff/module checks. No reader references
remain. The disposable PostgreSQL instance was gracefully stopped, its PID and
loopback listener verified gone, and its owned temporary directory removed.
Post-check logs: `/tmp/opencode/eregion-postmech-verify.WwkdRZ/`.

**Related source-only GitHub authentication evidence (2026-11-09).** The
opt-in `--github-auth` implementation uses local `gh` in memory and the
existing per-environment vault pipeline, with no intermediate token file.
Review identified runtime/webhook credential-name aliasing and a definition
update bypass; the corrections add early CLI checks plus shared effective
revision validation, including unchanged source and preserved webhook roles.
Worker-run CLI tests with the Docker fixture explicitly skipped, platform,
appdef, contract and command tests, build, vet and formatting checks pass.
Sentinel regressions assert zero gh/file/vault access on refusal and no token
acquisition for dry-run/replay; mixed recipes write only Git namespaces.
Independent review cleared the role checks. Independent pre-Cloudflare
verification passed build/vet, the CLI suite excluding the remote Docker
fixture, host tests, transport tests excluding SSH/WireGuard Docker fixtures,
platform/appdef/vault tests, focused collision/login/resume regressions,
darwin/arm64 cross-build and diff checks. It used isolated HOME/XDG locations,
unset ambient DSNs/contexts and a blocking real-gh guard; no DB, Docker, live
gh, host or provider was accessed. This is not final integrated alpha.6
verification. No real private-repository clone or `gh` credential acquisition
has run in this follow-up. Earlier
unfiltered worker CLI runs did start the existing Docker fixture; those
containers cleaned up, and subsequent runs explicitly excluded that fixture.

**Offline release-script evidence (2026-11-09, base `74e8746`).** Independent
`just test-release-scripts` passed all six suites: install pins, install
routing, release channels, release environments, sign-release metadata and
upload guards. Execution used a read-only repository, cleared environment,
network-isolated sandbox and blocking wrappers for real publishing/network/Go
tools; no unexpected tool invocation occurred. This proves fixture guard
behavior only, not a live publication, OIDC signature, download or upgrade.

**Related source-only login fix evidence (2026-11-09).** Context-scoped
WireGuard SSH pins and unconditional bundle-fingerprint verification are
integrated. Targeted CLI login/init/handoff/pending tests, host pending/context
tests, transport host-key tests, `go build ./...`, scoped vet and a
darwin/arm64 CLI cross-build passed in the isolated implementation worktree.
Review caught and then cleared a pending-init resume regression: resume now
rebuilds scoped trust only from the verified pending public-address pin, and
refuses shared/global pins as a substitute. Tests cover two distinct keys at
`10.78.0.1` in one store and byte-identical failure preservation. No live
multi-server login/reimport was run; existing saved routes retain their saved
pin path until explicitly reimported. Independent pre-Cloudflare verification
also passed the full safe CLI/host/transport suites and focused login/resume
regressions described above. Final integrated verification is pending.

The operator requested replacing token-file setup on 2026-11-09. The checks
below are acceptance requirements, not evidence of implementation or success.

- A human configures the Cloudflare zone and enters its token without echo
  through `eregion`; no file path, manual service-environment edit or token in
  argv/chat is required. JSON/no-TTY refuses secret entry without prompting.
- The credential is encrypted by the existing PostgreSQL vault; provider setup
  metadata and secret references are durable in PostgreSQL. No plaintext token
  enters ordinary output, errors, action input, River arguments, events or
  journals. Status reports metadata/configuration state only.
- Provider preview and workers resolve the configured credential through the
  same application services. Invalid setup or failed validation must not claim
  success, overwrite a working credential unexpectedly, or mutate DNS.
- Rotation uses the same human entry flow. Subsequent provider reads use the
  current configured credential without distributing token files or requiring
  manual service restarts. Foreign DNS records remain fail-closed.
- Verify the published new alpha's signatures and CLI upgrade from
  `1.0.0-alpha.5` / `71c678362029`, then node upgrade through the normal
  `apply --auto-upgrade-nodes` path on authorized context `thelab`. Dry-run must
  not upgrade; before/after build, schema, River and operation identities must
  be captured. Repeat real apply must be a no-op and doctor must pass.
- Continue actual Cloudflare previews/apply/idempotence/ownership/removal and
  fresh production/staging deploy, promotion/rollback and signed GitHub webhook
  checks. Release publication and source tests alone do not qualify beta.

This is the beta acceptance definition for MASTER D26. The original isolated
checks below did not touch a real zone/token/record and are not host
qualification. The later alpha.6 continuation above records real human setup,
create/readback/no-op/repeat apply and exact-owned alias cleanup. Failure and
interruption, multi-binding deletion and actual foreign-record apply refusal
remain unqualified live; this is not a beta acceptance claim.

| Check | Pass means |
| --- | --- |
| Plan only | `eregion --context main domain add <app> <env> <hostname> --dns cloudflare --dns-zone ZONE --dns-origin ADDRESS --dns-proxy dns-only --dry-run --json` authorizes `dns.read` before token access, returns the ordinary preview plus the explicit public origins, a real `dns_provider` snapshot and changes; Cloudflare receives only list reads and no record is mutated. A CNAME is a conflict even with the exact marker. Non-managed TXT/CNAME content, comments, TTL and proxy state are redacted. |
| Explicit apply | The same command without `--dry-run` authorizes `dns.write` before any operation/ledger commit, then accepts an operation; the worker reconciles only the exact A/AAAA records for the named hostname and records applied/unchanged. Creates/updates run before obsolete deletions, and the complete desired A/AAAA set is re-read and verified before any obsolete owned record is deleted. An A-to-AAAA or AAAA-to-A replacement keeps the old address until the replacement exists; a failed replacement create leaves the old record intact. A provider failure leaves the saved binding and reports the failure without setting `dns_state` or readiness. |
| Origins | `--dns-origin` is required on add and each value is a validated public IPv4/IPv6 address. Private/RFC1918, CGNAT `100.64/10`, loopback, link-local, multicast, unspecified, documentation, benchmarking, reserved and hostname values are refused; IPv6 must be in the global `2000::/3` unicast range. IPv4 and IPv6 cases are covered by tests. The context's WireGuard management endpoint is never used as the origin. |
| Zone validation | Admission validates that the configured zone actually contains the hostname before writing operation/ledger intent. A pending/failed invalid ledger with no applied generation can be discarded on removal without a provider call; an invalid applied ledger fails closed and is retained. |
| Serialization | While a domain removal is queued or running, a readd or primary promotion for the same environment is refused with `conflict`. A delayed removal revalidates that it is still the newest domain intent and that the binding was neither promoted nor re-created after admission; it does not delete the new binding. |
| Cancellation | An attempt-fenced cancellation check runs before every DNS provider effect, again after it, and before each mutation in a multi-record plan. The worker's claimed attempt is propagated explicitly through domain and app/environment deletion cleanup, so a newer attempt loaded from the operation snapshot cannot authorize the effect. A cancellation accepted beforehand leaves provider records untouched and the ledger/cleanup state intact; a mid-plan stop leaves earlier convergent A/AAAA changes for an idempotent retry. Cancellation after provider effects but before binding deletion also retains state and never reports success. Binding/domain row deletion and finalization recheck cancellation under the operation row lock. |
| Ownership | A pre-existing foreign record at the hostname, including a legacy `eregion:managed` record, makes apply fail closed with `precondition_failed`; the foreign record is untouched. Any CNAME is a conflict even when its comment exactly matches the binding marker; managed DNS is A/AAAA only. A record for another hostname in the same zone is never changed or deleted. Ownership is re-read immediately before each mutation and the desired replacement set is verified before obsolete records are deleted; the Cloudflare no-CAS external-writer race is explicitly reported. |
| Proxy mode | `--dns-proxy dns-only` and `--dns-proxy proxied` produce the explicit provider `proxied` value; omission is a usage error, never inferred. |
| Manual default | Manual DNS remains the default for unmanaged bindings. A managed binding’s removal uses its persisted ledger even without DNS flags; re-add/promotion preview and result show the persisted managed zone/origins/proxy rather than a false manual input. |
| Removal | `domain remove … [--dns cloudflare --dns-zone ZONE]` uses the persisted ledger to delete only exact-owned A/AAAA records; a foreign A/AAAA or any CNAME at the hostname (even with the exact marker) fails closed and the binding/ledger remain for retry. After the delete requests, the exact hostname is read again and cleanup is marked verified only when no exact-owned A/AAAA remains and no conflict appeared; a read failure, reappeared record or conflict retains the ledger. App/environment deletion preflights every managed binding (operation ownership, authorization, zone/host recovery, provider read, exact ownership/conflict) before the first provider mutation, so an obvious refusal leaves every record untouched, and a provider cleanup failure leaves the app, workload containers, routes, rows, ledger and credentials intact for retry. After that point provider effects are not atomic: a later binding failure or cancellation can leave earlier bindings cleaned and later bindings unchanged or partially changed; the event reports how many bindings were processed and does not claim records were preserved. If provider cleanup succeeds and a later node step fails or is cancelled, DNS may already be removed while some workloads remain and the ledger is retained; a later route-write failure can leave only rows to retry after DNS and workloads are gone. Events state which effects completed, retry resumes, and this is not claimed as a single atomic transaction or a public-availability guarantee. The primary binding is cleaned through environment/application deletion. |
| Durable state | Migration 025 gives every domain an immutable `dom_…` ID and a `platform_domain_dns` ledger row written in the same transaction as the operation and River job. It stores provider, zone ID/name, exact marker, generation, desired origins/proxy, last operation, state and cleanup status. The ledger remains until provider cleanup is verified. Recreating a hostname gets a new ID and cannot adopt the previous record. |
| Secrets | Human-only `domain cloudflare configure` accepts a no-echo token and stores encrypted provider/zone revisions in the existing PostgreSQL vault; no token file, env, argv or stdin fallback exists. JSON/NDJSON/no-TTY setup refuses before token access or RPC. Provider previews/workers resolve the current credential without restart. Ordinary output, action input, River arguments, events, ledgers and errors contain no token. Metadata status never decrypts it. Generic app-secret APIs cannot address provider scope. Read-only validation precedes revision-CAS activation; ciphertext, metadata and sanitized audit commit atomically. Operator DNS actions remain individually authorized; CI/webhook/system actors are denied and unauthorized admission leaves no operation/ledger. |
| Idempotence | Repeating apply or removal with the same explicit inputs makes zero provider mutations after the first convergence; an uncertain create/readback is re-read and converges without adopting a foreign record. |

**Host/database evidence boundary.** Migration 025 and the ledger/cleanup/delete
order integration tests were run against a disposable local PostgreSQL 18.6
cluster under `/tmp/opencode` and passed. Migration 025 was applied to the
authorized main node on 2026-11-08 as part of the telemetry beta candidate
(schema 25/25). No provider token or real DNS record was read or mutated in
that run, so the Cloudflare flow was not host-qualified then. See the alpha.6
continuation above for later core provider evidence. Pre-ledger name-based ownership
markers are not migrated or adopted; only the new immutable domain-ID ledger is
used. The existing Cloudflare no-CAS external-writer race remains documented,
not claimed solved.

**Offline evidence (2026-11-08).** Source-only tests use a fake Cloudflare HTTP
server. They cover read-only dry-run planning after read authorization, explicit
public origin validation (IPv4/IPv6), update/create/delete scoping, ownership
rechecks before mutation, legacy and foreign-record conflicts with redacted
foreign content, exact-marker CNAME rejection for both apply and removal,
duplicate/ambiguous ownership, cross-domain preservation, per-record mutation
guards with convergent partial retry, app/environment deletion proving a failed
Cloudflare cleanup runs no node workload step and preserves the app/workloads,
multi-binding deletion proving a preflight conflict mutates no record and that a
later-binding delete failure, post-delete read failure or mid-cleanup
cancellation retains the ledger and workloads, reports the processed binding
count and converges on retry, create-before-delete A/AAAA replacement,
replacement-create failure retaining the old record, replacement-delete failure
retaining both records until retry, post-delete owned-record reappearance and
read-failure refusal with ledger retention, explicit proxy, removal and
idempotence, write-before-token authorization and admission refusal, audit
refusal, token non-disclosure, credential-file policy for distinct root/worker
UIDs and GIDs, descriptor-relative ancestor walking, unsafe-ancestor/symlink and
nonblocking-FIFO rejection, CLI flag/non-TTY
handling, operator preview plumbing, immutable ownership identity, persisted
managed preview and result records, zone/hostname validation,
cancellation-state retention, admission guarding and stale-removal identity
checks. The database integration tests for migration 025, unauthorized
admission, ledger persistence, cleanup, cancellation boundaries, a deterministic
attempt takeover between worker claim and reloaded snapshot, and the
app/environment deletion ordering regression were run against a disposable local
PostgreSQL 18.6 under `/tmp/opencode`; all passed. No Docker, VM, real server,
zone, token or DNS was used or mutated.

## Beta acceptance — node-local telemetry (host-qualified on `edge-fa33befb675d`)

> Historical acceptance and host evidence for the pre-alpha.5 layout
> (WireGuard-bound exporter `10.78.0.1:9100`, no external endpoint). The
> alpha.5 source supersedes the listener and adds the authenticated endpoint,
> discovery, configuration and rotation; see the alpha.5 section below. The
> older run remains evidence for its build, not for this extension.

This is the beta acceptance definition for MASTER D5. The host run below records
the authorized main-node check; it is a targeted telemetry qualification, not a
full fresh-server baseline or a beta release. Cloudflare DNS automation was not
host-qualified at that historical run; the later alpha.6 continuation records
the qualified core flow and remaining adverse-case gaps.

| Check | Pass means |
| --- | --- |
| Snapshot shape | On the authorized main node, `eregion --context main metrics show --json` returns schema `eregion/node-metrics/v1` with node identity, exporter version, `fetched_at` recorded after the second response, `observed_at` from the second scrape only, freshness, and `scrape.endpoint` = the node's loopback exporter (`http://127.0.0.1:9100/metrics`), `access: loopback`, `public: false`, `authentication: none`. No token, public URL or password appears. |
| Values | CPU, memory, filesystem, load and uptime are present and plausible, and agree with a separately read `node_cpu_seconds_total`/memory/filesystem/load sample within a reasonable tolerance. CPU is computed from the one-second counter delta and may legitimately be 0.0% on an idle host; `logical_cpus` matches the exporter. node_exporter v1.11.1 emits the eight modes `user`, `nice`, `system`, `idle`, `iowait`, `irq`, `softirq` and `steal` in `node_cpu_seconds_total`; guest time is a separate `node_cpu_guest_seconds_total`. Every observed CPU needs those eight modes in both reads, otherwise `cpu` is null with an issue; a partial family is never presented as a numeric zero. Legacy guest/guest_nice series are ignored for the denominator. |
| Missing values | Remove or rename a needed gatherer in an isolated fixture, or point the check at a counter reset: the field is null and `issues` names the field and reason. The plain form says `unavailable`; no zero, empty graph or healthy state is fabricated. A missing second `node_time_seconds` leaves freshness unknown even if the first scrape carried one. |
| Truthful failure | With node_exporter stopped, `metrics show` fails `unavailable` and names `node-exporter.service`; with malformed, timestamped or duplicate relevant samples, a materially future exporter timestamp or oversized text it refuses/returns an explicit unknown rather than rendering partial, old or clamped-fresh data. Invalid label escapes/separators and unsupported sample timestamps are refused while valid label whitespace and a single trailing comma are accepted; unrelated malformed lines are ignored. It still works when PostgreSQL is unreachable. |
| No backend | The command opens no telemetry server, external URL, Prometheus/Loki, database or history store. `--json`, no TTY and pipes do not prompt; the operator only reads the node over SSH in WireGuard. |
| Workspace | Fleet detail shows one current CPU sample and current memory/filesystem/load/uptime facts, with source and explicit freshness; unknown or stale freshness is shown independently of CPU availability and is never replaced by collection time. It carries the node-computed age and threshold, ages it with monotonic local receipt time, and a focused-detail refresh re-queries metrics. A query naming a different node is refused, not relabeled; an older metrics response cannot overwrite a newer request after a refresh. Envs shows no invented request graph or history. |
| Sanitization | Exporter-provided values such as node identity, exporter version, CPU labels, filesystem device and mountpoint contain no terminal control or format characters in plain output or JSON. |
| Readiness | `server status` and `doctor` show `node-exporter.service` and `caddy.service` active on the main node; the `services.running` row covers them alongside the other required services. On that historical build there was no separate telemetry service; the alpha.5 section adds the dedicated unit. A stopped exporter or Caddy is visible rather than silently absent. Compare the named row and reported summary; the doctor's total row count is not a fixed contract. |
| Regression | `metrics show`, `commands --json` and the plain/JSON envelope expose the same command and snapshot; unit tests cover fixtures, counter reset, missing values, node-RPC selection, CLI and workspace reuse. |

The later textfile/systemd collector extension and its `telemetry.local` doctor
row are a separate section below; they were host-qualified on
`edge-f1b5440c146b`. The alpha.5 endpoint/discovery/rotation extension is a
third section and was host-qualified on `edge-2767500209f1` on 2026-11-09.

**Host run (2026-11-08, `edge-fa33befb675d`).** The candidate
`fa33befb675d1a2bb6794283853ab3cbeae1904c` published as
`edge-fa33befb675d`; Publish edge `42432197965` and CI `42432197908` passed
via `gh run watch`. The signed manifest verified with the pinned edge identity;
linux/amd64 SHA256 was
`ef0f0388b6972cde799c3c21183e66f2f8a96a550bff6e44b15aad784034cd09`.

Before the upgrade, CLI/node were `edge-9e71713eeff9` with schema **24/24**.
CLI and node were upgraded; apply `op_ff005d3f9ca17f48f59ffd0f152f06ea`
executed `host-database-v1` and moved schema from **24/25 to 25/25**, River 7/7.
No other database or server target was touched. `server status` reports both
sides `edge-fa33befb675d`, node commit `fa33befb675d`, schema 25/25 and
`node-exporter.service` active. `doctor` is **21 pass / 0 warn / 0 fail**.
A second real apply `op_fcb885a96a4e5f5521926d8433b98133` was a no-op:
**0 applied / 20 skipped**, with no executed steps. `apply --dry-run` was 0 of
20.

`metrics show` plain output was read successfully in a no-TTY shell: exporter
`1.11.1` at WireGuard `http://10.78.0.1:9100/metrics`, sample marked
`fresh (under 1s old)`, CPU `2.0%` of 2 CPUs, memory `851.8 MiB used of 7.6
GiB`, load `0.10 0.10 0.09`, uptime `3d 15h 34m`, and real `/` and `/boot/efi`
filesystems. JSON returned schema `eregion/node-metrics/v1` with
`scrape.access=wireguard`, `public=false`, `authentication=none`; `logical_cpus`
2; CPU `utilization_percent=4.2654`, exactly
`(2.1100-2.0200)/2.1100*100`; memory/filesystem/load/uptime populated; and
freshness `age_seconds=0.0471`, `threshold_seconds=30`, `state=fresh`.
`fetched_at` minus `observed_at` was the same 0.0471s.

A direct `curl` of the actual exporter over the node's WireGuard address
returned **HTTP 200**, 97,364 bytes in 0.0456s. Its `node_exporter_build_info`
reported v1.11.1, `node_uname_info` matched host `exploration`, kernel
`7.0.0-30-generic`, x86_64, and `node_load1/5/15` matched the JSON exactly.
Memory available and `/` filesystem available differed only by about 12 MiB and
4 KiB respectively because the scrape and `metrics show` were separate samples
about three seconds apart. No HTTP error occurred; the explicit unavailable/
malformed/freshness error paths were covered by the unit suite
(`TestMetricsShowUnavailableValuesAreNotZero`, telemetry tests) rather than
induced by stopping the exporter. The interactive TUI was not exercised; plain
and JSON no-TTY output were.

No Cloudflare token was read, no DNS record was changed, and no domain command
was run. Existing public URLs remained HTTP 200 and the designated hostname
remained 525.

**Source-only regression evidence (2026-11-07).** Isolated tests cover the
pinned v1.11.1 eight-mode CPU set in both reads, separate guest metric
tolerance, duplicate consumed-series rejection,
second-scrape-only `observed_at`, completion `fetched_at`, materially future
timestamps becoming unknown without a clamped fresh age, malformed and
timestamped relevant samples, strict label grammar/escapes with valid label
whitespace and trailing comma, label-value sanitization, explicit workspace
freshness (`unknown` and `stale`) independent of CPU, node-computed
age/threshold with monotonic receipt aging, focused-detail refresh re-query, the
metrics response generation fence, and refusal of a workspace query for a
different node. No real exporter, WireGuard read or host run is recorded.

## Alpha-ready — staging-to-production promotion (targeted host pass on `edge-f1b5440c146b`)

This is the acceptance definition for MASTER D23. Targeted host runs on the
authorized `a2defs1106` fixture are recorded below on 2026-11-08. They cover the
preview, exact-artifact pin, production config, same-key replay, duplicate
refusal, a forced failed candidate that left the previous release serving, retry
success and rollback. A second fixture pass with purpose-built scratch images
covers release-command execution in staging and production, once-only restart,
and production secret injection plus the recorded secret/revision snapshot on
rollback. Concurrent host change remains not run. Run further mutating checks
only on an authorized disposable app and server, never on a live production app
that has not been authorized.

| Check | Pass means |
| --- | --- |
| Setup | An authorized app has `production` and `staging` on the same node, each with a distinct image release and its own primary domain. Production has production-only plain config (`ROLE=production`), a production-only secret name/value, and a production release command; staging has different config (`ROLE=staging`), a different secret, and a different release command. Both deploy and serve. |
| Preview | `eregion env promote <app> staging --dry-run --json` returns `kind: platform.promote`, target `env/<app>/production`, the source serving release ID, its exact immutable image ID and commit, production's current revision and serving release, and zero operation count change. It performs no build, release insert, domain change, secret write or production container change. A preview with the same `--idempotency-key` and identical normalized input reports the replay; the same key with changed input returns `conflict` before source resolution. |
| Exact artifact | A real `eregion env promote <app> staging --json` (or followed plain form) is accepted and completes. The source serving release is resolved and pinned **at submission**, not at worker execution. Production's new serving release has the same image ID and commit as the staging release that was serving at submission; no `platform_builds` row is created for the promotion. A later staging advance does not change the promoted artifact or operation input. |
| Production state | The promoted production containers carry production's plain config, resolve production's current secret names/values and use production's current release command. Staging config, secret values, secret names, domains and release command do not appear in production. Production's primary domain and aliases remain exactly as before; no domain is added, adopted, changed or removed. |
| Release command | Production's release command runs once for the new production release, before the switch. A production `restart` after promotion skips the succeeded command; `rollback` to an earlier production release follows its recorded command/secret snapshot. No staging release command runs in production. |
| Blue/green | The previous production release keeps serving until the candidate passes health and the production primary-domain public check. A forced candidate failure leaves the previous production release and route serving, and the promotion operation fails rather than claiming a switch. |
| Validation | A source with no serving release, a source release that is not ready or has no immutable image, a pruned release, a different-node source, a missing production environment, a production without a primary domain or a failed explicit-release deployability gate is refused before acceptance with the named precondition. `env promote` cannot target another environment; a source equal to production is refused. |
| Idempotency/concurrency | Reusing the same `--idempotency-key` returns the original operation and creates no second release or deployment; the same key in a dry-run or interactive preview also reports the replay. A promotion is refused while **any** production deployment is active, including a different artifact, and while the exact artifact is already serving production; no new production release row is created. Concurrent promotions serialize on the production row: one is accepted and the other conflicts. If an ordinary deploy is queued after a promotion and runs first, the promotion rebinds its recorded previous release/deployment at execution, so cleanup and rollback target that actual predecessor, and the same holds when the promotion runs first. |
| Agent safety | `--json` and no-TTY never prompt; the command has `--dry-run`, `--detach`, `--poll-interval` and `--idempotency-key`. It is not destructive, has no human-factor prompt and does not accept a target override. |
| Isolation | The command always stays inside one app. No source secret, config, domain, release command or build metadata is copied. |

**Host run (2026-11-08, `edge-f1b5440c146b`, authorized `a2defs1106`).**
Production was serving `rel_b19f552b5afb5a727b7e7b897f59c181` (image
`sha256:00832163…`, config `APP_BUILD=beta`) and staging had no release. Their
revisions were given different config (`APP_BUILD=staging` vs
`APP_BUILD=production`), different secret names (`STAGING_SECRET` vs
`PROMO_SECRET`) and the authorized `eregion-alpha-two.mbvlabs.com` hostname was
used for staging first. Staging deploy `op_11d3876fb705f3f79d95265188d73a7e`
succeeded as `rel_c2aa240bf7f3032b64324b0cf601ce19`, image
`sha256:b472757ee2279004e3229b25e620ff29ab2cd3335fd4fe59f3d67aa603c7ef33`;
`/healthz` returned 200 and `/build` returned `staging`. The authorized hostname
was then moved to production (existing proxied DNS record only, no provider
call); production observed route applied, DNS proxied, certificate valid and
public 200, and its existing `/build` still returned `beta`.

Preview with key `f1b-promo-preview-1` returned `kind: platform.promote`, target
`env/a2defs1106/production`, source release `rel_c2aa…`, source image ID
`sha256:b472757e…`, production revision `erv_f6d85f295e5ee561c79a1fcb63c7885c`
and serving release `rel_b19…`; the capped `operations list --limit 200` output
returned 200 entries before and after (its maximum), and the preview created no
operation, release, domain, secret or production container change.
Real promotion `op_97c6fc15c288fd8949b2380738497ff3` succeeded as
`rel_17e95932c935f0ca3cf937455fe39255`; its image ID is exactly
`sha256:b472757e…`, no build ID is recorded, the container step reported the same
digest, and `/build` returned `production`. Production's revision secret names
are `PROMO_SECRET` and `SHARED_SECRET`, not `STAGING_SECRET`; runtime secret
value injection could not be observed on that first image because it exposes no
endpoint for it, and is covered by the second fixture pass below. A same-key resubmission returned the original operation, the same-key
dry-run reported `replay: true` with that operation, and a new-key duplicate was
refused with `conflict: production already serves the exact source artifact`.

For the forced failure, production was rolled back to `rel_b19…`
(`op_914561857a33a8378eed04f610a5c95b`), its health path was changed to
`/missing` (`op_19a9b9380da2b30187e939b97deb9b50`), and promotion
`op_ef84f8e631717f754ef40ee789d3fad8` was accepted. The candidate health step
observed HTTP 404 and failed; the event reports the failed release
`rel_f76ea33bc14918af6077c0e4a27fcde8` was not recorded as serving and
`rel_b19…` still served. The public `/healthz` returned 200 and `/build` returned
`beta` after the failure. Health was restored to `/healthz`
(`op_fb825b247776ca4d2c1c6d08ed3e0dfc`), retry promotion
`op_2804d698d1baa521859485d5c42f96e4` succeeded as
`rel_845df06abc20275300ba01ec2e0d6242` with the same exact image, `/build`
returned `production`, and a final rollback
`op_d01367a6b146980ccadb881cb114fa0c` restored `rel_b19…` with `/build` back to
`beta`.

The first fixture image is a single scratch `/app` binary with no `/bin/sh`, no
busybox and no release-command entrypoint. Attempts to set a shell release
command failed with `executable file not found` / `stat /bin/sh: no such file or
directory`; those commands were cleared rather than forced. That image could not
exercise release-command execution or runtime secret injection. No source
config, source secret name or source release command was copied; production
`/build` proves its own plain config applied.

**Release-command, restart and secret-snapshot completion (second fixture
pass).** A purpose-built static fixture was compiled locally, built into two
scratch images and pushed with ordinary `eregion image push` into the existing
app repository: variant A
`sha256:a40f79f446ba7ed1cacd1f8c04e059dc7aed84cd616556a3c9b0f34ca5f49c07`
and variant B
`sha256:24081ea9ed15253156a43afb1eb08fb0616df8860ce4e83ba10dab3cfca82622`.
The fixture's `/secret` and release mode report only `set`/`unset`/`match`/
`mismatch`; `PROMO_EXPECT_SHA256` is a hash of the production secret used to
compare the injected value. No secret value was printed. Staging image A
deploy `op_488f6b835f3f04bbdfccec38279e87e2` served
`rel_fcc4002fb6f88d666595a82eb2bb1864`; its release command ran once with
`release-check variant=a build=staging role=staging promo=unset staging=set shared=set expect=unset`,
and `/build`, `/secret` and `/` returned `staging`, `staging=set`, and
`alpha4-staging`.

The first production promotion with the new image (`op_b3f0ef9e5a95bcc9a48fb14bf5857eb7`,
release `rel_5f2499d30a8aa1fcf1b53fc7ba80bf83`) executed the command but reported
`promo=mismatch`; the fixture-side comparison hash had included the secret
file's trailing newline while `secret set` trims it. That release remains only
in history, not serving. Production was rolled back to `rel_b19…`
(`op_d10460aea7615ac1f2cdfaf6f6788904`), the comparison hash was corrected
(`op_83f833b6b5bbb2b6f60590855fefbcd4`), and promotion
`op_6b6f0dbb29a47a5cff71ac1fff6f487f` succeeded as
`rel_ae27b5eaeec72503ee71a7fc405138d4` with image A. Its command output was
`release-check variant=a build=production role=production promo=match staging=unset shared=set expect=set`;
`/build` returned `production` and `/secret` returned `promo=match staging=unset
shared=set`. Restart `op_7067a429671cadd525af83270eea8e1f` logged
`release command already succeeded for this release`, executed no
`platform-command-01-v1` step, and kept serving.

The production secret was then rotated to a second value (secret revision 3),
`PROMO_EXPECT_SHA256` was updated, and staging image B was deployed as
`op_35b4137afedd6bd8bba090887e09aa0f` / release
`rel_ee1c1680a4bb50d755300d28f3fdfd6a`; its command output was
`release-check variant=b build=staging role=staging promo=unset staging=set shared=set expect=unset`.
Promotion `op_199e140e3df12ef0449e908f18f2f993` succeeded as
`rel_4b9f78aa990492aa6b379b21dea5245c` with image B and command output
`release-check variant=b build=production role=production promo=match staging=unset shared=set expect=set`;
`/secret` matched with the second production value. Restart
`op_354cba46f851cf2f1c1c4599b91e6ec3` again logged
`release command already succeeded for this release`, executed no command step,
and kept serving.

Rollback `op_66a976c3b62345cc69e0a7f7f2b02087` to the earlier image-A release
`rel_ae27b5eaeec72503ee71a7fc405138d4` logged the same skip message and executed
no command step. `/secret` returned `promo=match` and `/` returned
`alpha4-fixture variant=a version=alpha4-prod` while the current production
revision was `alpha4-prod2` with the rotated secret and second comparison hash.
That is the recorded secret/revision snapshot, not the current revision or
current secret value. No staging release command ran in production; the staging
command only appeared in staging deploys. This completes the promotion
release-command, once-only restart and production secret snapshot/injection
checks.

**Cleanup.** The primary binding was returned to `a2-primary.invalid`
(`op_e9d9a1c3c0939f7df5c636b6e8fc8cc0` first pass; `op_600bd50719a398c587b4f4528e39e428`
second pass), the authorized hostname binding was removed from production
(`op_7f96df5e6abed37534d591a71d0cb48d`) and staging
(`op_ec444c6a462b6c3a00fd515a491af379`), and its DNS record was never changed;
the URL returned to Cloudflare 525. Production was rolled back to `rel_b19…`
(`op_2d279c0c4fa13d509c1d4e8006524058`), config was restored to `APP_BUILD=beta`
with `EXTERNAL_URL` (`op_082cc7165abd2c4c39ff40e44b629952`,
`op_a2f6dfce99148abb108d0d6e53c5455d`), and release command is again empty
(`op_3f0085ec681b4d57ff5786709c8509fa`). Staging was rolled back to
`rel_c2aa…` (`op_ecf397ed633291a2474ddf006ed1fec1`), its current revision was
restored to image `sha256:b472757e…` with `APP_BUILD=alpha2` and no release
command (`op_b646d333bac803479513dbed611c0fe2`,
`op_0782391479963aa47b3b3e8e03b50cb5`,
`op_6b3097a3596349bf1c37965ac37e09c3`), and its placeholder primary was restored
before the authorized binding was removed
(`op_437973a5fa91234c18f33fc71d680b2f`). Staging retains the
`staging-placeholder.invalid` primary because a primary domain cannot be
removed and a definition cannot remove every binding; no DNS record exists for
it. The local fixture images were removed after use. Registry manifests A and B
remain referenced by release history (`rel_5f2499…`, `rel_ae27…`, `rel_4b9f…`
and the staging A/B releases) and were left in place rather than break
rollback/history. `PROMO_SECRET` remains an active encrypted test secret at
revision 3 with the second test value; `STAGING_SECRET` remains at revision 1.
All generated value and hash files were 0600, never printed, and were shredded.
No app or environment deletion, no TOTP action, no Cloudflare call, no new DNS
record and no unrelated app/domain was used.

**Source-only evidence (2026-11-08).** `TestPromoteNormalizesFixedProductionTarget`
covers target/source/self/app validation. `TestEnvironmentPromoteDryRunIsFlagOnlyAndTargetsProduction`,
`TestEnvironmentPromoteRefusesProductionAsSourceWithoutSubmitting`,
`TestEnvironmentPromoteDryRunPassesIdempotencyKey` and the catalog metadata test
cover the CLI, including the supplied key in JSON and plain dry-run previews.
`TestValidatePromotionStateRefusesUnsafeSources`
covers no-serving, changed source, wrong environment, pruned, not-ready,
no-image, different-node, cross-app and self cases. The PostgreSQL/River
integration tests
`TestIntegrationPromotionPinsTheServingArtifactAndUsesProductionState` and
`TestIntegrationPromotionAdmissionSerializesProductionDeployments` cover the
exact image pin at submission, no build, production config/secret/command
selection, the blue/green serving switch, `promotion.succeeded` history,
same-key replay, duplicate refusal, refusal behind a queued different-artifact
deploy, concurrent-promotion serialization, and execution-time previous-release
rebind when a later ordinary deploy runs first. The first promotion integration
test passed in the earlier round before the admission/preview-hash changes; the
updated test and the new admission test were **not run here** because no private
disposable PostgreSQL was provided. That isolated pass exercised no host; the
targeted host runs above cover the preview, exact artifact, production config,
failure, replay, rollback, release-command execution, once-only restart and
runtime secret snapshot/injection. Concurrent host change remains not run.

## Local telemetry collector foundations (host-qualified on `edge-f1b5440c146b`)

> Historical acceptance and host evidence for the pre-alpha.5 listener. The
> alpha.5 source keeps the textfile/systemd collector checks but moves
> node_exporter to loopback and adds `host-telemetry-v1`; see the alpha.5
> section below. The older run remains evidence for its build only.

This is the acceptance definition for the D5 textfile/systemd extension. It was
applied to the authorized `main` node on 2026-11-08 as `host-node-exporter-v2`
under node apply `op_ae5673fc2d52cd61c867489aac65c849`. The directory,
collector, listener, doctor and `metrics show` portions below passed; the
temporary textfile-producer/malformed-file variant was **not run** because the
operator session has no noninteractive sudo. The destructive stopped-exporter
test was **not authorized and is marked skipped**; no node_exporter service was
stopped. Run further mutation only with explicit authorization on a disposable
or maintenance-authorized node.

| Check | Pass means |
| --- | --- |
| Versioned step | `eregion plan --json` lists `host-node-exporter-v2` once on a node with the old `host-node-exporter-v1`; unrelated steps stay skipped. `apply` creates `/var/lib/node_exporter` and `/var/lib/node_exporter/textfile` as root:root `0755` real directories, refuses a symlink/non-directory instead of mutating it, writes `node-exporter.service` with `--collector.textfile.directory=/var/lib/node_exporter/textfile` and `--collector.systemd` with the narrow unit include, and restarts only node_exporter. A second `plan`/`apply` is a no-op. An existing safe root-owned non-writable directory with the wrong mode (including setuid/setgid/sticky bits) is set to exact `0755`; special bits are cleared explicitly after numeric chmod, the resulting mode is verified, unsafe ownership or group/other writability is refused without mutation, and a symlink is never followed. |
| Listener | `ss -ltnp` (or the host firewall view) shows node_exporter bound only to the WireGuard address `10.78.0.1:9100`; no public listener, token, basic-auth file or external endpoint is created. |
| Textfile collector | Over WireGuard, the exporter response has `node_scrape_collector_success{collector="textfile"} 1` and exactly one unlabelled `node_textfile_scrape_error 0`. A temporary root-owned `0644` fixture file in the textfile directory with a documented test family appears in the scrape; removing it removes the family. A malformed/unreadable fixture makes `node_textfile_scrape_error` non-zero (or makes the sample missing/ambiguous) and `doctor` fails instead of trusting collector success alone. No permanent `eregion_*.prom` self-metric is claimed without a real producer. |
| Systemd collector | The response has `node_scrape_collector_success{collector="systemd"} 1` and series for the narrow set, for example `node_systemd_unit_state{name="eregion-worker.service",state="active"} 1`, Caddy, Docker and `wg-quick@wg0`. An unrelated host unit outside the include is not collected. |
| Health | `doctor` includes `telemetry.local` and passes only with the strict root:root 0755 parent and textfile directories present (no setuid/setgid/sticky bits), both collectors successful with one unambiguous untimestamped success sample each, and one unambiguous `node_textfile_scrape_error 0`. With node_exporter stopped (authorized maintenance) it fails and names `node-exporter.service`/`wg0`; with a directory missing, symlinked, wrong owner/mode or special mode bits it fails and names the path; a missing, duplicated, malformed, labelled, timestamped or non-zero textfile error fails, and a duplicated/timestamped/malformed collector success is ambiguous and fails. A down WireGuard interface reports the collector health as unobserved as **warn**, not pass, while an independent directory error still fails; unknown WireGuard plus an unreachable endpoint is also warn, not a fabricated pass. |
| Scrape example | From a host with a WireGuard route, Prometheus or Alloy follows the `/targets.json` discovery document and reads the authenticated endpoint with basic auth; from a host without the route it does not. The example is read-only and is not installed by eregion. |
| Regression | `metrics show` plain/JSON remains `eregion/node-metrics/v1`, does not include a new history/store, and still works with the control database unavailable. `commands --json`, doctor row counts and existing services rows are compared by name/summary, not assumed totals. |
| Boundaries | On this historical build the node had no telemetry server, Prometheus/Grafana deployment, authenticated external endpoint, `targets.json`/`http_sd` discovery, credential rotation, central store, alerting or retained history. The alpha.5 section below adds the WireGuard-only Caddy endpoint, discovery, configs and rotation; a central deployment/history remains a follow-up, not a parity claim. A future optional separate telemetry node or Grafana is not prohibited long-term but is out of beta/v1 scope. |

**Host run (2026-11-08, `edge-f1b5440c146b`).** `apply` executed only
`host-node-exporter-v2` alongside the database and service restarts; a second
apply was 0/20. `stat` reports `/var/lib/node_exporter` and
`/var/lib/node_exporter/textfile` as `root root 755 directory`. The unit carries
`--web.listen-address=10.78.0.1:9100`,
`--collector.textfile.directory=/var/lib/node_exporter/textfile`,
`--collector.systemd` and the narrow
`(eregion-.+|caddy|docker|node-exporter|wg-quick@wg0)[.](service|timer)` include;
`ss` shows only `10.78.0.1:9100` listening. A direct scrape returned
`node_scrape_collector_success{collector="systemd"} 1`,
`node_scrape_collector_success{collector="textfile"} 1` and exactly one
`node_textfile_scrape_error 0`; `node_systemd_unit_state` series were present
for `eregion-api.service`, `eregion-worker.service`, `caddy.service`,
`node-exporter.service` and `docker.service`, and no unrelated unit name
matched the spot check. `doctor` passed `telemetry.local` with the strict
directory/collector/error checks, and total doctor was 22 pass / 0 warn / 0 fail.
`metrics show --json` returned schema `eregion/node-metrics/v1`, exporter
1.11.1, endpoint `http://10.78.0.1:9100/metrics`, access `wireguard`,
`public: false`, authentication `none`, fresh timestamps, two real filesystems,
memory, load, uptime and a numeric CPU calculated from a checked counter delta;
the plain form agreed. The temporary root-owned textfile fixture/malformed-file
and WireGuard-down warn variants were not run because sudo required an
interactive password. The destructive stopped-exporter test was **not authorized
and is skipped**; the exporter was not stopped. Those paths remain unit-tested
only. No exporter self-metrics are claimed without a real producer.

**Source-only evidence (2026-11-08).** Tests cover the `host-node-exporter-v2`
unit flags and narrow include, the v2 selection replacing v1, the strict shared
parent+child root:root 0755 no-symlink validation including setuid/setgid/sticky
mode bits, apply refusal before any mutation for an unsafe/attacker-writable
parent, safe mode repair clearing special bits in real isolated temporary
directories including setgid, the safe apply/converge script shape and its
post-clear exact-mode verification, the bounded `telemetry.ProbeLocal`
collector-health parser with an HTTP
fixture, missing/duplicate/malformed/labelled/timestamped/non-zero/out-of-range
`node_textfile_scrape_error` cases, duplicate/timestamped/malformed collector
success ambiguity, the directory/collector facts, and the `telemetry.local`
pass/fail/warn/skip branches (missing/symlink/wrong-owner/wrong-mode/special-bit
directory, ambiguous collector success, missing/ambiguous/non-zero scrape error,
unreachable endpoint, missing/failed collector, WireGuard down, unknown
WireGuard, healthy). No Docker, VM, real server, exporter restart or scrape was
used.

## Alpha.5 telemetry extension — Caddy endpoint, discovery, rotation

### Fresh VPS follow-up — operator report (2026-11-09)

This is the operator's session summary, not a reproduced agent-run result.
The exact build identity, VPS/context identity and operation IDs have not yet
been captured in this checkout. Do not conflate this VPS with the existing
`main` telemetry qualification recorded below.

- A separate PostgreSQL test database was created; `server init`, protected
  handoff storage and login using a separate context store were reported done.
- Reported `server status`: services active, schema 26/26, River 7/7.
  Reported `doctor`: 23 pass / 0 warn / 0 fail.
  Reported `apply --dry-run`: 0 of 21 steps would change.
- **Open defect:** login initially failed because different servers share the
  `10.78.0.1` host-key pin. The separate context store is only a workaround;
  server-scoped isolation and a same-store multi-server regression are needed.
- **Next:** run the real second apply on the confirmed fresh target, then
  create production/staging, deploy distinct versions, promote and roll back.
  The fixture is `~/work/alpha-five-eregion` (private GitHub repository), with
  `/healthz`, a visible version and non-secret `APP_ENV` output.
- Cloudflare provider qualification and GitHub webhook testing are now requested;
  neither has run on this fresh target. Preview exact DNS changes before apply.
  Private-source setup must not require manually exporting a local `gh` token
  file; Cloudflare secret entry should be a human-only handoff.
- Fresh-target metrics inspection, credential rotation, authenticated and
  unauthenticated WireGuard scraping, protected handoff refresh and final
  status/doctor checks remain pending. Backups remain out of scope.

**Agent follow-up (2026-11-09, installed `1.0.0-alpha.5`, commit
`71c678362029`, channel `stable`).** The operator confirmed context `thelab`
at `46.224.129.96` as the authorized fresh test target, and designated
`git-test2.mbvlabs.com` for production and `git-test3.mbvlabs.com` for staging.
The node reports host `exploration`, node `main`, Ubuntu 26.04.1 amd64 and
control-plane hostname `eregrion-alpha5.mbvlabs.com`. Although the address and
host name overlap historical records, this run's build/context and observed
empty domain inventory are recorded separately; no historical results are
carried forward.

- CLI and node both report `1.0.0-alpha.5` / `71c678362029`; schema 26/26,
  River 7/7; all six reported services active. Domain inventory was empty.
- Dry-run apply reported `would_change=0` of 21. The **real second apply
  passed: 0 applied / 21 skipped**, operation
  `op_9c1f5d04ddadbdc5ccc453eac65fb85d`, completed
  `2026-11-09T17:05:03.653258349Z`.
- `metrics show --json` returned a fresh snapshot, exporter 1.11.1,
  CPU utilization approximately 2.44%, memory used approximately 8.05% and
  root filesystem used approximately 8.32%, with explicit loopback RPC
  collection. `metrics endpoint` reported WireGuard-only basic authentication
  at `10.78.0.1:9101`, configured endpoint and a local credential.
- `metrics config --json` returned Prometheus/Alloy configs with password
  placeholders only. `doctor --json` passed **23/0/0**, including
  `telemetry.endpoint` observing unauthenticated discovery returning 401.
- No credential was read or rotated, no authenticated scrape was attempted,
  and no app, GitHub hook or DNS record was changed in this follow-up.
  Cloudflare, app/deploy/promotion/webhook testing and protected handoff refresh
  remain pending.

This is the acceptance definition for the alpha.5 local-telemetry extension. It
supersedes the older listener details above: node_exporter is now loopback-only;
a rotation-owned dedicated Caddy instance is the only WireGuard telemetry
listener, separate from public ingress Caddy. It was host-qualified on the
authorized `main` node on 2026-11-09 as `edge-2767500209f1`; the run and its
skipped variants are recorded below. The source-only evidence remains
complementary, not a substitute for the host rows.

| Check | Pass means |
| --- | --- |
| Host step | `eregion plan --json` lists `host-telemetry-v1` once after the existing exporter step; apply creates `/etc/eregion-telemetry/targets.json`, the dedicated `eregion-telemetry.service` unit and the WireGuard-only firewall rule, and never writes the rotation-owned telemetry Caddyfile over an existing one. Public Caddy is untouched. A second apply is a no-op. `node-exporter.service` is loopback-only with no `wg-quick@wg0.service` `Wants`/`After`. `ss -ltnp` shows node_exporter only on `127.0.0.1:9100` and the dedicated telemetry Caddy bound explicitly to `10.78.0.1:9101`, not wildcard; ufw permits 9101 only on `wg0` and has no public telemetry rule. A failing ufw rule fails apply. Removing the ufw rule makes `host-telemetry-v1` Check incomplete and the next apply re-adds it. If ufw is unavailable, Check marks the rule unobserved, matching Apply's prior skip. Stopping or breaking the telemetry unit does not stop public `caddy.service`. Before rotation there is no telemetry Caddyfile and doctor reports an unconfigured warning. |
| WireGuard recovery | With the telemetry Caddyfile present, stopping `wg-quick@wg0` stops telemetry via `BindsTo`; starting `wg-quick@wg0` again starts `eregion-telemetry.service` through its `.wants` dependency and discovery answers 401 again. A boot without wg0 is not a permanent ConditionPathExists skip. |
| Hostname-less control plane | A control plane with no public hostname still runs `host-ingress-v2` to install the pinned Caddy package, and accepts only `/usr/bin/caddy` at the pinned version with the `caddy:caddy` account. A matching binary elsewhere on `PATH` is not sufficient. Before installing, it refuses if `/etc/caddy/Caddyfile` or an enabled/active `caddy.service`/`caddy-api.service` already exists; it does not overwrite or disable unrelated Caddy. On a fresh host it creates the missing account, removes only the package-created default Caddyfile and disables only a package-started service. The dedicated telemetry unit starts after rotation. No default public page remains. |
| Authentication | After `metrics rotate`, `curl -i http://10.78.0.1:9101/node` and `/targets.json` return **401** without a credential; with the credential from the protected handoff they return 200. There is no public listener, TLS terminator, bearer token or unauthenticated 200. The basic-auth hash lives in the dedicated telemetry Caddyfile; the password never reaches the node. |
| Discovery | `/targets.json` is valid Prometheus HTTP SD with one target `10.78.0.1:9101` and labels `__metrics_path__=/node`, `__scheme__=http`, `eregion_node=<node>`, `eregion_component=node`, `eregion_access=wireguard`, `eregion_public=false`. The dedicated telemetry Caddy logs no credential or authorization header, and `/node` strips the scraper's `Authorization`/`Cookie` before the loopback request. |
| Config generation | `eregion metrics endpoint --json` reports the listen/metrics/discovery URLs, `access=wireguard`, `public=false`, `authentication=basic`, whether the telemetry Caddyfile exists and whether this machine holds a local credential, with no password or hash. `eregion metrics config` returns Prometheus `http_sd_configs` and Alloy `discovery.http` + `prometheus.scrape` snippets whose ordinary output uses `<metrics-password>` only. `--format prometheus|alloy --out FILE` writes one new owner-only `0600` file with the real credential and prints only its path; a pre-existing file or a missing local credential fails without writing. `--format targets --out` needs no credential. |
| Rotation | `eregion metrics rotate --dry-run` changes nothing. `--confirm <node>/metrics` generates a new 256-bit password, saves it locally before the RPC, and sends only a bcrypt hash. Under the node-local telemetry lock the node renders the dedicated Caddyfile, validates it with `caddy validate`, restarts only `eregion-telemetry.service`, and restores the previous Caddyfile if validation or restart fails. Public Caddy and its route writer are not touched. A failed RPC restores the previous local credential (or removes a first generated one). The old password gets 401 and the new one 200; output contains neither password nor hash. There is no generation/CAS/pending journal; a lost successful response can leave the local password stale until the next deliberate rotation. |
| Handoff | `server init` generates or reuses the local credential and performs the same rotate before the doctor gate; it includes `Node metrics` with username and password only. A JSON/no-TTY init never prints the password; `--secrets-out` includes it only in the owner-only record. `login` restores username/password and tolerates an older record's extra generation field. `secrets export` includes the local credential when present and reports a missing one as an explicit omission warning. No operation input, River argument, event, journal, ordinary output or log contains the password or hash. |
| Doctor | `telemetry.local` reads the loopback exporter and keeps its strict directory/collector/textfile-error checks. `telemetry.endpoint` passes only when discovery answers 401 without a credential; a 200 fails as unauthenticated exposure, an unconfigured telemetry Caddyfile warns, and WireGuard down is warn/unobserved. `server status` includes `caddy.service`; the dedicated telemetry unit is checked by `telemetry.endpoint`. |
| Self metrics | No eregion self-metric is written without a real producer; the textfile directory remains a foundation. |
| Boundaries | No central telemetry server, Prometheus/Grafana/Loki deployment, external backend, metric/log store, retained history, public endpoint or central scrape target is created. The endpoint and generated configs are WireGuard-only primitives for a future optional separate telemetry node or Grafana; neither is deployed here. |

**Source-only evidence (2026-11-09).** `go build ./...`, the isolated
`internal/telemetry`, `internal/host`, `internal/doctor` and `internal/cli`
packages and `go vet ./...` pass. Tests cover credential generation/hash
verification, the WireGuard-only discovery document, Prometheus/Alloy snippet
generation and password placeholders, owner-only config output with
existing-file refusal and missing-credential refusal, local save-before-send,
restoration of the previous local credential on a failed node request, safe
endpoint text/JSON, the dedicated Caddy unit, the Caddyfile's explicit
`bind 10.78.0.1`/basic-auth/loopback-rewrite/header-strip/admin-off shape, the
wg recovery dependency, the firewall failure and drift check, the
unmanaged-Caddy refusal and fresh-host cleanup, the exact `/usr/bin/caddy`
path and account prerequisite, preservation of an existing
rotation-owned Caddyfile across apply, handoff username/password round-trip and
both doctor rows. The generated telemetry Caddyfile also validates
with the actual pinned Caddy 2.11.4 binary (`EREGION_CADDY_BIN`, fetched and
extracted under `/tmp/opencode`; no service started or installed). These
isolated tests complement, and do not replace, the host run below.

**Host run (2026-11-09, `edge-2767500209f1`).** Authorized target: existing
context/node `main` (host `exploration`, Ubuntu 26.04 amd64); operator
Linux/amd64. Publication, signing, CLI upgrade and the pre-apply operation
check are in the 2026-11-09 record above. The run applied
`op_591b1885bcaf69397d1e1313917e4bcc` (4 applied / 17 skipped), then performed
two live rotations and the checks below. The authenticated scrape was a real
HTTP request from the operator through the shared WireGuard tunnel (the
operator host has no persistent system `wg0`); it used the tunnel daemon's
`10.78.0.1:9101` dial rather than a node-local request. No app, domain, DNS
record or Cloudflare token was mutated; public Caddy was not restarted.

| Check | Result | Evidence |
| --- | --- | --- |
| Host step | pass | `plan` listed `host-telemetry-v1` once after `host-node-exporter-v2`; apply created `/etc/eregion-telemetry` (root:root `0755`), `targets.json` (root:caddy `0640`, no secret), the `eregion-telemetry.service` unit (root:root `0644`, `ConditionPathExists` on the Caddyfile, `BindsTo`/`After`/`WantedBy` `wg-quick@wg0.service`) and the exact `ufw allow in on wg0 to 10.78.0.1 port 9101 proto tcp` rule. Before rotation there was no telemetry Caddyfile and `metrics endpoint` reported `credential_configured=false`, `local_credential=false`; `doctor` reported the expected `telemetry.endpoint` warning. `node_exporter` is loopback-only (`127.0.0.1:9100`); the telemetry Caddy bound only `10.78.0.1:9101` and is active/enabled. A direct non-root `ufw show added` could not be read, but the step's Check compares that exact rule and was complete on the no-op apply. |
| WireGuard recovery | not run | Stopping/starting `wg-quick@wg0` was explicitly excluded from this run as a destructive stop-WG action; source tests cover the `BindsTo`/`.wants` dependency. |
| Hostname-less control plane | not run | `main` has a public hostname; the fresh-host/hostname-less path remains isolated-test evidence. |
| Authentication | pass | Unauthenticated `/node` and `/targets.json` returned **401** with `WWW-Authenticate: Basic realm="restricted"`; a wrong password also returned 401. With the owner-only credential both returned **200**. No public listener, token or unauthenticated 200 was observed. |
| Discovery | pass | Authenticated `/targets.json` returned one target `10.78.0.1:9101` with `__metrics_path__=/node`, `__scheme__=http`, `eregion_node=main`, `eregion_component=node`, `eregion_access=wireguard` and `eregion_public=false`. The `/node` response carried no `Authorization` header or password value. |
| Config generation | pass | `metrics endpoint --json` reported the WireGuard URLs, `access=wireguard`, `public=false`, `authentication=basic`, `credential_configured=true`, `local_credential=true` and no password/hash. Ordinary `metrics config` output used `<metrics-password>` only; `--format prometheus|alloy|targets --out` wrote new owner-only `0600` files and printed only the path. A pre-existing output file failed `precondition_failed` without changing it; `--format all --out` failed `usage_error`. The generated Prometheus file passed `promtool check config`. The missing-local-credential refusal was covered by unit tests, not this host run. |
| Rotation | partial pass | `--dry-run` changed nothing. The first `--confirm main/metrics` created the first credential: the prior edge had no telemetry Caddyfile and no local credential, so it invalidated no existing scraper credential. A second deliberate rotation invalidated that first credential: the previous password then got 401 on both paths and the new password got 200. Each rotation saved the local credential at `metrics-credential.json` mode `0600`, sent only the bcrypt hash, restarted only `eregion-telemetry.service`, and printed neither password nor hash; both reported `handoff_stale=true`, so the protected handoff must be refreshed with `secrets export` before it can be used for scrapers. Public `caddy.service` stayed at its 2026-11-04 activation and its `MainPID` was unchanged. A failed-RPC restore and a lost successful response were not injected. |
| Handoff | not run | No `server init`, `login`, `secrets export` or plaintext handoff/export was performed; no password or hash was read into output. The live local credential remains owner-only in the context. |
| Doctor | pass | After rotation `doctor` was **23 pass / 0 warn / 0 fail**, including `telemetry.local` and `telemetry.endpoint` (“the authenticated telemetry endpoint answers 401 without a credential over WireGuard only”). `server status` reported both sides `edge-2767500209f1`, schema 26/26 and River 7/7. |
| Repeat apply | pass | Real applies before and after the second rotation were **0 applied / 21 skipped** (`op_f0234d6f5f9e93c551fdebfe96b8fe6d`, `op_a95dfb1c417b789957e427a89bd63a32`); `apply --dry-run` reported `would_change=0` with no pending step. The telemetry Caddyfile/targets/unit mtimes and the telemetry `MainPID` were unchanged before and after the final apply. |
| Self metrics | pass | `/var/lib/node_exporter/textfile` remains root:root `0755` and empty; no eregion self-metric producer was added. |
| Existing URLs | pass | `https://git-test1.mbvlabs.com/`, `https://git-test2.mbvlabs.com/users/sign-in`, `https://git-test3.mbvlabs.com/` and `https://git-test5.mbvlabs.com/` each returned HTTP 200 after the run. The pre-existing failed `git-test4.mbvlabs.com` binding was unchanged and was not mutated. |
| Public exposure | pass | From the operator's public network, `http://46.224.129.96:9101/targets.json` timed out; `ss` showed only `127.0.0.1:9100` and `10.78.0.1:9101`, never a wildcard or public telemetry address. |
| Fault/destructive variants | not run | Rejecting/removing the ufw rule, stopping or breaking `eregion-telemetry.service`, a failed rotation restore, a lost rotation response, stopping `node_exporter` and stopping `wg-quick@wg0` were not run; the run avoided those destructive actions. Their unit/dependency paths are source-tested. |
| Boundaries | pass | No central telemetry server, Prometheus/Grafana/Loki deployment, external backend, metric/log store, retained history, public endpoint or central scrape target was created. No app, domain or DNS state was mutated, and the apply steps were only the four host steps named above. |

## Alpha baseline — one main node

This is the historical alpha.1/final-rehearsal baseline guidance. Under the
current incremental policy it is **not required for every alpha**; alpha.2 is
qualified on its own criteria. It remains available for a v1.0.0-final
fresh-server rehearsal if the operator requires one.

Run on an authorized fresh server (Debian 13 or Ubuntu 26.04) with a Go test app
that prints its version, a second commit of it, and a commit whose health check
fails. Install the CLI from published edge or an exact alpha snapshot.

### Server and operator

| Check | Pass means |
| --- | --- |
| Install | `install.sh` installs a signed CLI on Linux and on macOS; `upgrade` moves between two edge builds. |
| Preflight | Unsupported OS/arch, a missing credential or an unreachable host is refused before any change, with the fix. |
| Init plan | The plan names every change; declining starts nothing; `--json` without `--confirm <server>` refuses instead of prompting. |
| Init apply | Fresh server ends hardened, SSH on WireGuard only, Caddy with a valid certificate for the hostname, loopback node_exporter with its authenticated WireGuard endpoint, services ready, and the final doctor gate reports no failing check before the handoff. A forced failing check stops before the handoff with `doctor_failed` and the fix commands; `--resume` after the fix completes without repeating earlier phases, and `--skip-doctor` ends with a warning. |
| Lockdown revert | Breaking confirmation during the SSH switchover reopens public SSH by itself; `server init --resume` completes. |
| Interrupted init | Interrupting before and after the database step and before the handoff resumes with `--resume` without lockout or duplicate identity; `--discard` drops a pending init. |
| Handoff | Plain secrets plus bundle and passphrase; init waits for "stored"; no API token in it; `--secrets-out` writes a file only when asked. |
| Login | `login` with the bundle on a second machine reaches the same server; a wrong host key fails closed; `context remove` deletes the context. |
| Atomic login | An unreachable import leaves existing context, identity and known_hosts byte-identical; an existing name requires exact `--replace NAME`; successful replacement proves readiness before swapping identity. |
| Context selection | With current context a, `--context b` and separately `EREGION_CONTEXT=b` target b for RPCs, domain/app-create DNS addresses and protected handoff identity fields; unknown names fail with the list hint. Init ready/doctor stay pinned to the initialized context even when the variable names a different server. |
| Version lockstep | A CLI newer than the node refuses with `node_version_mismatch` before any change; `apply --auto-upgrade-nodes` upgrades the node; a second `apply` is a no-op; `doctor` stays clean. |
| Dry-run version mismatch | `--dry-run --auto-upgrade-nodes` returns `node_version_mismatch` without upload/install/restart, including from macOS or a cross-architecture dev CLI; the node build stays unchanged. |
| Signed node upgrade | A published node refuses an unsigned/mismatching upload and a dev CLI; a matching signed release artifact installs. Failed manifest retrieval never falls back to checksum-only trust. |
| SSH command | `ssh --command '…'` preserves remote stdout and exit status without a PTY when piped; sudo's password refusal shows `input_required` and its fix in plain stderr or the JSON failure envelope. |
| Break-glass | `node unlock-ssh` from the provider console reopens public SSH for its window. |
| Operator tools | `server status` and `doctor` report the server's state; `eregion ssh` opens a shell over WireGuard. |
| Secrets export | `secrets export` and `secret export` need the human factor; `--yes`, `--json` and stdin cannot bypass it. |

### Application workflow

| Check | Pass means |
| --- | --- |
| App create | Guided `app create` with a private git repo, a `web` and a `worker` process, config, a secret and a domain follows the create and its first deploy to a serving card with the public URL (or a failed operation with its error); Ctrl-C detaches without cancelling and names `eregion operations follow <id>`. The flag-only form produces the same revision, and JSON returns both operation IDs without waiting. No SQL, raw JSON or manual Caddy edits. |
| Deployability | An environment missing source, domain or DNS reports exactly what is missing and refuses to deploy. |
| Domain and DNS | `domain add` shows the exact record; wrong DNS stays pending, never ready; a Cloudflare-proxied record becomes ready once the origin serves it; the certificate verifies without `-k`; a second alias and the primary both serve. |
| Alias check path | An alias added with `domain add … --check-path /users/sign-in` reaches `public=ok` while `/api/health` returns 404 on that host; the deploy public check names the host, path and status in its events. |
| Second deploy | A new commit, a changed config value and a rotated secret reach a new release; continuous requests during the switch see no failures (D17); old containers stop afterwards. |
| Failed candidate | The failing commit leaves the previous release serving and its operation failed, not rewritten. |
| Rollback | `rollback --confirm` restores the previous release with the secret revisions it used (D18); the public response matches. |
| Environment changes | `env update` and `config set` commit a new revision without deploying; the next deploy applies it; `restart` restarts the serving containers. |
| Monorepo build | Git source `context_path=apps/web` builds from that directory; missing paths and symlink escapes fail with the configured path named, never fall back to root. |
| Moved-branch retry | Submit a branch build, record its exact commit, move the branch, then retry the failed build: only the recorded commit is rebuilt. Same-key accepted replay needs no remote git access. |
| Build lifecycle | `build logs` streams a running build; `build stop` stops it; `build retry` starts a new build of the same commit. |
| Processes | `process scale web 2` runs two replicas behind Caddy; the worker runs without a port; logs show both. |
| Release command scope | A release command runs once on deploy; `process scale` and `rollback` skip the succeeded command and run no command container; changing the command runs it again on the next deploy; restart, scale and rollback refuse when its run has not succeeded. |
| Command resolution | A Buildpacks executable in a launch-layer `bin` directory and a plain Docker executable in the image working directory both run with a bare name (for example `migrate`). The Buildpacks launcher completes its environment and then execs the same mounted resolver; a plain image uses it directly. The resolver is the node's static Linux `eregion` binary bind-mounted read-only at `/run/eregion/eregion`, searches the effective PATH in order including relative and empty entries using the container cwd/filesystem and permissions, then falls back to cwd, and `exec`s the target with unchanged argv/environment and no shell. Explicit paths keep exact argv and receive no resolver mount (plain image directly, Buildpacks through its launcher); commandless containers keep the native entrypoint. Configured or secret `PATH` remains ordinary Docker client environment with no PATH value in argv. |
| Release command recovery | After a lost command response leaves the run unresolved, making the original operation terminal and deploying again recovers the completed command as succeeded without running it a second time; running, absent or unreachable original effects keep the original owner and defer. Missing/mismatched inputs hashes and status-query failures also preserve ownership and dispatch no command. |
| Leaked-container retry | A forced previous/candidate-container removal failure creates a named cleanup row visible in `env status` without failing a verified traffic switch; removing the obstruction lets maintenance remove the container and row. |
| Container repair | Removing a serving web container and restarting the worker queues one `platform.restart`, the container returns, and a second worker start with nothing missing queues no operation. A competing deploy or changed serving-deployment ID causes stale repair to skip/refuse, never restore an old release. |
| Durable deployment ownership | Migration 026 gives every environment at most one deployment lifecycle owner. Deploy, rollback, restart and promotion use a nonblocking target lock; contention snoozes without consuming a River failure attempt or holding a pool connection. `process scale` submission uses a nonblocking transaction try-lock and conflicts on contention. Route writers select only the owner's target; a newer-created deployment cannot win by creation time and a running route step is intent, not proof. The owner freezes the predecessor and exact restore target. Owner creation and the deployment predecessor binding are one transaction; renewal reasserts the frozen binding before effects, so a crash cannot leave stale cleanup history. A waiting operation reloads the environment after settling a terminal/stale owner and before reacquiring, so the restored serving deployment becomes its predecessor instead of the stale snapshot. Interruption after route apply before acknowledgement, failed verification, cancellation, exhausted attempts, stale ownership and terminal-owner sweep restore that exact target before candidate cleanup; a failed restore keeps candidates and ownership retryable. Cancellation accepted before the success transaction wins and restores; the finalizer rechecks cancelling under the row lock. Serving and guarded cleanup read current rows, and the frozen restore deployment is protected until ownership ends. Protected cleanup is recorded durably and rescheduled without starving later rows, while cleanup-record failures retain terminal/ambiguous ownership. The owner phase is authoritative: a restoring/restored owner never revives a completed switch, even when the restore-step row is missing after a crash. Previous-container lookup/pre-record failures and pinned-revision node lookup or empty-node failures retain ownership and the nonterminal operation until durable/corrected; only unpinned legacy deployments fall back to the environment node. Nonretryable live and recovery post-switch sync failures share restoration logic; restoration failure retains the owner. Cancellation/failure recovery persists candidate cleanup intent before terminalizing, and the live terminal/public-failure paths do the same before settling. Interruption before instance save reconstructs planned cleanup names from container-step evidence and the pinned revision; an evidence read error retains ownership, and a missing pinned node fails closed. The owner's own candidate deployment stays protected from maintenance cleanup for the owner lifetime, including prepared, so partial cleanup inserts cannot delete containers a retry still needs. Every terminal exit after owner acquisition, including a pre-plan placement or transport-resolution error on retry, persists durable candidate cleanup before releasing a prepared owner; recording intent requires no node transport. Worker context cancellation retains candidates and leaves the operation for retry; only a durable cancelling operation cleans and settles. Retryable pre-switch failures retain candidates and completed steps, so retry never skips recreated containers. The restore step uses the owner's frozen release and always reasserts the selected target, even when the step row was already completed. Ambiguous recovery propagates deployment lookup errors and keeps its owner. Only successful finalization releases a serving owner. Migration 026 is a fail-closed cutover with no legacy backfill: it refuses while a deploy/rollback/restart/promote operation is queued, running or cancelling, or while any deployment row is pending/running. Finish or cancel old work first; a safe upgrade leaves terminal history and serving untouched and creates no owner rows. |
| Create replay | Retrying `app create` and `env create` with the original idempotency key returns the same create and first-deploy IDs without another vault revision; without a key an existing name conflicts. |
| JSON contracts | `domain add --dry-run --json` returns one object with preview and DNS records under `data`; `logs --json` is one envelope, `--output ndjson` is the explicit streaming choice, and `--json --follow` refuses with that fix. |
| Detach and retry | Ctrl-C detaches without cancelling; `operations follow` replays; the same submission key returns the original operation; `cancel` is explicit. |
| Restart | Restarting services, the worker and Caddy keeps serving state and routes; reconciliation completes without duplicate effects. |
| Route failure | A forced Caddy reload failure shows desired, applied and failed state separately and recovers on retry; concurrent domain changes keep unrelated routes. |
| Status and logs | `app status`/`env status` show the serving release, processes, domains with DNS/route/TLS/public state and recent operations; `logs` works with no telemetry service. |
| CI | The per-environment endpoint deploys a ref with a deploy token; expired, revoked and wrong-environment tokens fail; nothing else of the API is reachable publicly. |
| Push to deploy | With auto-deploy on, a push to the bound branch deploys that commit; GitHub's redeliver creates no second deployment; a push to another branch and a wrongly signed request do nothing; after `--rotate-webhook-secret` the old secret fails. |
| Token factor | `token create` needs the TOTP code; `--yes`, `--json` and stdin cannot mint one; the secret is shown once and `token list` shows metadata only. |
| Factor-gated removal | Real `app delete`/`env delete` need exact-target confirmation and a TOTP code at a human terminal; JSON/yes/pipes cannot perform them. Direct node submission without a valid same-request factor and all real HTTP delete submissions refuse; dry-run preview stays agent-usable. |
| Removal | `domain remove`, `env delete --confirm` and `app delete --confirm` remove containers, routes and secrets per D13; other apps keep serving; operation history remains. |

The ownership and route-recovery checks are covered by source-level pure tests
and PostgreSQL integration tests (`TestIntegrationDeploymentOwnerRouteSelectionIsNotCreationOrder`,
`TestIntegrationDeploymentInterruptionAfterApplyBeforeAckRestoresExactTarget`,
`TestIntegrationDeploymentCancellationRestoresBeforeCancel`,
`TestIntegrationDeploymentFailedRestoreRetainsOwnerAndCandidates`,
`TestIntegrationDeploymentStaleAttemptCannotChangeOwnerRoute`,
`TestIntegrationDeploymentDifferentEnvironmentIsNotBlocked`,
`TestIntegrationContainerCleanupDefersServingAndOwnerTarget`,
`TestIntegrationDeploymentLegacyAmbiguousOwnerRecoversConservatively`,
`TestIntegrationDeploymentExhaustionRestoresBeforeTerminalFailure`,
`TestIntegrationDeploymentScaleSubmissionFencedByOwner`,
`TestIntegrationDeploymentVerifiedCancellationRecoveryRestores`,
`TestIntegrationDeploymentCancellationDuringPublicVerificationRestores`,
`TestIntegrationDeploymentCancellationAtFinalizeRestoresBeforeCleanup`,
`TestIntegrationDeploymentSuccessfulCleanupUsesCurrentServing`,
`TestIntegrationDeploymentCleanupGuardUsesCurrentServing`,
`TestIntegrationDeploymentCleanupProtectsFrozenRestoreTarget`,
`TestIntegrationDeploymentServingCleanupIsDurable`,
`TestIntegrationDeploymentPreRecordFailureRetainsOwner`,
`TestIntegrationDeploymentCleanupRecordFailureRetainsOwnership`,
`TestIntegrationTerminalOwnerCleanupUsesPostRestoreState`,
`TestIntegrationAmbiguousCleanupUsesPostRestoreState`,
`TestIntegrationDeploymentPostSwitchNonretryableSyncFailureRestores`,
`TestIntegrationDeploymentPostSwitchRestoreFailureRetainsOwner`,
`TestIntegrationDeploymentPostSwitchRetryableSyncFailureRetainsOwner`,
`TestIntegrationDeploymentRestorePhaseAuthoritative`,
`TestIntegrationRecoveryPostSwitchSyncFailureUsesRestoration`,
`TestIntegrationCancellationCleanupIntentIsDurable`,
`TestIntegrationDeploymentPreSaveCleanupReconstruction`,
`TestIntegrationDeploymentRetryableFailureRetainsCandidates`,
`TestIntegrationLiveCleanupIntentFailureRetainsOwner`,
`TestIntegrationMaintenanceCleanupProtectsOwnerCandidate`,
`TestIntegrationWorkerShutdownRetainsCandidates`,
`TestIntegrationCleanupEvidenceReadFailureRetainsOwner`,
`TestIntegrationDeploymentTerminalRetryWithRetainedCandidates`,
`TestIntegrationDeploymentRenewalRepairsCrashGapPredecessor`,
`TestIntegrationWaitingOwnerReloadsAfterStaleSettlement`,
`TestIntegrationPreviousCleanupPinnedRevisionNode`,
`TestIntegrationAmbiguousDeploymentLookupFailureRetainsOwner`,
`TestIntegrationContainerCleanupDeferredRowsDoNotStarveQueue`,
`TestIntegrationScaleAdmissionContentionIsNonblocking`,
`TestIntegrationDeployWorkerLockContentionSnoozes`,
`TestIntegrationMigration026FailsClosedOnInflightDeployment`,
`TestIntegrationMigration026LeavesNoLegacyOwners`). They were run on 2026-11-08
against a private disposable PostgreSQL 18.6 cluster extracted under
`/tmp/opencode`, with loopback trust authentication and no shared DSN, Docker or
host mutation. The migration test builds schema 25 with an in-flight deployment
and verifies 026 refuses to apply; a second safe schema builds a terminal
deployment and verifies 026 creates an empty owner table without backfilling an
owner or changing serving history. There is no legacy restore-step hash
compatibility or historical owner inference: old in-flight work must finish or
be cancelled before upgrade. **No real
host interruption, Caddy race or public-route failure has been recorded**; host
qualification remains manual.

### Disk retention (D30, alpha.1)

Run the mutating steps only on the authorized disposable test app and database;
backdate fixture rows only there. Never prune the production app to satisfy these
checks.

| Check | Pass means |
| --- | --- |
| Retention policy | `eregion retention show --json` returns `scope: server` with `keep_releases 10`, `keep_newer_than_days 14`, `artifact_grace_days 14`, `buildpack_cache_max_bytes 10737418240` and `buildpack_cache_max_percent 80`, every source `server`. `eregion retention set --keep-releases 5 --keep-days 10 --artifact-grace-days 10 --cache-cap-bytes 1073741824 --cache-cap-percent 75 --json` stores that server default and `show` returns it. `eregion retention set --app <app> --env <env> --keep-releases 2 --keep-days 3 --artifact-grace-days 3 --json` commits a new environment revision (accepted operation ID printed); `retention show --app <app> --env <env> --json` marks only the three release fields `environment`, both cache fields stay `server`, and `env status` names the new revision. `--clear-override` restores inheritance; a flagless `set` with `--json` or no TTY refuses with `input_required`. Host status 2026-11-03: pass on main (236–252, 274). |
| Prune dry run | `eregion prune --dry-run --json` returns `dry_run: true`, one `items` entry per planned kind with `reference` and `bytes` where known, and `planned_bytes`; warnings name unreadable observations. No operation is accepted (`operations list` gains none), and `release list`, Docker images/containers and registry content are unchanged (compare counts and `docker system df` before and after). A second dry run prints the same plan. Host status 2026-11-03: pass on main (253–267); only the dangling images carried bytes, release rows and image items did not. Late run on `edge-690baaf`: the real run's 16 release items exactly matched this plan, and a retry dry run lists the four image and one dangling item left by defect 6 (311). 2026-11-04 on `edge-786fdcf`: **pass** — 6 items: four `image` items with their verified tag lists (2, 2, 4 and 3 tags), one `image_skipped` "used by container pedantic_jackson", and the `registry_skipped` diagnostic; `planned_bytes` 0; the real run removed all four images with no errors (338–342, 344). Candidate `edge-0a1a492`: **pass** — only the `image_skipped` remains; `registry_skipped` is gone, `items` is 1 and `planned_bytes` 0 (378, 379). |
| Pruned rollback | After a real `prune --confirm <server>` removes an old release, `release list` shows the row with `pruned_at` and its ID still present. `rollback <app> <env> --to <pruned-id> --dry-run --json` and `rollback … --confirm <app>/<env>` both refuse with `precondition_failed` naming the release and prune time; `deploy --release <pruned-id>` refuses the same way. Rollback to the kept target still succeeds and serves. Host status late 2026-11-03 (`edge-690baaf`): **partial pass** — `rollback --to rel_0170b1be… --dry-run` and the real `--confirm githello/production` both refused exit 5 `precondition_failed`, naming the release and its `2026-11-04T00:06:41Z` prune time (305, 306); rollback to the kept target was not run. |
| Registry GC | Record `du -sb /var/lib/eregion/registry` and the registry service state before a prune that removes at least one `registry_artifact`. The operation events show the manifest removal, then `Registry garbage collection completed`; `du` falls by at least the removed manifests/layers, `systemctl is-active eregion-registry` is active and `curl -fsS 127.0.0.1:5000/v2/` returns 200. A stale `eregion-registry-gc` container or a running `garbage-collect` process makes the step fail closed with the registry left stopped and its recovery instruction; a collection or restart failure is never suppressed, and `--delete-untagged` is never used. A deploy after GC builds, verifies publicly and serves. Host status late 2026-11-03 (`edge-690baaf`): **not exercised** — the plan had no `registry_artifact` items, and the real run failed on local images before any GC; registry `du` stayed 312,003,409 and no garbage-collection event appeared. The polkit path itself was verified separately: `sudo -u eregion systemctl restart eregion-registry.service` succeeded, the registry returned active with `/v2/` 200 (312); this was not a retention GC. 2026-11-04 on `edge-786fdcf`: still **not exercised** — the dry run's `registry_skipped` names retained failed release `rel_53a22cfea2e1f3a2fa9dd523ac4c5510` (no `image_id`, tag `eregion/githello-production:d0f76b56af71`, no local image); the planner therefore skips repository `eregion/githello-production` fail-safe. The registry catalog contains only `andurel`, whose 10 releases are all retained, so there was no eligible registry artifact. No GC and no deploy after GC (339, 345). Candidate `edge-0a1a492`: the imageless-release fix removed the `registry_skipped` diagnostic; the dry run is only the `image_skipped`. The registry still held only retained andurel artifacts, so GC was still not exercised in that candidate run (378, 379). Fresh-server `edge-f447aa3a0d30` 2026-11-05: **pass** — the earlier prune `op_14dacc9e25d8d900752bfd3b0f336bb9` removed a registry manifest and ran GC; the after-GC deploy `op_141d2d0b97cf9da670d5a631c97418d1` then built, passed public verification and served (584). The cache-qualification prune `op_2786073ca510236984f6267b73662cc1` removed registry manifest `img@sha256:67b46713…` (2,408,739 bytes); events show 197 blobs marked, 3 blobs deleted and `Registry garbage collection completed`; the registry stayed active and `/v2/` returned 200 (617, 630). |
| 30-release retention | On the disposable app run `eregion retention set --app <app> --env <env> --keep-releases 5 --keep-days 1 --json`, deploy 30 distinct releases, then use an authorized SQL update on the disposable control database to backdate all 30 `created_at` values to older than one day and run `prune --confirm <server>`. Exactly the newest five releases remain unpruned (serving release #30 and rollback target #29 are in that set); 25 release items mark rows with `pruned_at` while all 30 release rows remain, and separate `image`/registry items remove their images and manifests. A following `prune --dry-run` lists no release items. Git builds stay local and never reach the registry, so to cover **Registry GC** run this row (or a second app) with releases published by `eregion image push`; only those produce `registry_artifact` items, GC and the deploy-after-GC check. **2026-11-06: pass** on `edge-527592419f06` with 30 distinct genuine deployments and synthetic-aged image fixtures, temporary environment artifact grace one day; 25 marks, images and matching manifests in one successful prune with all 30 rows retained, five newest kept, follow-up plan empty. Details and separate GC measurement limitations in the run record below. |
| host-registry-v2 upgrade | On a node whose registry predates `host-registry-v2`, `plan` lists `host-registry-v2` once. `apply` writes `storage.delete.enabled: true` in `/etc/eregion/registry/config.yml`, restarts `eregion-registry.service` with `REGISTRY_STORAGE_DELETE_ENABLED=true`, and a second `plan`/`apply` reports the step skipped. A manifest delete no longer returns 405, and a deploy after the upgrade serves. Host status 2026-11-03: pass on main (230–233, 270–273); the manifest-delete/deploy-after-upgrade variants were not separately run. |
| Doctor retention | `eregion doctor --json` includes `host.retention`. With no run yet on a fresh install it is `pass` with "no retention run yet"; the startup catch-up or next daily run records one. After a run it names the finish time, status, removed items and reclaimed/planned bytes. A failed last run, a running run older than two days, or a last success older than two days with the worker up is `warn` with a fix. The main-node summary must name the row and have no unexpected warn/fail check; do not assert a fixed total row count. Host status 2026-11-04 (`edge-786fdcf`): **pass (20/0/0)** — the latest run succeeded at 2026-11-04T01:11:20Z, removed 4 items and recorded no errors; the row detail reads "4 item(s) removed, 0 B reclaimed" (343, 347, 352, 353). A transient control-DB read failure produced a 19/1 warn in 2 of 5 repeated doctor runs; every successful read was 20/0/0. Candidate `edge-0a1a492`: **pass (20/0/0) in all three runs and after the restart**; the latest daily run succeeded at 01:47:57Z with 0 items removed, and no transient read warning occurred (380–383, 391, 392). |
| Doctor Buildpacks cache | `host.build_cache` reads the latest durable cache measurement from `retention_runs`; it never walks Docker volumes or the filesystem in `doctor`. With no measurement, or one older than 48 hours, it is `pass` with unknown (a fresh host stays healthy), never a zero-byte pass. After a retention run records a measurement under the cap it is `pass` and names used and cap bytes. Set the server cap below the measured cache within the allowed range (`retention set --cache-cap-bytes 1073741824 --json`) or build until the cache exceeds it; `doctor` reports `warn` naming the cap. After `prune --confirm <server>` removes least-recently-used entries the next retention run refreshes the measurement and the row returns to `pass`. Cache volumes are named `eregion-build-cache-<hex app>_<hex env>`, `eregion-launch-cache-<hex app>_<hex env>` or legacy `pack-cache-*.<build\|launch>`. Host status 2026-11-04 (`edge-786fdcf`): **pass with a known measurement** — 4.7 GiB of the 10.0 GiB cap (350, 352). Candidate `edge-0a1a492`: **pass** — 4.7 GiB of the 10.0 GiB cap (383, 392). Fresh-server `edge-f447aa3a0d30` 2026-11-05: **warn-to-pass qualified** — the 09:06 retention measurement was 1.0 GiB over the 1 GiB/75% test cap, so the row warned; the build-cache volume was held by a stopped pack container from cancelled operation `op_337e60a9fced63a8d8a5b6d8a4c1a959`. Read-only selected-field inspection proved it stale and ownerless; only that disposable container was removed, then real prune `op_2786073ca510236984f6267b73662cc1` removed the 348,112,124-byte build cache and follow-up prune `op_80f9f730b33ec9d2ec5e4b55149fb740` recorded 725.1 MiB. The row passed with a known measurement (586–647). Unknown/stale-measurement and percentage-only variants were not run. |

Host status on main (2026-11-04 candidate, `edge-0a1a492d8207`): **policy**
show/set and the environment revision pass; **prune dry run** passes with only
the `image_skipped` for `pedantic_jackson` (registry_skipped gone, items 1,
planned bytes 0); **pruned-rollback refusal** remained verified; the daily
retry fix is host-verified — a new automatic run `op_9b72a9e32c2b5b49351f3ba75edb4664`
was submitted after the one-hour backoff and succeeded with 0 items; the manual
prune was not counted, as the submitter only sees its own daily-key operations.
**doctor retention and Buildpacks cache both pass at 20/0/0 in all three runs**,
with a known 4.7 GiB cache. A restart of the serving revision succeeded with
1,212/1,212 HTTP 200. **Registry GC** remained unexercised because the registry
held only retained `andurel` artifacts; the **30-release retention** and
fresh-server baseline were not recorded then. Later records cover that work;
alpha.1 is operator-confirmed. Evidence is in the 2026-11-04 candidate record.

## Fresh-server session checklist (edge)

The order in which we work through the alpha baseline above during one paired
session on a freshly rebuilt VPS. The tables above define what each check must
show to pass. This list only orders them and names who acts. Edge only:
install and upgrade come from the `edge` channel, and the GitHub `stable`
environment is not needed until a tag is authorized. The build under test is
`edge-f447aa3…`, which has the same code as candidate `0a1a492`. Record every
item as pass, fail, blocked or not run in a new run record. This run's logs
are under `/home/mbv/eregion-qual/2026-11-04-fresh/` (mode 0700, operator-local,
not Git artifacts), numbered from 400. Earlier runs stay under
`/home/mbv/.cache/eregion-run/`.

**You** = the operator at a human terminal, provider console or authenticator.
**Agent** = me, on the remote box `thelab`, which also hosts the control
database. Topology: you run `server init` (steps 1–3) from your **laptop**,
then the agent logs in here with the handoff bundle, so the login is a real
second machine (decision C closed) and every later step runs here. Capture each
laptop init run with `script -q -c '<command>' <log>` (or `| tee`), then copy the
logs here into the evidence directory. They must contain no secrets: stop the
capture before the handoff output is shown. Placeholders: context `fresh`, `$IP`,
`$MAIN` (server hostname), `$APP` = `git-test1.mbvlabs.com`, `$ALIAS` =
`git-test2.mbvlabs.com`, `$GUIDED` = `git-test3`, `$WRONG` = `git-test4` (an
origin that is not this server), `$IMG` = `git-test5`. App hostnames are
normally Cloudflare-proxied, which is the expected production setup. Only the
control database endpoint (connection URLs) must not be proxied. Behind the
proxy, eregion sees `dns=proxied` and readiness rests on the public check, so
a wrong origin must stay not ready because its public check fails. Never
advise turning off the proxy to make a check pass. The old `main` server (46.224.129.96) is torn down for this
rebuild. andurel and its `eregion-test*` hostnames go with it, and its
`eregion-cp-pg` database on port 5433 stays untouched until after the session.

### 0. Prerequisites (you)

- [ ] VPS rebuilt with Debian 13 or Ubuntu 26.04. Provide the IP, root SSH key
      access and provider console access.
- [x] DNS records: `$MAIN` → `$IP`; `git-test1`, `git-test2`, `git-test3` and
      `git-test5` → `$IP`; `git-test4` → a different origin. The app hostnames
      are as configured in Cloudflare and normally proxied.
- [x] A new, empty control database and role on the workstation PostgreSQL
      (`eregion-cp-pg`): database `eregion_fresh` owned by the non-superuser
      role `eregion_fresh`, reached through Funnel at
      `thelab.tailbbed89.ts.net:10000`. The DSN file is
      `/home/mbv/.config/eregion-fresh-database-url` (mode 0600, never
      printed) and carries `sslmode=verify-full&sslnegotiation=direct`, because
      Funnel terminates TLS itself. Pass the file to init with
      `--database-url-file`. Checked 2026-11-04: the Funnel certificate
      verifies and the role logs in over the tailnet to an empty database.
      psql 18 cannot test the Funnel path (it requires ALPN), so the first
      real connection through Funnel is init's database step. You copy the
      file to the laptop (`scp thelab:.config/eregion-fresh-database-url`,
      mode 0600) and delete the laptop copy after init.
- [ ] Authenticator ready. GitHub webhook admin access on `mbvlabs/eregion-hello`.
- [x] Agent: confirm the fixture commits in `/home/mbv/.cache/eregion-hello`
      (`ea1b225`, `6dc0468`, `034eb82` failing, `b895cb2` monorepo; HEAD
      `3461a87`), the git read token and the `HELLO_SECRET` files (0600), and
      Docker 29.8.2 on `thelab` for `image push`. Checked 2026-11-04. The
      stale local `main` context is left in place until step 6.

### 1. Install and the starting build

- [ ] **Install** (you, on the laptop): run edge `install.sh`. The signature is
      verified and `eregion version` shows `edge-f447aa3…`. If the laptop is a
      Mac, this also covers the macOS half (R10).
- [ ] You, on the laptop: run
      `eregion upgrade --channel edge --version edge-0a1a492d8207`. The
      downgrade warning appears. Initialize the server on this older build so
      that step 5 can upgrade the node across two edge builds.
- [ ] Agent, here: edge `install.sh` with an isolated `EREGION_INSTALL_DIR`
      verifies and installs `edge-f447aa3…` (Linux half). The CLI on `PATH`
      here stays on `edge-0a1a492d8207` until step 5.

### 2. Preflight and plan, before any server change (you on the laptop, agent guiding)

- [ ] **Preflight:** an unreachable host and a missing credential are both
      refused with the fix, and the server stays unchanged. Testing an
      unsupported OS/arch needs another host (decision A).
- [ ] **Init plan:** the plan names every change. Declining starts nothing:
      there is no admin user and no `/usr/local/bin/eregion`. `--json` without
      `--confirm fresh` refuses with `input_required`.
- [ ] **Interrupted init, `--discard`:** interrupt at the plan confirmation, then
      run `--discard`. The pending state under `pending/fresh/` is removed and the
      server is unchanged. Discard only here: a fresh init does not adopt a
      partially initialized server.

### 3. Init apply with faults (you on the laptop and console, agent probing `$IP` from here)

- [ ] **Interrupted init:** interrupt during host bootstrap before the
      database step, then `--resume`. Interrupt again after the database step,
      then `--resume`: there is one node identity and no duplicate context.
- [ ] **Lockdown revert:** kill the CLI after lockdown is armed and before it is
      confirmed. Within about 120 s public SSH reopens by itself (agent checks
      `nc $IP 22`, you watch the console). `--resume` then completes the switchover.
- [ ] **Init apply, doctor gate:** stop a checked service that is off the
      control path just before the gate. Init stops with `doctor_failed` and the
      fix commands. Fix it, then `--resume` runs only the remaining phases.
      `--skip-doctor` needs its own init (decision B).
- [ ] **Interrupted init, before handoff:** interrupt at the handoff, then
      `--resume`.
- [ ] **Handoff:** you get the plain secrets, the bundle and the passphrase.
      Init waits for "stored", the output has no API token, and
      `--secrets-out <owner-only path>` is the only way a file gets written. Copy
      the bundle here (`/tmp/opencode/fresh.bundle`, mode 0600). The
      passphrase stays in your password manager.
- [ ] **Init apply, end state:** public 22 is closed and SSH works over
      WireGuard. Root login and password auth are off and the firewall is
      active. `curl https://$MAIN` verifies without `-k`. node_exporter answers
      only over WireGuard. `doctor` reports no failing check.

### 4. Login here, operator tools and break-glass

- [ ] **Login** (you, in a terminal on this box): `eregion login` with the
      bundle as context `fresh`, entering the passphrase. It reaches the same
      server the laptop initialized, and both machines then work.
      `eregion --context fresh doctor` reports no failing check here.
- [ ] **Operator tools:** `server status`, `doctor`, an `eregion ssh` shell (agent).
- [ ] **SSH command:** piped `ssh --command` keeps stdout and the exit status.
      A sudo password refusal shows `input_required` (agent).
- [ ] **Break-glass:** you run `eregion node unlock-ssh --minutes 15` as root
      from the provider console. Public 22 opens, and SSH locks again when the
      window ends.
- [ ] You run `eregion protect init` and enrol the TOTP. `protect status` is
      configured.

### 5. Upgrades on the build under test (agent)

- [ ] **Install** row, second half: `eregion upgrade --channel edge` moves
      `0a1a492` → `f447aa3` and keeps `.prev`.
- [ ] **Version lockstep:** a node command refuses with `node_version_mismatch`.
      **Dry-run version mismatch:** `apply --dry-run --auto-upgrade-nodes`
      leaves the node build unchanged.
- [ ] `apply --auto-upgrade-nodes` upgrades the node. A second `apply` is a
      no-op and `doctor` reports no failing check.
- [ ] **Signed node upgrade:** an unsigned or mismatching upload and a dev CLI
      are refused. A failed manifest download refuses with no checksum-only
      fallback, the same method as evidence 193.

### 6. Login variants and contexts (you type passphrases, agent checks)

- [ ] **Login, wrong host key:** a login with a tampered pin fails closed.
- [ ] **Atomic login:** an unreachable import leaves the context, identity and
      known_hosts byte-identical. An existing name needs `--replace NAME`.
- [ ] **Context selection:** log in to the same bundle a second time as
      `fresh2`. With `fresh` as the current context, `--context fresh2` and
      `EREGION_CONTEXT=fresh2` target `fresh2`: check the domain/app-create DNS
      addresses and the protected handoff identity fields. Also check the
      unknown-name hint.
- [ ] **Context remove:** run `context remove fresh2` and remove the stale
      `main` for the torn-down server. Each context, its identity and its
      known_hosts entries are gone, and `fresh` still works here and on the
      laptop.

### 7. Application workflow on `fresh`

- [ ] **Deployability:** an environment missing source, domain or DNS (`$WRONG`)
      reports exactly what is missing and refuses (agent).
- [ ] **App create**, guided (you at the terminal, or the agent in a PTY): a
      private repo, `web` + `worker`, config, a secret and `$GUIDED`. It ends at a
      serving card with the URL. Ctrl-C detaches and names `operations follow`.
      A guided run with the failing commit ends at a failed operation.
- [ ] **App create**, flag-only and JSON (agent): the flag-only form produces the
      same revision, and JSON returns both IDs without waiting. **Create replay**
      with the same key returns the same IDs.
- [ ] **Domain and DNS / Alias check path:** `$APP` and `$ALIAS` become ready
      (`$ALIAS` with `--check-path /users/sign-in`), and the certificate
      verifies without `-k`. `$WRONG`, proxied to a wrong origin, never becomes
      ready: it shows `dns=proxied` and its public check fails.
- [ ] **Second deploy** under continuous curl on `$APP` and `$ALIAS` with zero
      failures. **Failed candidate.** **Rollback** with pinned secret revisions.
- [ ] **Environment changes**, **Monorepo build**, **Moved-branch retry**,
      **Build lifecycle**, **Processes**, **Release command scope**.
- [ ] **Command resolution:** a release command and a worker/service command
      with the same bare executable name run when the Buildpacks binary is in a
      launch-layer `bin` directory and the plain Docker binary is in the image
      working directory. The resolver mount is read-only at the fixed path, the
      plain image execs it directly, and the Buildpacks launcher execs it after
      setting the launch environment. A plain-image `PATH` containing
      `/usr/bin:.` or `bin:/usr/bin` resolves in the same order as the runtime
      would, including the relative entry, then cwd fallback; target argv and
      environment are unchanged. `./name` and absolute paths are unchanged and
      receive no mount. This needs real images and was not run by the
      fake-Docker source regressions.
- [ ] **Leaked-container retry**, which also host-tests defect 2 (cleanup is no
      longer "completed" after a failed removal, and the real error is
      redacted). **Container repair.**
- [ ] **JSON contracts**, **Detach and retry**, **Restart**, **Route failure**,
      **Status and logs**.
- [ ] **Release command recovery** and the stale/competing **Container repair**
      variants: best effort, using a timed worker kill. There is no real-host
      fault hook. Record not run if neither reproduces (decision D).

### 8. CI and push to deploy

- [ ] **Token factor:** you run `token create` with TOTP. `--yes`, `--json` and
      stdin cannot mint a token, and `token list` shows metadata only. Create
      tokens for: valid, short-expiry, to-revoke and other-environment.
- [ ] **CI** (agent): a valid token deploys a ref. Expired, revoked and
      wrong-environment tokens fail. Every other `/v1` path is unreachable publicly.
- [ ] **Push to deploy:** you run `env update --auto-deploy` and add the GitHub
      webhook. A push deploys, a redelivery creates no second deploy, and
      another branch or a bad signature does nothing. After
      `--rotate-webhook-secret` the old secret fails.

### 9. Disk retention (agent; SQL backdating authorized only on the new database)

- [ ] **Retention policy** show/set, the server default and the environment
      override.
- [ ] Create app `img` on `$IMG` from `eregion image push`. Push and deploy 30
      distinct images (**30-release retention**: keep 5, keep-days 1), backdate
      `created_at`, run `prune --dry-run`, then `prune --confirm fresh`.
- [ ] **Prune dry run** is unchanged across two runs. **Pruned rollback** is
      refused, and a rollback to the kept target serves.
- [ ] **Registry GC:** `du` drops, the registry is active and `/v2/` returns 200.
      A deploy after GC serves.
- [ ] **Doctor retention** and **Doctor Buildpacks cache** rows, including the
      cache-cap `warn` and its return to `pass`.

### 10. Factor-gated actions and removal (last)

- [ ] **Secrets export:** you run `secrets export` and `secret export`.
      `--yes`, `--json` and stdin cannot bypass it.
- [ ] **Factor-gated removal / Removal:** `domain remove`, `env delete` and
      `app delete`, including the last environment. Each needs confirmation and
      TOTP. A direct HTTP delete refuses. Other apps keep serving and the
      operation history remains.
- [ ] Agent: a final `doctor` and `server status`, the run record, and updates
      to the baseline host status and MASTER §8. Stop
      `eregion-alpha1-test-pg`.

### Findings from the 2026-11-04 fresh-server session (`edge-f447aa3a0d30`)

Defects (fix before the next edge build):

- **Defect 7, sudo-rs refusal not detected.** Ubuntu 26.04 ships sudo-rs
  0.2.13. Its "A terminal is required to authenticate" / "interactive
  authentication is required" did not map to `input_required`. Fixed in the
  working tree with case-insensitive matching plus the sudo-rs line, and
  regression tests. Not yet published.
- **Defect 8, follow summary names a failed release as serving.** When a
  first deploy fails after its route switch (public check failed, nothing
  served before), the final line reads `serving release <failed id>`.
  `env status` correctly shows none.
- **Defect 9, token create/revoke restarts the API.** Both write the token
  store and run `systemctl restart eregion-api.service`, because the API loads
  token hashes only at startup (`internal/cli/noderpc.go`,
  `internal/api/auth.go`). The listener was down for up to ~7 s, and the
  public CI and webhook endpoints returned 502 (observed on the first request
  after `token revoke`). Fix in progress: reload the token store in place,
  with no restart.
- **Defect 10, the push webhook answers too slowly for GitHub, is fixed in the
  working tree.** The handler now resolves the environment, revision and exact
  secret revision in one joined read with the secret read fresh per delivery,
  verifies the signature over the raw body, and commits a sanitized admission
  plus its River webhook job in one PostgreSQL transaction; it replies 202 only
  after that commit. One absolute 8-second budget covers the resolver, body
  read and admission, so a slow client or database returns a retryable 503
  rather than an optimistic 202. A worker then submits `platform.deploy` and
  links the operation to the delivery in another single transaction; a
  redelivery returns the original operation, and a deleted or rebound
  environment fails the accepted intent terminally. Offline handler, validation
  and job-disposition regressions pass; the 16 explicit-DSN PostgreSQL cases
  **pass** against PostgreSQL 17.11 on 2026-11-05 (820). **Not host-qualified:**
  the ping and push response times, a successful first delivery, a redelivery
  that does not deploy twice, a bad-signature delivery and rotation still need
  the real server and GitHub.
- **Defect 11, env-delete preview claims the last environment.** Found
  2026-11-05 on the same fresh build. `env delete guided failing --dry-run`
  printed "This is the app's last environment; deleting it also deletes the
  application record." although `guided` has `failing` and `production`. The
  preview count is target-scoped (`deletionCounts` filters to the named
  environment), so the warning fires for any single named environment. The
  actual deletion is safe: `deleteEmptyApplicationTx` re-counts the app's
  remaining environments after deleting the target and returns when any
  remain. Verified 2026-11-05: `env delete guided failing` succeeded and
  `guided/production` was preserved (680-689). Preview-only UX defect. **Fixed
  in the working tree:** the preview keeps target-scoped row counts but uses the
  application-wide environment count for the last-environment warning, with
  unit and explicit-DSN integration regressions; the DB-backed case **passes**
  against PostgreSQL 17.11 on 2026-11-05 (820). **Not host-qualified at that
  point:** the post-fix `env delete --dry-run` still needed a real terminal
  check.

Usability (not blocking a pass; not part of the alpha.1 qualification):

- **Handing secrets from a human to an agent is too much work.** Tokens are
  shown once on screen and the agent needs them in a protected file. The
  manual copy/`cat >`/permissions dance is error-prone; a file created with
  `vi` came out 0644. `token create` should be able to write the token
  straight to a protected 0600 file (for example `--out FILE`, refusing
  existing or loose-permission paths) while still requiring the human factor.
- **Guided secret prompts take only pasted values.** "Repository read token"
  and "Secret value for X" are masked value fields, and the flag form takes
  files. Pasting a 93-char PAT into an invisible field failed twice, and the
  error appeared only at the end. Offer a file path option, or validate the
  credential right after entry.
- **The guided create never asks for a release command,** although the
  flag form has `--release-command`.
- The `token create` "Use it as" hint leaves `<main-hostname>` unfilled and
  wraps the URL. It should print the context's hostname and the real
  endpoint.
- `protect status` still suggests `eregion protect init` once configured.
- Plain `process list` shows applied replicas while JSON shows desired.
- `secret list` takes `--app/--environment` flags where most commands take
  positionals.
- A failed first-deploy card still prints `public url:` although nothing
  serves.

### Historical open decisions

These were open during the 2026-11-04 session. Alpha.1 is now operator-confirmed
complete, so they are not current blockers; no outcome is invented here.

- **A.** Unsupported OS/arch preflight: use a second throwaway host or a VM
  with another OS, or record not run.
- **B.** `--skip-doctor`: rebuild the VPS for a second init or record not run.
  The alternative, `--resume --skip-doctor` after `doctor_failed`, would replace
  the resume-after-fix check.
- **C.** Closed: init runs from the laptop and the login runs on `thelab`.
- **D.** Whether the recovery and repair race variants stayed alpha.1 gates if a
  timed kill could not reproduce them on a real host. Closed by the operator's
  alpha.1 confirmation; no individual result is reconstructed.

## Resume testing — saved session state, updated 2026-11-03

Start a fresh session by reading [MASTER](MASTER.md), then
[IMPLEMENTATION](IMPLEMENTATION.md), then this section and the run records below.
MASTER row 16 is complete by operator confirmation; this is retained as
historical session state, not a second work list or an open checklist.
**Alpha.1 is qualified.** Facts below are the last recorded observations, not a
fresh host check.

### Target and build snapshot

| Item | Last recorded state |
| --- | --- |
| Repository | `/home/mbv/work/eregion`, branch `master`; tested revisions `a77d3bf` (worker rescue), `13fdb25` (control-DB failure classification plus `host-journal-access-v1`), `1e541a7` (retention CLI, migrations 017–019 and `host-registry-v2`), `690baaf` (worker-readable registry, polkit GC, cache sizing and package convergence), `786fdcf` (multi-tag image removal and stopped-container protection) and `0a1a492` (daily retry and imageless-release resolution), all with green CI and signed edge publication. `42a0541` and earlier builds are context, not qualification. |
| Tested CLI and main | `edge-0a1a492d8207`, source `0a1a492`; the 2026-11-04 candidate run upgraded `edge-786fdcfda4a8` -> `edge-0a1a492d8207`. Earlier passes ran on `edge-786fdcfda4a8`, `edge-690baafe8c91`, `edge-1e541a7fad3e`, `edge-13fdb2518757`, `edge-a77d3bfa4fe6`, `edge-42a0541c09d4` and `edge-ef91d70f6215`. Do not confuse a newer edge publication with an installed or qualified build. |
| Existing server | Context/node `main`, host `exploration`, Ubuntu 26.04 amd64; public IP `46.224.129.96`; main hostname `db2.mbvlabs.com`; operator `admin@10.78.0.1:22` through embedded WireGuard, endpoint `46.224.129.96:51820`. |
| Health | Doctor **20 pass / 0 warn / 0 fail** at `edge-0a1a492d8207` in all three candidate runs and after the restart: `host.retention` passes with the 0-item daily run; `host.build_cache` passes with a known 4.7 GiB of the 10.0 GiB cap; second apply 0 applied / 20 skipped; schema 20/20, River 7/7; Docker 29.1.3/containerd 2.2.2 were not upgraded or restarted; Caddy 2.11.4 was not restarted; `net.ipv4.tcp_migrate_req=1` from `host-ingress-sysctl-v1`; admin reads the eregion journal without sudo. No transient control-DB read failure occurred in this check. |
| Known open defect | **No open retention defect observed.** The two 2026-11-04 findings are fixed and host-verified on `0a1a492`: a new numbered daily run was submitted after the one-hour backoff, and the imageless-release `registry_skipped` is gone (only the `image_skipped` for `pedantic_jackson` remains). Defect 6 is fixed and host-verified on `786fdcf`; defect 5 remains fixed and host-verified on `690baaf`; defects 3 and 4 remain fixed and host-verified. |
| External database | PostgreSQL 17 on the operator machine, container `eregion-cp-pg`, host port 5433. Main reaches it through TLS-terminated Tailscale Funnel `thelab.tailbbed89.ts.net:10000`. No DSN belongs in this document. |
| Test app | `githello/production`, `APP_VERSION=v3`; web ×2 (`eregion-hello`, port 8080) and worker ×1 (`eregion-hello worker`, no port); release command `eregion-hello release`; health `/healthz`. |
| Serving release | `rel_b7099da06361a0bfaa8ee6866e178152`, source commit `3461a87`, deployment `dep_51d61163648e39d6adf4be5cdef005d3`, built and deployed by op_181c9da0f94c37ff6784c08201d366b1 on 2026-11-03 (the defect-4 rescue retest). Rollback target: `rel_b38312bd825f8c7e66ae3fe906148795` (image `5e277f663727`), deployed by op_5e277f6637276de54c3011fc1fc52a0f. Earlier: `rel_0170b1be56a64a747d522a2831cb02d8`, redeployed by op_4dc3431876c9f4f0e5caae4cd22d695f and op_6f4a7968f5823f71cd1003ec33a81293. |
| Source desired state | Private `https://github.com/mbvlabs/eregion-hello.git`, ref `main`, context `apps/web`, builder `paketobuildpacks/builder-jammy-base`, credential name `GIT_READ_TOKEN`. |
| Primary | `https://git-test1.mbvlabs.com/`, last recorded DNS-only, returns `eregion sample: v3 src3`. |
| Alias | `https://git-test2.mbvlabs.com/users/sign-in`, Cloudflare-proxied, returns `alias ready`; binding check path `/users/sign-in`. Both bindings last observed route=applied, certificate=valid, public=ok. |
| Secret metadata | Current `HELLO_SECRET` revision 2; original v1 rollback used its pinned revision 1. `GIT_READ_TOKEN` revision 1. Values must never be printed. |

Fixture checkout: `/home/mbv/.cache/eregion-hello`. Useful source commits:

- `ea1b2254217fd440837fd01a1b7a75e3926f9c63`: original v1 root app.
- `6dc04688b87d8b7d24087d4fe6ec74fcfd027fd7`: healthy root app with source-v2
  header and alias route.
- `034eb826f82fc134ecb38c6de523f32a4751be35`: intentionally unhealthy root
  app unless `FAIL_HEALTH=0`; has **no** `apps/web` directory.
- `b895cb2f54daeaedafac44397072db893cd8ef1c`: healthy `apps/web`; fixture
  branch `main` last pointed here. Root source still has the deliberate failure.

Do not deploy old root commits with the current `apps/web` context and mistake
missing-path rejection for a health test. Read desired state before changing it.

### Safe first checks

These are inspection commands, not an instruction to upgrade or deploy:

```sh
git status --short
git log -1 --oneline
eregion version
eregion context show --name main --json
eregion --context main server status --json
eregion --context main doctor
eregion --context main env status githello production --json
eregion --context main domain list --app githello --json
eregion --context main build list githello production --limit 10 --json
curl --fail --show-error --silent https://git-test1.mbvlabs.com/
curl --fail --show-error --silent https://git-test2.mbvlabs.com/users/sign-in
```

If CLI/node versions differ, first record both. A new `upgrade` changes the local
CLI only; `apply --auto-upgrade-nodes` mutates the server and possibly its
configured database. Obtain applicable target authorization before changing
builds, and rerun affected checks on the chosen published candidate. Never
silently upgrade just to make an inspection command work.

### Evidence already available

Detailed pass/fail records and operation IDs are below. Logs and sanitized JSON
are under `/home/mbv/.cache/eregion-run/` (operator-local, not Git artifacts):

| Evidence | Files |
| --- | --- |
| Pinned setup upgrade/convergence | `24-setup-apply.log` |
| Second deploy and failed-health isolation | `27-*` through `32-*` |
| Rollback secret metadata, scale and restart | `33-*` through `37-*` |
| Zero-failure curl continuity on primary and proxied alias | `38-continuous-v3.json`, `39-*`, `40-*` |
| Missing context, moved-branch retry, successful monorepo deploy | `41-*` through `46-*` |
| JSON/refusal/SSH contracts and container repair | `47-*` through `60-*` |
| Create replay and original build-stop failure | `61-*` through `69-*` |
| Build-stop regression checks, published upgrade and passing host retest | `70-*` through `78-*` |
| Alias removal, injected route-write failure and automatic recovery | `79-*` through `87-*`, `92-domain-recovery-final.json` |
| Latest inventory, NDJSON logs and unsigned-upload rejection | `88-*` through `91-*` |
| Release-command change/failure/refusal, cleanup obstruction, Caddy reload resets and `tcp_migrate_req`, rejected reload and recovery | `93-*` through `123-*` |
| TCP migration dense continuity | `124-*` through `127-*` |
| Release-changing dense switches, symlink escape, replay, failed-command rollback, failed-candidate cleanup, domain churn with worker kills, dev dry-run, detach/replay and cancel | `128-*` through `154-*` |
| CLI upgrade between two edge builds, signed node upgrades, CI endpoint path, worker-kill rescue, failed-manifest refusal, journal access | `155-*` through `207-*` (monitors `170`, `180`, `195`) |
| CLI upgrade to `1e541a7`, `host-registry-v2` and migrations 017–019, defect-4 rescue retest, retention policy/dry run and the retention-inventory defect | `208-*` through `277-*` (monitors `228`, `240`; summary `277`) |
| Late run: `690baaf` CLI/host steps, real prune partial failure (defect 6), pruned-rollback refusal, polkit restart | `278-*` through `314-*` (monitor `287`; prune `295`/`297`; summary `313`; final curls `314`) |
| 2026-11-04 run: `786fdcf` upgrade, migration 020, daily-key finding, manual prune success (defect 6 fixed), registry-skip evidence, doctor 20/0/0 | `315-*` through `358-*` (monitor `326`; prune follow `342`; registry skip `345`; doctor loop `353`; final curls `354`; summary `355`; final state `356`–`358`) |
| Candidate check: `0a1a492` upgrade, daily retry success, imageless-release skip gone, doctor 20/0/0 three times, restart | `360-*` through `393-*` (monitor `369`; daily follow `377`; dry run `378`; doctor `380`–`383`; restart follow `386`; final state `389`–`392`; summary `393`) |

The Python alias continuity monitor returned 403 and is **not** passing evidence;
use the later curl measurements. Saved binding/operation acceptance is not
completion. The route-write test failed its operation as expected; periodic
reconciliation subsequently recovered the routes without rewriting that failure.
It does **not** qualify rejection of a Caddy reload.

### Alpha.1 acceptance evidence and historical gaps

Alpha.1 is qualified by the operator confirmation above. The table retains the
coverage observed in this checkout; a gap listed here is a historical evidence
gap, not open alpha.1 work, and no individual result is reconstructed. The
baseline tables above remain authoritative for what each check means.

| Baseline area | Recorded coverage and historical gaps |
| --- | --- |
| Guided create | **Not run at that point:** guided `app create`/`env create` success and failure, ending at a serving URL or a failed operation; guided vs flag-only revision equivalence. Flag-only create and app-create replay passed. At that point it needed DNS A records git-test3/git-test4 → 46.224.129.96 and an agent in a PTY. |
| CI endpoint | **Not run at that point:** authenticated CI deploy; expired, revoked and wrong-environment tokens denied; nothing else of `/v1` reachable publicly. At that point it needed defect 1 published and applied, plus a token created at a human terminal. |
| Push to deploy | **Partial pass at that point (2026-11-05, `edge-527592419f06`):** a real empty push to `mbvlabs/eregion-hello@main` produced delivery `12b3bc42-d964-11f1-8ff4-80f7dc34db03` 202/OK and `op_232f45c1cd645304e92df57409fb5b83` succeeded, serving `rel_d231a28c17ce1aa345e6fa3cdfd7ae03` at `59017d6`; both githello domains and all four monitored URLs returned 200. The old fixture read credential was replaced with the authenticated `gh` token (`GIT_READ_TOKEN` revision 3) and the clone succeeded; the earlier rejection's root cause was not established. On the same build, redelivery returned the original operation without a second release; an invalid signature returned 401; and a real push to existing branch `qual-rc-never` was ignored. **Not run at that point:** old-secret rejection after rotation. |
| Factor-gated ops | **Partial pass (2026-11-05 fresh `edge-f447aa3a0d30`):** positive `secrets export` is operator-reported: the human completed the authenticator factor and the `Done`/`stored` gate, and the agent verified only that the `--secrets-out` file is 0600 in a 0700 parent (648). Positive `app delete guidedflags2` was run by the human with a fresh TOTP (`op_44ae1a2d25c010f9e42ec16b4472c6f2`, succeeded in 5.7 s); the agent verified the app, environment, release, deployment, build, domain, secret-namespace and revision rows are gone, durable operation/event history remains, and githello, guided and img still return 200 (660-669). Positive `env delete guided failing` was then run by the human with a fresh factor (`op_62837d11f91ad17e7a28a87846a5b594`, succeeded, 11 events); the agent verified only the failing environment's rows and secrets are gone, `guided` and its `production` environment still serve git-test3, all four monitored hostnames return 200, and operation history remains (680-689). `secret export` was not run; a read-only `secret list` confirms active `HELLO_SECRET` revision 3 in `guided/production` for the next human export (649). Positive last-environment `env delete guidedflags production` also passed (`op_844aaba1c862a0450452be5bd40ac624`, 8 events); the `guidedflags` app and all its rows are gone, history remains, and the other apps still return 200 (690-699). Still **not run at that point:** positive `token create` and positive `secret export`; container/network inventory was not independently inspected because the agent has no Docker socket permission without sudo, so cleanup rests on operation step results and zero platform rows. JSON/no-TTY refusals and deletion preview passed. The human authenticator was needed then. |
| Login and contexts | **Partial pass (2026-11-05 fresh `edge-f447aa3a0d30`):** the human's isolated `login --file main-handoff.md --passphrase-file <mktemp 0600>` on this box succeeded under `XDG_CONFIG_HOME=/home/mbv/eregion-qual/private-human-checks/login-profile`; the passphrase was extracted from the handoff inside the operator session and the temporary file was trap-removed. Read-only `context list`, `context show --name main` and `server status` under that profile pass (720, 721, 726; 722 is the concurrent-attempt failure noted below), and the normal `~/.config/eregion` profile still lists its separate `main` and passes `server status` (723–725, 728). Both profiles use the same operator WireGuard peer key from the same bundle, so a second live tunnel can take the node's single peer endpoint and a command may fail with "the WireGuard tunnel was lost"; this is local tunnel contention, not a context/store defect. The operator reports interactive login passed; the cause of earlier pasted-passphrase failures was not established. **Context remove: pass for the isolated disposable profile (operator-run).** The isolated store is now empty and its `servers/main` is gone, while the normal `~/.config/eregion` store and identity are unchanged and `server status` succeeds (730–739). The pasted command showed a blank line, but the unchanged normal store proves the executed removal was scoped to the isolated XDG profile, not a bare default-profile removal. **Wrong host key and atomic unreachable import now pass (agent-run, 2026-11-05, evidence 750–759):** the wrong-pin login failed at SSH host-key verification before authentication, and the network-isolated login failed with no WireGuard handshake; both left the copied context, identity and known_hosts byte-identical, and the normal profile still passes `server status`. **Exact conflict, successful `--replace main` and context selection now pass (agent-run, 2026-11-05, evidence 760–767):** the conflict exits 5 with the exact `--replace main` hint and leaves the profile byte-identical; replacement proves readiness before swapping the identity and keeps the imported files inside the disposable profile; `--context main` and `EREGION_CONTEXT=spare` target the selected context, and JSON/noTTY refuses without prompting. **True second-machine login: pass (operator-reported, 2026-11-05, evidence 768–770).** On the separate laptop, the handoff copied from `thelab` imported into a disposable profile with `login --file … --passphrase-file … --json`, returning context `main` on `mbv@10.78.0.1:22 over WireGuard`; the same scoped `server status --context main --json` returned CLI/node `edge-f447aa3a0d30`, node commit `f447aa3a0d30`, role `main`, schema 20/20, River 7/7 and all five services active. The earlier accidental `thelab` localhost attempt is not counted, and cleanup was operator command-based rather than independently verified. The normal `main` context and identity are intact; the isolated contexts are removed. |
| Upgrades | **Partial pass:** signed upgrade on main, unsigned/dev rejection and the dev-CLI dry-run version mismatch passed (`152-*`). `upgrade` between two edge builds passed on 2026-11-03 (`42a0541` -> `a77d3bf` -> `13fdb25`, evening `13fdb25` -> `1e541a7`, late `1e541a7` -> `690baaf`; previous-binary copy kept); a failed manifest download during a node upgrade refused with `unavailable` and no checksum-only fallback (`193-*`). **Not run at that point:** `install.sh` acceptance on Linux/macOS for these builds. |
| macOS | **Not run (no Mac available, R10):** `install.sh` on macOS and an installed CLI against the server. CI's macOS CLI job and the darwin/arm64 cross-build are not host acceptance. Required before v1.0.0 final; not an alpha.1 gate. |
| Disk retention (alpha.1 gate, R9) | **Partial pass (2026-11-04 candidate, `edge-0a1a492d8207`):** policy, dry run, serving/rollback protection, `host-registry-v2`, `host-directories-v2`, `host-registry-gc-polkit-v1`, worker registry read, polkit restart, pruned-rollback refusal, doctor retention and a known 4.7 GiB cache all pass. The real prune removed four multi-tagged images; the 1.3 GB dangling image is `image_skipped` "used by container"; the daily retry submitted a new numbered run after the one-hour backoff and it succeeded with 0 items; `doctor` is 20/0/0 in three runs; a serving-revision restart succeeded with 1,212/1,212 monitor responses. **In that candidate run, registry GC remained not exercised:** the imageless-release fix removed `registry_skipped`, but the registry still held only fully retained `andurel` artifacts. Deploy after GC, the 30-release row and the fresh-server baseline were not run then. **Fresh-server 2026-11-05 (`edge-f447aa3a0d30`):** registry GC and deploy-after-GC are now exercised (`op_14dacc…`, `op_141d2d…`), and the Buildpacks-cache warn-to-pass is qualified (`op_2786073…`: the 1.0 GiB measurement over the 1 GiB test cap warned; after removing a proven stale pack container, prune removed the 348,112,124-byte cache and the row passed at 725.1 MiB). The overnight 25-row prune plus aged fixtures partially cover the 30-release row; no single fresh-server baseline was recorded in this checkout, and alpha.1 qualification rests on the operator confirmation above. The manual prune is not counted by the daily submitter; docs/IMPLEMENTATION's “one successful run per day” is scoped to automatic daily-key operations (two runs succeeded today: manual at 01:11 and automatic at 01:48). |
| Fresh host | **Not run at that point:** preflight and plan variants, init apply with the pinned Caddy, doctor gate / `--resume` / `--skip-doctor`, interrupted init and `--discard`, lockdown revert, handoff and break-glass. At that point it needed an authorized fresh Debian 13 or Ubuntu 26.04 server and provider console access. |
| Release-command recovery | **Not run in this checkout:** lost or unresolved response recovery variants. Once-per-release skipping, a changed command running again and restart/scale refusal after a failed run passed. The scoped-fault gap is historical; alpha.1 is operator-confirmed complete (R4). |
| Repair variants | **Not run at that point:** competing or stale repair; a repair must never restore an old release when a deploy races it. Basic serving-container repair passed. At that point it needed a scoped fault. |
| Final pass | **Complete by operator confirmation:** the operator confirmed all alpha.1 testing is done, including the complete baseline and retention. This checkout does not contain a reconstructed complete-run record, and none is invented here. Release authorization remains separate. |

Defect 3 is fixed and host-verified on `a77d3bf`: a worker SIGKILL now rescues
the operation on worker start (9.0s measured, evidence 183; the earlier
observation was 30m21s, evidence 149/150). **Defect 4 is fixed and host-verified
on `1e541a7`:** the same rescue test now finds the git read credential on attempt
2, clones the private repository again and succeeds as
op_181c9da0f94c37ff6784c08201d366b1 (rel_b7099da06361a0bfaa8ee6866e178152;
evidence 240–246). The CI endpoint path (defect 1) is host-retested and passes;
the cleanup-summary fix is host-retested in a successful redeploy only, not with
a forced cleanup failure. The Caddy-reload reset fix is host-verified.

**Defect 5 is fixed and host-verified on `690baaf`:** `host-directories-v2`
converged `/var/lib/eregion/registry` to `root:eregion 0750`, `sudo -u eregion
test -r` returned true, the retention inventory succeeded, and
`host-registry-gc-polkit-v1` wrote the root-owned rule that let `eregion`
restart only `eregion-registry.service` (312). The earlier failure was
op_c90b2c9a9a1e6f0430dd13aec337ab40, attempt 8 at 2026-11-03T21:18:27Z.

**Defect 6 is fixed and host-verified on `786fdcf`:** the manual prune
`op_11f4602b7a2fd234d352779d74f7bd4a` untagged all 11 verified tags and deleted
`86d86a57796d`, `6c6a3f447152`, `1ef45b4e792c` and `871c4b7859aa` without
errors. The 1,308,545,594-byte dangling image `bd03726c07e9` is now
`image_skipped` "used by container pedantic_jackson"; the unlabeled stopped
container itself remains. The run succeeded in one attempt with 4 items
removed, and `doctor` is 20/0/0.

**Daily-key finding is fixed and host-verified on `0a1a492`:** after the
failed 00:06 daily operation aged past one hour, the candidate worker submitted
automatic `op_9b72a9e32c2b5b49351f3ba75edb4664` at 01:47:24 with the next
numbered key. It succeeded with 0 items. The manual prune `op_11f4602b…` was
not counted because the submitter queries only `maintenance` operations whose
key starts `retention-daily:<date>`. That is the intended scope: the
one-success-per-day rule covers automatic daily runs only (IMPLEMENTATION now
says so); two retention runs succeeded that day (manual at 01:11, automatic at
01:48).

**Imageless-release finding is fixed and host-verified on `0a1a492`:** the
prune dry run no longer contains `registry_skipped`; it contains only the
`image_skipped` for the container-held dangling image. No registry repository
was skipped.

**Observation — intermittent doctor read:** the candidate's three consecutive
runs and the post-restart run were all **20/0/0**, with no transient
`host.retention` read failure. The earlier 2-of-5 warning (317, 350, 351, 353)
remains attributable to the external control database through the
Tailscale/Funnel path, not retention state. The prerequisites and gaps listed
here were open at the time of these records; they are closed for alpha.1 by the
operator confirmation above and are not a current blocker list. macOS is
deferred (R10) until before v1.0.0 final. Variants without recorded evidence
remain evidence gaps, not implementation failures or passes; none is
backfilled.

### Safety and local resources

- Do not rerun `server init` on main, reset its database, edit applied migrations,
  erase journals or remove existing apps, especially `andurel`. Restrict test
  mutations to explicitly authorized fixtures and targets.
- Keep Cloudflare proxy support and normal TLS verification. Do not use `-k` or
  disable the proxy as a product fix. The earlier certificate problem was an
  incorrect origin IP, not an established Caddy-version defect.
- Saved operator credentials are under
  `/home/mbv/.config/eregion/servers/main/`. The protected `sudo-password` file
  can authenticate authorized OS fault-injection commands over SSH without
  displaying it. Plain `sudo -n` is insufficient for Docker/systemctl commands;
  do not log credentials or bypass the application's factor gates through root.
- Private fixture read credential file:
  `/home/mbv/.config/eregion-test-git-token`. Protected test secret files:
  `/home/mbv/.cache/eregion-hello-secrets/HELLO_SECRET` and `HELLO_SECRET_V2` in
  that directory. Reference files only; never print or copy values into docs.
- Historical unlabelled release container
  `eregion-githello-production-release-7e5cbbcbabf7` was removed. The injected
  routes-directory permissions were restored to 0755, uid 999/gid 983. No
  deliberate fault was left active.
- The control database depends on the operator machine's Tailscale/Funnel. The
  earlier outage recovered after the operator restarted Tailscale; diagnose
  connectivity without exposing the DSN rather than resetting state.
- Inspect selected tests before running them: `just check` includes tests that
  can start Docker. Do not use an ambient production DSN as a test target or
  touch `eregion-platform-test-pg` on port 55432. Prior disposable integration
  run used loopback 55439 and removed its container afterward; any new run needs
  its own authorized isolated target and explicit `EREGION_TEST_DATABASE_URL`.
- For local Go builds, use `GOTMPDIR` and `TMPDIR` set to
  `/home/mbv/.cache/eregion-gotmp` to avoid the previously full `/tmp`. Local race
  testing lacks a C compiler; race checks were performed in CI.
- This saved document does not authorize a new commit, push, branch, tag,
  release, database or host mutation. Confirm applicable authorization first.

## Current qualification boundary

Alpha.1 is qualified by explicit operator confirmation that all alpha.1 testing
is done. This checkout does not reconstruct a complete baseline or select a
qualifying build: the run records below span several published edge builds and
retain the evidence available, including partial and not-run checks. They are
history, not open alpha.1 blockers. Alpha.2 is implemented at source level and
remains unqualified. `edge-ef4a6f879f45` passed the definition-file, export,
secret-name and validation checks and applied migrations 022–023 on `main`, but
found the Docker 29.1.3 image-inspect and empty-slice removal defects. Both
fixes were retested on `edge-ba175353f00a`: resource provisioning, healthy exec
checks, secret injection, file-based resource provisioning, explicit empty
resource/secret removal, attach/detach and data retention across
destroy/re-apply all passed for own Redis and PostgreSQL fixtures, and
delete-data cleanup left no owned container, volume or network. An unhealthy
managed resource was refused at the deploy gate; after it was healthy, a real
deployment served over HTTPS at the authorized `eregion-alpha-two.mbvlabs.com`
and its response read the managed Redis (`alpha2:key=alpha2-serving`). A
wrong-port candidate failed health and left the previous release serving. The
authorized `gh`-assisted webhook created, verified-unchanged and rotated one
hook without duplicates. The user then completed the first TOTP-gated app
deletion (`op_90e6c3530ef81a575bba1542a70cda0b`); read-only checks found no
fixture rows, containers, network or volumes.

The user then authorized recreating the fixture for an end-to-end push test.
A separate `alpha2-webhook-20261106` branch was used and `main` was unchanged.
The gh-assisted hook delivered a real push with HTTP 202 and operation
`op_1380428ab35058512af820f5d7807a61`, building `rel_29c45e28…` and serving
`/marker` 200. The manual one-time secret was handled only in 0600 temporary
files and on `gh api` stdin, never printed and shredded; a second push delivered
HTTP 202 and operation `op_b4dc5f8b225d318510c10293aff9ebaf`, building
`rel_df70f2bf…` and serving `/marker` 200. The missing-`gh` fallback reported
`skipped` with manual setup available; missing admin permission was not tested
at that point (later simulated). The hook and test branch were deleted and the
desired source ref returned to
`main`. The user then completed the recreated fixture's TOTP-gated app deletion
(`op_362bea6789b9617855c43b37a093e16e`); read-only checks found no fixture
app/environment/domain/resource/release/secret rows, containers, network or
volumes. The three identified fixture image artifacts were then removed after
reference checks; the pre-existing Cloudflare DNS record remains because no safe
Cloudflare access was available. The hostname returns Cloudflare `525` because
its route/certificate is gone.

A further disposable pass on `edge-ba175353f00a` then covered file-vs-flags
desired state, distinct staging/production overrides, managed PostgreSQL app
consumption and persistence across restart/redeploy, stale-plan refusal, and a
simulated missing-admin `gh` fallback. `--confirm-domain` and an explicit
external dependency remain blocked. The user then deleted the recreated fixture
(`op_c113f640932b31df52c4a92117f3d105`); read-only checks confirmed the
default-retained PostgreSQL volume persisted untracked with its ownership
labels and no container references, and that authorized test volume was then
removed. The app, environments, domain binding, containers, network and images
are gone; only the pre-existing DNS record remains.

Alpha.2 remains unqualified pending the remaining incremental acceptance
criteria; a fresh-server baseline is not required by the updated policy.
`host.retention` remains intermittently 19/1 with healthy data, consistent with
a transient control-database read. See the 2026-11-06 alpha.2 records above and
MASTER section 8.

Beta node-local telemetry's original snapshot path is host-qualified on
`edge-fa33befb675d`; the later `host-node-exporter-v2`
textfile/systemd collector extension and `telemetry.local` doctor row are
host-qualified on `edge-f1b5440c146b`. The alpha.5 loopback exporter, the
WireGuard-only Caddy telemetry endpoint, HTTP discovery, Prometheus/Alloy config
generation, credential rotation and `telemetry.endpoint` doctor row are
host-qualified on `edge-2767500209f1` on the authorized `main` node: two live
rotations (the second proved the old password 401 and the new 200),
authenticated `/targets.json` and `/node` scrapes through the operator WireGuard
tunnel, unauthenticated 401, public port 9101 unreachable, generated
Prometheus config accepted by `promtool`, `doctor` 23/0/0 and a repeat apply
0/21 with the telemetry files and process unchanged. The 2026-11-09 record and
the alpha.5 acceptance host table above hold the evidence. WireGuard
stop/start recovery, node_exporter-stop failure, a real `.prom` fixture, the
hostname-less control-plane variant and handoff/export were not run. It still
creates no telemetry server, central Prometheus/Grafana, external backend,
metric store, PromQL, retained history or public endpoint. A future optional
separate telemetry node or Grafana is not prohibited long-term and is out of
beta/v1 scope. Staging-to-production promotion is implemented in source with
offline/pure and disposable-PostgreSQL integration coverage; **no real-host
promotion, production switch or release-command run is recorded**. Cloudflare
DNS automation is implemented in source as an explicit domain-scoped flow, with
no host or real-zone run recorded. Backups remain deferred; external PostgreSQL
is control-plane state, not a backup of managed-resource volumes. The beta and
alpha-ready acceptance definitions above are not pass records unless their own
host evidence says so.

The 2026-10-29/30 runs closed
release-changing dense switches, symlink-escape rejection, same-key replay after
the branch was deleted, rollback refusal for a release whose command never
succeeded, failed-candidate leak recording, interleaved domain changes with
worker kills, the dev-CLI dry-run mismatch and terminal detach/replay/cancel.
They found defect 3 (worker SIGKILL left an operation running for about
30 minutes); the earlier 2026-11-03 run verified its rescue fix, retested the CI
endpoint path on the host, passed the between-two-edge-builds upgrade and the
failed-manifest fail-closed check, applied `host-journal-access-v1`, and found
defect 4 (a rescued deployment attempt loses the git read credential). The
2026-11-03 evening run upgraded to `1e541a7`, applied `host-registry-v2` and
migrations 017–019, host-verified defect 4's fix, and passed the retention
policy and prune-dry-run checks; it found defect 5 (the worker cannot read the
root-only registry storage). The late run upgraded to `690baaf`, applied
`host-directories-v2` and `host-registry-gc-polkit-v1`, host-verified defect 5's
fix, and ran the automatic real prune: 16 release rows and two artifacts went,
the serving and rollback releases stayed and 2,190/2,190 curls were HTTP 200,
but four multi-tagged images and a dangling image held by an unlabeled stopped
container failed removal (defect 6). The 2026-11-04 run upgraded to `786fdcf`,
applied migration 020, worked around the already-used UTC-day key with a manual
prune, removed the four images by verified untagging, left the dangling image as
`image_skipped` "used by container", kept both apps serving through
1,200/1,200 responses, and reached doctor 20/0/0; defect 6 is fixed and
host-verified. The candidate run upgraded to `0a1a492`, confirmed no migration
race, saw the daily submitter create a new numbered run after the one-hour
backoff (it succeeded with 0 items), verified that the imageless-release
`registry_skipped` was gone, reached doctor 20/0/0 in all three runs, and
restarted the serving revision with 1,212/1,212 HTTP 200. Both 2026-11-04
findings are fixed and host-verified. The 2026-11-05 fresh-server run on
`f447aa3` exercised registry GC and deploy-after-GC (`op_14dacc…`,
`op_141d2d…`) and qualified the Buildpacks-cache warn-to-pass
(`op_2786073…`: 725.1 MiB of the 1 GiB test cap after removing a proven stale
pack container). Source
fixes have isolated regressions, build/vet and darwin/arm64 cross-build
evidence; published CI also ran race checks. These are distinct from manual
acceptance.

Failed deploy follows returned exit 5 on main, successful follows returned 0,
and the build-stop retest returned 130 with terminal cancellation and the prior
serving release preserved. Terminal Ctrl-C detach/replay and explicit cancel
passed (153/154); guided failure and the remaining output-mode variants were not
run in that record. The operator's confirmation closes alpha.1; no additional
baseline run is required for this qualification, and no result is invented here.
Release authorization remains a separate decision.

## Run record

### 2026-11-06 — complete 30-release retention run on edge-527592419f06

The fresh test node `main` and CLI ran published `edge-527592419f06`. On
disposable `retention905/production` and the already-authorized
Cloudflare-proxied `eregion-test.masterfullstackgolang.com`, 30 **genuine
successful deployments** used 30 unique Docker image identities built from a
test image with distinct content and synthetic 20-day-old `SOURCE_DATE_EPOCH`.
The node's server artifact grace stayed 14 days; an environment override of
one day was temporary. A keep-30 hold protected the release set while it was
populated. All four pre-existing URLs and the fixture health URL returned 200
after each of deployments 2–30; disk remained over 81% free.

Exactly 30 fixture release `created_at` values were backdated by two days,
preserving order; the environment policy then became keep 5 / 1 day, artifact
grace 1 day. The one global dry-run plan contained exactly 25 fixture release
marks, 25 images and 25 registry manifests. Each of the three identity sets
matched the oldest 25 fixture releases, and no current serving image was in
the plan. Prune `op_3dfe489047016719462c59743d9917d0` succeeded: events
confirm all 25 corresponding images and all 25 corresponding manifests were
removed, then registry GC completed. All 30 release rows still exist, exactly
25 show `pruned_at`, and the newest five remain. A following dry run is empty.
The environment artifact grace was restored to 14 days; server grace was
unchanged at 14. During the bounded monitor 560/560 requests to the four
existing URLs returned 200, and all four returned 200 in the final check.

Dry and real rollback to pruned `rel_198d5380bab8f379df6bcd649bbd0d45`, and
`deploy --release` to it, refused with `precondition_failed` and prune time.
Rollback `op_7a841a8af401039ee15a952ce0e70521` to kept release 29
`rel_c13cf979be5c7f6b8c7fcf57d89381c3` succeeded and served
`retention905-release-29`. After GC, deploy
`op_15f51d39353a169aad4e8a5e52c72ccd` of kept release 30 succeeded publicly
and served `retention905-release-30`. This was a retained-image redeploy, not
a new build. Registry active and `/v2/` 200; doctor 20/0/0. Registry `du` was
not measured on this build because it needs interactive sudo, and the separate
GC row's size-drop and fault variants are not established by this run.

The operator then deleted the app with factor-gated operation
`op_777bfba4daf68d989fde2083461ed592`, succeeded at 11:17:35 UTC. CLI and
read-only database checks confirm zero app, environment, release and domain
rows and retained operation history. Five fixture registry manifests from the
previously kept images are now eligible for a separate GC measurement; the
server grace remains 14 days, doctor is 20/0/0, and all four existing URLs
return 200. Nonprivileged `du` of registry storage is denied. A bounded human
terminal script `906-gc-human-measure.sh` measures first, checks planned local
image IDs against current serving image IDs and validates the permitted
test-artifact plan immediately before submission. Registry manifest protection
relies on the product planner's retained-release manifest, config-digest and
local RepoDigest matching, with fail-closed handling of unresolved identity;
the script does not equate manifest digests with image config IDs. It
then performs prune and sudo-after measurement. It records the byte counts the
operator transcribes, rejects a non-shrinking result and flags removals absent
from the preview. The worker rebuilds its plan under locks, so the preview is
not an atomic binding. The script has not yet been run. A distinct operator-local
`retention906:aftergc` image was built for the subsequent new-image deploy;
it has not been pushed or deployed. No registry policy is temporarily changed.
The earlier 30 operator-local fixture image tags were removed. No DNS change
or original-database mutation occurred. Evidence:
`/home/mbv/eregion-qual/2026-11-04-fresh/905-retention-*`. This passes the
30-release row, not the full alpha baseline; cleanup and continuation evidence
is in `906-gc-*`.

### 2026-11-06 — historical image and manifest cleanup on edge-527592419f06

After explicit authorization for historical test artifacts, the server
artifact grace was temporarily changed from 14 to 1 day; release and cache
policy fields stayed unchanged. The scheduled daily run had already succeeded
for the day. The global dry run listed five historical `img` images r26–r30
and 120 registry artifacts, 60 each in `img` and the deleted fixture's
`retentionqual1106` repository. No current serving image ID intersected the
image or manifest IDs in the plan; the planner also protects images used by
containers and retained releases. The images were built on 2026-11-04 and
were more than one day old. No timestamps or registry storage were edited.

Prune `op_879509b5b613873ed8b444b1845da3b8` succeeded, removing all five
planned images and 120 planned manifests, then reporting registry GC
completed. Its events include all 25 immutable manifest identities of the
fixture releases previously marked pruned by `op_717190293129690f4498466d388b4d64`.
The 14-day server grace was restored; a final dry run was empty, registry
active and `/v2/` 200, doctor 20/0/0, and all four existing serving URLs 200.
A bounded monitor recorded 400/400 HTTP 200 during the first part of the
prune; it did not cover the entire operation. Evidence is in
`/home/mbv/eregion-qual/2026-11-04-fresh/904-retention-*`.

This strengthens the **partial** 30-release evidence but is not a single
30-release pass under the 14-day grace: the fixture app and rows had already
been deleted before this separate shortened-grace prune. Registry `du` could
not be measured without interactive sudo, and no size-drop claim is made for
this run. The earlier fresh-server GC run has separate measured-size evidence;
this run alone did not complete the broader alpha baseline, which is now
operator-confirmed.

### 2026-11-06 — authorized retention continuation on edge-527592419f06

The operator authorized removal of the 25 shared historical `img` test image
identities, but not currently serving images. Before mutation, today's scheduled
maintenance had already succeeded; SQL and the planner showed no intersection
between the 25 candidate image IDs and any serving release, and the planner
protects images used by any existing container. With keep 5 / 1 day, a scoped
update backdated exactly the 30 `retentionqual1106/production` release rows by
two days, preserving order. The plan contained exactly 25 fixture release
items and 25 image items attributed to previously pruned `img` releases, with
the same immutable identities as fixture releases 1–25 and no other items.

Manual prune `op_717190293129690f4498466d388b4d64` succeeded: all 30 release
rows remain, exactly 25 have `pruned_at`, and releases 26–30 remain unpruned.
The 25 shared node-local images and their `qual-img:r01`–`r25` tags were
removed; the corresponding historical `img` releases were already pruned and
their artifacts cannot be used as rollbacks. A follow-up dry run was empty.
Rollback of `rel_7699784c4bf485b5a0df0c6493f17854`, both dry and real, and
`deploy --release` refused exit 5 with its prune time; rollback to kept
`rel_8c0aecf812cb8096a93d7b50db95759a` succeeded and served `img release 29`.
Four serving URLs returned 140/140 HTTP 200 during the first portion of prune,
and all four returned 200 after it. The **30-release row remains partial**:
the 25 releases' registry manifests were younger than the 14-day grace and
were not removed by this prune.

For registry GC on this build, a separate unreferenced fixture-only image was
built with a 20-day-old `SOURCE_DATE_EPOCH`, pushed to the fixture repository,
and inspected: its OCI index and child manifest were the only planned registry
items. Prune `op_603dcc8b04e3bc06421c053ca3c44bfb` removed both and reported
`Registry garbage collection completed`; the registry returned active and
`/v2/` HTTP 200. A redeploy of retained release
`rel_2c9c360c7db9958527f42c5821348a0a` after GC
(`op_d3a990bcde1c91b2ffd836c61ab79b08`) succeeded publicly and served
`img release 30`. Four existing URLs returned 120/120 HTTP 200 during the GC
monitor interval and 200 in the final check. The fixture build's local operator
image was removed. Registry storage `du` was **not measured**: the operator
could not read it without an interactive sudo prompt, so this run alone does
not satisfy the size-drop part of the registry-GC row. The earlier fresh-server
GC run on `edge-f447aa3a0d30` separately recorded the size drop and a new image
deploy after GC. Doctor is 20/0/0, and the final global prune dry run is empty.

The operator subsequently completed factor-gated app deletion
`op_09ea3f338f611ca051543bd1b9d2f28c` at 10:04:27 UTC. CLI and scoped
read-only database checks show zero fixture app, environment, release and
domain rows, while its deletion operation remains. The hostname is absent
from Caddy's active configuration; public HTTPS now returns 525. All four
existing serving URLs return 200; doctor remains 20/0/0. The post-deletion
global dry run lists five shared historical `img` images r26–r30, now eligible
because their fixture releases no longer protect them; no manual prune was
run. Delete-step source confirms duplicate ID lines in the operation output
arise from the labeled container/network list and corresponding removal
commands, not evidence of a second deletion. No DNS change or original-database
mutation occurred. Evidence: `/home/mbv/eregion-qual/2026-11-04-fresh/902-retention-*`
and `903-retention-cleanup-*`.

### 2026-11-06 — 30-release retention, held before prune

On `edge-527592419f06` / context `main`, Ubuntu amd64, the operator approved
continuation after reviewing the earlier automatic global deletion of the two
unknown dangling images. The new hostname
`eregion-test.masterfullstackgolang.com` remained Cloudflare-proxied; normal
app creation and deployment produced DNS `proxied`, route `applied`, TLS
`valid`, public `ok`/200. No DNS change was made. The distinct disposable app
`retentionqual1106/production` received **30 genuine successful deployments**
of 30 distinct existing `qual-img:r01`–`r30` images, pushed into its own
registry repository. Its initial deploy was `op_3f5b3757f2ace0bd651af85878a88af4`,
the 30th `op_335914c70cbbef7552f6fef9b43e0ff6`. The four existing serving URLs
and the fixture health URL returned 200 at each of deployments 3–30; disk
remained above 81% free.

An authorized scoped SQL update aged exactly those 30 release timestamps by
two days while preserving their order, with keep 5 / 1 day. The dry run planned
25 fixture release marks **and 25 local image items attributed to `img`**:
the reused image identities also belonged to that existing app's historical
releases, and removal would untag their `qual-img:r01`–`r25` node tags as well
as fixture tags. No unrelated image deletion was approved. Therefore **no
manual prune was submitted** and the 30-release acceptance is **not run to
completion**; registry artifacts were not in this plan (14-day grace). The
scoped timestamp change was reversed for exactly 30 rows, the fixture override
was changed to keep 30 (`op_d5e3c9ece20bc9969af5182bca8e76d1` succeeded), and
the subsequent global dry run was empty. All five URLs returned 200 afterward.

The disposable app, domain, 30 releases and image artifacts remain in place.
Keep the app and its keep-30 override until the shared-image deletion risk is
reviewed: deleting the app requires human TOTP and could expose those image
identities to the automatic global retention job. Evidence is in
`/home/mbv/eregion-qual/2026-11-04-fresh/900-retention-*`. No prune, cleanup,
registry GC, rollback-after-prune or deploy-after-GC was attempted in this run.

### 2026-11-05 — retention preflight on edge-527592419f06

**30-release retention: not run.** The required distinct disposable app has
no available verified primary domain: the four healthy test URLs are bound to
serving apps, and git-test4 is bound to deployab and has failed public/TLS
observation. Deploy requires a primary domain and public verification; DNS
changes and disrupting those apps were not authorized. The earlier 30 img rows
and six aged-image deployments remain partial historical evidence, not a pass
for this single-app acceptance run. The `eregion_fresh` role has SELECT and
UPDATE on releases, but no rows were changed. A real prune was not submitted:
the dry run listed two unrelated dangling images, not this run's fixtures.

Read-only fallback: doctor reported cache 725.1 MiB / 10.0 GiB (pass), but
`host.retention` warned that the last successful run could not be read, giving
19 pass / 1 warn / 0 fail. This resembles an earlier intermittent control-DB
read warning; no cause was established here. All four serving URLs returned
200; queued, running and cancelling operation lists were empty. No fixture,
credential copy, background task or cleanup was created. Sanitized evidence:
`/home/mbv/eregion-qual/2026-11-04-fresh/870-retention-preflight.txt`.
At this point the row still required an authorized unused, publicly verifiable
hostname and a safe fixture-only prune plan, followed by 30 actual distinct
releases and the specified scoped backdate and checks. Later records cover that
work; alpha.1 is now operator-confirmed.

### 2026-11-05 — fresh-server registry GC and cache qualification

- Published CLI/node: `edge-f447aa3a0d30`, context `main`, Ubuntu 26.04
  amd64. Evidence: `/home/mbv/eregion-qual/2026-11-04-fresh/558-*` through
  `647-*`. This does not qualify the uninstalled local fixes.
- The automatic overnight retention run marked 25 of the original 30 `img`
  release rows pruned; rows remain. Image config creation timestamps, unlike
  release timestamps, had not aged past the registry grace period.
- User authorized aged disposable fixtures. Built six distinct image fixtures
  with `SOURCE_DATE_EPOCH` (20 days old), pushed each using `image push`,
  and deployed each successfully. Backdated only those six release rows by
  two days, preserving their order. No registry blobs were edited.
- Two identical dry runs planned six release items, six local images, one
  registry artifact (aged01), and one Buildpacks cache. The newest five aged
  releases, including serving aged06 and rollback aged05, were protected.
- Confirmed prune `op_14dacc9e25d8d900752bfd3b0f336bb9` succeeded. Events
  record manifest removal and `Registry garbage collection completed`.
  Registry storage fell from 9,596,201 to 9,393,649 bytes; shared layers
  remain. Registry active, `/v2/` HTTP 200 afterward. Each of git-test1,
  git-test2, git-test3 and git-test5 returned 98/98 HTTP 200 during prune.
- Pruned aged01 rollback (dry-run and real) and `deploy --release` all
  refused with `precondition_failed`, naming the release and prune time.
  Rollback to kept aged05 succeeded and publicly served `img release aged05`.
  A new image built and pushed after GC deployed successfully as
  `op_141d2d0b97cf9da670d5a631c97418d1`, serving `img release aftergc`.
- Follow-up dry run before the after-GC deploy contained no release or
  registry-artifact items. The Buildpacks cache remained: removal was skipped
  because a container held the volume. Cache warn-to-pass qualification was
  completed later in this session (below); the holder was identified before
  removal.
- Doctor is not clean: `host-credentials-v1` drift after token creation is
  a known source-fixed, uninstalled defect. Do not apply the published build
  as a workaround: its credentials step may clear minted tokens.
- Cache warn-to-pass (same session, evidence 586–647): the 09:06 durable
  measurement was 1.0 GiB, over the server 1 GiB/75% test cap, and
  `host.build_cache` warned; disk was 84% free, so the absolute cap was the
  trigger. The dry run planned the githello build cache (348,112,124 bytes),
  but the 09:06 run had skipped it because container `practical_cori`
  (`1f79f2eac3b4…`) held the volume. Selected-field `docker inspect` (never
  Env) showed it stopped since 2026-11-04T10:50:10Z, exit 0, restart policy
  no, no app ownership labels, and command tag
  `eregion/githello-production:337e60a9fced`; that tag and build
  `bld_e4bf86d5…` identify terminal cancelled operation
  `op_337e60a9fced63a8d8a5b6d8a4c1a959` ("Build stopped by operator"). The
  pack container outlived cancellation by about 7s and was left holding the
  cache volume; it carries only builder image labels, not eregion ownership
  labels, so retention cannot identify it as disposable. **Fixed in the working
  tree:** each pack build gets a dedicated attempt-scoped network labeled with
  `io.eregion.build.*`, pack receives `--network`, cancellation sends SIGTERM
  with a five-second grace before SIGKILL, and a bounded deferred cleanup
  verifies the ownership labels, removes only `author=pack` containers on that
  network and removes the network; a foreign or ambiguous network is never
  touched. Offline fake-Docker regressions pass. **Not host-qualified:** a real
  cancelled build and the volume holder still need the authorized server.
- No queued/running/cancelling operation and no pending/running build existed;
  the container was the only volume user, so it was removed with
  `docker rm practical_cori` (613). The post-removal plan (614) was
  re-inspected: serving `rel_584e6c80…` and rollback `rel_4f87faa…` were
  protected; the only other items were aged02 `rel_4d563a61…` plus its image
  and registry artifact, which became eligible because the after-GC deploy
  added a release under the img keep-5 override. Real prune
  `op_2786073ca510236984f6267b73662cc1` succeeded: planned 4 items; events
  removed the manifest (2,408,739 bytes), the build cache (348,112,124
  bytes), the image tags, and ran `platform-retention-gc-01-v2` (197 blobs
  marked, 3 deleted, "Registry garbage collection completed"). The run
  recorded 5 removed items / 334.3 MiB reclaimed (the plan had 4 items;
  release marking and the registry GC are additional recorded effects).
  Follow-up `op_80f9f730b33ec9d2ec5e4b55149fb740` planned 0 items, confirming
  the empty plan and recording a second durable measurement.
- `doctor` after each prune: **19 pass / 0 warn / 1 fail**. `host.build_cache`
  **pass** with a known durable measurement, "Buildpacks cache uses 725.1 MiB
  of the 1.0 GiB cap"; `host.retention` pass; the only fail is
  `drift.pending` host-credentials-v1, the known source-fixed uninstalled
  defect. This is not a 20/0/0 summary.
- Four endpoints returned 324/324 HTTP 200 (81/81 each on git-test1,
  git-test2, git-test3, git-test5) through the container removal and both
  prunes; no non-200. Serving releases stayed githello `rel_e0e10c35…` and
  img `rel_584e6c80…` (aftergc); registry active, `/v2/` 200; all seven app
  containers plus registry remained running; no stale pack container or cache
  volume user remained. The removed container's anonymous `pack-layers`
  (346.4MB) and `pack-app` (0B) volumes remain orphaned; they are outside the
  retention cache prefixes and were not removed under the container-only
  authorization (646/647).
- Safe defaults restored: server retention keep 10 / 14 days / 14 days /
  10 GiB / 80%; img/production override cleared by
  `op_660d9f977526ddbdbea3da635fd48721` (no deploy queued). Final `doctor`
  remains 19 pass / 0 warn / 1 fail; `host.build_cache` pass at 725.1 MiB of
  the 10.0 GiB cap; final dry run is empty. Not exercised (not pass):
  no-measurement/unknown, >48-hour stale measurement, percentage-only trigger,
  and unreadable/truncated measurement (`cache_skipped`).

### 2026-11-05 — fresh-server handoff re-export and secret metadata

- Published CLI/node: `edge-f447aa3a0d30`, context `main`, Ubuntu 26.04
  amd64. Read-only `server status` returned CLI/node
  `edge-f447aa3a0d30`, node commit `f447aa3a0d30`, schema 20/20, River 7/7,
  and active Caddy, Docker, API, worker and WireGuard services (650).
- **`secrets export`: pass (operator-reported, not re-run by the agent).**
  The operator ran `eregion secrets export --context main --secrets-out
  /home/mbv/eregion-qual/private-human-checks/main-handoff.md` at the human
  terminal, entered the authenticator factor, and completed the `Done` /
  `stored` gate. The agent did not read, print, copy or hash the handoff; it
  verified only that the path is a regular file with mode 0600, owner
  `mbv:mbv`, in a parent directory with mode 0700, owner `mbv:mbv` (648). No
  handoff content, passphrase or secret value is recorded.
- **`secret export`: not run.** For the next human export, a read-only
  `eregion secret list --app guided --environment production --context main
  --json` returned active `HELLO_SECRET` revision 3 (and `GIT_READ_TOKEN`
  revision 3) (649). Only metadata was captured; no value was read or printed.
  This confirms the namespace exists and does not satisfy the positive
  `secret export` check.
- Not re-run in this evidence step: the `--yes`, `--json` and stdin/non-TTY
  refusal paths for both exports, positive `secret export` and positive
  `token create`; positive `app delete` and positive `env delete` were run
  separately and are recorded below.

### 2026-11-05 — fresh-server `app delete guidedflags2` and env-delete preview safety check

- Published CLI/node: `edge-f447aa3a0d30`, context `main`, Ubuntu 26.04
  amd64; node commit `f447aa3a0d30`, schema 20/20, River 7/7, all services
  active. Evidence: `/home/mbv/eregion-qual/2026-11-04-fresh/660-*` through
  `679-*`. All agent checks in this record are read-only.
- **`app delete guidedflags2`: pass for the verified subset (operator-run).**
  The human ran `eregion app delete guidedflags2 --context main --confirm
  guidedflags2` with a fresh TOTP; operation
  `op_44ae1a2d25c010f9e42ec16b4472c6f2` (`platform.app.delete`,
  target `app/guidedflags2`) succeeded in 5.7 s. The human reports that the
  dry-run preview promised container/route/vault/domain/platform-row cleanup
  and preserved history; that preview was not saved and cannot be replayed now
  that the app is gone. The agent did not rerun the deletion.
- Verified absent after deletion: `app list` has no `guidedflags2`;
  `app status guidedflags2` is `not_found`; `env list --app guidedflags2` and
  `secret list --app guidedflags2 --environment production` are empty.
  Read-only `eregion_fresh` counts are 0 for app, environment, release,
  deployment, build, domain, secret namespace, secret revision, environment
  revision and change rows (660-663, 668).
- Verified history retained: `operations get` and `operations follow
  --output ndjson` replay the terminal operation with 8 events; the final
  delete event says the app and every environment were deleted after workload
  and route cleanup and that operation history was kept; the DB has 8 events
  and 1 step. `operations list --app guidedflags2` returns the operation; the
  `--app ... --environment production` filter is empty because the operation
  target is `app/guidedflags2` (664-666, 669).
- Verified other apps unaffected: `githello/production` (git-test1, body
  `eregion sample: v2 src3`), its proxied alias (git-test2/users/sign-in,
  `alias ready`), `guided/production` (git-test3, `eregion sample: guided
  src3`) and `img/production` (git-test5, `img release aftergc`) all returned
  HTTP 200 with curl exit 0; env status showed healthy/applied/ok (667).
- Not verified: independent Docker container/route listing. `eregion ssh
  --command 'docker ps …'` returned Docker socket permission denied (exit 1)
  and no sudo was used; cleanup rests on the operation event and zero platform
  rows, not on a Docker inventory. This subset does not cover positive
  `env delete`, the last-environment case, `token create` or `secret export`.
- **Urgent preview finding (not a destructive defect):** the human's next
  dry-run, `eregion env delete guided failing --context main --dry-run`,
  printed "This is the app's last environment; deleting it also deletes the
  application record." even though `guided` has **two** environments. DB/CLI
  state: `guided` app row 1, environments `failing` and `production`;
  `production` serves `rel_4c43b3021250ee83d86051ed2aad068f` publicly at
  git-test3; `guidedflags` has one unused worker-only `production` env; total
  apps 5 (670-674, 678). The warning is a preview-only false positive:
  `deletionPreview` calls `deletionCounts` with the environment filter
  (`internal/controlplane/platform_delete.go:353`), and the count at line 384
  is restricted to `($2='' OR e.name=$2)`, so it is 1 for any named
  environment; line 366 then emits the last-environment warning. The CLI only
  prints preview warnings (`internal/cli/delete.go:118-133`). The actual
  worker path deletes only the target environment's rows
  (`deleteEnvironment`, lines 165-207; secrets scoped at lines 265-290), then
  `deleteEmptyApplicationTx` re-counts remaining environments after that
  deletion and returns early when `remaining != 0` (lines 293-303). Because
  `guided/production` remains, the app and production are preserved. The real
  `guided/failing` deletion was **not run**; no TOTP, confirmation, operation
  submission or database mutation was performed. Do not turn the warning into
  `app delete guided`; the next human target check is evidence 676.
- Prepared but not executed for the next human: `env delete guided failing
  --confirm guided/failing` (preserve `guided/production`) and the
  last-environment `env delete guidedflags production --confirm
  guidedflags/production` (unused worker-only app from the earlier comparison).
  Exact pre/post conditions and abort criteria are in 676.
- Skipped/not run in this step: positive `token create`, positive `secret
  export`, the last-environment deletion, container inventory, and the rest
  of the alpha baseline. Positive `env delete guided failing` was run later in
  this session and is recorded below. No apply, publish, commit, install,
  host/database mutation, fault injection or secret-value read occurred.

### 2026-11-05 — fresh-server `env delete guided failing`

- Published CLI/node: `edge-f447aa3a0d30`, context `main`, Ubuntu 26.04
  amd64; node commit `f447aa3a0d30`, schema 20/20, River 7/7, all services
  active. Evidence: `/home/mbv/eregion-qual/2026-11-04-fresh/680-*` through
  `689-*`. All agent checks in this record are read-only.
- **`env delete guided failing`: pass for the verified subset (operator-run).**
  The human ran `eregion env delete guided failing --context main --confirm
  guided/failing` with a fresh factor; operation
  `op_62837d11f91ad17e7a28a87846a5b594` (`platform.environment.delete`,
  target `env/guided/failing`) succeeded, attempt 1, 11 events, last sequence
  11. The operator reports 4.6 s; the DB row spans
  2026-11-05T10:28:25.450329Z–10:28:32.084685Z. The agent did not rerun it.
- Only the failing environment was removed: `app status guided` returns the
  app with one environment, `production`; `env list --app guided` has only
  `production`; `secret list --app guided --environment failing` is empty.
  Read-only `eregion_fresh` counts for `guided/failing` are 0 for environment,
  release, deployment, build, domain, secret namespace, secret revision and
  environment revision rows; the failed deploy's change rows are gone (only
  2 production-environment changes and 1 app-level change remain). The
  `guided` app row remains 1 and total apps remain 5 (680, 682, 685).
- `guided/production` remains serving: release
  `rel_4c43b3021250ee83d86051ed2aad068f`, route applied, public ok/200 at
  git-test3, with 1 release, 1 deployment, 1 build, 1 domain, 2 secret
  namespaces and 1 revision; both production secret names remain active at
  revision 3 (681, 685).
- Operation history remains retrievable: `operations get` and `operations
  follow --output ndjson` replay the terminal operation with 11 events;
  `operations list --app guided --environment failing` still returns the
  deletion plus the earlier failing-environment domain/deploy history, and
  the DB has 11 events and the completed `platform-delete-01-v1` step with
  `result={"removed": 0, "networks_removed": 1}` (682-685).
- All four monitored hostnames returned HTTP 200 with curl exit 0: git-test1
  (`eregion sample: v2 src3`), git-test2/users/sign-in (`alias ready`),
  git-test3 (`eregion sample: guided src3`) and git-test5
  (`img release aftergc`) (686).
- The two `stdout: a584eb7d7b62` node-delete lines are the environment network
  ID, not a duplicated container removal. `DeleteStep.Apply` lists containers
  first (step result `removed: 0`, so no container removal ran), then lists
  and removes the environment network (`internal/platform/delete_step.go:75,
  88, 94`); `docker network ls --quiet` and `docker network rm` each print the
  same ID, and `runArgv` reports each stdout line
  (`internal/platform/step.go:1258-1263, 1321-1352`). No ownership or cleanup
  defect. Minor message observation: `Check` says "managed workload containers
  still exist" when only a network remains (delete_step.go:69).
- The unused `guidedflags` app still had exactly one environment, `production`
  (worker-only, no domains, no releases/deployments/builds, 2 secret
  namespaces) when this step ended. The prepared last-environment deletion was
  subsequently executed successfully; see the next record (690-699).
- Not verified: independent Docker container/network inventory. The Docker
  socket was not readable by the agent without sudo, and no sudo was used; the
  operation step result and zero platform rows are the cleanup evidence.
  Not run in this step: positive `token create`, positive `secret export`, the
  last-environment deletion (executed later in this session; see next record),
  and the rest of the alpha baseline. No source edits, tests, build/vet,
  apply/publish/commit/install, host/database mutation, fault injection, TOTP
  beyond the human's recorded deletion, or secret-value read occurred.

### 2026-11-05 — fresh-server last-environment `env delete guidedflags production`

- Published CLI/node: `edge-f447aa3a0d30`, context `main`, Ubuntu 26.04
  amd64; node commit `f447aa3a0d30`, schema 20/20, River 7/7, all services
  active. Evidence: `/home/mbv/eregion-qual/2026-11-04-fresh/690-*` through
  `699-*`. All agent checks in this record are read-only.
- **`env delete guidedflags production`: pass for the verified subset
  (operator-run).** The human ran `eregion env delete guidedflags production
  --context main --confirm guidedflags/production` with a fresh factor; the
  operator reports the dry run correctly showed that deleting the last
  environment would also remove the application. Operation
  `op_844aaba1c862a0450452be5bd40ac624` (`platform.environment.delete`,
  target `env/guidedflags/production`) succeeded, attempt 1, 8 events, last
  sequence 8. The operator reports 5.6 s; the DB row spans
  2026-11-05T10:32:09.626303Z–10:32:15.177262Z. The agent did not rerun it.
- The `guidedflags` application is gone: `app list` now has exactly deployab,
  githello, guided and img (4 apps); `app status guidedflags` is `not_found`;
  `env list --app guidedflags` and `secret list --app guidedflags
  --environment production` are empty (690, 691, 695).
- Read-only `eregion_fresh` counts for `guidedflags` are **0** for app,
  environment, release, deployment, build, domain, secret namespace, secret
  revision, environment revision and change rows (694).
- Remaining apps are unaffected: `guided/production` still serves
  `rel_4c43b3021250ee83d86051ed2aad068f`; githello, guided and img env status
  all show public ok/200 and route applied; all four monitored hostnames
  return HTTP 200 (git-test1 `eregion sample: v2 src3`, git-test2/users/sign-in
  `alias ready`, git-test3 `eregion sample: guided src3`, git-test5
  `img release aftergc`) (696, 697).
- Operation history remains retrievable: `operations get` and `operations
  follow --output ndjson` replay the terminal operation with 8 events;
  `operations list --app guidedflags --environment production` and
  `operations list --app guidedflags` still return it after the app row is
  gone; the DB has 8 events and the completed `platform-delete-01-v1` step.
  The step result is null and there was no stdout because its check found no
  managed workload containers or environment networks (guidedflags was never
  deployed), so Apply had nothing to remove (691-694).
- Not verified: independent Docker container/network inventory. The Docker
  socket was not readable by the agent without sudo, and no sudo was used; the
  step check/result and zero platform rows are the cleanup evidence. Not run
  in this step: positive `token create`, positive `secret export`, and the
  rest of the alpha baseline. No source edits, tests, build/vet,
  apply/publish/commit/install, host/database mutation, fault injection, TOTP
  beyond the human's recorded deletion, real export, additional deletion or
  secret-value read occurred.

### 2026-11-05 — fresh-server isolated handoff login, profile verification and removal

- Published CLI/node: `edge-f447aa3a0d30`, context `main` in two separate
  profiles, Ubuntu 26.04 amd64. Evidence:
  `/home/mbv/eregion-qual/2026-11-04-fresh/720-*` through `739-*`. The human
  ran login and the isolated removal; all agent checks in this record are
  read-only.
- **Passphrase-file login: pass for the verified subset
  (operator-reported).** The human ran on this box
  `XDG_CONFIG_HOME=/home/mbv/eregion-qual/private-human-checks/login-profile
  eregion login --file /home/mbv/eregion-qual/private-human-checks/main-handoff.md
  --passphrase-file <mktemp 0600>`; the passphrase was extracted from the
  handoff inside the operator session and the temporary file was removed by
  trap. The agent did not read the handoff or any passphrase/key file. This
  verifies the passphrase-file login path and the isolated profile, not the
  baseline's second-machine topology.
  Read-only `context list` and `context show --name main` under that XDG show
  one current `main` context created at 2026-11-05T11:13:26Z with route
  `mbv@10.78.0.1:22 over WireGuard via 46.224.129.96:51820`; `server status`
  returns CLI/node `edge-f447aa3a0d30`, node commit `f447aa3a0d30`, schema
  20/20, River 7/7 and active Caddy, Docker, API, worker and WireGuard
  services (720, 721, 726); 722 is the concurrent-attempt failure described
  below.
- **Normal profile still works: pass.** The default `~/.config/eregion`
  profile still lists its separate `main` context (created
  2026-11-04T09:42:37Z) with the same route, and `server status` succeeds
  (723–725, 728; 730, 732, 733 after the isolated removal). The two profiles
  have distinct context files and identity directories; modes are 0700 for
  the profile/servers directories and 0600 for contexts.json, known_hosts and
  the identity files. No key or credential file was read.
- **Tunnel contention observation (not a context defect).** The isolated
  profile was imported from the handoff that carries the normal profile's
  operator identity, so both use the same operator WireGuard public key (same
  tunnel fingerprint and socket basename).
  Concurrent or rapid profile switching can make one command lose the node's
  single peer endpoint: isolated `server status` failed while the normal
  status ran concurrently (722), and the normal recheck failed immediately
  after a successful isolated status (727); each profile succeeded when run
  against the idle peer (725, 726). The tunnel daemons are local and exit
  after 10 minutes idle. Stop or let the unused daemon idle out before
  switching profiles.
- **Interactive login: pass, operator-reported.** Earlier pasted-passphrase
  attempts failed; their cause was not established. The real handoff's own
  passphrase opens its bundle (710), and synthetic prompt/round-trip tests pass
  (701).
- **Context remove: pass for the isolated disposable profile
  (operator-run).** The scoped command was
  `env -u EREGION_CONTEXT_FILE XDG_CONFIG_HOME=/home/mbv/eregion-qual/private-human-checks/login-profile eregion context remove main --confirm main --json`;
  the output was `changed: true, removed: main`. The isolated store is now
  empty and `servers/main` is gone; the isolated profile-level `known_hosts`
  remains. The normal `~/.config/eregion` store and identity are unchanged
  (contexts.json inode/size/mtime and every identity file inode/mtime match
  the pre-removal values) and `context list/show` plus `server status`
  succeed; isolated `context show/status` returns `not_found`, not a tunnel
  error (730–739). The pasted command showed a blank line before `eregion`;
  the unchanged normal store proves the executed removal was scoped to the
  isolated XDG profile, not a bare default-profile removal. Do not omit the
  XDG override: the default store is the working normal profile.
- **Wrong host key and atomic unreachable import: pass (agent-run, 2026-11-05).**
  See the next record. Still **not run:** a true second-machine login, the
  exact `--replace` conflict, a successful replacement swap and context
  selection. The normal `main` context and identity are intact; the isolated
  context is removed.
- No source edits, tests, build/vet, export/decrypt/import, apply/publish/
  commit/install, host/database mutation, fault injection or secret-value
  read occurred in this step. Doctor was not run; the known 19-pass/1-fail
  host-credentials state and the published f447aa3 fix remain untouched.
  `server status` is read-only; it spawned local tunnel daemons.

### 2026-11-05 — agent-run wrong-host-key and network-isolated login variants

- Published CLI/node: `edge-f447aa3a0d30`, commit `f447aa3a0d30`, Ubuntu
  26.04 amd64. The agent ran both logins under explicit authorization; the
  passphrase was extracted from the saved handoff only into a protected 0600
  file inside a 0700 disposable tree and was never printed, hashed or put on
  a command line. Evidence:
  `/home/mbv/eregion-qual/2026-11-04-fresh/750-*` through `759-*`; the
  private prep records 740-744 are not part of this evidence.
- **Wrong host key: pass.** A 0700 copy of the real normal `main` context,
  identity and known_hosts had only its copied top-level ED25519 known_hosts
  pin replaced with a locally generated wrong key. The noninteractive login
  with the saved handoff and the protected passphrase used
  `--replace main --json` with `EREGION_TUNNEL=inprocess`; it exited 6
  (`unavailable`) with "the host key ... does not match the pinned key".
  The SSH host-key verification failed during the handshake, before
  authentication; no node RPC was dispatched and no readiness/swap occurred.
  The copied context, identity and known_hosts stayed byte-identical (750–752,
  755). The only remote effect was the pre-auth WireGuard and SSH host-key
  exchange.
- **Atomic unreachable import: pass.** A second 0700 copy changed only its
  known_hosts hostname from 10.78.0.1 to 10.78.0.2 and ran the same login
  inside `unshare -rn` with zero network routes. It exited 6 (`unavailable`)
  with "the WireGuard tunnel ... did not come up ... no request was sent";
  the node was not contacted. The copied context, identity and known_hosts
  stayed byte-identical (753–755).
- **Cleanup and normal profile: pass.** The protected passphrase file and the
  whole qual-740 tree were removed; no copied-profile tunnel daemon or socket
  remained. Read-only `server status` on the normal profile then passed:
  CLI/node `edge-f447aa3a0d30`, node commit `f447aa3a0d30`, schema 20/20,
  River 7/7 and Caddy, Docker, API, worker and WireGuard active. The normal
  tunnel daemon started by that check was stopped. The normal profile's
  contexts.json/known_hosts hashes and identity inode/size/mtime and the
  isolated profile hashes were unchanged (756–758).
- **Still not run:** a true second-machine login, the exact `--replace`
  conflict, a successful replacement swap and context selection. This step
  made no source edits; the only repository edit made by this step is this
  authorized MANUAL-TESTING.md record. No apply, export, database, DNS,
  token, protect or host mutation was run.

### 2026-11-05 — agent-run batched login conflict, replacement and context selection

- Published CLI/node: `edge-f447aa3a0d30`, commit `f447aa3a0d30`, Ubuntu
  26.04 amd64. Agent-run under the existing protected handoff/profile
  methodology: the handoff was passed read-only to the CLI, its passphrase was
  extracted by `awk` and piped to `--passphrase-file /dev/stdin`, and was never
  printed, hashed, written to a passphrase file or placed on a command line.
  The disposable profile tree was 0700 and every command used
  `EREGION_TUNNEL=inprocess`; it was removed after the run. Evidence:
  `/home/mbv/eregion-qual/2026-11-04-fresh/760-*` through `767-*`.
- **Exact existing-name conflict without `--replace`: pass.** A disposable
  profile with a synthetic `main` context and identity ran
  `login --file main-handoff.md --passphrase-file /dev/stdin --json`; it exited
  5 (`conflict`) with "context main already exists" and the exact
  `--replace main` hint. The profile (contexts.json, known_hosts and synthetic
  identity files) was byte-identical before and after, with no
  `.eregion-login-*`/`main.replace-*` entry and no tunnel daemon. Source
  `internal/cli/login.go` 187-197 returns this conflict before identityDir,
  probe or ready, so no node contact occurs (760, 762).
- **Successful `--replace main`: pass.** The same disposable profile then ran
  the login with `--replace main`; it exited 0 and returned context `main` on
  route `mbv@10.78.0.1:22 over WireGuard`. The pre-existing synthetic identity
  directory was swapped out (stale sentinel gone; `ssh_ed25519` inode changed
  17435096 -> 17436650), the saved paths stayed inside the disposable profile,
  known_hosts gained one `10.78.0.1` pin, and no temporary or backup directory
  remained. The login's WireGuard host-key pin and authenticated version/ready
  RPC completed before the swap. The normal `~/.config/eregion`
  contexts.json/known_hosts hashes, identity inode/size/mtime and tunnels
  metadata were unchanged, and the isolated login-profile hashes were
  unchanged. The imported identity stayed only in the disposable profile, which
  was deleted in cleanup (763).
- **Context selection among disposable contexts: pass.** `context use --name
  spare` made spare current and `context list`/`show` with no `--name` returned
  it; `context use --name spare-bad` made a deliberately unusable context
  current and `show` returned that. With `spare-bad` current,
  `server status --context main --json` and
  `EREGION_CONTEXT=spare server status --json` both exited 0 and returned node
  `edge-f447aa3a0d30`, commit `f447aa3a0d30`, schema 20/20, River 7/7 and
  active Caddy, Docker, API, worker and WireGuard services. Current was then
  restored to main (764).
- **JSON/noTTY never prompts: pass.** With an empty disposable profile and
  stdin from `/dev/null`, `login --file main-handoff.md --json` and plain
  `login --file main-handoff.md` both exited 2 (`input_required`) with
  "--passphrase-file is required without a terminal"; stdout was empty, no
  bundle-paste or login prompt/header appeared, and neither waited for input
  (761).
- **Cleanup and normal profile: pass.** The qual-760 tree (including the
  CLI-written disposable identity) was removed; no
  `.eregion-login-*`/`main.replace-*` entry, tunnel daemon or eregion unix
  socket remained. The normal profile's contexts.json/known_hosts hashes,
  `servers/main` identity inode/size/mtime and `tunnels` metadata were
  unchanged, and contexts.json/known_hosts kept their 2026-11-04 09:42
  creation mtimes (before the run); `context list/show` still show the original
  current `main`. A read-only `server status` with
  `EREGION_TUNNEL=inprocess` exited 0: CLI/node
  `edge-f447aa3a0d30`, node commit `f447aa3a0d30`, schema 20/20, River 7/7 and
  Caddy, Docker, API, worker and WireGuard active. No daemon was started
  (765, 766).
- **Still not run:** a true second-machine login; it is not claimed. No source
  edits, source test suite, build/vet, export/decrypt/import, apply/publish/
  commit/install, host/database/DNS mutation, token minting, fault injection or
  secret-value read occurred. No `--auto-upgrade-nodes` was passed and the
  disposable context had `auto_upgrade_nodes` false, so the version check could
  not install or change the node; only read-only version/ready/status RPCs and
  the pre-auth host-key exchange reached the node. The normal `main` context and
  identity are intact.

### 2026-11-05 — true second-machine laptop login and status

- Published CLI/node: `edge-f447aa3a0d30`, commit `f447aa3a0d30`, channel
  `edge`; operator laptop linux/amd64, Go 1.27.1, tree reported clean
  (`dirtyfalse`). The laptop is a separate machine from `thelab`; the handoff
  was copied from `thelab` into a mktemp directory on the laptop. Evidence:
  `/home/mbv/eregion-qual/2026-11-04-fresh/768-*` through `770-*`. The parent
  session relayed the operator's report; the agent did not run any command
  against the server, laptop or `thelab`, did not read the handoff or any
  credential file, and did not independently inspect the laptop profile or
  its cleanup.
- **True second-machine login: pass (operator-reported).** On the laptop, the
  operator copied the handoff from
  `/home/mbv/eregion-qual/private-human-checks/main-handoff.md` (5235 bytes)
  into a mktemp directory with `scp`, extracted the paired passphrase there
  with `sed` under `umask 077`, and ran
  `env -u EREGION_CONTEXT -u EREGION_CONTEXT_FILE
  XDG_CONFIG_HOME="$d/profile" EREGION_TUNNEL=inprocess eregion login --file
  "$d/handoff.md" --passphrase-file "$d/passphrase" --json`. It exited 0 and
  returned schema `eregion/v1`, ok true, context `main` and route
  `mbv@10.78.0.1:22 over WireGuard`. No passphrase or secret value was
  printed, hashed or recorded.
- **Second-machine `server status`: pass (operator-reported).** The same
  scoped disposable profile ran
  `server status --context main --json`; it exited 0 and returned CLI/node
  `edge-f447aa3a0d30`, node commit `f447aa3a0d30`, role `main`, schema 20/20,
  River 7/7, Caddy, Docker, API, worker and WireGuard all active, and uptime
  100265. This is the baseline's second-machine login; the earlier accidental
  run on `thelab`'s localhost is excluded and is not a pass.
- **Cleanup: operator command-based, not independently verified.** The
  login/status commands ran inside a parenthesized shell with `set -eu` and
  `trap 'rm -rf "$d"' EXIT`; the wrapper returned successfully. The agent did
  not inspect the laptop profile, temporary directory, tunnel or socket state
  after the run, so the removal of the disposable profile is reported rather
  than verified.
- **Later correction:** the operator reports interactive login passed. The
  earlier pasted-passphrase failures are not independently explained. At this
  point the broader alpha baseline was incomplete, and the local source fixes in
  the working tree remained unpublished. This step records only the
  second-machine login/status sub-check; alpha.1 is now operator-confirmed.
- No source edits, source tests, build/vet, export/decrypt/import,
  apply/publish/commit/install, host/database/DNS mutation, token minting,
  fault injection or secret-value read occurred in this step. The agent ran no
  command against the server, laptop or `thelab`; the only repository edit made
  in this step is this authorized MANUAL-TESTING record, and the only evidence
  files written in this step are 768–770.

### 2026-11-05 — reviewed-fix integration (local source only)

- Target: `/home/mbv/work/eregion` on `master`, HEAD `f447aa3a0d30` plus the
  uncommitted working tree; the three source worktrees were based on the same
  commit. This is not a published build and not host evidence.
- Integrated reviewed fixes: defect 11 deletion-preview count; cancelled-pack
  cleanup (dedicated attempt network, SIGTERM grace, ownership-checked
  cleanup); durable GitHub webhook admission (migration 021, sanitized receipt
  and River job before 202, atomic operation link, fresh resolver inside an
  8-second budget). Pre-existing working-tree fixes were preserved
  byte-for-byte: auth reload, host credential store, sudo-rs detection, viewer
  serving summary and export KeyID.
- Integrated source files are byte-identical to the reviewed worktree copies
  (22 files); all 28 pre-existing modified/untracked files have the same
  SHA-256 before and after. No conflicts occurred.
- Local checks passed: `gofmt -l` on changed Go files (empty), `go build ./...`,
  `go vet ./...`, `GOOS=darwin GOARCH=arm64 go build ./...`; offline package
  tests for `internal/api`, `internal/vault`, `internal/platform`,
  `internal/controlplane`, `internal/viewer`, `internal/engine` and
  `internal/host`; CLI compile-only plus targeted SSH/sudo/runner/export tests;
  targeted regressions for the three fixes and the pre-existing auth, host,
  vault, viewer and engine fixes.
- Explicit-DSN cases compiled and **skipped** (`EREGION_TEST_DATABASE_URL`
  unset): the 16 webhook admission/resolver cases and the deletion-preview
  integration case. No ambient DSN, database or Docker fixture was used.
- Explicit-DSN PostgreSQL run (2026-11-05): **pass**. All 16 webhook
  admission/resolver cases and the deletion-preview integration case ran
  against PostgreSQL 17.11 in disposable databases created under a dedicated
  `eregion_test_parent_820_…` parent; 17/17 passed, no skips. Migration 021
  applied in every ephemeral database. Three test-fixture defects found by the
  run were fixed: the webhook fixture reused hard-coded host ports, two legacy
  HTTP cases used an `X-GitHub-Delivery` value with an underscore (the handler
  correctly accepts only `[A-Za-z0-9-]`), and the deletion-preview fixture
  named a node that did not match the in-process transport identity. Product
  code was unchanged. The parent database, temporary role and credential file
  were removed; the contents of the pre-existing `eregion`, `eregion_fresh`,
  `postgres`, `template0` and `template1` databases were unchanged. This is a
  local DB integration check, not host qualification.
- Not host-qualified in this step: migration 021 was applied only inside the
  disposable test databases, not to a live control database; no server, DNS,
  GitHub hook, release, install, commit, push or tag was touched; real webhook
  latency/first delivery/redelivery/rotation, a real cancelled pack build and
  the post-fix deletion preview were then host checks. Alpha.1 is now
  operator-confirmed; no individual result is backfilled here. No secret value
  was printed, logged or stored in evidence, and the protected DSN was not used
  as the test target.
- Evidence: `/home/mbv/eregion-qual/2026-11-04-fresh/810-integration-*.txt` and
  `820-postgres-*.txt` (target, selected list, first failing run, passing run,
  offline regressions, cleanup and sanitizer); source-worktree records
  `780-delete-preview-last-environment.txt`, `790-pack-cancel-*` and
  `800-webhook-*`.

Append one record per run. Do not backfill from history; runs before
2026-10-26 qualified older builds and earlier scope.

```text
Date / build / commit / channel: 2026-10-28 / edge-ed6e4f35b78c / ed6e4f3 / edge
Operator OS/arch: linux/amd64 (CLI upgraded edge-8a7179d6d95d -> edge-ed6e4f35b78c: pass)
Server: existing test server, context `main`, x86_64, node edge-8a7179d6d95d
Node upgrade dry run (`apply --dry-run --auto-upgrade-nodes`): pass; reported
  the upgrade, exit 5, node binary unchanged
Node upgrade (`apply --auto-upgrade-nodes`): FAIL, exit 2, validation_failed
  "invalid method params" / unknown field "version". The older node strictly
  decodes upgrade.install as {path, sha256}; ed6e4f3 added version/channel, so
  no existing node can be upgraded by it. `admin` has only the node-RPC sudo
  rule, so the RPC is the only upgrade path. Node left on edge-8a7179d6d95d.
CI for ed6e4f3: FAIL. macOS: unresolved checkout root for an empty git context
  path (/var vs /private/var). Integration: seven new tests registered nodes the
  test transport does not authenticate, three updated immutable revisions, and
  the scripted transport omitted the inputs hash a real node journals, so the
  release-command evidence rule correctly kept two deploys retrying.
Fixes (working tree, not yet published): upgrade.install params frozen at
  {path, sha256} with the release declaration uploaded as release.json beside
  the binary; empty context path resolves like "."; integration fixtures fixed.
Application baseline: not run (blocked on the node upgrade).
```

```text
Date / build / commit / channel: 2026-10-29 / edge-22f52c676669 / 22f52c6 / edge
CI for 22f52c6: pass (check with race, integration with race, macOS CLI);
  Publish edge: pass
Operator OS/arch: linux/amd64 (CLI upgraded edge-ed6e4f35b78c -> edge-22f52c676669: pass)
Server: existing test server, context `main`, ubuntu 26.04 amd64
Node upgrade dry run: pass; node_version_mismatch reported, node unchanged
Node upgrade (`apply --auto-upgrade-nodes`): binary install PASS (node now
  edge-22f52c676669, commit 22f52c676669); apply FAIL, exit 6, at
  host-database-v1 "control-plane database is unavailable"
Post-apply: eregion-api/eregion-worker crash-loop with exit 6; doctor 13 pass,
  1 warn, 4 fail (drift.pending, version.schema, both services); schema 0/0
  reported because the database cannot be read
Cause: not eregion. The control database is reached through the operator
  machine's Tailscale Funnel (TLS-terminated TCP); a network outage there at
  08:35 UTC, before the upgrade, left the public Funnel ingress dropping TLS
  handshakes. Re-adding the Funnel did not help; restarting tailscaled did
  (all six ingress addresses then completed TLS from the server).
Diagnosability gap (open): the fault cause is not surfaced by apply, plan,
  doctor or server status, and admin cannot read /etc/eregion or the eregion
  service journal, so the network error was not observable from the operator
  path.
Retry `apply --auto-upgrade-nodes`: pass, exit 0; applied host-database-v1
  (schema 13 -> 16), api/worker restart and readiness. Second `apply`: no-op
  (0 of 17). doctor: 18 pass, 0 warn, 0 fail. CLI/node edge-22f52c676669,
  schema 16/16, River 7/7, all services active.
Version lockstep / signed node upgrade from edge-8a7179d6d95d: pass.
App: githello/production, private git https://github.com/mbvlabs/eregion-hello
  (fine-grained read token), web + worker, release command
  `eregion-hello release`, config APP_VERSION=v1, secret HELLO_SECRET, domain
  git-test1.mbvlabs.com (Cloudflare proxied). Flag-only create (not the guided
  form): accepted, op_49bbb5e2e870fac4005801dec7351c17; first deploy
  op_14cdbdbbe86aaa79823ef246167c0e66.
Private git clone + Buildpacks build: pass (commit ea1b225, pack 82s, release
  rel_ef2f8f4e60684cb0722e616382a1d4af).
First deploy: FAIL, exit 6, unavailable after 4m56s. The release command ran as
  `docker run <image> eregion-hello release` without the CNB launcher, so the
  image's default web process started and never exited; at the 120s timeout
  the node killed only the docker client, the container kept running, the
  error was retryable and every retry hit a container-name conflict.
  Previous release unchanged (none). Leaked container
  eregion-githello-production-release-7e5cbbcbabf7 still runs on main (no
  published port); admin has no Docker access to remove it.
Fix (working tree, not yet published): the release command is its own node
  step (platform-command-01-v1, ReleaseCommandPayload) that runs through the
  launcher for a Buildpacks image like a process command; nonzero exit,
  start failure and timeout are terminal, and a timeout removes the container.
  The release container now carries the app/environment ownership labels, so
  app/env delete, status and logs find it; the leaked container above predates
  this and has none, so it needs a one-time manual `docker rm -f`.
Published e344781 (edge-e344781ced4b; CI pass, edge pass); CLI and main
  upgraded, apply no-op, doctor 18/0/0.
Second first-deploy attempt op_ec5db74078f168c2391b868a047f73c9: release
  command pass (1.5s, launcher), web + worker candidates started, web health
  pass, Caddy switch pass; public check FAIL after 30s, "tls: internal error"
  (no certificate yet for git-test1). Route restored, candidates removed,
  exit 5. Initial hypothesis: the 30s public-check window was too short for
  first certificate issuance. That cause was NOT established; seeing an HTTP
  redirect through Cloudflare did not prove successful ACME validation.
Diagnostic retry on the same published build, using the existing release:
  op_44c553d6e9d34a1740b79b870f3fd217. Release command correctly skipped;
  candidate health passed. Read-only Caddy admin polling saw git-test1 appear
  at 11:04:14 and disappear by 11:04:29, before the public probe failed at
  11:04:46. Confirmed routing race: periodic reconciliation renders the old
  serving pointer, undoing the candidate switch during public verification.
Working-tree correction: every route writer selects the saved candidate
  instances while its operation/deployment is running and the switch step
  is running/completed, unless public verification failed or restoration
  began. Otherwise it selects serving instances. Regression covers first
  deploy, switch-before-ack, verification, and restoration after failure.
  The public-check window also becomes 2 minutes, matching deploycrate, to
  allow certificate issuance; this alone does not fix the routing race.
Local verification of the route correction: just check (format/vet/unit),
  just build, darwin/arm64 build: pass. Full PostgreSQL integration suite:
  pass (189.620s) against a new disposable PostgreSQL 17 on loopback port
  55439, removed afterwards. Race detector not run locally (no C compiler).
Route correction published as 5329590 (edge-5329590a3f48), CI (including
  race integration) and edge publication pass. CLI/main upgraded; schema
  16/16, River 7/7, doctor 18 pass; second apply no-op.
Published-build retry op_85ed4ffb0f50d20de1b9be5835de8d46, existing release
  rel_63507d362852c4fd97048936ad1f7e7b: release command correctly skipped,
  candidate web/worker started, web health passed. Caddy polling confirmed
  git-test1 stayed installed from 11:24:23 through 11:26:19 despite periodic
  reconciliation. Public verification failed with TLS internal error after
  the full 2-minute window (11:24:21–11:26:22); only then was the route removed
  and both candidate containers cleaned up. Follow exit 5. No serving release.
  Route-race correction: pass on host; certificate problem remains separate.
Read-only diagnosis: ACME directory reachable from main over IPv4 and IPv6
  (HTTP 200). Caddy sends logs to the system journal; no log files exist in
  /var/log/caddy. Admin cannot read Caddy's journal or use sudo without human
  authentication. Need privileged Caddy logs for the verification interval
  before diagnosing ACME failure or retrying. No certificate bypass applied.
Operator-supplied privileged Caddy logs establish the certificate failure:
  HTTP-01 validation followed an HTTPS URL through Cloudflare and received
  525 (origin TLS handshake failure). TLS-ALPN-01 also failed because the
  Cloudflare edge did not negotiate acme-tls/1. Production Let's Encrypt
  reported failed-authorization rate limiting with retry-after 2026-10-29
  11:28:18 UTC. ZeroSSL fallback separately failed obtaining EAB credentials
  (caddy_legacy_user_removed). Increasing the timeout cannot solve this.
The proposed DNS-only workaround was rejected: proxied Cloudflare is a
  supported product requirement, and deploycrate works without that step.
Operator authorized and installed official Caddy 2.11.4 amd64 .deb on main,
  verified with the upstream SHA-512 checksum, replacing Ubuntu's 2.6.2-14.
  Configuration validated; Caddy restarted. Existing application hostnames
  still return HTTPS 200/303, and doctor remains 18 pass. DNS stays proxied.
Retry on Caddy 2.11.4: op_8587e038f577d90d756d0d174554bf76, same release.
  Release command skipped, candidate health passed; TLS still failed for the
  full 11:39:10–11:41:11 verification window. Follow exit 5; route restored
  and both candidate containers removed. Upgrading Caddy alone did not fix it.
Operator then disabled proxy for git-test1. At 11:43 UTC both 1.1.1.1 and
  8.8.8.8 resolved its A record to 46.225.150.17, not main's 46.224.129.96
  (confirmed by saved context and main's eth0 address). The other IP also
  answers as Caddy and redirects HTTP to HTTPS, which can explain challenge
  requests missing the Caddy instance holding the challenge. The previous
  proxied origin value was not observable and must not be assumed verified.
  No deploy was retried after discovering the DNS mismatch.
Operator corrected both origin targets. git-test1 remains DNS-only,
  git-test2 remains proxied. The first submission after the correction refused
  before operation creation because the stored DNS observation was still wrong;
  after the periodic observer refreshed to dns=ok, retry was accepted.
First successful deploy: op_5a0dee15a50da94ace13bae428eeb1b1, 18.6s, exit 0,
  edge-5329590a3f48 with manually upgraded Caddy 2.11.4, release
  rel_63507d362852c4fd97048936ad1f7e7b (full deployment ID not captured).
  Primary public check transitioned from TLS error at 11:49:43 to HTTP 200 at
  11:49:46. External HTTPS (normal certificate verification) returns
  `eregion sample: v1`; worker logs show `worker heartbeat version=v1`.
Proxied alias: added git-test2 with /healthz, operation
  op_eb772442b05bff4cbe21092e77f50b42. Within 12s external HTTPS through
  Cloudflare (172.67.142.154) returned HTTP 200 and `eregion sample: v1`.
  Local TLS verification against Caddy with git-test2 SNI/hostname verified
  the origin certificate successfully. Fresh certificate issuance works with
  the proxy ON and the correct origin; disabling proxy is not required.
Remaining qualification: second-commit/zero-downtime deploy, failed candidate,
  rollback, scale/restart and the rest of the baseline. The Caddy upgrade alone
  was not a fix for the wrong DNS origin; Caddy 2.6.2 was not re-tested after
  correcting DNS, so no version-specific ACME defect has been established.
```

```text
Date / build / commit / channel: 2026-10-29 / edge-6d0e6bcdf65a / 6d0e6bc / edge
Server: existing main, Ubuntu 26.04 amd64; schema 16/16, River 7/7
Setup pin: official Caddy 2.11.4 packages for amd64/arm64, pinned SHA-256,
  host-ingress-v2. Both upstream package hashes independently checked.
  Local check/build/vet/darwin build pass. Initial 654469b edge signing failed
  with Rekor conflict and macOS shell test failed on checksum tooling;
  7d70ff6 signed successfully but macOS shell test still failed. Final 6d0e6bc
  scopes Linux installer execution test to Linux; artifact/version tests stay
  cross-platform. All CI jobs and edge publication pass for 6d0e6bc.
CLI/main upgrade: pass, doctor 18/0/0, second apply 0/17. Caddy 2.11.4 already
  installed manually, so automatic fresh installation remains NOT RUN.
App: githello/production; git-test1 direct, git-test2 Cloudflare-proxied
Second source commit: 6dc0468; APP_VERSION v2 and HELLO_SECRET revision 2.
  Config/secret changes did not deploy (v1 kept serving until explicit deploy).
  op_feb09d5b91fae33c1b9b71acbfb20aad succeeded, release
  rel_6587e1fc95ab6a382279ed68b07eb076. Primary continuity: 107 v1 + 281 v2
  responses, zero failures. Python alias monitor got 541 HTTP 403 responses
  despite curl returning 200; not counted as alias continuity pass.
Failed candidate: source 034eb82, op_f3c8d98c922de1ccf77254b62fdefff9,
  release rel_b07293e8cb84db75c2d4cf64ad8a6718, HTTP health 503, exit 5.
  Candidate web/worker removed. Prior v2 kept serving: curl monitor 342 primary
  + 412 proxied-alias responses, all 200/v2.
Rollback: op_007901e79d9e111d91e7013c6343b147 succeeded, v1 public response,
  release rel_63507d362852c4fd97048936ad1f7e7b. Read-only node inventory
  confirms deployment dep_17829516b0867de58811cc5ad36566da pinned HELLO_SECRET
  revision 1, original deployment also 1, v2 deployment 2. No secret exported.
Scale/restart: scale op_a76e2a47dc63ab399307664ca27b76a9 queued restart
  op_ca824ea04f46caa801945e4617ad1295; explicit restart
  op_b500b918c472987bb16b2aae28d41b3d. Both succeeded, two running web
  replicas on distinct loopback ports plus worker without published port;
  release command skipped in rollback, scale and restart. Restarts used the
  current HELLO_SECRET revision 2, distinct from rollback's pinned revision 1.
Repeated continuity with curl: op_62aed6ec69fbb7ac82beb77427e01ffd,
  source 6dc0468, config v3, release rel_8353d51796535fdf752f2c3450052af8.
  Primary 129 v2 + 136 v3; proxied alias 153 v2 + 163 v3; zero failures.
  Previous containers removed after verification.
Monorepo/moved-branch retry: revision context apps/web at source 034eb82
  failed with named missing path, op_74435aa7a0c4fea621a9331f458d250a,
  build bld_b921cb6d6db5d2e0365010a352d983bd. Branch then advanced to b895cb2
  adding healthy apps/web. Retry op_038e1d3a1c7c6eb6e0207d2736a08742 still
  checked out 034eb82 and failed on the original missing path (exit 5), proving
  no fallback to moved branch/root. New deploy op_5e45b93d5ae086627a6ebfa63ab36304
  built b895cb2 from apps/web and succeeded, release
  rel_07d1c0fb27cdc0d68c7e5815a56e9328.
Alias-specific path: git-test2 bound to /users/sign-in, public 200; /api/health
  returns 404. Proxied alias serves v3; certificate verification remains on.
Repair fault injection: using saved sudo credential over SSH (never printed),
  removed only githello web replica githello-production-web-1-11ba8f12ce95.
  Worker restart queued one system/maintenance restart
  op_86d5c64bb534a6ae4df042651c0b4671; succeeded, both replicas restored,
  same serving release, release command skipped. A second worker start queued
  no additional repair. Doctor 18 pass, both test hosts serve v3.
Contracts: logs --json single envelope; --json --follow refused exit 2;
  domain dry-run JSON object; ssh command remote exit 7 preserved. App delete
  dry-run allowed, real env delete/secret export/token create in JSON/no-TTY
  refused exit 2. No factor bypass, export or deletion performed.
Create replay: original githello-create-1 returned original create
  op_49bbb5e2e870fac4005801dec7351c17 and first deploy
  op_14cdbdbbe86aaa79823ef246167c0e66; secret metadata remains revisions 1
  (GIT_READ_TOKEN) / 2 (HELLO_SECRET), no rotation from replay.
Build stop: build bld_ea19d783ac55541189e81f29dcbce1d3, original operation
  op_0dd481a2c083a6bf7901ee0af2775363. Stop op_e6734f665ded938f4bf7acf19e861b86
  accepted at 12:36:41, dispatched at 12:36:43, but its node Apply only ran
  at 12:36:57 when pack had finished. Deploy cancelled at boundary, exit 130;
  previous release preserved. Retry op_6937cbe93056c26a5108757865dbeb49
  built the same b895cb2 successfully. Stop-immediately check FAIL: global
  host lock prevented cancellation marker reaching the in-flight build.
Fix in source: cancellation-marker step is host-lock-independent, retaining
  its own effect lock and normal journal. All other steps remain serialized.
  Regression holds host lock while running stop and checks marker is written;
  another regression checks ordinary steps still block and signal replay is
  idempotent. Local check/build/vet/darwin build pass.
Build-stop retest: published ef91d70; CI and signed edge publication green.
  CLI/main upgraded to edge-ef91d70f6215, second apply 0/17, doctor 18/0/0.
  Build bld_d21b35126cd6010b2ec48ba11da6f328 was running go build when stop
  op_3d2383275011db0f4d11b8c3dd61ca1d wrote the marker immediately (node step
  2.0 seconds, entire stop 5.7 seconds). Pack reported interrupted rather than
  completing. Original deploy op_5244c4b7bb966618bca76e6a69d9d072 cancelled,
  exit 130; previous v3 release remained serving. PASS on this build.
Alias removal: op_8f136156eb93ba45ca9d6a0c60ae87c5 submitted, then repeated
  removal op recorded in 81-alias-remove-submit.json was followed to success.
  Inventory omitted alias; primary kept serving. Alias restored afterward.
Route-write failure/recovery: temporarily chmod 0555 the generated-routes
  directory /etc/caddy/eregion.d, with an EXIT restoration trap. Alias add
  op_09f89c35fa4f52ed12df2d76eae0d688 failed exit 5, both bindings visibly
  route_state=failed with could-not-write error; primary still served v3.
  Restored original 0755/uid999/gid983. Periodic reconciliation reapplied
  desired routes without re-submission, cleared errors; both domains now
  route=applied, certificate=valid, public=ok. Failed operation stays failed.
  This qualifies write failure/recovery, NOT a rejected Caddy reload.
Release upload rejection: RPC on published main given a private dummy upload
  with correct SHA-256. Missing declaration rejected; dev declaration rejected;
  naming the current signed edge release with unsigned payload rejected as
  not the signed linux/amd64 artifact. No binary replacement or service restart;
  temp upload removed; version still edge-ef91d70f6215. PASS.
Historical unlabelled release container from pre-fix attempt:
  eregion-githello-production-release-7e5cbbcbabf7 was still running with no
  published ports; removed explicitly after confirming identity. No other app
  containers targeted. NDJSON log snapshot parsed line-by-line successfully.
Evidence: ~/.cache/eregion-run/71-* through 92-*; latest doctor 18/0/0.
At this point the listed variants were not yet qualified. Alpha was not
  complete then; alpha.1 is now operator-confirmed without reconstructing these
  results.
```

```text
Date / build / commit / channel: 2026-10-29 / edge-ef91d70f6215 / ef91d70 / edge
Operator OS/arch: linux/amd64; CLI and main edge-ef91d70f6215 (resumed snapshot)
Server: existing main, Ubuntu 26.04 amd64, kernel 7.0.0-30; schema 16/16,
  River 7/7; doctor 18/0/0 before and after
App: githello/production; git-test1 direct, git-test2 Cloudflare-proxied
Release command scope (changed command): env update to
  `eregion-hello release changed`, op_9fb563245865bcaad59d77b502710111 (no
  deploy). deploy --release rel_07d1c0fb27cdc0d68c7e5815a56e9328,
  op_2e03c1f3b7d922dc49609a3b048e4ca2: command ran again for the same release
  ("release command ran version=v3"), succeeded, 52+52 monitor requests 200.
  PASS.
Release command failed run: command `false`, op_16a4eefef2536158a493a810627b8af0;
  deploy op_abfb7a138e27de0188d60aa28ee96b77 failed at platform-command-01-v1
  (exit status 1), exit 5 precondition_failed; rel_07d1 kept serving. Then
  restart and `process scale web 3` refused at submission, exit 5, "release
  command has not succeeded for this release"; no operation and no revision
  (replicas stayed 2). PASS. Rollback --to rel_8353d51796535fdf752f2c3450052af8
  (op_b55430000c5515b87c929a0d8c53622d) succeeded and skipped the command: it
  pins the release's recorded revision, whose command succeeded for that
  release, so the current failed command does not apply. Correct by design;
  a rollback refusal needs a release with no succeeded recorded command and
  was not constructed. Restored `eregion-hello release`
  (op_990bdfc52243088f97142a453396dd11); deploy
  op_831954d2473e0f9b201f8747d3787c4b skipped the succeeded command, rel_07d1
  serving again.
Leaked-container retry: fault = temporary root /usr/local/bin/docker wrapper
  failing only `rm -f githello-production-web-1-b99bb0a5d5a1` (everything else
  exec /usr/bin/docker), 30-min systemd-run removal guard. Deploy
  op_438306f739281d1a8b0a1586fcfa5859 SUCCEEDED; its cleanup step failed with
  the simulated error; "Previous release cleanup is pending" event; env status
  `pending_cleanup` named the container ("1 leaked containers pending
  cleanup"). Maintenance at 17:40:09 retried and failed (fault active). Wrapper
  and timer removed 17:41:28; the 17:45 maintenance run removed the container
  and the row. PASS. Observations: the cleanup event says "an internal error
  occurred" instead of the step error, and the final summary still says
  "candidate cleanup completed" while previous-release cleanup is pending.
Zero-downtime switch: FAIL under concurrent clients. During op_438306f7 one
  primary request (DNS-only, direct to Caddy) failed with curl 35 "Recv
  failure: Connection reset by peer" at 17:38:47.73, overlapping Caddy's
  /load at 17:38:47.67. Dense reproduction (4 parallel fresh-TLS loops on the
  primary, redeploying rel_07d1): op_a9e23a8a95ff5d75eed8933e0f530cd4 3 resets
  / 596, op_50c38192df1cf847784e547268fcaf6c 2 / 579, all within ~10 ms of the
  routes step. Earlier zero-failure runs used one sequential low-rate client.
  Cause: Caddy 2.11.4 (listen_unix.go) binds a new SO_REUSEPORT listener per
  config load and closes the old one; Linux resets connections still queued
  on the closed socket (reset after ClientHello, before ServerHello; same
  pattern as caddy.community topic 33952, no upstream fix). deploycrate
  changes routes through the admin API, which reloads the same way.
  Diagnostic: runtime-only `sysctl -w net.ipv4.tcp_migrate_req=1` on main;
  op_dc411627c7accd271fcdcc34dbb6791a 0/584,
  op_9ba884b586d44e50d2d79950612f756a 0/581 (4 loops),
  op_99521edfc690f22bc9e2e7b2fd1f1f79 0/900,
  op_94926b929ade66e8e30ecbb1a91179f3 0/851 (6 loops). Reverted to 0
  afterwards. Source fix (working tree, unpublished): new host step
  host-ingress-sysctl-v1 persists and applies the setting before Caddy.
  Requires publish, upgrade/apply and a dense continuity retest.
Rejected Caddy reload: domain remove git-test2
  (op_160c91170612fcf59d842ad0dd5bf149) succeeded, alias 525. Fault = root
  file /etc/caddy/eregion.d/zz-qualification-fault.caddy with an invalid
  directive (fragment /adapt passes, full /load rejects), 15-min removal
  guard. domain add git-test2 --check-path /users/sign-in,
  op_e33190b41329d608ab4dbde45f1081c5: failed exit 5 "Caddy rejected the
  routes (400)"; routes file restored without git-test2; git-test2 desired 1 /
  applied 0 route_state=failed, git-test1 failed state recorded but still
  serving v3. Deploy op_1542fa11ffa1d56408f9e157dbe57e33 under the fault
  failed at platform-routes-01-v1, exit 5, candidates cleaned up, rel_07d1
  still serving, monitor 131/131 200. Fault and timer removed 17:54:59;
  within 45 s periodic reconciliation applied both routes without
  resubmission: git-test1 24/24, git-test2 2/2 applied, certificate valid,
  public ok, alias `alias ready` 200 through Cloudflare. Failed operations
  stay failed. PASS (rejected reload and recovery). Concurrent/interrupted
  route changes remain NOT RUN.
Contract observation: `deploy --output ndjson` refuses with
  invalid_output_mode although deploy follows and its help says follow
  commands accept ndjson; `operations follow --output ndjson` works.
Final state: rel_07d1 serving, deployment dep_4ca1b01d314414a4fdaef2730cd1760b,
  revision 8, release command `eregion-hello release`, no pending cleanup,
  no wrapper, fault file, guard timer or sysctl change left; doctor 18/0/0.
Evidence: ~/.cache/eregion-run/93-* through 123-* (97/98/104/106/120 monitors,
  112-* and 114-* dense runs).
Remaining: publish and retest the TCP migration fix; guided create/detach,
  login/context variants, release-command recovery variants, symlink escape,
  CI/webhooks, factor-positive operations, fresh-host init and macOS install.
```

```text
Date / build / commit / channel: 2026-10-29 / edge-42a0541c09d4 / 42a0541 / edge
CI (race, integration, macOS CLI) and edge publication: pass
CLI upgrade edge-ef91d70f6215 -> edge-42a0541c09d4: pass
apply --auto-upgrade-nodes: pass; applied host-ingress-sysctl-v1 plus the
  api/worker restarts and readiness for the new binary (4 of 18); Caddy not
  restarted (active since 11:38 UTC). Second apply 0 of 18. Doctor 18/0/0;
  schema 16/16, River 7/7. Kernel value 1; boot file present.
Dense continuity retest (redeploy rel_07d1, six fresh-TLS loops on the
  direct primary plus one loop on the proxied alias):
  op_e63ac7b9a3dccba70827ac4e93ff1315 1200/1200 + 280/280,
  op_5409d44112c85916e41f0cab6fc9d08e 1366/1366 + 338/338,
  op_3757d4167c88fa5a17c703ee525f4286 1063/1063 + 269/269,
  op_d5f8122c1dee93272e3afc0360afb339 1389/1389 + 357/357; each run switched
  Caddy once. Zero failures (previous build: 2-3 resets per switch at four
  loops). Caddy reload reset fix: PASS on host.
Evidence: ~/.cache/eregion-run/124-* through 127-*.
Remaining: as in the previous record, minus the TCP migration retest.
```

```text
Date / build / commit / channel: 2026-10-30/31 / edge-42a0541c09d4 / 42a0541 / edge
Operator OS/arch: linux/amd64; CLI and main edge-42a0541c09d4
Server: existing main, Ubuntu 26.04 amd64; schema 16/16, River 7/7; doctor
  18/0/0 and second apply 0/18 before the run; no upgrade during it
App: githello/production; git-test1 direct, git-test2 Cloudflare-proxied
Release-changing dense switches (128-131; six fresh-TLS primary loops plus one
  alias loop): fresh build op_4da4ef81457eb47fc41e19bad2a70755 (commit 3461a87,
  release rel_0170b1be56a64a747d522a2831cb02d8) 4,597 primary + 967 alias
  responses (5,564 including the pre-switch phase); release redeploy
  op_4dc3431876c9f4f0e5caae4cd22d695f 1,150 + 245; rollback to
  rel_07d1c0fb27cdc0d68c7e5815a56e9328
  op_51c9ca8c40a26b61d559383bf6183866 1,049 + 227. 7,673/7,673 responses
  during the three switch windows (8,235/8,235 including the pre-switch
  phase), no resets; bodies changed v3 <-> v3 src3 at the switches. A preceding
  rollback op_ce09a4dbb762c46375677f4a19c8b6c9 also succeeded (24.4s, 61
  events). PASS.
Symlink escape (132-133): relative symlink op_e70ba096d8386648f75e73fb63025c49
  and absolute symlink op_30fe551ab69119ebc83d3eecb1226118 both failed
  precondition_failed naming context path "apps/web" absent/outside; previous
  release kept serving. PASS.
Same-key replay after branch deletion (134-136): key qual-replay,
  op_f82e084105cafb638dfd52f6b7106445 succeeded (release
  rel_ab664af01d2828346c363defe223e01e); replaying the same key returned the
  same succeeded operation without another deploy (135); a new key was refused
  because the branch was gone (136, precondition_failed). PASS.
Failed release-command rollback (137-140): env update to command `false`
  op_d6e7c1e706c0f9d51699e5caf753b888; deploy
  op_af19655248dabe3766a06b28b52c4a89 failed at platform-command-01-v1 (exit
  status 1) and rel_ab664af01d2828346c363defe223e01e kept serving; rollback to
  rel_73e26b4973dd4d1deef36aed0e8ab334 was refused at submission
  (precondition_failed, "release command has not succeeded for this release");
  command restored op_d595b0296338811caf53a97477b323a3. PASS.
Failed candidate with removal failure (141-146): a temporary docker wrapper
  failed only the candidate web-1 removal (30-minute guard); FAIL_HEALTH config
  op_0cccf6cb8ab9b041e0287871a0f2c385; deploy
  op_308fae9fa06fe611113e1bb90381fbf7 failed health 503 and the previous
  release kept serving; its cleanup step failed with the injected error and
  144 showed pending_cleanup ["githello-production-web-1-037149ee58d2"].
  Fault removed 09:04:26; FAIL_HEALTH unset
  op_c34b332d6bfab5d8880bb2114d4ac91f. Candidate-cleanup warning was correct,
  but the final summary still said "candidate cleanup completed" (defect 2).
  No post-removal env status was saved, so the later maintenance removal was
  not captured in 141-146. PASS for isolation and leak recording; removal not
  evidenced.
Interleaved domain changes and worker kills (147-150): add git-test3
  op_adab749ca783474b144b1c08b8414ead, add git-test4
  op_79bb8ebc76a8ba5c57e1705e40b165b7, remove git-test3
  op_53147c6f3d81cd1b7d2d2d96a917d081, remove git-test4
  op_20732130eeeb39f4262243381bd62da8 (follow not saved), re-add git-test3
  op_f0d20fed6c26eb6bd138c84bd211e170. Monitor 147: 7,074/7,074 HTTP 200
  (3,537 primary + 3,537 alias). SIGKILLs at 09:06:50 and 09:07:21 left the
  re-add running until 09:37:39 (elapsed 30m21s), then it succeeded. DEFECT 3.
Dev-CLI dry-run mismatch (151-152): `apply --dry-run --auto-upgrade-nodes`
  refused node_version_mismatch and `server status` refused with the same code;
  node binary SHA-256, size and service start timestamps were unchanged
  before/after (152-node-*). 151 config set
  op_cf611159edfef3dc7880f943aadb1455 succeeded; no lock left. PASS.
Detach and cancel (153-154): real-terminal Ctrl-C detached from
  op_6f4a7968f5823f71cd1003ec33a81293 after 39 events, exit 130, with the
  follow hint; `operations follow` replayed it to success in 26.8s with
  rel_0170b1be56a64a747d522a2831cb02d8 serving. Explicit `operations cancel`
  op_39546ea45e1125630c1d8011a093f39e reached cancelling at sequence 14 and
  cancelled at a step boundary (19.6s); rel_0170b1be56a64a747d522a2831cb02d8
  still served. PASS.
Live `build logs --follow`: streamed in a real terminal; no numbered evidence
  file or operation ID was saved. PASS observed; evidence gap.
Defects:
  1. Caddy's public path `/v1/environments/*/deployments` let `*` match one
     segment, so `/v1/environments/<app>/<env>/deployments` returned Caddy's
     404. Fixed in 2943e18 with `*/*` and a matcher regression test; not yet
     host-retested on main.
  2. A cleanup failure was summarised as "candidate cleanup completed".
     Observed in 143 (cleanup warning plus "completed" summary) and 144
     (pending_cleanup). Fixed in 4e827aa: cleanup failures are warning events
     naming the containers and failed state is retained; not host-retested.
  3. A worker SIGKILL leaves its platform operation running until River's
     rescue, about 30 minutes (30m21s observed on op_f0d2...). The fix was then
     in progress for alpha.1 (rescue this host's orphaned platform jobs on
     worker start).
Final state: rel_0170b1be56a64a747d522a2831cb02d8 serving; git-test3 re-added,
  git-test4 removal accepted without a saved follow; no post-session env status
  was captured. No injected fault, wrapper or guard timer was left; no upgrade
  ran during the session.
Evidence: ~/.cache/eregion-run/128-* through 154-*.
Remaining: the full baseline on the candidate build from a freshly initialized
  server (R3, R11), including guided create, CI, push to deploy, factor-positive
  operations, login/contexts, retention and the other open rows.
```

```text
Date / build / commit / channel: 2026-11-03 / edge-a77d3bfa4fe6 and
  edge-13fdb2518757 / a77d3bf and 13fdb25 / edge
CI and publication: a77d3bf CI and Publish edge pass (run 41691456825);
  13fdb25 CI and Publish edge pass (run 41692644196). Publication of the two
  later master commits was not used by this run.
Operator OS/arch: linux/amd64. CLI upgrade edge-42a0541c09d4 ->
  edge-a77d3bfa4fe6: pass, previous binary kept at ~/.local/bin/eregion.prev
  (reports edge-42a0541c09d4). CLI upgrade edge-a77d3bfa4fe6 ->
  edge-13fdb2518757 with `upgrade --version` for the second-build check: pass,
  previous binary reports edge-a77d3bfa4fe6.
Server: existing main, Ubuntu 26.04 amd64; before the run CLI and node
  edge-42a0541c09d4, schema 16/16, River 7/7, doctor 18/0/0.
Safe first checks (155-163): pass. Context main reaches admin@10.78.0.1 over
  WireGuard; server status role=main, all five services active, 57.4 GiB free;
  githello/production serving rel_0170b1be56a64a747d522a2831cb02d8 with web x2
  and worker x1; git-test1 200 "eregion sample: v3 src3", git-test2 200
  "alias ready". Only the leftover git-test3 alias remains from the 2026-10-30
  domain churn (dns=wrong, certificate/public pending); git-test4 is gone.
Node upgrade dry run (42a0541 -> a77dbf): pass, exit 5, node_version_mismatch
  "would upgrade node ..."; node binary sha256/size/mtime and API, worker and
  Caddy start timestamps identical before and after (169 vs 171).
Node upgrade (`apply --auto-upgrade-nodes`): pass, exit 0, 18:17:36-18:17:51,
  op_74a0c28c3ed7ca35e57c297fbf6a8419, 4 applied of 18: host-ingress-v2 (the
  CI path matcher fix; Caddyfile now /v1/environments/*/*/deployments), API and
  worker restarts, readiness. Node now edge-a77d3bfa4fe6. Caddy was reloaded
  by the ingress step, not restarted (caddy.service ActiveEnterTimestamp stayed
  2026-10-29 11:38:24 UTC). Monitor 170: 306/306 HTTP 200 (102 each on
  git-test1, git-test2 and andurel's eregion-test), zero failures.
Second apply: pass, op_5b4b1b9f317d0c2673349d350a796d5d, 0 applied of 18, no
  step executed. Doctor: 18 pass / 0 warn / 0 fail.
CI endpoint retest on the main hostname db2.mbvlabs.com (read from the managed
  Caddyfile, 177-179): GET
  /v1/environments/githello/production/deployments returns HTTP 405,
  allow: POST, with the API's {"error":"use POST ...","code":"usage_error"}
  (not Caddy's 404); POST without a token returns 401 unauthenticated "exactly
  one Bearer token is required"; /v1/apps returns Caddy's 404. Defect 1 host
  retest: PASS. No deploy was submitted on the CI path.
Defect 3 retest (180-183): deploy op_d0f76b56af719f63cb8141fa0c6f61c4 (fresh
  build of 3461a87) submitted 18:21:23; eregion-worker.service SIGKILLed at
  18:21:41 (systemd "on client request"); restart 18:21:46; the new worker
  logged "Recovered orphaned platform jobs before worker start" with
  requeued=1 at 18:21:48; operations follow showed the warning "Recovered
  orphaned platform job 167151 left running by an earlier worker process on
  this host" at 18:21:48.660; attempt 2 started at 18:21:50.543. Rescue latency
  about 9s, not 30 minutes. PASS for defect 3.
  The rescued attempt then FAILED at 18:21:54 with precondition_failed
  "git read credential is unavailable": bld_fc680448ac7db566b4d0dae3cd5a15f6
  and rel_53a22cfea2e1f3a2fa9dd523ac4c5510 failed; rel_0170b1be... kept
  serving; follow exit 5. This is defect 4 (below), not a recurrence of the
  30-minute stall. A fresh deploy of the same revision,
  op_5e277f6637276de54c3011fc1fc52a0f, succeeded 18:23:25-18:25:19 and built
  bld_48d3acf6d2555726880f191d12a5d1bc -> release
  rel_b38312bd825f8c7e66ae3fe906148795, deployment
  dep_7f256e534322e9bf0fc627047c5523d4. So the credential itself is intact;
  monitor 180: 1,773/1,773 HTTP 200, zero failures.
Failed-manifest check (a77dbf -> 13fdb25, 188-194): pass. The CLI was moved to
  the exact signed edge-13fdb2518757; `apply --dry-run --auto-upgrade-nodes`
  refused node_version_mismatch. Temporary scoped fault: /etc/hosts on main
  mapped eregion.mbvlabs.com to 127.0.0.1, with a copied backup and a
  10-minute systemd-run restoration guard. The node's resolver returned
  127.0.0.1 and its manifest fetch failed. `apply --auto-upgrade-nodes` then
  failed closed: exit 6, unavailable "release URL is unreachable", naming
  https://eregion.mbvlabs.com/releases/edge-13fdb2518757/manifest.json. The node
  binary sha256/size/mtime and API, worker and Caddy start timestamps were
  identical to before the attempt, and no /tmp/eregion-* upload was left; there
  was no checksum-only fallback. The fault was removed (original /etc/hosts
  restored, backup deleted, timer stopped, marker count 0) and the manifest
  fetch returned 200 again.
Completed upgrade (`apply --auto-upgrade-nodes`): pass, exit 0, 18:28:32-18:28:46,
  op_f9559862af54228b02167a95f2ff3615, 4 applied of 19: the new
  host-journal-access-v1 (admin added to systemd-journal), API and worker
  restarts, readiness. Node now edge-13fdb2518757, binary sha256
  aefec7b9f65ccbb94e5766e6e5cd44d2d2edb0f5d87226615421a7a2a540d7e9, exactly
  the signed manifest's linux/amd64 artifact. Caddy was not restarted (same
  ActiveEnterTimestamp). Monitor 195: 300/300 HTTP 200, zero failures.
  Second apply op_3a20c78b82b65cb6054c8ef72a1f0432: 0 applied of 19. Doctor:
  18 pass / 0 warn / 0 fail (the new step adds no doctor check). As admin,
  `journalctl -u eregion-worker.service` and `-u eregion-api.service` now
  return entries without sudo, exit 0 (199).
Defects:
  1. Fixed in 2943e18 and now host-retested: the API's 405/401 replace Caddy's
     404 on the CI deploy path; /v1/apps stays Caddy's 404.
  3. Fixed in a77d3bf and host-verified: SIGKILL rescue in about 9s with a
     warning event naming the orphaned job (was 30m21s).
  4. NEW, open: a rescued/retried deployment attempt that already recorded its
     deployment secret snapshot loses the git read credential and fails at the
     build step (op_d0f76b56af719f63cb8141fa0c6f61c4 attempt 2,
     "git read credential is unavailable") although GIT_READ_TOKEN revision 1
     is active and a fresh deploy of the same revision succeeded. The snapshot
     pins include the credential, but platform_deploy.go only resolves the
     credential into buildSecrets when len(dep.SecretRevisions)==0 and discards
     the resolver's credential value on the snapshot path, while the credential
     is not in the revision's secret_names. No code was changed.
Final state: rel_b38312bd825f8c7e66ae3fe906148795 serves githello/production
  (dep_7f256e534322e9bf0fc627047c5523d4, health healthy); git-test1 and
  git-test2 route=applied, certificate=valid, public=ok; git-test3 remains
  dns=wrong/pending; no pending cleanup; CLI, node and publish line
  edge-13fdb2518757; schema 16/16, River 7/7; doctor 18/0/0; no injected
  fault, hosts backup or guard timer left.
Evidence: ~/.cache/eregion-run/155-* through 207-* (monitors 170, 180, 195;
  rescue journal 183; blocked manifest 193; node before/after 190/193/198).
Remaining: defect 4; the full baseline on a freshly initialized server (R3,
  R11), including guided create, an authenticated CI deploy, push to deploy,
  factor-positive operations, login/contexts, retention and the other open
  rows. The Upgrades row now has the two-edge-build upgrade and failed-manifest
  checks; `install.sh` on Linux/macOS and macOS remain not run (R10).
```

```text
Date / build / commit / channel: 2026-11-03 (evening) / edge-1e541a7fad3e /
  1e541a7 / edge
CI and publication: 1e541a7 CI and Publish edge pass (runs 41718550479,
  41718550856).
Checks: safe first checks pass; CLI upgrade pass; apply dry run expected
  node_version_mismatch; apply/node upgrade pass; defect 4 retest pass;
  retention policy pass; prune dry run pass; real prune blocked (defect 5);
  second apply pass; doctor 19/1 (not the expected 20/0/0).
Operator OS/arch: linux/amd64. CLI upgrade edge-13fdb2518757 ->
  edge-1e541a7fad3e: pass; previous binary kept at ~/.local/bin/eregion.prev
  (reports edge-13fdb2518757). CLI upgrade happened before any node mutation.
Server: existing main, Ubuntu 26.04 amd64; before the run CLI and node
  edge-13fdb2518757, schema 16/16, River 7/7, doctor 18/0/0.
Safe first checks (208-219, 222-223): pass. Context main reaches
  admin@10.78.0.1 over WireGuard; server status role=main, all five services
  active, 56.8 GB free; githello/production serving
  rel_b38312bd825f8c7e66ae3fe906148795 with web x2 and worker x1;
  andurel/production serving rel_801151fb866e89e87e017fe9c5c0042c; git-test1 200
  "eregion sample: v3 src3", git-test2 200 "alias ready", andurel 200; TLS
  verified with the default CA store (no -k) before, during and after.
  Pre-state: 18 githello and 10 andurel releases, none pruned; registry
  du 312,003,409 bytes; 26 Docker images; Caddy start 2026-10-29 11:38:24.
Apply dry run (229): expected refusal, exit 5 node_version_mismatch "would
  upgrade node" - a dry run never installs a node binary.
Apply and node upgrade (230-233): pass, exit 5 confirmation_required after
  20.35s, op_502d5afadf4f948aaeca090418483413, because host-registry-v2 changes
  the registry unit/access. Confirmed on the node at 21:18:16.917 before the
  21:18:22.735 deadline (231). host-registry-v2 applied once: config
  delete.enabled true, unit REGISTRY_STORAGE_DELETE_ENABLED=true, registry
  restarted 21:16:55. host-database-v1 applied migrations 017-019 (schema
  19/19, River 7/7), API and worker restarted, readiness passed. Node now
  edge-1e541a7fad3e, binary sha256
  e2c3abb1fc5cebe3f879b8b8c300aee21e8bcaf1890648b8e1f342df85091711. Caddy was
  not restarted (ActiveEnterTimestamp stayed 2026-10-29 11:38:24 UTC).
  Monitor 228: 633/633 HTTP 200 across the dry run, apply, confirmation and
  second apply (211 per URL), zero failures; no public request depended on the
  registry restart.
Second apply (234): pass, no operation ID printed, 0 applied / 19 skipped, 5.78s.
  Doctor (230, 275): 19 pass / 1 warn / 0 fail. host.retention WARN: the
  startup daily run failed at 21:18:26 with 1 error. host.build_cache pass with
  unknown (no successful measurement). The no-run-yet host.retention pass was
  not observable: the startup catch-up submitted op_c90b2c9a… at 21:17:09.
  This is defect 5, not a 20/0/0 pass.
Defect 4 retest (240-246): pass. Deploy op_181c9da0f94c37ff6784c08201d366b1
  submitted 21:21:10; fresh git build started 21:21:19; worker SIGKILLed
  21:21:29; systemd restarted it 21:21:37; rescue logged at 21:21:39
  (requeued=1), warning event at 21:21:38.904 naming orphaned job 169781;
  attempt 2 started 21:21:40.980, retried platform-pack-01-v1, cloned the
  private repository again at 21:21:47.874 (git read credential available) and
  succeeded at 21:22:59.464 as rel_b7099da06361a0bfaa8ee6866e178152 /
  dep_51d61163648e39d6adf4be5cdef005d3. Monitor 240: 423/423 HTTP 200
  (141 each), zero failures. Defect 4 is fixed and host-verified.
Retention policy (236-252, 274): pass. Server default show is scope=server with
  keep_releases 10, keep_newer_than_days 14, artifact_grace_days 14, cache
  10 GiB/80%, every source server. githello override set keep-releases 3,
  keep-days 1, artifact-grace-days 1: accepted op_ba80a58d26ba832bf5325b022878cb1a,
  succeeded, new environment revision erv_b8f5163889b07167f955396f2f5da061
  number 15. Show marks exactly the three release fields environment and both
  cache fields server. Flagless set with --json refuses exit 2 input_required
  without prompting (248, 274).
Prune dry run (253-267): pass. Plain 258, JSON 259, repeat 265 identical.
  30 items: 16 release (all githello), 5 image, 6 image_skipped, 1
  registry_skipped, 2 dangling_image; planned_bytes 1,355,246,769 (only the
  dangling images carry bytes; release rows have no bytes and image items do
  not attribute shared-layer bytes). Kept: githello serving
  rel_b709 and rollback rel_b383 (image 5e277f663727), andurel serving rel_801
  and rollback rel_a68 (digest 56166f); no andurel items. The plan lists the
  older rel_0170 because it is no longer the rollback target after the new
  deploy. No operation accepted (108 before/after, 256/266); githello and
  andurel release rows and pruned_at unchanged; Docker 27 images / 9 containers
  unchanged; registry du 312,003,409 unchanged and /v2 200; /var/lib/eregion
  at 27% used before and after. The plan was safe to execute.
Real prune (5c): NOT RUN - blocked by defect 5. The automatic daily retention
  op_c90b2c9a9a1e6f0430dd13aec337ab40 failed its 8th attempt at
  21:18:27.076 with unavailable "retention did not complete: the node's
  registry storage is unreadable". /var/lib/eregion/registry is root:root 0700
  (host-directories-v1); eregion-worker runs as uid 999 eregion and
  `sudo -u eregion test -r` is false (233, 238). The operator dry run works
  because its node RPC path runs as root, masking the worker path
  (retention_data.go retentionInventory). A real prune cannot plan until the
  worker can read the registry, so pruned-rollback refusal, registry GC,
  deploy-after-GC, disk-freed verification and the 30-release row are not run.
Second apply after the deploy (270-273): pass, 0 applied / 19 skipped, 9.98s;
  schema 19/19; node and CLI edge-1e541a7fad3e; Caddy timestamp unchanged;
  registry config/unit persisted. Doctor (271) remains 19/1 because of defect 5.
Defects:
  4. Fixed in 1e541a7 and host-verified: a rescued deployment attempt now finds
     the pinned git read credential and finishes the private clone/build
     (op_181c9da0..., attempt 2; 240-246). No code was changed in this run.
  5. NEW, open: worker-run retention cannot read the root:root 0700 registry
     storage; the daily run fails unavailable and doctor warns. The real prune
     and all checks after it are blocked. The operator surface's root RPC path
     hides the mismatch. Needs a release fix (worker-readable registry storage
     or a root inventory path), not a host chmod.
Final state: rel_b7099da06361a0bfaa8ee6866e178152 serves githello/production
  (dep_51d61163648e39d6adf4be5cdef005d3, health healthy); git-test1 and
  git-test2 route=applied, certificate=valid, public=ok;
  andurel/production still serving rel_801151fb866e89e87e017fe9c5c0042c; no
  pending cleanup; no injected fault left; CLI/node edge-1e541a7fad3e; schema
  19/19, River 7/7; doctor 19/1.
Evidence: ~/.cache/eregion-run/208-* through 277-* (summary 277; monitors 228,
  240; apply 230/231/234/270; rescue 242-244; retention failure
  232-233/238/268/275).
Remaining: defect 5; the real prune, pruned-rollback refusal, registry GC,
  deploy-after-GC, disk-freed verification and 30-release retention; then the
  full baseline on a freshly initialized server (R3, R11), including guided
  create, an authenticated CI deploy, push to deploy, factor-positive
  operations and login/contexts. `install.sh` on Linux/macOS and macOS remain
  not run (R10).
```

```text
Date / build / commit / channel: 2026-11-03 (late; 2026-11-04 UTC) /
  edge-690baafe8c91 / 690baaf / edge
CI and publication: 690baaf CI and Publish edge pass (runs 41743084793,
  41743084750).
Checks: CLI upgrade pass; apply and host steps pass (host-packages-v2 skipped:
  nothing missing); real prune partial then failed (defect 6); pruned-rollback
  refusal pass; second apply pass; doctor 19/1, not 20/0/0.
Operator OS/arch: linux/amd64. CLI upgrade edge-1e541a7fad3e ->
  edge-690baafe8c91: pass; previous binary kept at ~/.local/bin/eregion.prev
  (reports edge-1e541a7fad3e).
Server: existing main, Ubuntu 26.04 amd64; before the run CLI/node
  edge-1e541a7fad3e, schema 19/19, River 7/7, doctor 19/1 (the failed Nov 3
  retention run); githello serving rel_b7099da06361a0bfaa8ee6866e178152;
  19 githello and 10 andurel releases, none pruned.
Apply dry run (288): expected refusal, exit 5 node_version_mismatch; no node
  binary was installed.
Apply (289): pass, 00:05:58-00:06:18, exit 0, applied 4 of 20:
  host-directories-v2 (registry 0700 root:root -> 0750 root:eregion),
  host-registry-gc-polkit-v1 (rule 0644 root:root), host-api-service-v1 and
  host-worker-service-v1. host-packages-v2 was skipped because all requested
  packages, including polkitd 127-2ubuntu1.1, were already installed; no apt
  install ran, so its install-only path was not exercised. Docker 29.1.3 /
  containerd 2.2.2 and their start timestamps (2026-10-27 08:32:52 /
  2026-10-28 06:07:20) were unchanged; Caddy stayed 2026-10-29 11:38:24.
  `sudo -u eregion test -r /var/lib/eregion/registry` returned true; the rule
  is 0644 root:root. `sudo -u eregion systemctl restart
  eregion-registry.service` succeeded, the registry returned active with
  /v2/ 200 (312).
Automatic daily prune (the real prune): the worker startup catch-up submitted
  op_0f541f120149580a1124893389f720b2 at 00:06:11. It ran as
  rtr_7554300ccfa037b24533b67d2e5d0110 and failed attempt 8 at 00:12:28
  `unavailable`. 16 githello release rows were marked pruned
  00:06:33.980-00:06:43.642, exactly the 16 planned at 259; serving rel_b709
  and rollback rel_b383 stayed; andurel stayed at rel_801 with 0 pruned. Two
  artifacts were removed: image 4e2b9afed6f5 (tag f3c8d98c922d) and dangling
  image 8a671e0f8a5b (46,701,175 bytes).
  Defect 6: four images failed `docker image rm <id>` because Docker refused
  an image referenced by multiple tags: 86d86a57796d (2 tags), 6c6a3f447152
  (2), 1ef45b4e792c (4), 871c4b7859aa (3); stderr "image is referenced in
  multiple repositories". The 1,308,545,594-byte dangling image bd03726c07e9
  failed because unlabeled stopped container ea1e68560301
  (`pedantic_jackson`, exited 5 days) still uses it; the inventory only sees
  `io.eregion.app`-labeled containers. Each attempt recorded 5 failures;
  attempts 2-8 recorded 35 errors. The retry dry run 311 still lists the four
  image and one dangling item (plus the registry_skipped diagnostic).
Monitoring (287): 2,190/2,190 HTTP 200 (730 per URL) from 00:05:53 to 00:14:56,
  covering the apply, the whole prune and the rollback refusals; githello and
  andurel never failed.
Retention, GC and disk: host.build_cache now passes with a known 4.7 GiB of
  the 10.0 GiB cap (298); host.retention warns with 35 errors. The registry
  stayed active and /v2/ 200; registry du stayed 312,003,409 and no
  garbage-collection event ran because the plan had no registry_artifact
  items. The polkit restart check (312) is not a retention GC. Disk used
  20,588,044,288 -> 20,567,863,296 (20,180,992 bytes freed); Docker tag count
  27 -> 26, unique images 16 -> 14, containers 9. The 1.3 GB dangling image
  and four images remain.
Pruned rollback: pass. `rollback githello production --to
  rel_0170b1be56a64a747d522a2831cb02d8 --dry-run --json` and the real
  `--confirm githello/production` both refused exit 5 precondition_failed,
  naming the release and its 2026-11-04T00:06:41Z prune time (305, 306).
  Rollback to the kept target was not run.
Second apply (307): pass, 0 applied / 20 skipped, 6.65s, 00:14:42-00:14:48.
  Doctor (308): 19 pass / 1 warn / 0 fail; host.retention warns.
Defect 5: fixed and host-verified (directory ownership/mode, worker read,
  inventory run, polkit restart). Defect 6: new, open, as above.
Final state: rel_b7099da06361a0bfaa8ee6866e178152 serves githello/production;
  andurel/production still serves rel_801151fb866e89e87e017fe9c5c0042c; both
  public 200 (314); CLI/node edge-690baafe8c91; schema 19/19, River 7/7; doctor
  19/1; Docker, containerd and Caddy were not restarted; no injected fault
  left.
Evidence: ~/.cache/eregion-run/278-* through 314-* (monitor 287; prune
  295/297; node 303/304/310; polkit 312; summary 313; final curls 314).
Remaining: fix defect 6 (force or untag multi-repository image removal and
  protect or remove images used by unlabeled stopped containers); rerun the
  real prune and verify the remaining images, registry GC, deploy after GC,
  disk freed and 20/0/0 doctor; then 30-release retention and the full baseline
  on a freshly initialized server. The fresh githello deploy after GC was not
  run because the prune failed and GC did not run.
```

```text
Date / build / commit / channel: 2026-11-04 / edge-786fdcfda4a8 / 786fdcf /
  edge
CI and publication: 786fdcf CI and Publish edge pass (runs 41750854190,
  41750854160).
Checks: CLI upgrade pass; apply pass (migration 020; 4 of 20 steps); daily
  automatic run blocked by the used UTC-day key (finding); manual prune pass
  (4 images removed, defect 6 fixed); registry GC not exercised (no eligible
  artifacts); second apply pass; doctor 20/0/0, with intermittent control-DB
  read warnings.
Operator OS/arch: linux/amd64. CLI upgrade edge-690baafe8c91 ->
  edge-786fdcfda4a8: pass; previous binary kept at ~/.local/bin/eregion.prev
  (reports edge-690baafe8c91).
Server: existing main, Ubuntu 26.04 amd64; before the run CLI/node
  edge-690baafe8c91, schema 19/19, River 7/7, doctor 19/1 (the failed Nov 4
  retention run); 19 githello releases with 16 pruned, 10 andurel releases with
  0 pruned; 26 Docker tags / 9 containers; registry du 312,003,409.
Apply dry run (327): expected refusal, exit 5 node_version_mismatch; no node
  binary installed.
Apply (328): pass, 01:08:12-01:08:35, exit 0, applied 4 of 20:
  host-database-v1 (schema 19/20 -> 20/20, migration 020), host-api-service-v1,
  host-worker-service-v1 and host-readiness-v1. Worker startup raced the
  migration: three restarts logged `precondition_failed: database migrations
  are pending` (01:08:16-01:08:32), one logged `unavailable: control-plane
  database is unavailable` (01:08:45), then it became active at 01:08:51 and
  River started at 01:08:56. Apply still exited 0 after readiness. Schema
  20/20, River 7/7.
Daily-key finding: after the restart the maintenance jobs ran (01:09:01-16),
  but `operations list` gained no 2026-11-04 daily retention operation.
  op_0f541f120149580a1124893389f720b2 remained failed and was not retried; the
  UTC-day idempotency key was already bound to that terminal operation. A
  manual prune was required (320, 332, 335, 337, 357).
Manual prune: dry run 339 returned 6 items and planned_bytes 0: four `image`
  items with their verified tag lists (86d86a: 2 tags; 6c6a3f: 2; 1ef45b: 4;
  871c4b: 3), one `image_skipped` for the 1,308,545,594-byte dangling image
  bd03726c07e9 with "used by container pedantic_jackson", and the
  `registry_skipped` diagnostic. No andurel items; serving rel_b709 and
  rollback rel_b383 were not candidates.
`prune --confirm main --detach` accepted
  op_11f4602b7a2fd234d352779d74f7bd4a at 01:10:29. The operation succeeded in
  one attempt at 01:11:22.982: it planned 4 items, untagged all 11 tags,
  deleted 86d86a, 6c6a3f, 1ef45b and 871c4b, recorded no errors, and left the
  dangling image skipped. The run status is succeeded with 4 items removed and
  0 B reclaimed. The post-run dry run has only the `registry_skipped` and
  `image_skipped` diagnostics (342, 344).
Registry GC: none planned, so none ran and no deploy-after-GC was performed.
  The `registry_skipped` reason is exactly: release
  rel_53a22cfea2e1f3a2fa9dd523ac4c5510 has no immutable image digest or config
  digest that resolves to a registry inventory artifact (image
  "eregion/githello-production:d0f76b56af71"). That release is failed, has
  `image_id` null and no local image; it is retained as the third-newest
  release. The registry catalog contains only `andurel`, whose 10 releases are
  all retained. This is acceptable fail-safe over-skip, not a normal-git-build
  ConfigDigest/RepoDigests mismatch (339, 345).
Monitoring (326): 1,200/1,200 HTTP 200 (400 per URL) from 01:08:07 to 01:13:07,
  covering the apply, both dry runs, the manual prune and the second apply;
  githello and andurel never failed. Final curls 354 are 200.
Disk and registry: /var/lib/eregion used 20,568,334,336 -> 20,425,904,128
  (142,430,208 bytes freed); Docker tags 26 -> 15, unique images 16 -> 10,
  containers 9. The 1.3 GB dangling image and the stopped container remain by
  design. Registry stayed active, `/v2/` 200, du 312,003,409 unchanged,
  ActiveEnterTimestamp 2026-11-04 00:16:03 unchanged, and no garbage-collection
  event appeared (348).
Doctor: 347 and 352 are 20 pass / 0 warn / 0 fail. host.retention passes with
  "last retention run 2026-11-04T01:11:20Z succeeded: 4 item(s) removed, 0 B
  reclaimed"; host.build_cache passes with a known 4.7 GiB of 10.0 GiB. Five
  repeated runs (353) took 4.9-23.5s and returned 20/0/0 three times; two runs
  returned 19/1 with `host.retention` "cannot read the retention runs"
  (317, 350, 351, 353). The successful reads are the target state; the warning
  has an unknown underlying cause; these results do not establish the external
  control-DB/Tailscale path as the cause.
Second apply (346): pass, 0 applied / 20 skipped, 5.70s, 01:12:18-01:12:23.
Releases remained 19 githello with 16 pruned (serving rel_b709, rollback
  rel_b383) and 10 andurel with 0 pruned (serving rel_801). No new prune of
  serving or rollback targets.
Defect 6: fixed and host-verified by the manual prune above. Defect 5 remains
  fixed and host-verified.
Final state: githello and andurel serve publicly; CLI/node
  edge-786fdcfda4a8; schema 20/20, River 7/7; doctor 20/0/0 on successful DB
  reads; Docker, containerd and Caddy were not restarted; no injected fault
  left.
Evidence: ~/.cache/eregion-run/315-* through 358-* (monitor 326; prune follow
  342; registry-skip evidence 345; doctor loop 353; final curls 354; summary
  355; final environment/app/server state 356–358).
Remaining: registry GC and deploy-after-GC coverage; the 30-release acceptance;
  the daily-key behavior needs a product decision; the full baseline on a
  freshly initialized server. The fresh githello deploy after GC was not run
  because no registry artifact was eligible for GC.
```

```text
Date / build / commit / channel: 2026-11-04 / edge-0a1a492d8207 / 0a1a492 /
  edge (candidate check)
CI and publication: 0a1a492 CI and Publish edge pass (runs 41755555642,
  41755555690).
Checks: CLI upgrade pass; apply pass (2 of 20, no migration race); daily retry
  submitted and succeeded (manual prune not counted); imageless-release
  registry_skipped gone; doctor 20/0/0 three times; second apply no-op;
  restart succeeds; monitor 1,212/1,212 HTTP 200.
Operator OS/arch: linux/amd64. CLI upgrade edge-786fdcfda4a8 ->
  edge-0a1a492d8207: pass; previous binary kept at ~/.local/bin/eregion.prev
  (reports edge-786fdcfda4a8).
Server: existing main, Ubuntu 26.04 amd64; before the run CLI/node
  edge-786fdcfda4a8, schema 20/20, River 7/7, doctor 20/0/0; 19 githello
  releases with 16 pruned, 10 andurel with 0 pruned; 15 Docker tags, 9
  containers; registry du 312,003,409.
Apply dry run (370): expected refusal, exit 5 node_version_mismatch; no node
  binary installed.
Apply (371): pass, 01:46:41-01:46:54, exit 0, applied 2 of 20:
  host-api-service-v1 and host-worker-service-v1. No new migration and no
  pending-migration race: the worker logged one transient
  `unavailable: control-plane database is unavailable` at 01:46:54, then
  restarted and started River at 01:47:11. Schema stayed 20/20, River 7/7;
  Docker, containerd and Caddy timestamps unchanged.
Second apply (384): pass, 0 applied / 20 skipped, 11.54s, 01:49:51-01:50:02.
Daily retry (the key check): the worker's on-start hourly check submitted
  op_9b72a9e32c2b5b49351f3ba75edb4664 at 01:47:24, after the failed
  op_0f541f120149580a1124893389f720b2 had been terminal for more than one
  hour. It planned 0 items, 0 bytes and succeeded at 01:48:03. The manual prune
  op_11f4602b7a2fd234d352779d74f7bd4a (01:11) was not counted as today's
  success: the submitter queries only `maintenance` operations whose key starts
  `retention-daily:<date>`, and the manual prune is an operator operation. This
  matches the implemented submitter but makes docs/IMPLEMENTATION's “at most
  one run succeeds per UTC day” literally misleading: two retention runs
  succeeded today (manual 01:11, automatic 01:48); the sentence should say
  “one successful automatic daily run”.
Dry run (378, 379): 1 item, `image_skipped` for
  `sha256:bd03726c07e9e32c45151e211789e707e6a867adaeaacc670140901b190ee125`,
  reason "used by container pedantic_jackson"; `planned_bytes` 0. The githello
  `registry_skipped` is gone after the imageless-release fix, and no andurel
  items appear.
Doctor (380-383): 20 pass / 0 warn / 0 fail in all three consecutive runs;
  post-restart doctor (391, 392) is also 20/0/0. `host.retention` reads the
  latest daily run at 01:47:57Z succeeded with 0 items removed; `host.build_cache`
  passes with 4.7 GiB of the 10.0 GiB cap. No transient control-DB read failure
  occurred in this check.
Restart (385, 386): op_94933ee7304f03349f1b428fe172b708 submitted 01:50:08,
  succeeded at 01:51:13 with the public check
  https://git-test1.mbvlabs.com/healthz → 200. It kept
  rel_b7099da06361a0bfaa8ee6866e178152 serving as
  dep_390ff2d6904bce7d83fb3e051307f08b; env status is healthy.
Monitoring (369): 1,212/1,212 HTTP 200 (404 per URL) from 01:46:37 to 01:51:39,
  covering apply, the daily run, dry run, second apply and restart. Final
  curls 388 are 200.
Registry GC: no `registry_artifact` item was planned; the registry still holds
  only retained `andurel` artifacts, registry du is 312,003,409, `/v2/` is 200,
  and no GC ran. The deploy-after-GC check therefore was not applicable.
Final state: githello rel_b709 healthy, andurel rel_801 public ok; CLI/node
  edge-0a1a492d8207; schema 20/20, River 7/7; doctor 20/0/0; no injected fault
  left.
Evidence: ~/.cache/eregion-run/360-* through 393-* (monitor 369; daily follow
  377; dry run 378; doctor 380-383; restart follow 386; final state 389-392;
  summary 393).
Remaining: registry GC and deploy-after-GC coverage; the 30-release acceptance;
  the full baseline on a freshly initialized server. No open retention defect
  is observed in this candidate check.
```

### 2026-11-05 — edge-527592419f06 release/install and final fresh-server batch

Build `edge-527592419f06` (commit `527592419f06`), published from `5275924`;
CLI and node both this build; schema 21/21, River 7/7; API `MainPID 157182`,
`ActiveEnterTimestamp 2026-11-05T15:25:47Z`, `NRestarts=0`; doctor 20/0/0;
read-only plan 0 of 20 changes. The edge workflow succeeded and the signed
manifest and linux/amd64 artifact verified with cosign; migration 021 was
applied by the upgraded node binary. All four serving URLs
(git-test1, git-test2/users/sign-in, git-test3, git-test5) returned 200
throughout.

**Step 1, token reload: pass.** A fresh `ci-reload` token for
`deployab/production` was used against the public CI endpoint: deploy 202,
same-actor status 200, revoke, same status 401 `unauthenticated`; the
disposable operation `op_72aae6743ed3b4151b131f3825b78b04` is terminal
(`failed` at git clone) and its recovery record says `phase=terminal`.
`ci-reload` is absent; the existing `ci-valid`, `ci-expiring` and
`ci-otherenv` metadata is unchanged (3 tokens). Evidence `840-host-token-*`,
`840-host-token-reload-recovery-20261105T204024Z-114225582.json`.

**Step 2, individual secret export metadata: pass (operator-reported).**
The human exported one secret to a protected file, saw mode 600/size 32, and
removed it. Only the stat metadata is recorded
(`840-host-secret-export-metadata.txt`); the destination is absent, so no
independent post-deletion permission check is claimed.

**Step 3, disposable env delete: pass.** `deployab/previewtwo` delete
`op_f3532c3264cfaa9ed25540f469efe6b6` succeeded; `deployab/production` and its
desired state (`erv_57b47993bff9177051a60d53ef4ae97b`) remain; the delete
history is retained.

**Step 4, disposable app delete: pass.** `packcancel` app delete
`op_7f003359f6234a00f6a61ade274ed6a2` succeeded; the app is gone, the delete
history is retained.

**Step 5, webhook recovery: pass after an initial 422.** The first GitHub hook
PATCH returned 422. The old body sent only `{"config":{"secret":...}}` to
`/hooks/{id}` and no 422 body was retained, so the incomplete config URL/type/
SSL fields are the **probable** cause. The recovery helper exported the
current `EREGION_WEBHOOK_SECRET` revision 3 behind a fresh factor, required
revision 3 `active` before and after PATCH, PATCHed
`/hooks/707317891/config` with `url`, `content_type`, `insecure_ssl` and
`secret`, and required a new same-GUID redelivery attempt. Result: hook
`updated_at` `2026-11-05T21:22:16Z`; redelivery GUID
`2e7e4340-d930-11f1-801f-060fc01b8e29`, new attempt
`2026-11-05T21:22:24.045Z`, 202/OK; vault revision 3 remains active; serving
200. Evidence `840-host-webhook-recovery-20261105T212102Z-20452552.txt`,
`840-host-webhook-422-*`.

Cleanup: no human-batch/export/webhook recovery secret or PTY temp remains;
the local tunnel daemon was stopped and its socket/log removed; no host app,
database, Docker prune or reset was run.

**Remaining, not a baseline pass:** a post-fix webhook-triggered deployment
success was blocked at that point because the fixture `GIT_READ_TOKEN` was
rejected from the node (root cause not established); webhook acceptance (202)
and the failed build are distinct. The next record replaces that credential with
the authenticated `gh` token and records the successful deployment. The
operator now reports interactive login passed. Old-secret rejection
after rotation was not run because the old secret was not available and was not
read. At this point the broader alpha baseline and the 30-release/registry-GC
checks were unrun. Alpha.1 is now operator-confirmed; no individual result is
backfilled. Evidence:
`/home/mbv/eregion-qual/2026-11-04-fresh/830-*` and `840-*`.

### 2026-11-05 — gh-sourced `GIT_READ_TOKEN` and webhook deploy success

Build `edge-527592419f06` (commit `527592419f06`), CLI and node both this build;
schema 21/21, River 7/7; all four serving URLs returned 200 at the start. The
existing `gh` login was used only as an internal credential source: account
`MBvisti`, an OAuth token (`gho_` prefix) with `repo`, `read:org`, `gist` and
`workflow` scopes and admin permission on the private fixture. No token value or
hash appears in any evidence, output or log. `gh auth token` was piped into a
0600 regular file inside a 0700 temporary directory held by a cleanup trap; the
supported command was
`eregion secret set --app githello --environment production --name GIT_READ_TOKEN --expected-revision 2 --file <path> --json`
(the flag is `--file`; there is no `--from-file`). `secret set` is not one of the
D7 factor-gated actions, so no authenticator code was needed and no prompt
occurred.

The write produced `GIT_READ_TOKEN` revision 3 `active`; the command reported
`attached: true` and `revision_updated: false`, so the environment revision
already named the credential and no new revision was written (still
`erv_7a4a4774ce201e809fc283221258c63e`, number 10). The empty test commit
`59017d6d93c0b8eae720f51c9b9a9677daecdad4`
(`test: verify webhook deploy with gh git credential`) was pushed normally to
`mbvlabs/eregion-hello@main` with no force.

GitHub delivered webhook GUID `12b3bc42-d964-11f1-8ff4-80f7dc34db03` at
`2026-11-05T21:58:48.004Z`: event `push`, 202/OK, non-redelivery, payload
`refs/heads/main`, `before 59a8d7e2…`, `after 59017d6d…`. The linked
`op_232f45c1cd645304e92df57409fb5b83` (`platform.deploy`, webhook actor,
target `env/githello/production`) succeeded at `21:59:54.913Z` after 151
events: the clone succeeded (`HEAD is now at 59017d6`), pack built
`eregion/githello-production:232f45c1cd64`
(`sha256:9ae6522687df1c3ff9444f32d9e80173186ee0ec1348971ce70d4b185bbf4fe8`),
the release command ran (`version=v2`), web replicas 1 and 2 and the worker
started on that image, candidate health passed, Caddy load-balanced
`rel_d231a28c17ce1aa345e6fa3cdfd7ae03` across two web replicas, the public check
`https://git-test1.mbvlabs.com/healthz` returned 200, and the previous
containers were cleaned up. The environment is `healthy` and serving
`rel_d231a28c…` at commit `59017d6`; both githello domains are route-applied,
certificate-valid and public 200, with web 2/2 and worker 1/1 running. The
previous ready release `rel_e0e10c35ff89ed28d5ca4add52340ef9` remains listed
for rollback; no rollback, destructive reset or database change was needed.
git-test1, git-test2/users/sign-in, git-test3 and git-test5 all returned 200
after the switch.

The 151-event log was scanned for token prefixes, URL userinfo and
credential/password markers: none were present. Evidence is sanitized under
`850-gh-push-*`, including the baseline, the secret metadata (no hash), the
fixture commit/push, the GitHub delivery and payload fields, the operation
event log and the final serving/URL state. The credential temp file and its
directory were trap-removed; no tunnel or background process was started.

At this point rejection of the old secret after rotation (the old value was not
available and was not read), redelivery, another-branch and bad-signature
variants remained unrun. The following record covers the latter three. The
operator now reports interactive login passed. At this point the broader alpha
baseline was incomplete; alpha.1 is now operator-confirmed, with no result
backfilled here.

### 2026-11-05 — webhook deduplication and rejection variants

On the same `edge-527592419f06` CLI and node, a GitHub redelivery of GUID
`12b3bc42-d964-11f1-8ff4-80f7dc34db03` returned 202 in 0.99 seconds and
explicitly named the original `op_232f45c1cd645304e92df57409fb5b83` as a
duplicate. Before and after operation and release lists for githello/production
were byte-identical. A request with a deliberately invalid SHA-256 signature
returned 401; it did not alter those lists. An empty commit
`64677eabfad5e2efae35dfed9e76816ca0ed7ea3` was pushed without force to the
already-existing disposable fixture branch `qual-rc-never`; GitHub delivery
`5c5a2fd8-d965-11f1-9b8e-55d968ae9de8` returned 200 in 0.38 seconds with
`push is not to the bound branch`. The operation and release lists remained
byte-identical to the baseline. The temporary clone was removed; the existing
fixture branch retains the empty test commit. Four monitored HTTPS URLs returned
200 after redelivery, and doctor reported 20 pass / 0 warn / 0 fail after the
other-branch push. Sanitized evidence is in `860-*` under the fresh-server
evidence directory. This does not qualify old-secret rejection after rotation,
the remaining baseline, or tests performed on earlier builds.

### 2026-11-05 — retention doctor read-only follow-up

On context `main`, CLI and node `edge-527592419f06`, the preflight at
22:10:49 UTC reported 19 pass / 1 warn / 0 fail: `host.retention` could not
read the last successful run, while the latest-run cache measurement was
readable. Five sequential doctors, each bounded to 60 seconds, at 22:12:18–
22:12:50 all exited 0 and reported 20/0/0. Each read the same successful run
at 09:26:46 UTC (0 items, 0 bytes) and a 725.1 MiB cache measurement. A
bounded server status exited 0, schema 21/21, River 7/7, all five services
active; all four serving URLs returned 200.

The warning cleared, but its root cause is **unknown**, not proven to be the
database route, pool, permissions, or schema. Source inspection shows the
latest-run query succeeded before the final successful-run query failed; the
policy and both queries share one five-second context, and the final query's
underlying error is discarded. Thus deadline exhaustion is possible but not
established. The same queries subsequently succeeded on this schema and
identity, making a persistent schema or permission failure unlikely, not
excluding an intermittent failure. Sanitized counts from API and worker
journals for 22:09–22:14 found 60 lines, zero matching database, retention,
timeout/deadline or permission-denied terms; doctor executes on the node and
these service logs do not expose its suppressed query error.

Evidence: `870-retention-preflight.txt` and `880-doctor-*` under
`/home/mbv/eregion-qual/2026-11-04-fresh/`. No product code, fixture, prune,
host apply, restart, or database mutation was performed. No offline test was
run: no specific source defect has been established. Next, if the warning
recurs, capture the sanitized underlying query error and elapsed time with a
reviewed diagnostic change or authorized read-only instrumentation; do not
assume network failure. At that point the 30-release check was blocked by an
unused verified hostname and the unrelated images in the global prune plan;
later records completed it, and alpha.1 is now operator-confirmed.

### 2026-11-05 — CI unauthenticated surface follow-up

On the same `edge-527592419f06` build, the normal token-list metadata showed
`ci-valid`, `ci-expiring`, and `ci-otherenv` all expired on November 4; the
first two grant deploy on githello/production and the third on
guided/production, each with submit and read scopes. No reusable token fixture
was found by filename in the qualification directories. No token file was
read, no token was minted, and authenticated valid, expired, revoked and
wrong-environment variants were not rerun. Earlier results on another build
are not evidence for those variants on this build.

Public HTTPS: `/readyz` returned 200; apps, operations, tokens, secrets,
environments and the environment root returned 404; the deploy collection GET
returned 405, and unauthenticated status GET and deploy POST returned 401.
Malformed JSON without authorization, an empty bearer and duplicate empty
authorization headers each returned 401; authentication precedes body parsing,
so this does not test authenticated JSON validation. Before and after, the
githello/production operations list (limit 200) contained the same 47 projected
items. All four serving URLs returned 200. Sanitized evidence:
`890-ci-before.json`, `890-ci-surface.txt`, `890-ci-malformed.txt`,
`890-ci-after.json`, and `890-ci-final.json` under the fresh-server evidence
directory. No tunnel, header file, deployment or server configuration change
was made.

### 2026-11-06 — alpha.2 protection redesign host run (`edge-6d1ae98b9ac1`)

Build `edge-6d1ae98b9ac1` (commit `6d1ae98`, edge run `42285442054`) was
published, verified from the signed edge manifest, and installed on the CLI and
`main`. Node apply `op_fca44c39b625e2216d05d7581de99c18` applied migration 024.
The node reports schema **24/24** and River 7/7; `doctor` is 20 pass / 0 warn /
0 fail and the repeat `apply --dry-run` is 0 of 20. During the final checks
`apply --dry-run` alternated between 0 of 20 and 1 of 20: `host-readiness-v1`
timed out waiting for `GET /readyz`, while direct measurements of that endpoint
returned 200 in 0.35–8.1s. `doctor` alternated between 20/0/0 and 19/1 on the
known intermittent `host.retention` read warning. Immediate repeats passed;
no configuration was changed, so these are recorded as transient observations
rather than host failures.

All five pre-existing environments (`deployab`, `githello`, `guided`, `img` and
production) were inspected and are `protected: true`. None was unprotected and
no existing row was lowered.

| Check | Result | Evidence |
| --- | --- | --- |
| `app create` builds production and staging, both protected, production domain only, staging no invented domain | **Pass** on the host. Guided file-free create `op_0e2c1e66043489dc1941825c705a8c1a` created both environments without a TOTP prompt; both read `protected: true`; staging had no domain. | `a2defs1106` env list/status; `SHARED_SECRET` revision 1 in both namespaces |
| App + both environments + revisions commit atomically; failure on the second environment rolls back the app; same-key replay returns the original; a different key is a conflict | **Pass (disposable PostgreSQL integration, not host).** A forced staging-insert error after production and its current revision already existed rolled back the app, environments, revisions, operation and River job completely. A same-key retry/replay then produced one app, two environments, two revisions, one operation and one River job. The host separately refused a different-key resubmit with `conflict`. | Five selected integration tests on `6d1ae98`; temporary cluster removed; host conflict JSON |
| Other names default unprotected; production/staging default protected; migration 024 backfills every existing environment protected | **Pass.** Migration 024 applied on `main`; every pre-existing environment is protected. `pr-42`, `hook` and `prot-audit` were created unprotected because their names are not production/staging and no `--protected` was passed. | `op_fca44c39b625e2216d05d7581de99c18`; env rows; migration 024 |
| App delete always needs TOTP, including direct service submission | **Pass for refusal.** CLI `app delete` returned `input_required`; direct node `platform.app.delete` returned `precondition_failed` with `details.reason=code_required`. The positive TOTP deletion is the residual operator action. | CLI JSON refusal; node RPC request `app/a2defs1106` |
| Protected env delete needs TOTP; unprotected non-last env delete works with exact confirm and no TOTP in JSON/no-TTY | **Pass.** Protected `env delete` returned `input_required`; direct node `platform.environment.delete` returned `precondition_failed`/`code_required`. Unprotected `pr-42` deleted with `--confirm a2defs1106/pr-42 --json` as `op_9eb5aa06b06766f163adfbe61b2c74bc`. | CLI/API refusals; `op_9eb5…` succeeded; `pr-42` gone |
| Last-environment delete cannot bypass app-deletion TOTP | **Pass for factor admission (injected verifier, not real TOTP).** On the disposable PostgreSQL test, a last unprotected environment with a missing verifier, wrong code or invalid code was denied and created no operation. A valid injected test verifier was accepted and queued the delete while workers were stopped; the delete was not completed by a worker, and no real authenticator code was used. | Selected integration tests on `6d1ae98`; temporary cluster removed |
| `env protect` is agent-safe; `env unprotect` without a terminal refuses; server verifies the code in the locked submission transaction | **Pass for the host-visible policy.** `env protect a2defs1106 prot-audit --json` succeeded as `op_e248d36683e6317674fc36b9c5711439` and the row read `protected: true`; its `env delete` and `env unprotect` then returned `input_required`. Locked-transaction verification is covered by source and integration tests, which were not run on the host. | `op_e248…`; status/refusals |
| HTTP refuses only actions whose current locked state needs the factor; direct service with no verifier fails closed | **Not run over host HTTP.** API tests cover app-delete refusal and preview. | `internal/api/factor_test.go` |
| `env create --from` for `pr-42` uses the shared recipe with no `pr-42` override | **Pass.** `env create --from` created `pr-42` unprotected as `op_1724f76965e70df26a026cfde545ceb8`; the recipe file has no `pr-42` section. It was then deleted as `op_9eb5…`. | `prot-recipe.toml`; create/delete JSON |
| Fresh arbitrary-name environment creation uses the name default, not a protected default | **Pass.** `fresh-audit` was created without `--protected` and read `protected: false` (`op_4fa609d1c8502d17ab4e11baa2b4de02`); shared-recipe `recipe-audit` also read `protected: false` (`op_a1505d66121293de4f0c8b9931de94c4`) and took the shared config/secret name with no production domain override. | Create/status JSON |
| `env create` same-key replay returns the original operation without a second environment | **Pass.** Repeating the `fresh-audit` create with `--idempotency-key alpha2-fresh-audit-20261106` returned the same `op_4fa6…`; the env list still had one `fresh-audit`. | Replay JSON; env list |
| Unprotected non-last `env delete` works unattended with exact confirmation | **Pass.** `fresh-audit` deleted as `op_d1144d2a44c97b3b8b284f827b6806ba` and `recipe-audit` as `op_17179495fec1805fad3341a51366b0a8`, both with `--confirm <app>/<env> --json` and no TOTP. Their status then returned `not_found`, and their secret namespace lists were empty. | Delete/follow JSON; env/secret lists |
| Environment-specific secret names do not leak into another namespace | **Pass.** `FRESH_ONLY` existed only in `fresh-audit`; production and staging kept only `SHARED_SECRET`. The 0600 value file was shredded and no value was printed. | `secret list`; create JSON |
| Secret values never enter operation input, events or River arguments; only names and ephemeral request factor data | **Pass by inspection of the observed outputs.** Create, status, operation and environment output showed only `SHARED_SECRET`/`GIT_READ_TOKEN` names; no value was printed. | CLI JSON outputs; redaction tests |
| Shared creation secret values materialise in both production and staging namespaces; an environment-specific name never leaks into the other namespace | **Pass.** `SHARED_SECRET` was present in both production and staging at revision 1; `FRESH_ONLY` existed only in `fresh-audit`. The isolated namespace test also passes. | `secret list`; `TestSetupSecretWritesRespectEnvironmentSpecificNames` |
| TOTP protect-store read-modify-write mutations serialise across independent processes | **Not run on the host.** Multiprocess helper-process tests pass on the workstation; no concurrent node RPC code submission was attempted. | `TestMultiprocessVerifyAcceptsOneReplay` and related tests |
| `--confirm-domain` removes a bound alias from an environment | **Pass.** With `a2-primary.invalid` added as the primary (`op_e78773b42588dc9b80a3febf9b896e60`), file update with `--confirm-domain eregion-alpha-two.mbvlabs.com` removed the designated hostname as `op_134d51a4d723161563903870b4389275`; only the `.invalid` primary remains. | Domain list; update JSON |

**Independent disposable PostgreSQL retest (2026-11-06, `6d1ae98`).** A
separate temporary PostgreSQL cluster ran five selected tests with no ambient
production DSN. The app-create test forced an error in the staging insert
after verifying production and its current revision already existed; the
transaction rolled back the app, both environments, revisions, operation and
River job completely. A same-key retry/replay then succeeded with one app, two
environments, two revisions, one operation and one River job. The
last-unprotected-environment test denied a missing verifier, wrong code and
invalid code without creating an operation; a valid injected test verifier was
accepted and queued the delete with workers stopped. That verifier is a test
injection, not a real TOTP, and the queued delete was not completed by a
worker. The temporary cluster was removed and no DSN or secret value was
stored in evidence.

**Real TOTP in isolation.** `go test ./internal/protect -count=1` passes with
an isolated generated local secret and real TOTP codes, including confirm,
verify, replay refusal, rotation and multiprocess lockout. The CLI factor tests
and `TestIntegrationDirectDeleteWithRealTOTPCode` also pass: the latter uses a
generated local secret and a real current TOTP code through the direct service
submission path, accepts the protected delete as queued with workers stopped,
and rejects the same TOTP step on replay. The verifier there is local to the
test, not `protect.Store`; the real store is covered separately by the protect
tests. The live node protect store, its seed and any live code were never read
or used. The independent PostgreSQL factor test above used an injected
verifier, not a real TOTP, and its accepted delete was queued with workers
stopped rather than completed.

Additional audited alpha.2 cases on the same fixture:

| Check | Result | Evidence |
| --- | --- | --- |
| Failed-health resource apply retry preserves volume identity | **Pass.** A deliberately unhealthy `cache` apply degraded as `op_bf3718dfff0d897b0fa305b1d026e08d` with volume `er-res-e8ea4b4fc7468548f507969953efa8e0-cache-data`; the corrected retry `op_452dd3b405d027e46fc6e98fbc0cf6d9` reached ready and reused the same volume. This was a failed health check, not an interrupted operation. | Resource status before/after |
| Actual interruption: worker SIGKILL during provisioning is recovered and retried | **Pass.** Resource `interrupt2` in disposable `resource-audit3` was applying with a deliberately slow health command. The worker was SIGKILLed while the health step ran (PID changed, `NRestarts=2`). On restart the worker recovered the orphaned platform job (`op_2eee5d4a5320d71203a4bf075288cf58`, attempt 2), the health step then failed, and the resource became degraded with volume `er-res-d81502e434ca7f0ba8db431214eac64a-data`. The corrected retry `op_c457f164e6709764f59cd15c48120e8a` succeeded and reused the same volume with the expected ownership labels. The resource was destroyed with `--delete-data` (`op_649d92412f7ffd3234ca13b0d04b3bea`) and the environment deleted (`op_bf535c24a1a5a8fd4d6cde4c6e3f45b6`). | Worker restart/PID, operation events, resource status/labels |
| Interrupted-and-cancelled apply cleanup | **Pass after the `8199864` fix; initially blocked on `6d1ae98`.** The cancelled attempt `op_3eded76779595d3f9d9fbf7b6b8f4d55` left the resource `provisioning`, so apply/destroy refused `conflict` and env delete refused `precondition_failed`. Its container, volume and network were removed by exact name. After upgrading to `edge-81998646916d`, normal reconciliation moved the row to `degraded` without SQL; ordinary destroy `op_8a88…` and noninteractive env delete `op_905f…` succeeded. See the cancellation-recovery section below. | Operation events; `resource status`; CLI refusals; post-upgrade status |
| Deletion target with foreign ownership is explicitly refused | **Pass.** Resource `foreign` in disposable `resource-audit2` was provisioned (`op_3046da8110a17a3129bcaf31120a37c8`), default-destroyed retaining volume `er-res-2805d0c1ae918657dbe48b1fdc11a9f4-data` (`op_a1db98f6f9a408d32af6db1df6ef91eb`). The owned volume was replaced with the exact same name carrying foreign labels (`io.eregion.app=foreign-owner`). `resource destroy --delete-data` `op_6eefcf4cb76445e54433544241fb65cf` failed `precondition_failed` with "volume \"…\" belongs to another owner; refusing to delete data"; the foreign volume remained. After removing that test volume, a second `--delete-data` `op_aab313d6e42aed65888ca7564a742f62` succeeded, and the environment was deleted (`op_77c79ca8d88e60b7bdf8a90da089da04`). | Operation events; volume labels; `docker volume inspect` |
| Destroying a resource with an active dependent is refused | **Pass.** With `cache` attached to the serving revision, `resource destroy` returned `precondition_failed` and left it untouched. | Destroy refusal; revision 2 `resources: [cache]` |
| Unhealthy dependency blocks its dependent deploy without disturbing the serving release | **Pass.** The degraded `cache` (`op_c91e0504533cf7975b7abde3e12fe5a7`) made `deploy` refuse `resource:cache`; the serving release `rel_f7dbda77a2a443a1d58c7c9a7196ec05` stayed healthy and `/external` returned 200. | `config set` `op_21c1d789dcc2f6eeade6b3fa26ae8976`; deploy refusal; `/external` |
| Removing a resource declaration retains its data | **Pass.** `env update --from` removed `cache` as `op_4aebafcf4f3afdce9fc64caccfb3803d`; the container and volume remained and Redis key `audit:key` still read `persist`. A later deploy (`op_14d8ba9c5463d48b8b726c64060c363a`) moved to revision 5 without it. | Update JSON; resource status; Redis read |
| `resource destroy --delete-data` removes only owned volumes and leaves an unrelated foreign volume | **Pass (unrelated-volume behavior only; actual ownership refusal is tested above).** `op_298de5b1c2fc97cdc4b45c8ca40d6017` removed the owned cache volume; the separately created `a2-foreign-audit` volume was never a deletion target and remained, then was removed explicitly. | Destroy JSON; `docker volume ls` |
| External dependency without a managed resource | **Pass.** After the final no-resource deploy, `GET /external` returned `external 200` and `/build` returned `beta`. | `op_14d8…`; public responses |
| An ordinary deploy never reads a working-directory `eregion.toml` | **Pass.** A rogue `/tmp/opencode/rogue/eregion.toml` (`wrongapp`, `wrongnode`, `example.invalid/wrong:1`) was present when deploy `op_ae6e1053f5c84f2045bd68ae62461969` was submitted; the operation pulled `127.0.0.1:5000/a2defs1106@sha256:b472757e…` from the platform revision, not the rogue file. That deploy then failed its public check because the test image lacked CA certificates; the rebuilt image with `ca-certificates` passed as `op_2ae2a4bedcd7d16f574ca4006f37dcf0`. | `op_ae6e…` pull events; `op_2ae2…` success |
| Failed `gh` setup then successful retry | **Pass.** A temporary `gh` shim that reported admin permission but failed `/hooks` enabled auto-deploy without creating a hook. After disabling that attempt and deleting the earlier stale hook `708329427`, the real `--rotate-webhook-secret --github-webhook` created hook `708330096` and secret revision 3. | `gh api` hook list; secret list; env status |
| Real push delivery deploys the pushed commit and ignores the rogue file | **Pass.** Push `8fa8c1151b1a4c45115bb9b30474284acb0b1b27` produced GitHub delivery `abf898d0-da24-11f1-972b-eda7107b1a7c`, HTTP **202**, and `op_73c9e2addecfd556686d431b1fd4c916`; it served `rel_c2df6de6c35011f6be98b270c859fc7e` with `/` returning `eregion sample: hello-v1 src3`. | Hook delivery; operation; public response |
| Hook and branch cleanup | **Pass.** Hook `708330096` was deleted, auto-deploy disabled (`op_200e3cc4e758abf54cd5d24754b9d60b`), branch `alpha2-prot-audit` deleted locally and remotely, and the working tree returned to `main`. Only the pre-existing `githello` hook and the original branches remain. | `gh api` hook/branch lists; git status |

**Cleanup and residual state.** `hook` was deleted as
`op_7cef2d95874a413696ed97203695ad7c`. `a2defs1106` now has production,
staging and `prot-audit`, all protected; no unprotected disposable test
environment remains (the protected `prot-audit` audit environment stays).
`resource-audit` was recovered after the `edge-81998646916d` upgrade:
ordinary destroy `op_8a88…` and noninteractive env delete `op_905f…` succeeded.
`resource-audit2`, `resource-audit3`, `cancel-audit`, `cancel-ok`,
`cancel-final`, `fresh-audit` and `recipe-audit` are gone. The foreign ownership test volume
and all other test resource volumes were removed. Protected delete, unprotect and app-delete
refusals were re-confirmed against `production` and `prot-audit` with
`input_required`. No migration text was changed; 024 remains as applied.
Production serves `rel_b19f552b5afb5a727b7e7b897f59c181` on the placeholder
primary `a2-primary.invalid`; staging and `prot-audit` have no serving release.
The designated hostname has no binding and returns Cloudflare `525`; its
pre-existing DNS record remains because no safe Cloudflare access is available.
There is no fixture resource container, network or volume; the destroyed cache
row remains `destroyed`. The fixture registry manifests (`pgv3`, `pgv4`) remain
referenced by the environment revisions and the serving app, their local image
tags also remain, and the unused `eregion/a2defs1106-hook` image was removed
after a reference check. App deletion still needs the operator's TOTP factor:

```sh
eregion --context main app delete a2defs1106 --confirm a2defs1106
```

**Remaining evidence boundaries.** The multi-environment app-create same-key
replay and a last-environment delete were not re-run on the host. The
independent disposable PostgreSQL test above covers atomic rollback/replay and
the last-environment factor admission with an injected verifier, but not a real
TOTP and not a worker-completed delete. Positive real-TOTP authorization was
not exercised against the live host gate; no live authenticator code, live seed
or live protect store was accessed. The isolated protect-store/CLI tests and the
direct-service local-verifier test use generated local secrets and real TOTP
codes. The cancellation fix is published in `edge-7e74edbed8bc`; its
successful-step host cancellation test passed and the finalization race has
deterministic integration coverage. A new app
fixture was not created because its deletion would add another
TOTP-gated residual; fresh name-based creation was exercised with the disposable
environments above instead. The `env create` same-key replay passed. Factor
policy over public HTTP was not exercised on the host. The `gh`-assisted flow
has not been tested with a real non-admin account; the permission response was
simulated. macOS acceptance (R10) remains required before v1.0.0 final, and a
fresh-server baseline is not required for this alpha under the incremental
policy. No compatibility alias or shim was added: the create input replaces the
single `environment` field with `environments`. No migration text was changed.

### 2026-11-06 — resource cancellation recovery and finalization (host-qualified on `edge-7e74edbed8bc`)

During real-host qualification, cancelling a resource provisioning operation and
then SIGKILLing the worker left the operation terminal `cancelled` while the
resource row stayed `provisioning`. `resource apply` and `resource destroy`
then refused `conflict`, and `env delete` refused `precondition_failed`. The
host fixture was `resource-audit/interrupt` on
`main` (`op_3eded76779595d3f9d9fbf7b6b8f4d55`); its container, network and
volume were removed by exact name, but the platform rows were left untouched.
No direct database edit or ad-hoc repair command was used.

Root cause: cancellation was recorded on the operation (`cancelPlatform`) but
did not settle the resource row it owned. The failure transition already marked
an active apply/destroy `degraded`, but the cancellation path deliberately
skipped that and left `provisioning`/`destroying` behind. A pre-existing terminal
operation had no reconciliation path either.

Source fix, no migration:
- `cancelPlatform` and `failPlatformOperationTx` now settle a
  `provisioning`/`destroying` resource row to `degraded` in the same transaction
  that makes the operation terminal. The update is fenced by
  `eregion_platform_resources.operation_id`, so a newer apply/destroy is never
  clobbered.
- The cumulative volume ledger, image/container/network identity and spec are
  preserved. Retrying apply reuses the same identity and volumes; destroy still
  finds every owned volume.
- The ordinary maintenance `Reconcile` path now sweeps rows in those transient
  states whose owning operation is already `cancelled` or `failed`. A fixed
  worker therefore repairs the existing stuck row on its normal startup
  reconciliation, without a repair command, migration or direct database edit.
- A degraded resource remains non-destroyed, so environment/app deletion still
  requires the normal explicit resource destroy first; that guard was already
  intended and is unchanged.

Tests for the published `8199864` fix ran against a **local disposable
PostgreSQL 18** database with a fake node transport (no main host, Docker used
only to run the disposable database). The follow-up `7e74edb` regressions and
direct-TOTP test below were also run against the existing disposable
PostgreSQL test container:

| Check | Result |
| --- | --- |
| Cancel an apply during its run step while the resource is `provisioning`; operation becomes `cancelled`, resource becomes `degraded` with `error_code=cancelled`; ledger preserved; retry apply and then destroy succeed | Pass: `TestIntegrationResourceCancelSettlesStateAndAllowsRetryAndDestroy` |
| Cancellation accepted while a step is in flight and that step then succeeds must still cancel the operation, not silently mark the resource ready | Pass: `TestIntegrationResourceCancelAfterSuccessfulStepIsHonored` |
| Cancellation accepted after the last step check but before the operation-row-locked finalization must cancel an apply | Pass: `TestIntegrationResourceApplyCancelBeforeFinalizationIsHonored` |
| Cancellation accepted after the last step check but before the operation-row-locked finalization must cancel a destroy | Pass: `TestIntegrationResourceDestroyCancelBeforeFinalizationIsHonored` |
| Existing rows left `provisioning`/`destroying` by an already `cancelled`/`failed` operation converge on normal `Reconcile`, preserving the ledger and using the correct apply/destroy detail | Pass: `TestIntegrationReconcileTerminalResourceStatesRepairsExistingStuckRows` |
| Existing full resource lifecycle, secret redaction and retained-volume behavior unchanged | Pass: `TestIntegrationManagedResourceLifecycleRetainsVolumesAndSecrets` |
| Package build/vet and all non-DB tests | Pass: `go build ./...`, `go vet ./...`, `go test ./...`, darwin/arm64 cross-build |

After the `edge-7e74edbed8bc` cancellation fix, the full
`go test ./internal/controlplane -count=1` passed against the existing
disposable PostgreSQL test container (`127.0.0.1:55432`; no new container), as
did `go test ./internal/protect -count=1`,
`go test ./internal/cli -run 'Factor|Protect' -count=1`, `go build ./...`,
`go vet ./...` and the darwin/arm64 cross-build.

**Host verification (`edge-81998646916d`).** Publish edge run `42302477645`
succeeded (`gh run watch`); CI run `42302477691` also succeeded. The channel
manifest at
`https://eregion.mbvlabs.com/edge/manifest.json` reported
`edge-81998646916d`; its sigstore bundle verified with the pinned publish-edge
identity, the linux/amd64 SHA256 matched
`c32b41daeafe92391da71499986f5ef6cdf0fcd036b9c53855c90dd779f521b3`, and the
binary reported `edge-81998646916d` / commit `81998646916d`. The CLI was
upgraded with the signed release; node apply
`op_e23e2d114b670c1cb5e192de1db73038` restarted the API and worker. Schema
stayed **24/24**, River 7/7; `doctor` is 20/0/0 and `apply --dry-run` is 0 of
20.

**Finalization fix release (`edge-7e74edbed8bc`).** Commit
`7e74edbed8bcf521ad9e9bf93e67c744e0e0ab45` published through Publish edge
`42309997476` (CI `42309997630`), both passed via `gh run watch`. The signed
manifest reports `edge-7e74edbed8bc`; its sigstore bundle verified with the
pinned publish-edge identity, the linux/amd64 SHA256 matched
`1bee0caf35bb0f1dd6462005ca0b592d18d5915812013c1d13f317594df09284`, and the
binary reported `edge-7e74edbed8bc`. The CLI and `main` upgraded to that build;
node apply `op_fc5bec9e0e40767839fa8f298682fb87` restarted the API and worker.
Schema stayed **24/24**, River 7/7; no migrations were added or edited. The
first `doctor` immediately after the upgrade reported a transient
`drift.pending: host-readiness-v1`; immediate retries were 20/0/0, `apply
--dry-run` was 0 of 20, and `/readyz` and `/healthz` returned 200 (readyz
0.63s). One later `doctor` run showed the known intermittent
`host.retention` read warning; the next runs were 20/0/0. No configuration was
changed.

**Host successful-step cancellation — pass.** In disposable unprotected
`cancel-final`, resource `cancelprobe` was submitted with a slow successful
health command (`op_3bc86507be687b2fa91245565942ee91`). Cancellation was
accepted while running; the network step then completed and the operation ended
`cancelled` at the step boundary (not `succeeded`), with resource state
`degraded`, `error_code=cancelled`, and the documented interrupted-apply
detail. No worker was killed. Ordinary destroy `op_9cb1f3ec929f2ee64b2480b4f7e435d6`
(delete-data) and noninteractive env delete
`op_36df44f8e1da1d446c8eb97bf1a87119` succeeded; `cancel-final` and its resource
are gone. **Existing stuck-row recovery — pass.** Without SQL, normal reconciliation
moved `a2defs1106/resource-audit/interrupt` from `provisioning` to `degraded`
with `error_code=cancelled` and the interrupted-apply detail. Ordinary
`resource destroy` `op_8a880ab27ae28ad86b3b88e6a90d4066` succeeded and retained
the ledger; noninteractive `env delete`
`op_905fdccf680fbb8c7d8925702e394d64` succeeded. `resource-audit` is gone. Its
Docker volume had already been removed by exact name during the earlier manual
cleanup, so this verified the state and ledger path, not physical data
preservation; the earlier cache default-destroy/re-apply test remains the
data-retention evidence.

**Fresh cancellation retest — real bug found and source-fixed.** Two no-worker-kill
retests were run. On `cancel-audit/cancelprobe`, cancellation was accepted while
the slow health step ran, but the health step failed at its deadline; the
operation ended `failed` and the resource degraded, so that run did not
distinguish failure from cancellation. On `cancel-ok/cancelok3`, cancellation
was accepted while the health step was in flight, the health command then
succeeded, and the operation still ended **`succeeded`** with the resource
**`ready`** (`op_80c0f44da0189678f2faeb427ba35e34`). That is a real product
bug on `edge-81998646916d`: resource apply/destroy had no cancellation check
between steps, so an accepted cancel could be silently converted into success.

The fix published in `edge-7e74edbed8bc` checks cancellation at every resource
apply/destroy step boundary and finalizes both actions under the operation row
lock, rechecking `cancelling` before the resource finalization callback. A
`cancelling` row makes the scoped `finishResourceOperation` return
`errPlatformCancelled` through the normal settlement path; other operation
completion policy is unchanged. This closes the reviewer P1 race where a cancel
could commit between the last step check and `finishPlatformOperation`. New
regressions `TestIntegrationResourceCancelAfterSuccessfulStepIsHonored`,
`TestIntegrationResourceApplyCancelBeforeFinalizationIsHonored` and
`TestIntegrationResourceDestroyCancelBeforeFinalizationIsHonored` pass. They
use a test-only seam immediately before the operation-row lock and require
`cancelled` plus a degraded resource with the ledger preserved. The full
platform/CLI/control-plane tests and builds pass as recorded above. The
`edge-7e74edbed8bc` host cancellation test above exercised a successful network
step; the finalization race itself is covered deterministically by the
integration tests, not by timing the host. All `cancel-audit` and `cancel-ok`
resources/environments were destroyed and deleted after the retests.

**Limitations.** At the time of that fix, `main` (`edge-7e74edbed8bc`) included
the published step-boundary and finalization fixes; no known cancellation-state
defect remained on that build. Alpha.3 `edge-9e71713eeff9` later superseded it.
Only rows whose owning operation is already `cancelled` or
`failed` are swept by reconciliation; queued, running or cancelling operations
still need the normal River worker/rescue path. Reconciliation does not infer
whether a node container exists: a subsequent apply/destroy is the deliberate
recovery step, which is why the row becomes `degraded`, not `ready` or
`destroyed`. The finalization race is covered by deterministic integration
tests, not a timed host interleaving. No migration was added or edited. The
tagged publication record is below.

### 2026-11-07 — alpha.2 tag publication (succeeded; signed snapshot verified)

The first attempt, tag `v1.0.0-alpha.2` at `7e74edb`, failed Publish stable
`42312127636` because that commit was on `master` and not an ancestor of `1.x`.
With explicit authorization the tag was replaced. PR #1 (`feat: deliver alpha.2
release`, merged 2026-11-07T10:13:48Z) squash-merged the tested source onto
`1.x` as `85f2a4f173de61eb86b843ce66ea62850c804a68`; its tree
`48fa59d6653247e34c87cb21f13f4c149465c2dc` is identical to `7e74edb`, so the
tagged tree is the previously tested `edge-7e74edbed8bc` source. The new
annotated tag object `736a5edb332c74645692972cc392a1e96b0b1695` (tagger
2026-11-07T10:13:58Z) points at that commit; it is not GPG-signed.

Publish stable run `42363078147` (event push, head `85f2a4f`) completed
**success**; `gh run watch` exited 0. `classify` and `publish_prerelease`
passed, and the finals-only `publish` job was skipped. CI run `42363078061`
also passed (`gh run watch` exit 0). The upload step copied fifteen files under
`s3://eregion/releases/1.0.0-alpha.2/` (manifest, signature, installer, four
binaries, four `.sha256` files and four binary signatures); the RC promotion
step was skipped.

The immutable snapshot is published at
`https://eregion.mbvlabs.com/releases/1.0.0-alpha.2/`:
`manifest.json` (200), `manifest.json.sigstore.json` (200), `install.sh` (200)
and all four platform binaries and `.sha256` files returned 200. The manifest
reports `schemaVersion: 1`, `channel: stable`, `version: 1.0.0-alpha.2`, and
`releaseBaseUrl: https://eregion.mbvlabs.com/releases/1.0.0-alpha.2`. The
release layout has no separate `version.json`; the version is the manifest's
`version` field. Responses carried
`Cache-Control: public,max-age=31536000,immutable`; last-modified was
2026-11-07T10:17:51–56Z.

Cosign verified the manifest and each of the four binaries against the exact
tag identity
`https://github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-alpha.2`
and issuer `https://token.actions.githubusercontent.com`; all five verifications
returned `Verified OK`. Each binary and its published `.sha256` matched the
manifest hash:

| Platform | SHA-256 |
| --- | --- |
| linux/amd64 | `bd1da630bb6fc9802935560025b6570f6bd52c0293afad992b004622519bf7c1` |
| linux/arm64 | `a344e5bbc754a55cb62afd41840b3c68fdb9b1e47217d806830b30f5620fa8c0` |
| darwin/amd64 | `61190934eff16dae8911dcebc9dd562a51e3eaf015ec3f809215731a50a88a5e` |
| darwin/arm64 | `abdc10b9181603313b364138b1e829b8d3aa96c5bed5657f578c42d9ee6b771a` |

The downloaded linux/amd64 binary reported `version=1.0.0-alpha.2`,
`channel=stable`, `commit=85f2a4f173de`, `dirty=false` (`eregion version
--json`, go1.27.1); the 12-character commit is the tag commit's prefix.

**Caveats.** The tag was deleted and replaced with explicit authorization; the
earlier tag at `7e74edb` is no longer the published tag. The tag annotation is
unsigned, so artifact authenticity rests on the cosign bundles. `install.sh`
is version-pinned (`snapshot_version="1.0.0-alpha.2"`) but is not one of the
cosign-signed files; the signing script covers the manifest and four binaries.
No channel pointer moved: root, `1.x` and `rc` manifests remained HTTP 404, and
the edge manifest still reports `edge-7e74edbed8bc`. The publication
verification itself did not install the snapshot; a separate targeted smoke
below installed it on `main`. Publication success is not a new full host
qualification; the tagged tree is the already host-qualified
`edge-7e74edbed8bc` source. No server, DNS record, repository branch or tag was
mutated by the publication verification.

### 2026-11-07 — alpha.2 tagged snapshot install and smoke

The published `1.0.0-alpha.2` snapshot was installed on the authorized `main`
with the signed release path. The CLI upgraded from `edge-7e74edbed8bc` to
`1.0.0-alpha.2`; the manifest linux/amd64 SHA256 is
`bd1da630bb6fc9802935560025b6570f6bd52c0293afad992b004622519bf7c1` and the
manifest signature was verified against the `v1.0.0-alpha.2` publish-stable tag
identity in the publication record. Node apply
`op_a817ab835599be1908ec5dfa88f9fd9a` auto-upgraded the node; `host-database-v1`
was skipped, schema stayed **24/24** and River 7/7. `server status` reports
CLI/node `1.0.0-alpha.2`, node commit `85f2a4f173de`; `doctor` is 20/0/0;
`apply --dry-run` is 0 of 20; `/readyz` and `/healthz` returned 200.

Safe disposable smoke on `a2defs1106`: unprotected `alpha2-smoke-pr` was
created noninteractively with the existing fixture image, no domain and no
secrets (`op_ce2468e6a0de1fc12a778d44dcaa4a2d`, `protected:false`); it was
deleted noninteractively (`op_1189a69ede6b011628124074e7b11a3b`), status
`not_found`, with no TOTP. The fixture retains only production, staging and
protected `prot-audit`; no test container, volume or network remains. Four
existing URLs returned 200; the designated hostname remains unbound at
Cloudflare 525.

This is a targeted smoke/regression pass, not a repeated full manual acceptance.
The earlier alpha.2 evidence and known gaps (live TOTP, HTTP factor, macOS R10)
stand as recorded. No commit, push or tag was made by this smoke.

```text
Date / build / commit / channel:
Operator OS/arch:
Server provider, OS/arch, size:
External PostgreSQL version; schema current/required; River current/required:
App / environment / domains (non-secret):
Source commits; release, deployment and operation IDs:
Checks: pass / fail / blocked / not run, one line each
Public response markers and TLS result:
Failures and recovery observations:
Remaining qualification gap and next action:
```
