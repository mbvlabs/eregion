# Terminal interface direction

**Design reference, retained for ongoing implementation.** Product scope and
completion status live in [MASTER](MASTER.md); the current presentation code is
described in [workspace](workspace.md). This document defines the intended
experience, not a claim that every screen or interaction is complete.

Open the [interactive sketches](design/tui-sketches.html) to review the visual
direction. All servers, operations, log lines, timings and charts are synthetic.

## One platform, three presentations

**Agent first does not mean human-hostile.** Build an excellent terminal interface
as a projection of complete machine-readable contracts, never as the place where
business logic lives.

1. **Structured:** JSON results and NDJSON events, stable schemas/codes, no prompts,
   animation, ANSI, or terminal queries. Preferred for agents and CI.
2. **Plain:** append-only text suitable for pipes, logs, accessibility, and limited
   terminals. No cursor motion or hidden state.
3. **Interactive:** Charm-based inline progress or opt-in full-screen views. Same
   operations, event identities, authorization, and confirmation requirements.

Explicit machine mode always wins; non-TTY output uses plain text unless machine
mode was requested. A TTY can show inline progress; full-screen navigation is
explicit. Guided app/environment commands should use the same interview, plan,
approval, live progress and result-card language as `server init` and vaire.
The full-screen workspace must not be required to complete ordinary work.

## Visual direction: restrained, precise, alive

- Quiet terminal-native ground; warm amber focus/active work; green success; yellow
  caution; red failure. Always pair color with words and distinct glyphs.
- Consistent left alignment, compact typographic hierarchy, whitespace, and thin
  separators. Use borders for meaningful regions, not every field.
- Keep operation identity, context, environment, and serving release visible.
  Never hide a production context behind a generic dashboard title.
- Avoid giant ASCII logos, decorative animation, emoji, Nerd Font requirements,
  excessive rounded boxes, or mouse-only actions.
- Respect terminal light/dark backgrounds and downsample color. `NO_COLOR` disables
  color, not essential status information. Offer ASCII symbols as a fallback.
- Use modest, purposeful motion. Reduced-motion mode replaces animated spinners
  with static state labels and retains changing elapsed/status information.

Browser sketches illustrate cell-based layouts, not pixel-perfect terminal output.
Fonts and terminal cell metrics belong to the user's terminal. Validate the actual
Bubble Tea/Lip Gloss rendering before declaring the visual design complete.

## Charm stack and component boundaries

- **Bubble Tea v2:** event-driven rendering, keyboard handling, resize, screen mode.
- **Bubbles v2:** spinner, progress, viewport, help, inputs, list/table components.
- **Lip Gloss v2:** shared theme, spacing, borders, responsive composition.
- **Huh v2:** guided forms, as used by current `server init`. Every field must
  have a noninteractive equivalent.
- **Charts:** start with small terminal sparklines/bars. Evaluate an external chart
  library only when it supports the chosen Bubble Tea version and accessibility.

Keep a plain operation model/reducer independent of Bubble Tea. Client event
adapters update it; JSON/plain/interactive presenters consume it. Route actions
through a shared application client. Do not enqueue River jobs inside a view
update function. Presentation must not depend on direct database access or own
the execution transport/lifetime.

