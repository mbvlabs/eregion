# M0.3 control-plane setup

The first vertical slice is **diagnostic.test**, not deployment. It runs through the
node-dispatch boundary and records progress, retries, cancellation, and terminal
state. Each step creates one marker file under the node's private state directory. No
shell commands, service/package/network configuration, image builds, or application
changes run.

## Database and execution decisions

- Use a dedicated external PostgreSQL database, version 17 or newer. Integration
  CI validates PostgreSQL 17. Do not point migrations at an unrelated application DB.
- Use native `pgx` transactions and River's `riverpgxv5` driver, pinned to River
  0.40.0 as in the deploycrate-ce research. This preserves its transactional
  enqueueing approach without introducing Bun or fx.
- Explicit, ordered migrations only: API/worker startup checks every version's
  checksum and fails closed if a migration is missing, newer, or edited. Version 2
  adds `eregion_operation_steps`; the version 1 checksum is unchanged.
- Migration processes serialize with a dedicated PostgreSQL session advisory lock.
  River versions commit individually because PostgreSQL enum changes require it;
  domain DDL is transactional. Interrupted migration is resumable, not globally
  atomic across every version. Services remain unready until migration completes.
- Submission commits operation, River job, and accepted event together. Scoped
  idempotency keys bind normalized input hashes. Conflicting inputs return conflict.
- Each step row and progress event commits in a short row-locked transaction. No
  transaction stays open while a node command runs.
- Diagnostic jobs allow three attempts, retry after attempt-number seconds, time
  out after one minute, and become rescue-eligible after two minutes. Actual rescue
  also depends on River maintenance/election cadence; this is not a failover SLA.
- A River maintenance job reconciles discarded/missing execution records to visible
  domain failure, or leaves them running with a warning while the node effect still
  holds a live local lock. Operation history is independent of River job retention.
  Events are not pruned yet; missing history returns an explicit cursor precondition
  error.

These guarantees cover the local node boundary. Pinned SSH and userspace WireGuard
transports, remote node supervision, and distributed leases remain future work.

## Local node execution and recovery

The worker calls a `node.Transport`, not a shell or SSH directly. The only implemented
adapter is in-process `local`; pinned SSH and userspace WireGuard will implement the
same `Call(ctx, Request, onEvent)` interface.

Execution identity is `op_<id>:<attempt>:<step>`; the stable effect key is
`op_<id>:<step>`. The attempt changes on retry, the effect key does not. Before every
dispatch the worker checks both the PostgreSQL step row and the node's durable status.
A completed effect is acknowledged, never re-run.

`EREGION_NODE_STATE_DIR` is one shared directory per host (default for a non-root CLI:
`$XDG_STATE_HOME/eregion/node` or `~/.local/state/eregion/node`). Every worker process
on that host must use the same directory. A production service should set an explicit
root-owned path such as `/var/lib/eregion/node`. Layout:

```text
<state>/journal.json              durable per-effect execution journal
<state>/journal.json.lock         short journal read-modify-write lock
<state>/host.lock                 global local host-mutation flock
<state>/locks/<sha256>.lock       per-effect flock; ownership/liveness fence
<state>/effects/<op>/<step>.json  durable effect record
<state>/effects/<op>/<step>.applied  create-once diagnostic marker
```

The journal records `running|completed|failed`, `inputs_hash`, attempt, execution ID,
request ID, owner diagnostics, and the attempt that actually applied the effect. A
corrupt journal is an explicit `precondition_failed`; it is never reset silently. All
writes use temp+chmod+fsync+rename+directory-sync. The diagnostic marker uses
create-once (temp + hard link), so a duplicate application cannot overwrite which
attempt produced the effect.

`flock` coordinates processes on **one host only**. The effect lock is the liveness
fence: the kernel releases it when an owner dies. Status tries the lock without waiting;
success means a `running` journal entry is stale. PostgreSQL row locks plus attempt
equality fence domain writes, and the `(operation_id, step)` primary key makes
acknowledgements idempotent. Neither is a distributed lock; multiple control-plane
instances are not claiming HA.

Reconciliation is a total decision over the database step row and node status:

