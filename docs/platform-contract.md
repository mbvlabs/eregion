# Platform API and extension contract

**Status:** implementation contract · 2026-10-20  
**Scope:** M2/M3 nodes, apps, environments, releases, builds, deployments,
rollback, and the generic action path used by later resources, telemetry, DNS, and
access work. It adds no second queue: every platform action is one durable
`eregion_operations` row and one River job.

This file is the coordination point for the CLI, resources, telemetry, DNS/access,
and upgrade work. The implementation lives in `internal/platform`,
`internal/controlplane`, and `internal/api`; the CLI only presents these contracts.

## 1. Transport

### Submit one action

```http
POST /v1/operations
Authorization: Bearer <scoped token>
Content-Type: application/json
Idempotency-Key: ci:deploy-2026-10-20.1

{
  "kind": "platform.deploy",
  "target": "env/api/production",
  "input": { "release_id": "rel_..." },
  "dry_run": false
}
```

- `target` is always canonical and validated against the action kind. The body
  cannot choose a different scope.
- A successful submission returns `202 Accepted` with the durable
  `contract.Operation`. The caller keeps the operation ID.
- The same authenticated actor, kind, target, and idempotency key returns the
  original operation. The same key with different canonical input fails with
  `conflict`. This is the CI deduplication rule.
- `dry_run: true` validates and returns a plan. It inserts no operation, domain
  row, or River job and needs no idempotency key.
- `Idempotency-Key` is required for a real submission. A timed-out submission is
  ambiguous: reuse the key, never a fresh one.

### Inventory

```http
GET /v1/platform?collection=environments&app=api&limit=50
Authorization: Bearer <scoped token>
```

Response data:

```json
{ "collection": "environments", "items": [ ... ], "limit": 50 }
```

Collections implemented here: `summary`, `nodes`, `apps`, `environments`,
`releases`, `deployments`, `changes`. Extension packages may add collections
through the registry (section 5). Unknown collections fail with
`validation_failed`. Filters are `app`, `environment`, `node`, `state`, `limit`
(1–200, default 50); no ad-hoc SQL or free-form query language.

Reads require `operations:read`; submissions require `operations:submit`. A CI
credential is therefore an ordinary scoped actor token. Actor identity always
comes from authentication.

### Events

Platform actions use the existing operation snapshot and event cursor unchanged:

```http
GET /v1/operations/{operation_id}
GET /v1/operations/{operation_id}/events?after=N&limit=100
```

Stage events name the deployment stage (`preflight`, `build`, `candidate`,
`health`, `switch`, `verify`, `cleanup`, `rollback`). Progress is only emitted
when measured. A client detaching never cancels the operation.

Platform observations are structured as well as human-readable. Log and warning
events may carry `step` (the durable node step), `release_id`, and
`serving_release_id`. A traffic-switch acknowledgement, a deploy success, and a
deploy failure name the release they are about; failure additionally names the
release that still serves when it is known. Completed node steps carry a
measured `duration_ms`. Health probes carry:

```json
"health": {
  "target": "http://127.0.0.1:20000/healthz",
  "type": "http",
  "passed": true,
  "checks": 3,
  "timeout_seconds": 30,
  "detail": "http status 200"
}
```

`checks` is zero when the node completed the probe from its read-only Check and
therefore did not report a probe count. Clients must render that as "not
reported", not invent a count. All fields are additive: an event written before
them remains valid and viewers keep a message-parsing fallback.

## 2. Targets

| Kind | Canonical target | Example |
| --- | --- | --- |
| `platform.app.create` | `platform` | `platform` |
| `platform.node.register` | `platform` | `platform` |
| `platform.node.check` | `node/<node>` | `node/main` |
| `platform.node.enroll` | `node/<node>` | `node/builder-1` |
| `platform.node.reconcile` | `node/<node>` | `node/builder-1` |
| `platform.environment.create` | `app/<app>` | `app/api` |
| `platform.release.create` | `env/<app>/<env>` | `env/api/production` |
| `platform.deploy` | `env/<app>/<env>` | `env/api/production` |
| `platform.rollback` | `env/<app>/<env>` | `env/api/production` |

Names match `^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$` (40 characters maximum).
Targets contain no whitespace and are at most 200 bytes. Extension actions use
the same grammar; the definition owns its target kind.

## 3. Built-in inputs

All inputs are strict JSON objects. Unknown fields, trailing JSON, control
characters, and values outside the stated limits fail with `validation_failed`.

### `platform.app.create`

```json
{ "name": "api" }
```

### `platform.node.register`

