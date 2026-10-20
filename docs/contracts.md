# Agent and operation contracts

**Schema:** `eregion/v1` · **Scope:** M0.1 CLI contracts, M0.2 diagnostic execution, and the M0.3 local node boundary.

CLI discovery, operation persistence, HTTP transport, scoped authorization,
transactional River insertion, and local node dispatch are implemented. Token
issuance, destructive-operation confirmations, pinned SSH/WireGuard transports, and
interactive viewing are not. See
[control-plane setup](control-plane.md) for the current API and limitations.

## Compatibility

Use **`eregion/v1`** for response envelopes and stream records. Binary release
versions are independent. JSON Schema lives at [`schema/eregion-v1.json`](../schema/eregion-v1.json).
Tests validate actual CLI envelopes and serialized stream records against it.

Within this schema version, readers must ignore unknown object fields. Optional
fields and new command entries can be added. Do not rename existing fields, change
their meaning/types, add required fields to existing records, or change exit-code
meanings without a new schema version. Treat new enum members as unknown values
and report them explicitly rather than assuming success. Breaking operation contract
changes during development must update the schema/version and tests deliberately.

JSON Schema checks shape; domain validation adds cross-field constraints, unsigned
64-bit numeric range checks, state transitions, authorization, and expiry/revision
checks. For example, measured progress cannot exceed its total even though both
counts are wire-encoded decimal strings.

## Implemented command surface

The root, `commands`, `help`, and `version` provide discovery. M0.2 adds `serve`,
`worker`, `system migrate`, and `operations test/get/events/cancel`, plus their
help-only groups. There are no deployment stubs or implicit completion commands.
Planned capabilities appear in the roadmap, not the catalog.

`commands --path version` returns the exact command descriptor, including inherited
flags. `commands --json` lists the complete implemented tree, including root/help.
Metadata validation gates **every invocation**, not just discovery. Missing metadata,
handlers, safety declarations, flag references, or unknown error codes fail closed.

Descriptors include usage, flags, audience, platforms, mutation/destruction behavior,
dry-run support or a reason it is unavailable, noninteractive input flags, restart
requirements, output shape, stable error codes, examples, and next steps. Metadata
is a build/test gate, not an authorization mechanism or proof of handler safety.

## Output and noninteractive behavior

- `--output plain` is the default for both terminals and pipes. It is
  append-only text: successful output on stdout; errors and hints on stderr.
- `--output json` / `--json` emits exactly one newline-terminated result envelope
  on stdout. Expected failures do not also print usage, diagnostics, or a second
  copy to stderr. Help also has structured output.
- Conflicting output modes fail. If JSON was explicitly selected, output-selection
  failures are JSON too. Early command/flag failures recover output intent from
  arguments; values belonging to known flags and arguments after `--` are not
  mistaken for output flags.
- Commands never read stdin, prompt, start Bubble Tea, query terminal capabilities,
  or emit ANSI. Missing values fail with `input_required`, naming the flag.
- Output I/O failure exits 70. A partial write cannot be repaired by appending
  another envelope; the CLI emits only a fixed safe diagnostic on stderr.
- `NO_COLOR` and `TERM=dumb` require no special handling yet because output is
  already unstyled. Interactive presentation is reserved for M0.4. Foreground
  service commands log lifecycle information to stderr and emit their final result
  only on shutdown; request commands retain the one-envelope behavior.

Success example (build information abbreviated):

```json
{"schema":"eregion/v1","ok":true,"command":"eregion version","data":{"version":"dev"},"next":["eregion commands --json"]}
```

Failure example:

```json
{"schema":"eregion/v1","ok":false,"command":"eregion","error":{"code":"usage_error","message":"unknown command","hint":"Discover available commands with eregion commands --json."}}
```

Messages are for humans; agents branch on `ok`, `error.code`, and the process exit
status. Do not parse prose or treat stderr silence as success.

## Error/exit contract

- **0:** successful request. Submission success will mean accepted durably, not
  completed successfully; inspect operation state separately.
- **2:** `usage_error`, `invalid_output_mode`, `input_required`, `validation_failed`.
- **3:** `not_found`.
- **4:** `unauthenticated`, `forbidden`.
- **5:** `precondition_failed`, `confirmation_required`, `conflict`.
- **6:** `unavailable` (including a request deadline).
- **70:** `internal_error`, including failure to write output.
- **130:** `cancelled` for an interrupted client request/viewer.

