# Operation evidence and read-only diagnostics

## Status and intent

**Approved 2026-11-14 through Plannotator, with an emphasis adjustment:** useful
failure evidence comes first. Reuse normal redaction guardrails; do not turn this
into a separate security project or unnecessarily suppress useful diagnostics.
Approval establishes the contract, not deployment or qualification. MASTER
remains the product authority; Linear owns actionable work and status. This
requested spec is not a parallel backlog.

The primary outcome is useful evidence from the execution that actually
happened, especially when it failed. Operators and agents should see what
failed, what was observed and what could not be recovered, without launching a
new execution to guess at the cause. API and worker Prometheus instrumentation
is also in scope, but is not a substitute for durable operation evidence.

## Outcomes and scope

Every defined action returns a useful typed result. A failure retains safe,
bounded evidence from the original action whenever possible, before cleanup
destroys it. Existing progress and events remain available. A caller can follow
an accepted operation to a meaningful terminal result, and diagnose it later
without changing the system.

This covers the existing Go API and River worker, shared application services,
node RPC, operation history and existing authenticated telemetry integration. It
does not add another worker process, queue, event bus or service for jobs.

## Existing execution model

The API accepts an operation and its River job atomically in PostgreSQL. The
existing worker executes defined actions in the background and records their
results. Acceptance means queued work, not successful completion. Losing the
caller's connection does not own or cancel that work. The API and worker remain
Go processes supervised by systemd; ephemeral node-mode RPC invocations are not
new persistent metrics processes.

## Action result and evidence contract

Results identify the operation, action, attempt and target, with revision,
source, release, artifact or container identities where relevant and known.
They include status, timing and action-specific typed return values rather than
an undifferentiated success message. Failure includes a stable stage, safe
error code and message, plus evidence availability.

Capture the original command's stdout and stderr or build logs, actual managed
container exit, OOM and health observations, and safe launch context when
available. Include useful actual Docker or Caddy command errors, without
introducing a generic host-shell facility. These are observations, not a promise
that every source emits output. Logs cannot reconstruct swallowed child stderr
or other facts that were never observed.

Distinguish observed facts from inference and label unavailable, not collected,
truncated and interrupted evidence explicitly. Never invent a root cause.
Historical failure-container snapshots and current live observations are
separate, identified and timestamped; a healthy current container does not
rewrite the original failure. Include the actual lifecycle state of stopped as
well as running candidates when available. Container existence or a default
zero exit code alone is not proof that the command ran; identify whether a start
and exit were actually established.

## Capture before cleanup

Collect evidence from the original execution before every relevant cleanup
path, not only the nominal finalizer: terminal failure or cancellation,
same-name container replacement, early container-port failure and an actual
release command that would otherwise disappear through automatic removal.
Existing build and candidate evidence remains available through its established
selectors. Capture must identify and inspect the actual owned execution, not a
new diagnostic container.

Capture is best effort and has a bounded budget. It must never veto traffic
restoration or safe cleanup. If collection or PostgreSQL is unavailable, proceed
with safety work and report the loss wherever durable reporting remains
possible. Where a crash or removal makes recording impossible, later reads must
not claim complete capture. Capture failure grants no additional cleanup
authority: existing ownership and execution fences still apply.

## Failure response and read-only diagnosis

Submission returns an accepted operation ID. Following a terminal failure
returns its stable failure stage and summary, a safe bounded excerpt, evidence
availability and operation/cursor references for retained history. Do not put
all logs inline. Plain and JSON/non-TTY callers receive consistent facts, with
no prompts; node RPC and API use the same application-service contracts,
subject to their existing authority boundaries.

Diagnosis is read-only: it creates no operation, container or other execution.
It correlates the original operation with its source build, release, immutable
image, history and actual managed containers where those relationships are
provable. Preserve real build and candidate log selectors. A diagnostic read
failure reports that failure separately and never rewrites the original
operation or presents evidence-fetch success as execution success. Removed
execution flags and diagnostic actions have no compatibility aliases.

## Durability, retries and evidence loss

Use the existing durable operation and step records, attempt fences and
idempotency. Preserve each failed attempt's result and evidence. A legitimate
subsequent attempt may succeed under the existing retry semantics; it must remain
distinct from the earlier attempt. Evidence collection or retrieval must not
trigger re-execution, duplicate a result or change execution status. Report the
real action exit state, independently of evidence retrieval.

Separate pagination or a bounded read from actual capture loss: a next cursor
means more retained history may be read, not that data was lost. Conversely,
missing, truncated or interrupted capture must not look like complete history.
Preserve generic readability of old operation history; do not delete old rows or
journals, or add compatibility shims or historical rewrites.

## Useful output and collection bounds

