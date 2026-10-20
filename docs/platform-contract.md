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

## 2. Targets

| Kind | Canonical target | Example |
| --- | --- | --- |
| `platform.app.create` | `platform` | `platform` |
| `platform.node.register` | `platform` | `platform` |
| `platform.node.check` | `node/<node>` | `node/main` |
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

### `platform.environment.create`

```json
{
  "name": "production",
  "node": "main",
  "container_port": 8080,
  "host_port": 0,
  "health": { "type": "http", "path": "/healthz", "timeout_seconds": 30 },
  "env": { "APP_ENV": "production" }
}
```

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

`source.type` is `image`, `directory`, or `git`. Git URLs are `https://` without
embedded credentials; `ref` is optional. Directory paths are absolute and must
exist on the target node. A build that cannot run fails honestly with
`precondition_failed`; it never records a fake release. The release row is
created durably as `pending`, then becomes `ready` only after the image exists on
the node.

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

### `platform.node.check`

`{}` — runs a read-only Docker (and, when present, `pack`) availability check on
the node and reports observations in operation events.

## 4. Domain state and guarantees

- **Desired/applied/serving.** Deploy submission records the desired release.
  Deployment success atomically sets `applied_release_id` and
  `serving_release_id`. A failed candidate never changes either. A failed
  post-switch verification restores the previous serving release and records a
  failed deployment plus a `rollback` change.
- **Order.** `preflight → candidate → health → switch → verify → cleanup`.
  Candidate health is checked before the serving container is touched. The
  direct Docker runtime has a short switch window while the serving container is
  replaced; it does not claim zero-downtime. An edge proxy can be added later
  without changing these states.
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
  cleaned up and the serving release stays intact.
- **Reconciliation.** A platform operation whose River job disappeared is not
  left `running`. Trivial accepted rows may be completed; builds fail their
  release; deployments fail visibly and inspect the serving container labels so
  inventory reflects the container that is actually serving when possible.
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
- Docker registry credentials, remote source upload, build caches, and release
  commands. A remote directory/git build requires the source on that node.
- Host bootstrap, package installation, WireGuard enrollment, and telemetry
  placement. Node registration records facts; other agents own those effects.
- Webhooks and web UI. Both will call the same submission path.
