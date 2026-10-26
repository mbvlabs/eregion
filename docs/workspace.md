# Current terminal presentation

[tui-design.md](tui-design.md) remains the dedicated **design specification** and
links to the synthetic interactive sketches. [MASTER](MASTER.md) owns product
scope/status. This file maps the existing views, not another UI plan.

## Presentation boundaries

- `internal/cli/ui`: inline guided forms/progress/cards used by `server init`, `app create` and `env create`.
- `internal/viewer`: presentation-neutral operation reducer and polling/replay.
- `internal/tui`: explicit interactive operation view.
- `internal/workspaceui`: `eregion ui` full-screen platform workspace.

Views consume shared clients/services. They do not enqueue River jobs, query
PostgreSQL or own the lifetime of server work. The current API adapter may use
private SSH/WireGuard forwarding; transport belongs below presentation.
Opening the workspace is not required for future guided app/env commands.

## Current workspace

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
cleanup (MASTER D19, work list rows 4, 5 and 9).

## Honest observations

The operation viewer displays observed stages, build logs, health details,
duration and release/serving-release IDs. Only measured progress has a percentage.
Zero/unreported probe counts remain “not reported”. Old events may fall back to
message parsing; missing observations stay unknown. Inventory may carry newer
reconciliation results than the event view.

Failed refreshes preserve last-known rows with their age. Empty inventory is not
unreachable inventory. Graphs require a configured metrics source and label unit,
window and freshness. Absent/stale/no-data is never a zero-valued healthy graph.

Detach never cancels work. `operations follow <id> --interactive` reattaches;
machine output takes precedence and must not launch Bubble Tea. Current follow
exit 0 means the observation reached terminal state, not that deployment succeeded;
read the state. See [contracts](contracts.md).

Qualification must include narrow/tiny/wide layouts, NO_COLOR, ASCII/TERM=dumb,
reduced motion, hostile log escapes, keyboard help/resize and terminal restoration.
Golden/unit tests and a browser sketch are not a live terminal acceptance run.
