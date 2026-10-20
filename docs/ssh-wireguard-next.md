# Next transport increments: pinned SSH, then userspace WireGuard

**Status:** Increment A (pinned SSH) implemented · 2026-10-20
**Increment B (userspace WireGuard):** Planned, not implemented.
This document lists what remains after M0.3's
local node-dispatch boundary. It is not evidence that userspace WireGuard works.

The decisions that the SSH increment had to make are recorded in
[the M0.3 SSH plan](m0.3-ssh-plan.md). This document keeps the increments and
their tests visible.

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

## Increment A: pinned SSH — done

Implemented. The worker, the operation contract, the River arguments, and the
PostgreSQL step model did not change: the SSH adapter implements the existing
`Transport` interface and the existing worker decision procedure.

What shipped:

- `internal/transport`: a pinned SSH transport. It dials one configured node
  through a `Dialer` seam (a `net.Conn` source), verifies a pinned host key
  before any request, runs the fixed node command, and exchanges the existing
  request/event/response shapes.
- `internal/noded` and `cmd/eregion-node`: the node binary. It answers node
  requests over the channel's standard input and output, dispatches by step name
  through a step registry, and runs each step as a **detached** task that
  outlives its caller. The request reaches the task through an inherited pipe,
  so a transient secret never touches the node's disk.
- A protocol-version handshake that fails closed before any request is accepted.
- A bounded (8 KiB), redacted stderr tail and an explicit "stream lost"
  outcome that never implies the node stopped. The retry path calls `status`
  before `run`.
- A documented authentication and privilege policy: key-only authentication to a
  dedicated unprivileged account, a pinned host key, no root/sudo, no agent
  forwarding, no PTY, and no shell interpolation of any request value.
- Node-side journal compaction with a retention policy.
- A machine-readable error code on the terminal state of a failed operation.

Delivered tests (all green):

| Scenario | Where |
| --- | --- |
| successful run + status | `internal/transport` (in-process node and the disposable container) |
| host-key mismatch aborts before execution | `internal/transport` |
| version mismatch fails closed | `internal/transport` |
| connection drops after dispatch, retry recovers through status | `internal/transport` and `internal/controlplane` |
| duplicate requests produce one effect | `internal/transport` and `internal/controlplane` |
| cancellation while connected never kills a step mid-effect | `internal/transport` and `internal/controlplane` |
| failure injection for a lost response at the transport boundary | `internal/transport` (cut proxy in front of the container) |
| killed worker, duplicate submission, cancellation at a step boundary | `internal/controlplane` against the disposable SSH endpoint |

The disposable SSH endpoint is a throwaway `sshd` container with a generated host
key; `just ssh-fixture` builds its image and the tests skip cleanly when Docker
is unavailable. See [the M0.3 SSH plan](m0.3-ssh-plan.md) for the fixture
details and the exact decisions.

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

The dialer seam is in place: the SSH transport asks a `Dialer` for a `net.Conn`
and never dials a socket itself. The SSH transport, the host-key pinning, the
node protocol, and the node binary do not change when the dialer does.

## Remote supervision and reattachment — decided for SSH

The local implementation uses process lifetime plus `flock` as its liveness fence.
These questions are decided for SSH in
[the M0.3 SSH plan](m0.3-ssh-plan.md); the short answers are:

1. **Node execution lifetime:** a detached node task plus a durable journal
   queried by `status`. The SSH session is a transport, not the owner of
   execution lifetime.
2. **Ownership fencing:** the node-local effect lock and the durable effect
   record remain the effect fence; the transport adds a request fence (the
   execution identity), not a distributed lease. Conflict scopes between
   different nodes and cross-host leases stay open for node enrollment.
3. **Cancellation:** there is no remote kill. A cancellation request is honoured
   at the step boundary; a detached task is left to finish and reconciled through
   `status`.
4. **State directory:** `/var/lib/eregion/node`, owned by a dedicated
   unprivileged account, mode 0700, files 0600. The journal is compacted; the
   effect records are the durable exactly-once evidence and are not compacted
   automatically.
5. **Observability:** the request ID and the execution identity cross the
   boundary; a lost stream is surfaced as an explicit unknown outcome that never
   claims the work stopped.

## Gate for the remote half

The remote M0.3 gate is open for the pinned SSH endpoint: it passes the same
exactly-once scenarios as the local suite, including a lost response, a killed
worker, a duplicate submission, cancellation at a step boundary, and a node task
that outlives its caller. The gate stays closed for userspace WireGuard until a
WireGuard endpoint passes the same scenarios. Advertise only the tested path.