```json
{
  "name": "main",
  "roles": ["application", "builder", "registry"],
  "capabilities": ["docker", "buildpacks", "registry"],
  "address": "10.77.0.1:22"
}
```

Registration records placement facts only. It does **not** install packages or
change host state; bootstrap owns that. Roles are
`application|builder|edge|registry|telemetry|control-plane`. An environment may
only be placed on a node that has the `application` role and `docker` capability,
and a build only on a node with `builder` + `buildpacks` (or Docker for an image
release). A hostname is never authorization.

A registered node is declared, not verified. It is served only by the worker's
own configured transport, and only when that transport authenticates as exactly
that node. Remote nodes are enrolled (below); registration can never overwrite an
enrolling or enrolled node (`conflict`).

### `platform.node.enroll`

```json
{
  "roles": ["builder"],
  "address": "203.0.113.10:22",
  "user": "eregion",
  "host_key": "ssh-ed25519 AAAA...",
  "host_key_fingerprint": "SHA256:...",
  "identity_fingerprint": "SHA256:...",
  "build_concurrency": 2,
  "build_user": "eregion-build",
  "private_network": { "endpoint": "203.0.113.10:51820", "ssh_port": 22 }
}
```

`{"retry": true}` re-runs the recorded enrollment and accepts no other field.
`roles`, `address`, and `host_key` are required otherwise. The canonical input is
public material only. Enrollment is one durable operation with these steps, each a
Check/Apply node step: `node-identity-01-v1` (the node signs a challenge with a
key it generated; the key is pinned on first verification or checked against
`identity_fingerprint`), `node-probe-01-v1` (distribution, architecture, docker,
pack, wg), `node-hostcfg-01-v1` (role directories). The pinned `host_key` is the
SSH trust anchor; there is no trust on first use. The node ends `enrolled` with
its probed capabilities, or `failed` with `enrollment_error`; a transient failure
leaves it `pending` and River retries. Allowed role sets for an enrolled node:
application, edge, application+edge, builder, registry, builder+registry,
telemetry. `control-plane` is never enrollable. Only an `enrolled` node receives
remote work; a refusal is a `precondition_failed` fault whose message starts
`placement_violation:` with `details.reason` in `not_enrolled`, `role_missing`,
`capability_missing`, `builder_required`, `identity_mismatch`,
`transport_unpinned`, `registry_scope`, or `build_capacity`, and a
`placement_violation` operation event.

### `platform.node.reconcile`

`{}` or `{"via":"direct"}`. Converges the role directories and, for a node with
a private network, generates the WireGuard key on the node, stores the public
keys and allowed IPs in PostgreSQL, and renders (and, with a privileged helper,
applies) the peer set: the control plane plus every other enrolled node with a
key and endpoint. Rerunning it changes nothing when the node already matches.
`via: direct` forces the direct SSH address, which repairs a broken tunnel.

### `platform.environment.create`

```json
{
  "name": "production",
  "node": "main",
  "build_node": "builder-1",
  "container_port": 8080,
  "host_port": 0,
  "health": { "type": "http", "path": "/healthz", "timeout_seconds": 30 },
  "env": { "APP_ENV": "production" }
}
```

- `build_node` is optional. It must be a node with the `builder` role and
  `buildpacks` capability; `node` keeps the `application` + `docker`
  requirement. An omitted `build_node` means builds run on `node`, which is the
  working co-located development path (the main node). A distinct build node is
  accepted and placement-checked, but a local Docker image is not visible on the
  application node without a shared registry. `platform.release.create` for a
  directory or git source therefore fails with an explicit
  `precondition_failed` ("shared registry") instead of pretending the image
  moved. Image sources do not need a build node.
- `host_port: 0` allocates a stable host port from 20000–29999 under a
  transaction-scoped PostgreSQL lock. A candidate port is allocated from
  30000–39999 and stored, so a retry never depends on an ephemeral Docker port.
- `env` is **non-secret configuration** only. It is validated, bounded, and
  passed to the container. Secret material needs the revisioned-secret resource
  work and must not be put here.
- Health `type` is `http` or `tcp`. HTTP paths are loopback-only on the node.

### `platform.release.create`

Image source:

```json
{ "source": { "type": "image", "image": "ghcr.io/acme/api:2026-10-20" }, "pull": true }
```

Build source (requires `pack` on a builder-capable node):

```json
{
  "source": { "type": "directory", "path": "/srv/src/api" },
  "build": { "builder": "paketobuildpacks/builder-jammy-base", "env": { "BP_GO_TARGETS": "./cmd/api" } }
}
```