| DB step | Node | Action |
| --- | --- | --- |
| completed | any, same hash | Skip; already acknowledged |
| completed | any, different hash | `precondition_failed`; never re-apply |
| incomplete | completed, same hash | Acknowledge once and emit a recovery event |
| incomplete | running (live lock) | Attach/retry; never start a parallel Apply |
| incomplete | stale/absent/failed | Run under the effect lock; Check decides |

A worker crash or a lost transport response leaves the journal/effect record behind.
The retried River job first asks status, acknowledges the completed effect, and only
then continues. A River job discarded while the node effect is still live-running is
not failed by reconciliation; it stays `running` with a warning until the effect
settles. This covers local effects only.

Cancellation is separate from detaching a viewer or client. `operations get`,
`operations events`, a closed connection, or an interrupted request never cancel.
Only `RequestCancel` sets `cancelling`. Every diagnostic step is atomic and declares
an `after_apply` boundary: the worker starts no new step after observing cancellation,
but lets an in-flight step finish and acknowledges it. Completed effects are not rolled
back. If all steps finished, the honest terminal state is `succeeded`; otherwise
`cancelled`. River job timeout or process shutdown is not a cancellation request; it
may leave a `running` journal entry that the next attempt reconciles through Check.

## Start a local development control plane

Requires Linux, Go, just, and a separately available PostgreSQL database. Supply
database credentials through your environment/secret manager, not CLI flags or
committed files. Remote databases require `sslmode=verify-full`; development may
use `sslmode=disable` only with literal loopback hosts, not `localhost`.

```sh
# Example shape only: obtain real credentials without putting them in shell history.
export EREGION_DATABASE_URL='postgres://USER:PASSWORD@database.example/eregion?sslmode=verify-full'
export EREGION_NODE_STATE_DIR="$PWD/.local/node-state"  # one shared directory per host
just build
bin/eregion system migrate --dry-run --json
bin/eregion system migrate --json
```

The database migration role needs permission to create the domain and River schema
objects. Least-privilege deployment roles and automated credential distribution
are not provided by this milestone. Direct database connections are the tested
configuration; transaction-pooling proxies are not validated.

Create a random client token and a private server credential file containing its
SHA-256 hash. Do not print or commit the token:

```sh
umask 077
mkdir -p .local
export EREGION_API_TOKEN="$(openssl rand -hex 32)"
export EREGION_AUTH_FILE="$PWD/.local/api-auth.json"
python3 - <<'PY'
import hashlib, json, os
from pathlib import Path
path = Path(os.environ['EREGION_AUTH_FILE'])
path.write_text(json.dumps([{
    'actor': {'kind': 'operator', 'subject': 'local-operator'},
    'token_sha256': hashlib.sha256(os.environ['EREGION_API_TOKEN'].encode()).hexdigest(),
    'scopes': ['operations:submit', 'operations:read', 'operations:cancel', 'system:read'],
}]))
path.chmod(0o600)
PY
bin/eregion serve --listen 127.0.0.1:8080
```

In another terminal, securely supply the same client token and optional
`EREGION_API_URL` (default `http://127.0.0.1:8080`):

```sh
bin/eregion operations test --dry-run --json
bin/eregion operations test --idempotency-key demo:first --json
# Use the returned real operation ID:
bin/eregion operations get OPERATION_ID --json
bin/eregion operations events OPERATION_ID --after 0 --limit 100 --json
bin/eregion operations cancel OPERATION_ID --json
```

The accepted response is not proof of execution success. Retain the ID and poll
state/events. If submission times out, reuse its original idempotency key and input.
Do not create a fresh key merely because a response was lost.

`--steps` accepts 1–20; `--step-delay-ms` accepts 0–1000; and
`--fail-until-attempt` accepts 0–3 to exercise retries/terminal failure. Dry-run
authenticates and validates input without creating an operation or job; it is not
a guarantee that a worker or database will be available for future execution.

## Separate API and worker processes

```sh
bin/eregion serve --workers=false
bin/eregion worker --concurrency 2
```

