# Machine and operation contracts

Reference for current mechanisms. Product requirements and gaps are tracked only
in [MASTER](MASTER.md). Authority: `internal/contract`, `internal/output`, command
metadata/tests and [the JSON Schema](../schema/eregion-v1.json).

## Discovery and output

- `eregion commands --json` lists implemented commands, their flags, platforms,
  safety declarations, error codes, examples and next steps. `help <path> --json`
  describes one command. Metadata validation runs before normal dispatch.
- Plain is append-only by default. JSON (`--json` or `--output json`) returns one
  newline-terminated `eregion/v1` envelope. Help and expected errors use it too.
- Streaming commands opt into NDJSON; ordinary commands do not accept an arbitrary
  streaming mode. Service commands log on stderr and return their final result on
  shutdown. Explicit machine mode must never start Charm or prompt.
- Structured/no-TTY parity is a product requirement. Some newer setup/login paths
  still need qualification; discovery metadata is not proof that they comply.
- Output write failure exits 70; do not append a second envelope after a partial
  write. Human messages are not machine identifiers.

```json
{"schema":"eregion/v1","ok":true,"command":"eregion version","data":{"version":"dev"}}
```

```json
{"schema":"eregion/v1","ok":false,"command":"eregion","error":{"code":"usage_error","message":"unknown command"}}
```

MASTER adds vaire's codes to this table as their flows land: `missing_tool`
(preflight), `node_version_mismatch` (lockstep), `remote_unreachable` and
`partial_failure`.

Readers ignore unknown optional object fields and handle unknown enum members
explicitly, never as success. Breaking changes to existing field meaning/type,
required fields or exit meanings require deliberate contract/schema changes.
Binary, node-protocol and database versions are separate from envelope version.

| Exit | Error codes / meaning |
| --- | --- |
| 0 | Successful request/observation. Submission means accepted, not completed. |
| 2 | `usage_error`, `invalid_output_mode`, `input_required`, `validation_failed` |
| 3 | `not_found` |
| 4 | `unauthenticated`, `forbidden` |
| 5 | `precondition_failed`, `confirmation_required`, `conflict` |
| 6 | `unavailable`, including request deadlines |
| 70 | `internal_error`, including output failure |
| 130 | `cancelled` for interrupted client/viewer |

## Submission, identity and retry

Operation IDs are opaque `op_` + 32 lowercase hex digits. Actor kind is operator,
ci, webhook or system; authenticated provenance is not itself authorization.
Remote input cannot choose its own actor.

Submission deduplication is scoped by **actor + kind + target + idempotency key**,
bound to normalized input hash. Same key/input returns original work; changed
input conflicts. A different key is deliberately different work. Reuse the key
after a timeout; a lost response is an unknown outcome, not proof nothing ran.
Keys contain 1–128 ASCII letters/digits or `._:-`.

Current platform create/deploy helpers require an explicit key. Domain commands
generate one if omitted; resources derive one from intent if omitted. For safe
manual retry after an ambiguous response, supply and retain an explicit key.
Dry-run validates through the service without operation/job insertion. It does
not prove current host readiness, provider diff or future execution success.

Operations, initial event, relevant domain intent and River insertion commit
together. River jobs contain identity, not secret payloads. Queue status is not
the operation's authoritative domain state.

```text
queued     -> running | cancelling | failed
running    -> cancelling | succeeded | failed
cancelling -> cancelled | succeeded | failed
```

Terminal states are immutable. River retry increments the attempt of unfinished
work. Explicit retry of a failed deployment submits new work with the same frozen
target/release; it does not reset the failed operation. Do not assert a universal
corrective-link model that is not present in the schema.

Cancellation is requested explicitly. Viewer detach, request timeout and worker
shutdown are not cancellation requests. Atomic in-flight effects finish and are
acknowledged at safe boundaries. Deploy cancellation before switch preserves old
serving state; after switch the worker verifies/completes or reconciles. No
general remote-kill RPC or automatic rollback of all prior effects is promised.

## Node effects and recovery

- Effect key: `operation:step`, stable through River retries. Execution identity:
  `operation:attempt:step`. Request ID is correlation, not effect deduplication.
- Check the DB acknowledgement and node status before dispatch. Completed matching
  effects are acknowledged, not repeated. Hash mismatch/corrupt journal fails closed.
- Detached tasks outlive SSH sessions. A lost stream/tunnel does not imply a stopped
  task. A retried worker asks status before starting another effect.
- Per-effect/host file locks coordinate one host. PostgreSQL row/attempt fencing
  protects state writes; environment advisory locks serialize deployments across
  workers. River concurrency is not a distributed environment lock.
- External effects are at-least-once with convergent Check/Apply and effect evidence.
  The diagnostic marker's exactly-once test is not a universal exactly-once claim
  for Docker, DNS, Caddy or arbitrary commands. Domain reload currently bypasses
  journaled node steps; see [network](network.md).

## Events and following

Sequences, not timestamps, order events. Cursor `after` is exclusive and starts
at 0; events start at 1. Pages are bounded (1–200). 64-bit cursor/sequence/progress
counters are decimal JSON **strings**; attempt is a bounded JSON integer.
Kinds: state, step, log, warning and measured progress. Optional health, duration,
release and serving-release observations are rendered only when present.

`operations events` reads a JSON page. `operations follow` polls snapshots/pages,
deduplicates sequences and refreshes after reconnect. Missing history/gaps are
explicit, not silently skipped. Connecting/live/stale/reconnecting are viewer
states, separate from operation state. Stale means **last known**, not stopped.

- `--json` waits and returns one final observation envelope.
- `--output ndjson` streams events and a terminal result record.
- `--interactive` explicitly requests the terminal viewer and requires a TTY.
- Current follower exits 0 when it observes any terminal operation state, including
  **failed**. Check `data.state`/error code; exit 0 is not deployment success.
- Early detach exits 130 and never invokes cancellation.

```json
{"schema":"eregion/v1","type":"event","event":{"operation_id":"op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":"1","at":"2026-10-26T12:00:00Z","kind":"state","attempt":0,"state":"queued"}}
{"schema":"eregion/v1","type":"result","result":{"schema":"eregion/v1","ok":true,"command":"eregion operations follow","data":{"state":"succeeded"}}}
```

An incomplete stream without its final record is not a complete request result.

## Confirmation is not authorization

Target binding, revision/intent checks, authorization and human approval are
different protections. Current mechanisms vary:

- Bootstrap/credential-add use intent/revision/expiry-bound confirmation checks.
- CLI rollback uses a deterministic kind/target/input digest checked **client-side**;
  this is not an unforgeable server approval. Generic authenticated action
  submission can bypass that CLI check.
- Resources and upgrades have their own confirmation/precondition mechanisms.
- Current `server init --yes` bypasses its interactive apply question; it must
  never be reused as the human factor for future token issuance.

The target is MASTER D12: destructive commands take `--confirm <target>`
(`<app>`, `<app>/<env>`, `<server>`), `--yes` alone never suffices, and the node
validates the token against current state. Token issuance needs the D7 human
factor. A public digest is never a credential.

## Secret and terminal safety

Register secrets before output; redact exact values and relevant encoded forms.
Redaction is a last defense, not permission to persist plaintext in action input,
events, River or journals. Credential endpoints/export/handoff are explicit
exceptions with their own protected output rules. Secret environment injection
travels transiently through `node.Request.Secrets`, not its persisted payload.

Untrusted text is sanitized for control/ANSI/OSC and bidi formatting before human
presentation. JSON redaction decodes/re-encodes without corrupting precision or
protocol identity. Never fall back to printing raw sensitive values on errors.