Pin released compatible versions during implementation; upstream `main` is not a
dependency policy. Official references:
[Bubble Tea](https://github.com/charmbracelet/bubbletea),
[Bubbles](https://github.com/charmbracelet/bubbles),
[Lip Gloss](https://github.com/charmbracelet/lipgloss).

## Primary screens

### Operation viewer

The existing implementation is in `internal/viewer` (presentation-neutral reducer)
and `internal/tui` (Bubble Tea presentation). The reducer extracts per-target
deployment state, build log lines, health observations and serving-release evidence
from events. See [workspace](workspace.md) for current behavior and limits.

The main question is **“What is happening, and what can I safely do next?”**

- Header: context, application/environment, operation ID, operation status.
- Stage list: completed, active, pending, failed, skipped. Stage changes come from
  events or an explicit server plan, not a client-side timer or guessed duration.
- Detail pane: bounded, redacted, terminal-safe event/log viewport; target/server
  identity; attempt and timing where known.
- Footer: connection freshness, follow/pause state, and context-sensitive keys.
- On completion: stable summary, serving release, warnings, and copyable next step.
- On failure: failed stage, stable error code, serving-state observation, and safe
  diagnostic/recovery command. No automatic claim that retry is safe.

The sketches include an inline compact view and an expanded view. Most invocations
should not require entering a full-screen management application.

### Fleet and application overview

Role-first rows distinguish control-plane, application, and build hosts. Show
health, current work, last observation, and a focused detail region. Preserve a
selected row when background data refreshes. A degraded host should not disappear
because the sort order changed.

Application/environment details must distinguish desired configuration from
applied and observed state. A saved domain is not proof of a route, valid
certificate or healthy public application: show DNS, route application, TLS and
public health independently, with pending/failed/unknown states.

Optional charts identify source, unit, time window, and freshness. If no external
backend is configured, say so and show a setup action. Do not synthesize charts or
display “0” for unavailable telemetry. Durable operation history remains available.

### Plan and confirmation

Show target context, exact proposed changes, destructive effects, affected workloads,
and a revision-bound confirmation token. A form can help supply inputs but cannot
bypass server-side checks. Never use a generic “yes” action as blanket authorization.

No working destructive controls are included in the browser sketches. Current
confirmation mechanisms vary; [contracts](contracts.md) separates existing
behavior from the required server-validated protection.

## Loading and progress semantics

- **Connecting/loading:** indeterminate indicator plus the action and elapsed time;
  no pretend operation ID or claimed server status before a response arrives.
- **Accepted/queued:** show the real ID, queued duration, and wait reason when known.
  Unknown queue position stays unknown.
- **Running:** spinner plus active stage and elapsed time. Completed-stage count is
  a count, not an estimate of total duration.
- **Measured transfer/work:** progress bar only with a known denominator, such as
  bytes transferred. Do not show an invented overall deployment percentage.
- **Waiting for health:** show actual check observations and remaining deadline
  when known; do not imply that more probes mean better health.
- **No new events:** do not equate silence with failure. Distinguish a healthy idle
  stream from missed heartbeats, stale observations, and a closed connection.
- **Reconnecting:** prominently label last known status and its age. Replay from the
  last event cursor, deduplicate events, and refresh the authoritative snapshot.
  A progress spinner must not falsely claim the worker is currently alive.
- **Cancelling:** distinguish requested cancellation from acknowledged cancellation;
  warn if an irreversible stage cannot be interrupted safely.
- **Completed/failed/cancelled:** stop active animation and preserve the result.

On worker retry, show the attempt and why it changed. Monotonic event sequence is
not the same as monotonically increasing progress: reconciliation can revisit work.

## Interaction and layout

Key conventions: `?` help, `tab` change pane, arrows or `j/k` navigate,
`/` filter, `g/G` start/end of logs, `space` pause local follow, `q` leave/detach.
Pausing follow changes the viewport only; the server operation continues. Keyboard
focus is visible. Mouse support is optional enhancement.

Leaving a viewer, including Ctrl+C, does **not** request operation cancellation.
Cancellation is an explicit command/action with server-side authorization and safe
boundary semantics. Print the operation ID and resume command when detaching.

- **Wide terminal:** stages/details beside a log viewport; bounded line lengths.
- **Standard terminal:** stage summary above logs; no critical data clipped.
- **Narrow terminal:** single-column essentials with wrapped details, no dense tables.
- **Very small terminal:** actionable compact summary or plain mode; never panic.

Use measured terminal-cell widths, not byte/string lengths. Account for Unicode,
long app names, paths, and hostile log control characters. Preserve meaningful
timestamps and identifiers in copyable detail views.

## Acceptance checks

- Reducer tests cover ordered/replayed/duplicate events, stale snapshots, retry,
  reconnect, cancellation, failure, success, and event retention gaps.
- Golden render tests cover narrow/standard/wide layouts, light/dark/no-color/ASCII,
  empty data, long labels, unavailable telemetry, and secret redaction.
- Manually exercise resize, navigation, detach, interrupt and terminal restoration
  on the selected test host; check screen-reader-friendly plain output separately.
- Machine output never starts Bubble Tea, prompts or inherits presentation escapes.
- Test injected ANSI/OSC sequences and unbounded log bursts before shipping logs.
- No animation implies progress that has not been observed; no chart hides its
  data source or stale state.
- Guided setup and its flag-only equivalent produce the same intent and safety
  checks; final cards never turn accepted or pending work into claimed readiness.
