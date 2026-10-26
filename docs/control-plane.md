# Control-plane setup and transport

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

These guarantees cover the local node boundary, the pinned SSH transport, and pinned
SSH inside a userspace WireGuard tunnel. Remote node enrollment, pinned identity,
and WireGuard peer reconciliation are implemented ([nodes](nodes.md)); a
cross-node distributed conflict scope and automatic multi-host failover remain
open ([next increments](ssh-wireguard-next.md)).

## Local node execution and recovery

The worker calls a `node.Transport`, not a shell or SSH directly. Two adapters
implement the same `Call(ctx, Request, onEvent)` interface: the in-process
`local` dispatcher and the pinned `ssh` transport. Userspace WireGuard is not a
third adapter: it is the dialer the `ssh` transport opens its connection through.

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
recovered effect keeps the attempt that applied it, reported by the step through
`CheckResult.AppliedAttempt`; `0` is the documented "not known" sentinel. A
corrupt journal is an explicit `precondition_failed`; it is never reset silently. All
writes use temp+chmod+fsync+rename+directory-sync. The diagnostic marker uses
create-once (temp + hard link), so a duplicate application cannot overwrite which
attempt produced the effect.

The journal is an index and is compacted: terminal tasks older than the retention
window are dropped unless they are among the most recent ones. The compaction runs
after every node task and through the explicit `eregion node-agent compact` command. A
serving process never compacts: every status poll starts a new serving process, and
a compaction on every poll would take the journal lock away from a running step. The
effect records are the durable exactly-once evidence and are **not** compacted
automatically; pruning them is an explicit operator action, because dropping one
would let a stale retry re-apply an effect.

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

## Remote node execution over pinned SSH

The worker dispatches to a remote node through the same `Transport` interface. The
worker, the operation contract, the River arguments, and the PostgreSQL step model
do not change; only the adapter does.

```sh
export EREGION_NODE_TRANSPORT=ssh
export EREGION_NODE_SSH_ADDRESS='node-1.example:22'
export EREGION_NODE_SSH_USER='eregion-node'
export EREGION_NODE_SSH_IDENTITY='/etc/eregion/control-plane-key'   # mode 0600
export EREGION_NODE_SSH_KNOWN_HOSTS='/etc/eregion/known_hosts'       # the pinned host key
# Optional: the node name the handshake must report, and a non-default remote command.
export EREGION_NODE_SSH_NODE='app-1'
```

The node side needs the eregion binary and its configuration:

```sh
just build
install -m 0755 bin/eregion /usr/local/bin/eregion
install -d -m 0700 -o eregion-node -g eregion-node /var/lib/eregion/node
install -m 0600 -o root -g root node.json /etc/eregion/node.json
```

`/etc/eregion/node.json` holds the node name and its state directory:

```json
{"node": "app-1", "state_dir": "/var/lib/eregion/node"}
```

An optional `task_command` replaces the command that starts a detached node
task. It is the node operator's own local program, it must be an absolute path,
and it is never derived from a request.

The node account is unprivileged and owns only its state directory. The control
plane pins the node's host key; a missing pin, a changed key, or a protocol
version mismatch fails closed and ends the operation with
`precondition_failed` instead of retrying forever. A lost stream is an explicit
unknown outcome: the node task keeps running, and the retry asks the node for its
status before it runs anything.

The node runs each step as a detached task, so the task outlives the connection
that started it and the control plane polls the node's status. A task that died
or never started is reported at once, as a retryable failure that carries a
bounded tail of the node task log, instead of a wait until the job deadline.
The full decisions,
including the privilege policy, the state directory layout, and the retention
policy, are in [the M0.3 SSH plan](m0.3-ssh-plan.md).

## SSH inside a userspace WireGuard tunnel

The SSH transport can reach its node through a userspace WireGuard tunnel. The
tunnel runs inside the control-plane process (wireguard-go on gVisor netstack):
no TUN device, no root, and no routing change on the control plane. SSH, the
host-key pin, the node protocol, and the node binary are unchanged. The tunnel
only replaces the TCP connection under SSH.

```sh
export EREGION_NODE_TRANSPORT=ssh
export EREGION_NODE_SSH_ADDRESS='10.77.0.2:22'                  # the node's tunnel address
export EREGION_NODE_SSH_USER='eregion-node'
export EREGION_NODE_SSH_IDENTITY='/etc/eregion/control-plane-key'
export EREGION_NODE_SSH_KNOWN_HOSTS='/etc/eregion/known_hosts'   # pinned for 10.77.0.2
export EREGION_NODE_WG_PRIVATE_KEY_FILE='/etc/eregion/wg.key'    # mode 0600, wg genkey output
export EREGION_NODE_WG_ADDRESS='10.77.0.1'                       # the control plane's tunnel address
export EREGION_NODE_WG_PEER_PUBLIC_KEY='<node public key>'
export EREGION_NODE_WG_ENDPOINT='node-1.example:51820'
# Optional: the node subnet (default: the node address alone) and the keepalive in seconds.
export EREGION_NODE_WG_ALLOWED_IPS='10.77.0.0/24'
export EREGION_NODE_WG_KEEPALIVE=25
```

On the node, use kernel WireGuard with the control plane as its only peer:

```sh
wg genkey | tee /etc/wireguard/node.key | wg pubkey      # give the public key to the control plane
ip link add wg0 type wireguard
wg set wg0 listen-port 51820 private-key /etc/wireguard/node.key \
  peer '<control-plane public key>' allowed-ips 10.77.0.1/32
ip address add 10.77.0.2/24 dev wg0
ip link set wg0 up
```

The node needs no endpoint for the control plane: WireGuard learns it from the
first authenticated packet. The node must answer ICMP echo on its tunnel
address, because the control plane's liveness probe uses it.

- **Key handling.** The private key comes only from a file. The file must be a
  regular file with no group or other permission bits. There is no variable that
  takes the key itself. The CLI registers the key for redaction before it builds
  anything, and every printed form of the key in the process is `[REDACTED]`.
  The key is never in a request, a River argument, an event, a log, or a row.
- **One process per key.** The node knows one endpoint per peer, so two
  processes with the same key steal the peer from each other. A worker takes a
  host-local lock for its key in `$XDG_RUNTIME_DIR` (or the temporary directory)
  and refuses to start when another process holds it. Give every worker process
  its own key and tunnel address. Each is a separate peer on the node.
  `serve --workers=false` builds no node transport and takes no key.
- **Allowed IPs.** Only the node subnet is routed: a default route or a prefix
  broader than `/16` (IPv4) or `/48` (IPv6) is refused, and the SSH address must
  be inside it.
- **Lifecycle.** The tunnel comes up on the first dial and is shared by every
  call. Its first handshake is bounded by 10 seconds. A tunnel that cannot come
  up fails the attempt before any SSH byte is sent, so no work is dispatched.
  The error names the endpoint and the peer key, and it is retryable: wrong keys
  look exactly like a node that is down. While the tunnel is up, the control plane
  probes it. When it has received nothing for 15 seconds, it is lost: open
  connections fail as lost streams, and the next dial rebuilds the tunnel. The
  worker closes the tunnel when it stops.
- **Failure meaning.** A tunnel lost after dispatch is a lost stream, never proof
  that the node task stopped. The node task is detached, as for plain SSH, and
  the retry asks the node for its status before it runs anything.

The decisions are in [the M0.3 WireGuard plan](m0.3-wireguard-plan.md).

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
token_sha256="$(printf '%s' "$EREGION_API_TOKEN" | sha256sum | cut -d' ' -f1)"
cat > "$EREGION_AUTH_FILE" <<JSON
[{"actor": {"kind": "operator", "subject": "local-operator"},
  "token_sha256": "$token_sha256",
  "scopes": ["operations:submit", "operations:read", "operations:cancel", "system:read"]}]
JSON
chmod 600 "$EREGION_AUTH_FILE"
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
The SSH transport reaches its node over plain TCP, or over a userspace WireGuard
tunnel when `EREGION_NODE_WG_PRIVATE_KEY_FILE` is set.

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
Token rotation/revocation is a credential-file change plus service restart.
`eregion context credential add` issues a plan-bound scoped automation credential
without printing its token, and contexts support login/list/show/use/export.
Automatic expiry and a remote rotation API are not implemented; rotation is an
explicit credential-file change.

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
its node effect still holds a live local lock.

### Control-plane database recovery drill

The external control-plane database has a separate recovery procedure in
[resources and recovery](resources.md#separate-external-control-plane-database-recovery).
The integration suite exercises it through Ready/PlanMigrations:

```sh
EREGION_TEST_DATABASE_URL=... \
  go test -count=1 ./internal/controlplane -run TestIntegrationControlPlaneRecoveryDrill -v
```

The drill dumps the source with `pg_dump`, restores it into two generated
`eregion_cp_recovery_drill_<hex>` databases on the same server, verifies key
tables and `eregion system migrate --dry-run --json` readiness (application and
River versions), and compares both restores. It creates and drops only those
generated databases and never writes to or drops the source. It does not test a
production failover, writer quiescence, DNS/DSN cutover, or recovery-time
objectives; those remain operator procedures.

The pinned SSH transport has its own fixture: a throwaway `sshd` container with a
generated host key and one authorized key. `just ssh-fixture` builds its image and
`just transport` runs the transport tests. The same scenarios run against the
disposable SSH endpoint in the integration suite, including a lost response injected
at the transport boundary, a killed worker, a duplicate submission, a cancellation at
a step boundary, and a node task that outlives its caller. The SSH tests skip
cleanly when Docker or the fixture image is unavailable; they never fail the rest of
the suite.

The WireGuard dialer has two test nodes. The first is an in-process wireguard-go node
that serves the in-process SSH node inside the tunnel, so the tunnel behaviour runs
without Docker. The second is the same `sshd` fixture container behind kernel
WireGuard, with only its UDP port published. A UDP relay in front of it black-holes
the tunnel to make a real loss. `just wireguard` runs the WireGuard transport tests.
The integration suite runs the same exactly-once scenarios over SSH inside the tunnel,
and checks that the WireGuard key is in no table and no worker log. The container
tests skip cleanly when Docker, `NET_ADMIN`, or the host's `wireguard` module is
missing.

The `local` transport is in-process. Enrolled application/builder/telemetry nodes
use their own pinned SSH transport, with node-generated identity attestation and
WireGuard peer reconciliation. A distributed conflict scope across nodes and
automatic failover are still open; see [nodes](nodes.md) and
[next transport increments](ssh-wireguard-next.md).
