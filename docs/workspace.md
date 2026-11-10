# Current terminal presentation

[tui-design.md](tui-design.md) remains the dedicated **design specification** and
links to the synthetic interactive sketches. [MASTER](MASTER.md) owns product
scope/contracts; Linear tracks execution. This file maps the existing views,
not another UI plan.

## Presentation boundaries

- `internal/cli/ui`: inline guided forms/progress/cards used by `server init`, `app create` and `env create`.
- `internal/viewer`: presentation-neutral operation reducer and polling/replay.
- `internal/tui`: explicit interactive operation view.
- `internal/workspaceui`: `eregion ui` full-screen platform workspace.

Views consume shared clients/services through node RPC over pinned SSH inside
WireGuard. They do not enqueue River jobs, query PostgreSQL or own the lifetime
of server work; transport belongs below presentation.
Opening the TUI is not required for guided app/env commands. The full-screen
view is not itself a workspace identity or an authorization boundary; it renders
the workspace/node selected through this operator's local context.

## Current TUI

The header distinguishes the actual node name, portable workspace and captured
local context from the SSH route. A failed authoritative workspace read is
shown as unknown, separately from a genuinely unconfigured installation.

Tabs: `1` Fleet, `2` Apps, `3` Envs, `4` Releases, `5` Deployments. Keys:
arrows/`j/k`, `g/G`, `/` filter, `enter` details, `r` refresh, `:` discovery,
`?` help, `q`/Ctrl-C detach. The operation view's space key pauses the local
viewport, not execution.

Existing workspace forms cover deploy and rollback. They preview through the
node-mode action service and show their exact noninteractive command equivalent.
The mapping is owned by `internal/workspaceui/discovery.go` and catalog tests.
App and environment setup is not a workspace form: use the guided
`eregion app create` and `eregion env create` commands. These views do not
provide app/env deletion. The former app/env and image-release forms, Resources
and Backups tabs, node-check action and their forms are deleted with the surface
cleanup (MASTER D6/D19; historical acceptance reference).

## Honest observations

The operation viewer displays observed stages, build logs, health details,
duration and release/serving-release IDs. Only measured progress has a percentage.
Zero/unreported probe counts remain “not reported”. Old events may fall back to
message parsing; missing observations stay unknown. Inventory may carry newer
reconciliation results than the event view.

Failed refreshes preserve last-known rows with their age. Empty inventory is not
unreachable inventory. Fleet details show one current `metrics.show` snapshot
from the node's loopback node_exporter: one measured CPU sample plus
current memory, filesystem, load and uptime facts, with source and explicit
freshness named. Unknown exporter time is not replaced by collection time; the
node-computed age and threshold are aged with monotonic local receipt time, and
a focused-detail refresh re-queries metrics. A query for a different node is
refused rather than relabeling this snapshot. There is no telemetry server,
history or PromQL, so the view never draws a multi-sample history graph or
invents request metrics. Unavailable/stale/no data is never a zero-valued
healthy graph.

Detach never cancels work. `operations follow <id> --interactive` reattaches;
machine output takes precedence and must not launch Bubble Tea. A followed
failed operation returns its recorded error code (fallback `precondition_failed`),
not exit 0 merely because polling succeeded. Remote cancellation and viewer
detach both return cancellation status with distinct messages; detach explicitly
says the operation was not cancelled. Accepted-only submissions still do not
claim completion. See [contracts](contracts.md).

Qualification must include narrow/tiny/wide layouts, NO_COLOR, ASCII/TERM=dumb,
reduced motion, hostile log escapes, keyboard help/resize and terminal restoration.
Golden/unit tests and a browser sketch are not a live terminal acceptance run.