A domain operation in state `cancelled` is not the same as an interrupted viewer.
Typed errors expose code/message/hint/details; wrapped implementation causes never
serialize. Untyped errors become a generic internal error, not raw implementation
text. Cobra argument/flag errors become typed usage/input errors.

## Operation identity, actors, and submission deduplication

- IDs are opaque strings: `op_` followed by 32 lowercase hex digits from 128 random
  bits. Callers retain the returned ID; do not infer placement, time, or order.
- An actor has `kind` (`operator`, `ci`, `webhook`, `system`) and a nonempty stable
  `subject` of at most 256 UTF-8 bytes. Actor kinds describe provenance, not privilege.
- The authenticated service resolves actors. A remote body cannot choose its own
  identity or gain authority by claiming `kind: system`.
- Submission keys use 1–128 ASCII letters/digits or `. _ : -`. Scope them by
  authenticated actor, operation kind, and target. The service must bind the key to
  a digest of normalized validated inputs.
- The same scoped key/input returns the original operation; the same scoped key
  with different input fails with `conflict`. Database constraints and transactional
  insertion enforce this across control-plane processes in M0.2.
- A different idempotency key is deliberately a different operation, even with the
  same input. Its node effect key also differs. Deduplication is scoped by
  authenticated actor, kind, target, and key, not by input alone.
- A timed-out submission is ambiguous: reuse its idempotency key to discover the
  original outcome. Never blindly submit again with a fresh key.

The `operations.Service` interface is shared by future application/transport/UI
adapters. It defines submit/get/events/request-cancel without coupling consumers
to River, PostgreSQL, Cobra, or Charm. Submission inputs and job arguments must not
contain credential plaintext. The implemented diagnostic accepts bounded numeric
input only; River arguments contain only the operation ID. The PostgreSQL service
and HTTP client/handler are concrete adapters, not separate business-logic paths.

## State transitions and retry

Allowed transitions:

```text
queued     -> running | cancelling | failed
running    -> cancelling | succeeded | failed
cancelling -> cancelled | succeeded | failed
```

`succeeded`, `failed`, and `cancelled` are terminal. Repeated observations do not
constitute state transitions. River retries within a still-running operation
increment `attempt`; they do not rewrite terminal state. Attempt 0 means execution
has not started. An explicit retry of terminal work creates a new linked operation;
the corrective-link persistence model and terminal retry command remain future work.

A cancellation request is not an acknowledgement. Work can complete while a
request is in flight or after reaching a noninterruptible boundary. Leaving a
viewer never invokes request-cancel. The worker observes cancellation at explicit
step boundaries: it starts no new step, but lets an atomic in-flight step finish and
acknowledges its effect. Completed effects are not rolled back. If every step had
already completed, `succeeded` is the honest terminal state; otherwise `cancelled`.

“Connecting”, “loading”, “stale”, and “reconnecting” are client observation states,
not operation states. A stale viewer must say **last known**, not assert execution
has stopped or is still healthy.

## Node execution identity and reconciliation

M0.3 introduces two internal identifiers, both derived from the public operation ID:

- **execution identity:** `op_<id>:<attempt>:<step>`, persisted in the node journal
  and in the step row while running;
- **effect key:** `op_<id>:<step>`, stable across attempts.

The public operation snapshot and event sequence do not change. A `step` event may
carry the optional additive `execution_id` field. No other event kind may carry it,
and no client needs it to submit, read, follow, or cancel an operation.

Before dispatching a step, the worker checks the PostgreSQL step row and then the
node's durable status. A completed effect is acknowledged, not re-run. A live-running
node effect is attached/retried, never started a second time. A completed effect with
a different canonical inputs hash is `precondition_failed`. River arguments still
contain only the operation ID; no secret material enters events, step rows, or River
arguments.

Exactly-once applies to the **effect key**. Duplicate submissions with one scoped
idempotency key share one operation and one effect key. River retries and lost
acknowledgements reuse the same effect key. A different idempotency key is a different
operation and therefore a different effect; future corrected retries of terminal work
must make their steps convergent rather than assuming the old effect is reused.

The local transport uses node-local `flock` plus a journal. It coordinates processes
on one host; it is not a distributed lock. SSH and userspace WireGuard transports are
future increments and will not change the worker's decision procedure.