This is an operator tool for user-owned servers. Return the useful failure
summary, observed state and available evidence; do not hide the whole result
because one log source is unavailable. Use existing target and ownership checks,
not a new authorization or secret-gating system for read-only diagnosis.

Apply existing registered-secret redaction before storing or displaying output.
For historical output, use the original action's pinned values rather than
current values after rotation. If those values are unavailable, omit only the
affected text and explain the gap; other useful facts remain available. An
action with no required secrets needs no additional secret prerequisite.

Redact bounded text before normalization or truncation and again after text
sanitization, so presentation cannot reconstruct a known secret. Do not persist
known plaintext secrets or dump secret-bearing environment/argument values.
These are guardrails on evidence handling, not the purpose of this work.

Reuse existing capture and read limits where possible. Any additional limit
is **proposed**, not an already approved numeric cap. Bounded capture does not
guarantee recovery of output that exceeded its budget or was never forwarded.

## Docker logging and retention

Fresh servers configure Docker's default logging driver as `local`, with
`max-size=20m`, `max-file=5` and compression enabled. Docker manages stdout and
stderr rotation: approximately 100 MB nominal uncompressed per container, by
size and count rather than days, not a strict disk quota. Removing a container
removes its native Docker logs. Bounded, redacted evidence retained in existing
PostgreSQL operation history is distinct from those native logs and is not a
general application-log archive.

This is fresh-server behavior only: no pre-v1 migration of existing containers
or compatibility path is required. Normal container logs remain readable
through Docker without a central collector.

## API and worker metrics

Instrument the real API and River worker with a Prometheus client; the client
is not present today. Export process and Go runtime measurements, HTTP request
counts, in-flight requests, latency and outcomes by bounded route class, method
and status, and actual job/action attempt duration, outcome and retries.
Distinguish capacity waits and snoozes from failures. Include in-memory database
pool statistics where available. These are real producer measurements, never
placeholder or constant-healthy series, and scrapes must not run expensive
database queries.

Labels are bounded, such as component, node, workspace, job kind, action kind
and status. Never label with operation or app IDs, raw URLs, error strings,
DSNs or secret values. Metrics are low-cost process observations, not durable
audit records or authoritative operation status. Export failure must not stop
the API, worker or applications; support clean process shutdown.

Extend the existing authenticated HTTPS gateway, discovery and generated
collector configuration with fixed API and worker targets, retaining the
existing node_exporter target and its host disk-space and inode measurements.
Process listeners remain loopback-only. External collectors use authenticated
HTTPS and normal TLS verification; raw ports and management APIs are not public,
and WireGuard-only reachability is not a fallback. No numeric port or new path
is selected by this spec. No managed collector or backend is required.

## Acceptance and verification

Automated behavioral acceptance uses real PostgreSQL and/or running
API/application-service integration, with explicitly confirmed disposable
fixtures, never an ambient production DSN. No unit or mock-only tests, Docker
or VM harnesses are added. Build, vet and static checks remain applicable.

Integration acceptance must establish:

- Failures capture real available output and container observations before each
  relevant cleanup, including early failure, replacement, release-command
  removal and cancellation. Full, partial, unavailable, truncated and interrupted
  evidence are distinguishable; paging is not mistaken for loss.
- Redaction protects multiline and control-character secrets across
  normalization and truncation, including secret rotation and pinned historical
  context. Missing ownership, placement or redaction context fails safely.
- Retries, crashes and cancellation preserve fencing, original exit state and
  safe traffic restoration, with no duplicate effects or executions just for
  evidence. Capture/database failure cannot block safety cleanup.
- Terminal JSON and non-TTY responses expose safe failure evidence and cursors.
  Diagnosis remains read-only; removed execution flags and actions are
  unregistered, no sandbox is created, and read failure leaves the original
  operation unchanged.
- Metrics reflect real requests and attempts, separate waiting from failure,
  use bounded labels, and do not block work when export fails. Authentication,
  discovery, generated targets and preservation of the node target are verified
  through the relevant running services.

Fresh-host Docker rotation, SSH/WireGuard and actual process scraping require
separate manual qualification on an explicitly authorized fresh host and
appropriate dedicated database. Build or static checks do not establish those
host results. Spec approval does not authorize changing a live server, database
or release. Local disposable verification follows the repository test policy;
fresh-host qualification requires separate target authorization.

## Deferred work and decisions

Out of scope: executable diagnostic containers and their proposed database
transcripts; arbitrary new diagnostic execution; a generic log store, queue or
daemon; scheduled River log harvesting; Alloy, Vector, Loki, Grafana or a
telemetry node; and any new artifact or release model. cAdvisor was investigated
and may be considered later, but is not required for this contract. There is no
new backup or historical migration commitment.

No product decision remains open in this spec. Exact implementation limits and
target endpoint details are implementation choices constrained by the contracts
above, not approval of new limits, ports or public surfaces.