`source.type` is `image`, `directory`, or `git`. Git URLs are `https://` on an
explicitly allowed hostname with no embedded credentials, query, fragment, or
non-default port; `ref` is optional. The default allowlist is GitHub, GitLab,
Codeberg, Bitbucket, and SourceHut; `EREGION_GIT_ALLOWED_HOSTS` replaces it for a
self-hosted forge. The host is re-resolved at build time and default public
forges must resolve to public addresses. Directory paths are absolute and must
exist beneath a configured build source root on the build node. A build that
cannot run fails honestly with `precondition_failed`; it never records a fake
release. The release row is created durably as `pending`, then becomes `ready`
only after the image exists on the node.

Build sources run on `environment.build_node` (or its `node` by default) after a
second placement check. The worker takes one of that node's
`build_concurrency` slots through a PostgreSQL session advisory lock, so the
limit holds across control-plane processes and a dead worker releases its slot.
Waiting emits `build_queued`; taking the slot emits `build_running`. The build
uses the existing isolated per-operation HOME, Docker config, cache, temp, and
`pack` tree. A build node distinct from the application node is refused before
any command runs until registry transfer exists (see `build_node` above).

### `platform.deploy`

```json
{ "release_id": "rel_..." }
```

### `platform.rollback`

```json
{ "to_release_id": "rel_..." }
```

`to_release_id` is optional; the default is the environment's last applied
release. Rollback is a normal deploy of that release and keeps change history.

### Retrying a terminally failed deployment

A failed deployment is immutable history. To retry it, submit the same action
again through `POST /v1/operations` with a new idempotency key (or reuse the key
of a timed-out retry). The CLI helper is:

```sh
eregion platform deployments retry \
  --deployment dep_...|op_... \
  --idempotency-key ci:retry-1 [--dry-run] --json
```

It reads the deployment from inventory, requires `state=failed`, and re-submits
`platform.deploy` (or `platform.rollback` for a failed rollback) with the frozen
target and release. It does not rewrite the failed operation, create a second
queue, or bypass the action service. `--dry-run` returns the ordinary preview.

### `platform.node.check`

`{}` — for a registered node, runs a read-only Docker (and, when present, `pack`)
availability check through the worker's own transport. For an enrolled node it
re-verifies the pinned identity key, re-probes the host, and re-checks that the
declared roles are still backed by probed capabilities. Either way it records
`last_check_at`, `last_check_state`, and the detail, so clients can show
freshness. A host that answers with a different identity key is quarantined: its
state is `identity_mismatch` and it receives no work until a check verifies the
pinned identity again.

## 4. Domain state and guarantees

- **Desired/applied/serving.** Deploy submission records the desired release.
  Deployment success atomically sets `applied_release_id` and
  `serving_release_id`. A failed candidate never changes either. A failed
  post-switch verification restores the previous serving release and records a
  failed deployment plus a `rollback` change.
- **Order.** `preflight → [release command] → candidate → health → switch →
  verify → cleanup`. Preflight is a durable node step that proves the immutable
  release image is present before anything is replaced. Candidate health is
  checked before the serving container is touched. The direct Docker runtime has
  a short switch window while the serving container is replaced; it does not
  claim zero-downtime. An edge proxy can be added later without changing these
  states. Cleanup is the durable `platform-cleanup-01-v1` step: it removes the
  candidate container, appears in operation history with a duration, and is
  attempted again by reconciliation after a crash or cancellation.
- **Per-target lock.** A deployment holds a PostgreSQL session advisory lock for
  its environment across every control-plane process. River concurrency is not
  used as a distributed lock. A second deployment waits briefly, then fails with
  `conflict`; it cannot interleave.
- **At-least-once effects.** Every remote step goes through the existing pinned
  node journal/effect record. Docker steps are convergent: inspect by
  deterministic container name before creating; a lost acknowledgement
  reconciles instead of starting a second effect. Container names include the
  operation ID except for the stable serving container.
- **Cancellation.** Cancellation is observed before candidate creation and
  before traffic switch. After switch the deployment verifies and completes;
  completed effects are not rolled back implicitly. A cancelled candidate is
  cleaned up and the serving release stays intact. Cancellation reconciled after
  the process died inspects the switch step: a completed switch is recorded as
  the serving release with healthy health if verification completed, or unknown
  health if it did not, instead of leaving the old release in inventory.