## Events, cursors, and NDJSON

Events have operation ID, monotonically increasing per-operation sequence,
timestamp, kind, and attempt, with optional stage/message/state/measurement fields.
A `step` event may also carry the optional additive `execution_id`; M0.2 readers that
ignore unknown fields remain compatible.
Timestamps are RFC 3339; producers use UTC. Sequences—not timestamps—define order.
Sequence starts at 1. Cursor `after` may be 0. Accepted M0.2 snapshots contain at
least their accepted-event sequence; zero represents pre-acceptance in shared types.

64-bit counters (`sequence`, `last_sequence`, `after`, progress counts) are canonical
decimal **strings** on the wire to avoid JavaScript precision loss. Readers must
parse/check the unsigned 64-bit range. `attempt` is a bounded 32-bit JSON integer.

- `state`: carries a valid domain operation state.
- `step`: names an observed execution stage.
- `log`: redacted diagnostic text.
- `warning`: nonfatal diagnostic text.
- `progress`: measured current/total/unit; total must be positive and current must
  not exceed it. Unknown duration is not a percentage.

A cursor is `{operation_id, after}`. Fetch returns bounded ascending events strictly
after `after`. Reconnect reuses it and refreshes the snapshot. Clients deduplicate
by operation ID/sequence. The service must report expired retention cursors as
`precondition_failed`, not silently skip history. Sequence allocation, event/snapshot
transactionality, authorization, and bounded paging are implemented in M0.2.
Events are not pruned yet, gaps are explicit errors, and no live event-stream
transport or wake-up mechanism is exposed yet.

The tested NDJSON serializer emits records shaped as:

```json
{"schema":"eregion/v1","type":"event","event":{"operation_id":"op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":"1","at":"2026-10-16T12:00:00Z","kind":"state","attempt":0,"state":"queued"}}
{"schema":"eregion/v1","type":"result","result":{"schema":"eregion/v1","ok":true,"command":"eregion operations follow","data":{"state":"running"}}}
```

Exactly one final result terminates an ordinarily completed viewer request, not
necessarily the operation. A lost transport/partial write can truncate a stream;
absence of a final result means the request outcome is incomplete. The serializer
rejects invalid events, mixed operation IDs, nonincreasing sequences, and writes
after the final record. It never appends after a partial write.

There is **no `operations follow` command yet**. The example specifies its future
wire shape only. `operations events` currently returns a bounded JSON page.
`--output ndjson` remains rejected; future following commands will opt into streaming.

## Confirmation binding

`PlanBinding` hashes a fixed JSON representation of actor, action, target, base
revision, normalized input SHA-256, and expiry (canonicalized to UTC). Changing any
bound value changes the digest. **The digest is not an authorization token.**

A future destructive-operation service must issue unforgeable confirmations, enforce expiry,
re-resolve identity, revalidate the current revision/inputs, and compare the binding
before enqueueing a confirmed operation. Token signing/key distribution and replay
policy need design before destructive commands ship. There is no `--yes` bypass,
token issuer, or destructive operation command in M0.2.

## Redaction and safe terminal text

Adapted from eru's longest-first pattern, with invocation/request-scoped registries
instead of global mutable state. Register all nonempty secrets, including short
values, **before** emitting derived data. Common Go-quoted representations are also
registered because Cobra/pflag can quote rejected inputs before formatting errors.
One-pass replacement prevents overlapping values or the replacement marker itself
from producing partial leaks.

Dynamic JSON payloads are decoded/redacted/re-encoded so quoted, multiline strings
and object keys are redacted without invalidating JSON. Fixed protocol keys,
schema/error-code constants, and typed event identity/counters remain intact even
if a secret happens to match a public protocol literal. Numeric precision is preserved. Failure to
serialize never falls back to printing raw sensitive values. Plain rendering makes
terminal control and bidi-format characters visible; it does not execute ANSI/OSC.

Redaction is a final defense, not permission to place secrets in domain history.
It only recognizes registered exact values; encoded/transformed variants must be
registered explicitly or omitted. CLI environment credentials are registered before
parsing; API response registries include the request's Bearer credential; service
logs use a redacting, terminal-safe writer. Diagnostic jobs/events never accept
arbitrary secret material.
