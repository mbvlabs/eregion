# Next transport increments: pinned SSH, then userspace WireGuard

**Status:** Planned, not implemented. This document lists what remains after M0.3's
local node-dispatch boundary. It is not evidence that remote execution works.

## What is already fixed

The worker depends only on:

```go
type Transport interface {
    Name() string
    Call(context.Context, Request, func(Event)) (Response, error)
}
```

`Request` already carries a request ID, execution identity, effect key inputs, and an
optional transient secret map. `Response` distinguishes absent, running, stale,
completed, and failed effects. The local adapter is in-process. Adding SSH or
WireGuard must not change the worker, the operation contract, River arguments, or the
PostgreSQL step model.

The node journal, effect records, create-once diagnostic marker, canonical inputs
hash, redacted shell, and node-local `flock` live in `internal/engine` and
`internal/node`. They are transport-independent.

## Increment A: pinned SSH

Deliverables:

- An SSH transport that dials one configured node, verifies a pinned host key, runs a
  fixed node command, and exchanges the existing request/event/response shapes.
- Version/handshake compatibility before any request is accepted.
- A bounded stderr tail and an explicit "stream lost" outcome that does not imply the
  node stopped. The retry path must call `status` before `run`.
- Request secrets registered with a per-request redaction registry before events,
  logs, errors, or journal writes.
- Authentication and privilege policy documented (root/sudo, key ownership, file
  modes). No shell interpolation of untrusted values.
- Tests against a disposable SSH endpoint in CI or a documented local fixture:
  - successful run + status;
  - host-key mismatch aborts before execution;
  - version mismatch fails closed;
  - connection drops after dispatch, retry recovers through status;
  - duplicate requests produce one effect;
  - cancellation while connected never kills a step mid-effect.
- Failure injection for lost responses at the transport boundary, not only locally.

Non-goals for this increment: node enrollment, SSH CA, public-IP hardening, mesh
reconciliation, multi-node placement, and distributed leases.

## Increment B: userspace WireGuard

Deliverables:

- A userspace WireGuard dialer (no TUN/root on the operator side) that the SSH
  transport uses as its `net.Conn` source.
- Lazy handshake, keepalive, node-subnet-only allowed IPs, and an explicit failure
  when the tunnel cannot come up.
- Tunnel identity and key handling documented; private keys never enter logs,
  events, River arguments, or the operation database.
- The same node identity/status/reconciliation semantics as the SSH increment. A
  dropped tunnel is a transport loss, not proof that the node task stopped.
- Tests:
  - tunnel up/down and reconnect;
  - handshake failure does not dispatch work;
  - lost tunnel after dispatch recovers through status;
  - no duplicate effect after reconnect;
  - key material is absent from redacted output and persisted rows.
- A documented choice of supervision model: the node continues a detached task and
  is queried by status, or the transport reports the task as unknown. Do not assume
  the SSH session owns execution lifetime.

## Remote supervision and reattachment still to decide

The local implementation uses process lifetime plus `flock` as its liveness fence.
Before remote SSH/WG ships, decide and test:

1. **Node execution lifetime:** one-shot dispatch with a durable journal (current
   local model) versus a supervised node service. Either way, the stream is not the
   source of truth.
2. **Ownership fencing:** whether a remote effect needs a lease/fencing token in
   addition to the node-local lock. The local lock cannot coordinate multiple
   control-plane instances or multiple hosts.
3. **Conflict scopes:** which steps may mutate the same resource across nodes, and
   how those scopes are represented in PostgreSQL. River concurrency is not a
   distributed environment lock.
4. **Cancellation:** which remote steps are cooperative/interruptible, and how a
   cancel request reaches a detached node task. The current `after_apply` boundary
   remains the default until a step proves a safe cooperative boundary.
5. **State directory:** the remote node's root-owned journal/effect directory,
   backup/upgrade behavior, and retention/cleanup policy.
6. **Observability:** correlation ID propagation, node-side log retention, and how a
   lost stream is surfaced without claiming work stopped.

## Gate for the remote half

Do not tick the remote M0.3 gate until an SSH test endpoint and then a WireGuard
endpoint pass the same exactly-once scenarios as the local suite, including a lost
response, a killed worker, a duplicate submission, cancellation at a step boundary,
and a node task that outlives its caller. Until then, advertise only the local test
path.
