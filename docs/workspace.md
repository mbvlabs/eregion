# Terminal workspaces

**Status:** M2/M3/M4/M6 terminal views implemented · 2026-10-20

`eregion ui --interactive` opens the keyboard-driven workspace. It reads the
same authenticated platform API as every noninteractive command and submits
through the same action contract. The workspace never opens SSH, touches a
database, enqueues a River job, or cancels work when it detaches.

## Tabs and keys

| Key | Tab | Collection |
| --- | --- | --- |
| `1` | Fleet | `nodes` |
| `2` | Apps | `apps` |
| `3` | Envs | `environments` |
| `4` | Releases | `releases` |
| `5` | Deployments | `deployments` |
| `6` | Resources | `resources` |
| `7` | Backups | `backups` |

`j/k` or arrows move, `g/G` jump, `/` filters, `enter` opens focused details,
`r` refreshes, `:` opens command discovery, `?` shows help, `space` pauses local
follow inside the operation view, and `q`/`Ctrl+C` detach. Detaching prints the
operation IDs and a resume command; it never requests cancellation.

Fleet rows show the declared roles, explicit node health, and the age of the
last observation (`never observed` when there is none). The focused-host line
shows the same facts plus the address. Empty and unreachable inventories keep
the last known rows when a refresh fails and mark their age; they never invent a
host, release, resource, or backup.

## Interactive actions and noninteractive equivalents

Every guided action starts with the API dry-run preview. The preview shows the
plan, warnings, idempotency key, and a plan binding derived from the exact kind,
target, and input. Destructive actions additionally require typing the target
name, and submission re-checks the plan binding, so an edited form cannot reuse
the earlier approval.

| Action | Key | Interactive behavior | Noninteractive equivalent | Destructive |
| --- | --- | --- | --- | --- |
| Create application | `a` | Guided form → dry run → submit | `eregion platform apps create <app> --idempotency-key <key> --json` | no |
| Create environment | `e` | Guided form → dry run → submit | `eregion platform environments create <app> <environment> --node <node> [--build-node <node>] --container-port <port> --host-port <port> --health-path <path> --idempotency-key <key> --json` | no |
| Create image release | `i` | Guided form → dry run → submit | `eregion platform releases create <app> <environment> --image <image> --idempotency-key <key> --json` | no |
| Preview a deployment | — | Palette documents the command | `eregion platform deployments plan <app> <environment> --release-id <release> --json` | no |
| Deploy release | `d` | Guided form → dry run → typed environment → submit | `eregion platform deployments deploy <app> <environment> --release-id <release> --idempotency-key <key> --json` | replaces serving release |
| Roll back environment | `o` | Guided form → dry run → typed environment → submit | `eregion platform deployments rollback <app> <environment> --dry-run --json`, then submit the same target with the returned `--confirm <token>` and an idempotency key | yes |
| Retry failed deployment | — | Palette documents the command | `eregion platform deployments retry --deployment <deployment-or-operation-id> --idempotency-key <key> --json` | no |
| Check node runtime | `n` | Guided form → dry run → submit | `eregion platform nodes check <node> --idempotency-key <key> --json` | no |
| Destroy application resource | `x` | Guided form → dry run → typed resource name → submit | `eregion platform submit --kind platform.resource.destroy --target node/<host> --input-file action.json --idempotency-key <key> --json` | yes |
| Create resource backup | `b` | Guided form → dry run → submit | `eregion platform submit --kind platform.backup.create --target node/<host> --input-file action.json --idempotency-key <key> --json` | no |
| Verify backup archive | `v` | Guided form → dry run → submit | `eregion platform submit --kind platform.backup.verify --target node/<host> --input-file action.json --idempotency-key <key> --json` | no |
| Restore verified backup | `R` | Guided form → dry run → typed resource name → submit | `eregion platform submit --kind platform.backup.restore --target node/<host> --input-file action.json --idempotency-key <key> --json` | yes |
| Read an operation snapshot | — | Palette documents the command | `eregion operations get <operation_id> --json` | no |
| Follow an operation | `f` (after submission) | Opens the shared operation viewer | `eregion operations follow <operation_id> --interactive` | no |
| Read the fleet snapshot | `r`/palette | Refresh the selected inventory | `eregion platform fleet --json` | no |

The resource and backup kinds are registered by `internal/resources`; the
workspace writes their canonical input objects and lets the server validate
them. `internal/workspaceui` owns the single mapping table; the test
`TestEveryDocumentedActionResolvesToTheCLICatalog` builds `eregion commands
--json` through the real CLI and fails if a row stops resolving to an
implemented command.

## Operation viewer

The M3 viewer derives its presentation only from event data the platform worker
emits:

- **Per-target state:** step IDs map to preflight, build, candidate container,
  candidate health, traffic switch, serving health, rollback, restore health,
  cleanup, and node targets. Unknown stages remain generic steps.
- **Build log:** sanitized log lines attributed by the structured `step`/`stage`
  fields the worker stamps on node log events. Hostile ANSI/OSC and control
  characters are escaped and registered secrets are redacted before rendering.
- **Health observations:** structured `health` objects with target, probe count,
  timeout/deadline, attempt, pass/fail, and detail. A zero probe count is shown
  as “not reported”. The old message parser remains only as a fallback for
  pre-structured event streams.
- **Serving release:** traffic-switch acknowledgements, deploy success, deploy
  failure, and restore events carry `release_id` and `serving_release_id`. The
  normal success path therefore states the actual serving release ID, and the
  container log supplies the immutable image and container name.
- **Cleanup:** `platform-cleanup-01-v1` is a durable step. The viewer shows it as
  a target and as a cleanup line with its measured duration; “not observed” is
  distinct from “pending”.
- **Durations:** completed node steps carry `duration_ms`, shown beside the event
  and used to mark the stage complete.

### Remaining limits

These are rendered honestly; they are not synthesized:

1. An older event stream without structured fields falls back to the last
   observed step and message parsing. A late log may then be a generic log.
2. An interrupted deployment that never wrote a switch, restore, or failure event
   shows only what the durable events prove; inventory remains authoritative for
   the after-the-fact reconciliation result.
3. Cross-node image transfer and registry credentials are not implemented, so a
   distinct `build_node` build is refused rather than shown as progress.

## Graphs and telemetry

`internal/workspaceui` defines a small `MetricsSource` interface (`Query`,
`MetricsQuery`, `MetricsSeries`). The workspace does not import a telemetry
client. When no source is configured, focused details state that no telemetry
node is configured and draw no chart. When the source returns no samples, the
view says so; it never renders unavailable telemetry as zero. A rendered graph
always names its metric, unit, source, window, sample count, and freshness.

## Verification

```sh
gofmt -l cmd internal schema examples
go vet ./...
go test ./...
```

Render tests cover narrow/standard/wide layouts, `NO_COLOR`, ASCII glyphs,
empty and unreachable inventories, destructive previews, and hostile ANSI in log
lines. Catalog tests execute the real `eregion commands --json` path.