- **Reconciliation.** A platform operation whose River job disappeared is not
  left `running`. Trivial accepted rows may be completed; builds fail their
  release; deployments inspect completed step records before choosing a serving
  release. A completed `platform-container-03-v1` restore means the previous
  release is serving again, so that release (and its measured restore health) is
  recorded; the failed new release is never recorded as serving. A completed
  switch without a restore records the new release with healthy or unknown
  health according to whether verification committed. Reconciliation also
  removes a leftover candidate through the durable cleanup step.
- **No fake success.** Docker, `pack`, SSH, or node failures produce failed
  operations with a machine-readable code. Buildpacks absence is a failure, not
  a synthetic artifact. Image-source deployment works without `pack`.

## 5. Extension registry

Packages that own resources, telemetry, DNS, or access register durable actions
in `internal/platform`. They reuse the same `eregion_operations` row, River job,
event cursor, cancellation, and idempotency path.

```go
package platform

// ActionInput is one decoded, validated action input.
type ActionInput interface {
    // Target returns the canonical target this input resolves to. The service
    // compares it with the submitted target before accepting the action.
    Target() Target
    // Preview is safe, secret-free dry-run data.
    Preview() any
}

// Normalizer decodes strict JSON for one canonical target.
type Normalizer func(target Target, raw json.RawMessage) (ActionInput, error)

// Executor runs one accepted action. A nil Executor means the control plane's
// built-in handler owns the kind. Extension packages must register one.
type Executor interface {
    Execute(ctx context.Context, exec Execution) (Result, error)
}

type Execution struct {
    OperationID contract.OperationID
    Attempt     uint32
    Kind        string
    Target      string
    Actor       contract.Actor
    Input       json.RawMessage // canonical, validated, no plaintext secrets
    Report      func(Event)     // best-effort durable progress; redacted by worker
}

type Result struct {
    Message string
    Data    any
}

type Definition struct {
    Kind      string
    Summary   string
    Normalize Normalizer
    Executor  Executor // optional for built-ins, required for extensions
}

// Register adds an action to the process-wide registry used by the API and the
// platform River worker. It fails on a duplicate kind or invalid definition.
func Register(def Definition) error

// RegisterInventory adds a read-only `/v1/platform?collection=...` provider.
func RegisterInventory(provider InventoryProvider) error

type InventoryProvider interface {
    Collection() string
    Read(context.Context, InventoryQuery) (any, error)
}
```

Rules for extension executors:

1. Register in package `init()` (or an explicit constructor invoked before
   `controlplane.New`). Duplicate kinds fail closed.
2. Parse and validate in `Normalize`; the service stores only canonical JSON.
   Never accept secrets in action input. Reference them by durable identity.
3. Own domain idempotency under the operation ID and effect keys. The worker
   guarantees one accepted operation per scoped key; it does not make arbitrary
   external effects exactly-once.
4. `Report` is best-effort event output. Atomic domain state is the extension's
   transaction; do not hold a database transaction open across remote work.
5. A terminal error becomes a failed operation. Return a `*contract.Fault` for a
   machine-readable code; other errors are `internal_error`.
6. Extensions must not start their own queue. If work needs retries or a
   different lifetime, return an error and use the operation's River attempts.

The built-in platform worker looks up `Definition.Executor` *after* claiming the
attempt and owning the target lock where applicable. This keeps extension
actions on the same durability and observation path.

## 6. Client surface (owned by `internal/api`)

The CLI agent should use these exported methods rather than reimplementing HTTP:

```go
// SubmitAction submits or previews any registered action kind.
func (c *Client) SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool) (json.RawMessage, error)

// PlatformInventory reads one inventory collection.
func (c *Client) PlatformInventory(ctx context.Context, collection string, filters url.Values) (json.RawMessage, error)
```

Both validate the envelope and return the `data` object. `SubmitAction` returns
an operation snapshot or a dry-run preview. The CLI owns command metadata,
flags, plain rendering, and exit-code mapping; it must not add a second
submission path.

## 7. Deliberately out of scope in this increment

- Zero-downtime edge proxy/Caddy traffic switching. Direct Docker publishes a
  stable host port and retains the previous image for rollback.
- Revisioned secret storage and injection. Use non-secret `env` for now.
- Docker registry credentials, cross-node image transfer, remote source upload,
  shared build caches, and release commands. A directory/git build runs on the
  environment's build node (the runtime node by default); a distinct build node
  is refused with an explicit shared-registry error. The source must exist on
  that node.
- Host bootstrap, package installation, and telemetry placement. Node
  enrollment provisions nothing on the host beyond role directories; see
  [nodes](nodes.md) for what the operator installs first.
- Webhooks and web UI. Both will call the same submission path.
