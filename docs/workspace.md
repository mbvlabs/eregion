# Current terminal presentation

[tui-design.md](tui-design.md) remains the dedicated **design specification** and
links to the synthetic interactive sketches. [MASTER](MASTER.md) owns product
scope/status. This file maps the existing views, not another UI roadmap.

## Presentation boundaries

- `internal/cli/ui`: inline guided forms/progress/cards used by `server init`.
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

Existing forms cover app/env creation, image release and deploy/rollback. They
preview through the action API and show their exact noninteractive
`platform …` command equivalent. The mapping is owned by
`internal/workspaceui/discovery.go` and catalog tests. These forms do not
provide complete source/settings/secrets/domain setup or guarded app/env
deletion. Do not confuse an app form with deploycrate's complete application
workflow. The former Resources and Backups tabs, node-check action and their
forms are deleted with the surface cleanup (MASTER D19, work list row 9).

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
