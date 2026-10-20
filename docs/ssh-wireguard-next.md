# Next transport increments: pinned SSH, then userspace WireGuard

**Status:** Increment A (pinned SSH) and Increment B (userspace WireGuard) implemented · 2026-10-20

This document lists the transport increments after M0.3's local node-dispatch
boundary, and what each one delivered.

The decisions that the SSH increment had to make are recorded in
[the M0.3 SSH plan](m0.3-ssh-plan.md), and the WireGuard decisions in
[the M0.3 WireGuard plan](m0.3-wireguard-plan.md). This document keeps the
increments and their tests visible.

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
  so a transient secret never touches the node's disk, and the serving process
  writes that request before it returns, so a serving process that exits with
  the channel never drops it.
- A protocol-version handshake that fails closed before any request is accepted.
- A bounded (8 KiB), redacted stderr tail and an explicit "stream lost"
  outcome that never implies the node stopped. The retry path calls `status`
  before `run`.
- A dead or never-started node task is reported at once: a stale status and an
  absent status past a short grace window are retryable failures, and the node
  answers them with a bounded, redacted tail of the node task log, so the
  reason reaches the operation detail.
- A documented authentication and privilege policy: key-only authentication to a
  dedicated unprivileged account, a pinned host key, no root/sudo, no agent
  forwarding, no PTY, and no shell interpolation of any request value. The
  `known_hosts` markers are honoured: a revoked key is refused, a
  cert-authority line is skipped, and a hashed entry matches its host.
- Node-side journal compaction with a retention policy, after every task and
  through the explicit `eregion-node compact` command. A serving process never
  compacts, because every status poll starts a new one.
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
| a node task that dies mid-step is reported at once, with the node task log | `internal/transport` (in-process node and the disposable container) |
| a node task that never starts is reported at once, with the node task log | `internal/transport` (in-process node and the disposable container) |
| a revoked, cert-authority, or hashed `known_hosts` entry | `internal/transport` |
| a serving process that exits never drops the task request | `internal/noded` |
| a serving process never compacts the journal | `internal/noded` |
| killed worker, duplicate submission, cancellation at a step boundary | `internal/controlplane` against the disposable SSH endpoint |

The disposable SSH endpoint is a throwaway `sshd` container with a generated host
key; `just ssh-fixture` builds its image and the tests skip cleanly when Docker
is unavailable. See [the M0.3 SSH plan](m0.3-ssh-plan.md) for the fixture
details and the exact decisions.

## Increment B: userspace WireGuard — done

Implemented. The SSH transport runs inside a userspace WireGuard tunnel when it is
given the WireGuard dialer. The worker, the operation contract, the River arguments,
the PostgreSQL step model, the SSH transport, host-key pinning, the node protocol,
and the node binary did not change: the dialer is the only addition.

What shipped:

- `internal/transport`: `WireGuardDialer`, a `Dialer` on an in-process wireguard-go
  device with gVisor netstack (no TUN, no root). It adapts the tunnel core from
  vaire: the UAPI configuration, the 1380-byte MTU, and the 25-second keepalive.
  It also adds vaire's one-process-per-key rule as a host-local key lock.
- Lazy handshake on the first dial, bounded by 10 seconds. A tunnel that cannot
  come up is a `TunnelDownError` before any SSH byte, so nothing is dispatched. The
  error is retryable, because wrong keys look exactly like a node that is down.
- One tunnel per process, shared by every call; persistent keepalive; a liveness
  probe (ICMP echo when the tunnel is quiet) that declares the tunnel lost after 15
  seconds of silence. A loss closes open connections, which the SSH transport
  reports as `ErrStreamLost`. The next dial rebuilds the tunnel. The worker closes
  it when it stops.
- Allowed IPs limited to the node subnet (no default route, nothing broader than
  `/16` or `/48`), and an address outside them is refused without touching the
  tunnel.
- Key handling: the private key comes only from a strict-mode file, every printed
  form of it is redacted, and the CLI registers it for redaction before it builds
  anything. It never enters a request, a River argument, an event, a log, or a row.
- CLI: `EREGION_NODE_TRANSPORT=ssh` plus `EREGION_NODE_WG_*`, listed in the `serve`
  and `worker` metadata.
- Supervision: unchanged. The node runs a detached task and is queried by
  `status`; the tunnel never owns execution lifetime, and a lost tunnel never means
  the task stopped.

Delivered tests (all green):

| Scenario | Where |
| --- | --- |
| tunnel up, status, and reuse across calls | `internal/transport` (in-process node and the kernel WireGuard container) |
| handshake failure dispatches no work | `internal/transport` (in-process node and the kernel WireGuard container) |
| tunnel down, then reconnect | `internal/transport` (UDP relay black hole, both nodes) |
| lost tunnel after dispatch recovers through status, no duplicate effect | `internal/transport` and `internal/controlplane` |
| host-key pinning still holds inside the tunnel | `internal/transport` |
| shutdown, the key lock, allowed IPs, configuration validation | `internal/transport` and `internal/cli` |
| key material absent from printed forms, logs, errors, persisted rows | `internal/transport`, `internal/cli`, `internal/controlplane` |
| duplicate submission, cancellation at a step boundary, killed worker whose node task outlives it | `internal/controlplane` against the kernel WireGuard node |

The kernel WireGuard node is the SSH fixture container with `NET_ADMIN`, a `wg0`
interface, and only its UDP port published. Its tests skip cleanly without Docker,
`NET_ADMIN`, or the host's `wireguard` module. See
[the M0.3 WireGuard plan](m0.3-wireguard-plan.md) for the decisions.

## Remote supervision and reattachment — decided for SSH

The local implementation uses process lifetime plus `flock` as its liveness fence.
These questions are decided for SSH, and carry over unchanged to SSH inside
userspace WireGuard, in
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
   unprivileged account, mode 0700, files 0600. The journal is compacted after
   every task and through an explicit command; the effect records are the
   durable exactly-once evidence and are not compacted automatically.
5. **Observability:** the request ID and the execution identity cross the
   boundary; a lost stream is surfaced as an explicit unknown outcome that never
   claims the work stopped. A task that died or never started is surfaced as a
   retryable failure with the node task log, not as a wait until the deadline.

## Gate for the remote half

The remote M0.3 gate is open for the pinned SSH endpoint: it passes the same
exactly-once scenarios as the local suite, including a lost response, a killed
worker, a duplicate submission, cancellation at a step boundary, and a node task
that outlives its caller. The gate is also open for userspace WireGuard: the SSH
endpoint reached through a userspace WireGuard tunnel to a kernel WireGuard node
passes the same scenarios, with a tunnel lost after dispatch as the lost response.
Advertise only the tested paths.