Each process uses `EREGION_DATABASE_URL`. Only API processes need
`EREGION_AUTH_FILE`; only operator clients need `EREGION_API_TOKEN`. Every worker on
one host must use the same `EREGION_NODE_STATE_DIR`; separate hosts need separate
directories. Multiple workers/control-plane processes share PostgreSQL and River, with
distinct generated River client IDs. This is not yet a complete HA deployment design.

API/worker commands run in the foreground. Lifecycle logs go to stderr; `--json`
emits the final result on stdout when the service stops. SIGINT/SIGTERM initiate
shutdown. River gets a five-second soft-stop window, then cancels active work;
shutdown waits up to fifteen seconds. Viewer interruption never calls cancellation.

Non-loopback API listeners require both `--tls-cert` and `--tls-key`. Clients require
HTTPS except for literal loopback HTTP and verify certificates through normal
system trust. No insecure TLS bypass or credential-bearing redirects are allowed.
Private WireGuard/SSH transport is not implemented yet.

## API contract

All routes emit `eregion/v1` envelopes. The `command` field identifies the HTTP
method plus “API request”; request URLs and credentials are not reflected.

- `GET /healthz`: unauthenticated process liveness, not database health.
- `GET /readyz`: `system:read`; database/schema readiness. Does not claim a worker
  is available. Unready responses use HTTP 503.
- `POST /v1/operations`: `operations:submit`, JSON body
  `{"kind":"diagnostic.test","target":"control-plane","input":{},"dry_run":false}`.
  Normal submissions require one `Idempotency-Key` header and return HTTP 202.
  Dry-run returns HTTP 200 without insertion.
- `GET /v1/operations/{id}`: `operations:read`, owning actor only.
- `GET /v1/operations/{id}/events?after=0&limit=100`: `operations:read`, owning
  actor only; exclusive sequence cursor and limit 1–200. Pages are JSON, not SSE
  or NDJSON streams yet.
- `POST /v1/operations/{id}/cancel`: `operations:cancel`, owning actor only.
  HTTP 202 means cancellation requested, not acknowledged. Terminal state is not
  rewritten; a worker acknowledges cancellation at a safe boundary.

Authenticate with `Authorization: Bearer TOKEN`. Tokens must be random and 32–256
ASCII letters/digits/underscore/hyphen characters. The credential file supports
1–128 entries, explicit scopes, and owner-only regular files (0600/0400). Identity
comes from the file, never the request body. Other actors receive not-found for
operation IDs they do not own; no cross-actor administrator role exists yet.
Token rotation/revocation is a credential-file change plus service restart. Token
issuance, expiry, login, and dynamic credential administration remain future work.

Requests are limited to 16 KiB, headers to 8 KiB, and handler contexts to ten
seconds. Clients time out after fifteen seconds and cap responses at 1 MiB. Errors
map to HTTP 400/401/403/404/409/412/503/500 with the existing stable code/exit
contract. No quota/rate-limiting system or production ingress setup ships here.

## Integration tests

```sh
export EREGION_TEST_DATABASE_URL='postgres://TEST_USER:TEST_PASSWORD@127.0.0.1:5432/postgres?sslmode=disable'
just integration
```

Use a disposable PostgreSQL server and a test role with `CREATEDB`. Tests create
random `eregion_test_*` databases and drop only those databases. They never fall
back to `EREGION_DATABASE_URL`. Default unit tests skip database tests when the test
URL is absent; `just integration` fails rather than silently skipping.

Coverage includes concurrent versioned migrations, enqueue/commit rollback injection,
concurrent submission deduplication, two worker clients, cursor replay, retry
exhaustion, cancellation before start and at a step boundary, viewer detach,
discarded-job reconciliation, and real CLI/API/worker processes with graceful
shutdown. Recovery tests inject a failure between the node effect and its database
acknowledgement, drop a transport response after dispatch, and kill a separate worker
process. Each asserts exactly one marker file, one completed step row, and one
progress event per step. Reconciliation is also tested not to fail an operation while
its node effect still holds a live local lock. This does not simulate remote host
execution or prove full fleet HA.

The `local` transport is in-process. There is no SSH, WireGuard, node enrollment,
remote supervision, or multi-host conflict scope yet; see
[next transport increments](ssh-wireguard-next.md).
