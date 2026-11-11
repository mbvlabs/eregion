# eregion for agents

eregion is one CLI for setting up and operating applications on servers you
own. The same binary runs on the operator's machine and on the server; after
`server init`, operator commands reach the node over SSH through WireGuard. The
secrets handoff at the end of `server init`, and the transfer bundle it contains,
are credentials: hand them to a human password manager and never print them.

## Output

Use `--json` (or `--output json`) for every call. It returns one
`eregion/v1` envelope; read `ok` first.

- When `ok` is true, use `data` and follow `next` when the next step is unclear.
- When `ok` is false, read `error.code`, `error.message`, `error.hint` and
  `error.details`.
- Plain output is for a person at a terminal. Do not parse it.
- JSON and no-TTY use never prompt. Every question has a flag; when a required
  value is missing the command fails with `input_required` and names the flag.
- Success is not proof of a remote outcome. Read the state the command reports,
  then observe the durable operation where one was created.

## Discovery

Run `eregion commands --json` before choosing flags. Every catalog entry has a
path, usage, flags, examples and safety metadata: `mutating`, `destructive`,
`supports_dry_run`, `confirmation`, `prompts`, `input_flags`, `needs_restart`,
`remote`, `audience`, `platforms` and `error_codes`. `terminal_only` marks an
interactive-only command; `human_factor` marks effectful operations that may
need a human code (dry-run remains machine-safe); `durable_submission` identifies
commands that actually submit an operation. Audience alone is not a machine-mode
capability or proof of durable work.

Use `eregion commands --search TEXT --json` to find tasks by command path,
summary or examples, and `eregion commands --check-meta --json` to validate the
catalog. Overview, help, search and metadata checks are offline and need no
workspace access. Do not combine `--search`, `--path` and `--check-meta`.

Use `eregion help <command...> --json` or
`eregion commands --path <command> --json` for one command. The catalog is
authoritative: a command that is not listed does not exist in this build.
`eregion node …` commands are server-side plumbing: they are hidden from the
operator catalog and are not an operator workflow. `docs/MASTER.md` records
product decisions and scope; Linear tracks actionable work. Discover commands
from the current binary, not an old ticket, historical work table or target tree.

## Confirmation

Destructive commands take a confirmation token that names the target:
`--confirm <target>`, where the target is `<app>`, `<app>/<env>` or `<server>`
(MASTER D12). The node checks the token against current state.

- Never invent a token, reuse one for another target, or pass `--yes` instead.
  `--yes` never satisfies a destructive confirmation.
- Ask the human before passing a destructive `--confirm`, and pass exactly the
  token the command reports.
- Every `app delete` and every protected `env delete` also needs the D7 human
  factor: a TOTP code typed at a human's terminal. Deleting an app's last
  environment counts as an app deletion and always needs that factor, even when
  the environment is unprotected. An unprotected environment that is not the
  last one is agent-operable with the exact `--confirm <app>/<env>` alone.
  `env protect` is agent-safe; `env unprotect`, exporting secrets and minting a
  token are human-only. You may preview a deletion with `--dry-run`. Do not
  attempt a human-only action on the human's behalf.
- `confirmation_required`, a missing token or a mismatched token means nothing
  was changed. It is not a retry signal.

## Dry runs and idempotency

- Pass `--dry-run` on mutating commands that advertise `supports_dry_run`
  before applying a change. A dry run validates through the service; it does not
  prove current host readiness or future execution success.
- Write commands that submit durable work take `--idempotency-key`. Choose a
  stable key (1–128 ASCII letters, digits, `._:-`) and keep it. The same actor,
  kind, target and key with the same input returns the original operation; a
  changed input conflicts.
- Reuse the key after a timeout or a lost response. A lost response is an
  unknown outcome, not proof that nothing ran.

## Accepted is not completed

A submission returns an accepted operation, not a finished deployment, route or
build. Keep the operation ID and observe it.

```sh
eregion operations follow <operation_id> --json
eregion operations follow <operation_id> --output ndjson
eregion operations get <operation_id> --json
```

`follow --json` waits for a final snapshot. A failed operation returns a failure
envelope and nonzero status; inspect its error details and operation state.
`follow --output ndjson`
replays the bounded event history and then streams new events; sequences, not
timestamps, order events. Cancellation is explicit (`operations cancel`);
detaching or a timeout does not cancel accepted work.

## Exit codes

These are the current machine contract in `docs/contracts.md`.

| Exit | Meaning and error codes |
| --- | --- |
| 0 | Success or an accepted submission. A submission is not completion. |
| 2 | `usage_error`, `invalid_output_mode`, `input_required`, `validation_failed` |
| 3 | `not_found` |
| 4 | `unauthenticated`, `forbidden` |
| 5 | `precondition_failed`, `confirmation_required`, `conflict` |
| 6 | `unavailable`, including request deadlines |
| 70 | `internal_error`, including an output write failure |
| 130 | `cancelled` for an interrupted client or viewer |

Do not retry a failure blindly. Read `error.code` first: some failures are
terminal, some need a human, and a retry may need the same idempotency key.

## Secrets and tokens

- Never ask a human to paste a secret into chat, an operation input, an
  idempotency key or a log.
- `config set` is for plain, non-sensitive values only. Credentials and other
  sensitive values belong in `secret set`, never in `config set`.
- `secret set` reads plaintext from a protected file (`--file`), not a flag.
  `secret export` writes only to an explicit protected destination
  (`--dest`), never prints the value, and needs a TOTP code at a human's
  terminal. `secret list` shows metadata only. `secrets export` rebuilds the
  whole server handoff behind the same factor.
- `app create`, `env create` and `env update` accept explicit `--github-auth`
  for an `https://github.com/...` git source. It reads the operator's
  authenticated `gh auth token` in memory and writes it as the repository read
  credential; it never prints the token and never creates a token file. This is
  opt-in because a gh token can carry broader scopes than a read credential.
  `--git-credential-file` remains the explicit-file alternative and cannot be
  combined with `--github-auth`. A read-credential name that is already a
  runtime secret or the webhook secret is refused before any gh read or vault
  write, for both the gh and explicit-file paths. The same invariant applies to
  the effective desired revision of a definition-file update, even when its
  source binding is unchanged. A dry run or idempotent replay does not read gh.
- Cloudflare provider setup is a human handoff: ask the operator to run
  `eregion --workspace NAME domain cloudflare configure` in their own terminal.
  They enter the zone and token through its hidden prompt; never request the
  token in chat, a file, flags or service-environment edits. The command stores
  it encrypted in the provider vault. `domain cloudflare status --json` is a
  metadata-only read an agent may use after the human finishes. Then preview
  exact DNS changes before applying them; saved/read-validated credentials do
  not prove DNS-write permission or a ready application.
- A command that prints a secret once is a human handoff: ask the human to run
  it in their own terminal. Never capture the one-time value for them.
- `metrics endpoint` reports the configured main hostname's public HTTPS
  metrics and HTTP service-discovery URLs, the loopback gateway, and whether
  this machine holds a local credential. The raw exporter and gateway stay
  loopback-only; a missing hostname is unconfigured with no WireGuard fallback.
  `metrics config` ordinary output always uses a password placeholder.
  `metrics config --format prometheus|alloy|targets --out FILE` writes an
  owner-only file with the real telemetry credential when one is held locally;
  never print that file or pass it as a normal log. Both discovery and scrape
  use basic auth and normal TLS verification. `metrics rotate` replaces the
  credential behind exact `--confirm <node>/metrics`, prints no password or
  hash, and warns that the protected handoff is stale. It saves the new
  password in the resolved workspace credential directory before the request
  and restores the previous one if the request fails; a lost successful
  response can leave the local password stale, so a later deliberate rotation
  may be needed. It is a deliberate destructive rotation, not a retry loop.
- Token issuance needs a human factor at the human's terminal, such as a code
  from their authenticator app or a master password (MASTER D7). There is no
  flag, environment variable or stdin path that lets an agent mint its own
  token; do not look for a way around this.
- Serious durable application state belongs in external services under the
  current recovery policy (MASTER D4/D19). Optional managed Docker resources
  can provision local dependencies, but their volume data is expendable and not
  backed up by eregion or the control database. External dependencies currently
  use config/secrets such as `DATABASE_URL`, not first-class external-resource
  records. Default volume retention still applies; expendable is not deletion
  authorization. Replacement-VPS recovery requires keys/identities and artifacts
  as well as the surviving database; reconnecting a DSN is not a supported full
  recovery procedure.

## Workflows

### Server setup and login

`eregion server init` contacts the server, shows a plan and applies versioned
host steps, then shows or writes the secrets handoff. In JSON or no-TTY use,
pass every answer, `--accept-new-host-key` or `--host-key-fingerprint`,
`--secrets-out` and `--confirm <server>`; it never prompts. The external
database URL is a protected file, never an argument.

```sh
eregion server init --host root@203.0.113.10 --hostname app.example.com \
  --workspace-name lab --node main \
  --database-url-file /protected/database-url \
  --accept-new-host-key --secrets-out /protected/lab-secrets.md \
  --confirm lab --json
```

An interrupted init is resumed with `--resume --workspace-name <workspace>` and a
first-contact credential; `--discard --workspace-name <workspace>` drops only the local
state. `eregion login` imports the transfer bundle from the handoff into private
workspace access. Use `--file` and `--passphrase-file` when no terminal is available.
`eregion workspace list|show|use` inspects and selects workspaces without printing
credentials; `workspace remove <name-or-id> --confirm <name-or-id>` deletes local access and its owned key
files.

Workspace identity names the installation; node name names its server. A
workspace is the only named operator target; local keys and routes are private access
data, not another identity. `--workspace-file` or `EREGION_WORKSPACE_FILE` selects a local **store**.
For `server init`, use a directory exclusive to that store so servers/ and
pending/ cannot mix with another store;
`--workspace NAME`, then `EREGION_WORKSPACE`, then the saved current workspace selects
access within that store. Use an explicit workspace in automation. A missing name
in the selected store is not evidence that the server is missing.

Workspace identity travels through a refreshed protected handoff and login;
never infer it from a private access alias or waive a conflicting ID/fingerprint.
A human must refresh `secrets export` after identity or recoverable credential
changes; old exports do not update themselves. Agents inspect only metadata and
human confirmation, never the exported content.

Fresh setup requires `--workspace-name NAME` independently of `--node`. An upgraded
alpha.6 installation remains unconfigured until explicitly assigned through
`apply --assign-workspace NAME`; preview first and obtain authorization for that target.
Reads and migration never assign a workspace from a private access alias. Old bundles
remain unconfigured access artifacts; ask for a human-refreshed export to carry
the portable identity. If an old pending init already applied its host phase,
finish that pending flow before assigning through `apply`; do not edit its
private JSON. Repeating an existing workspace choice on resume must not change
its identity.

### Deploy, rollback and promotion

Git sources use Buildpacks; existing image sources bypass builds. Repeat
`--buildpack` in the desired order, or omit it for builder detection. An empty
builder uses the build node's platform default; an explicit builder selects an
image. Configure optional Go targets with `--go-target`, Node inputs with
`--frontend-directory` and repeated `--frontend-script`, and advanced inputs
with `--go-version`, `--go-build-flags` and `--node-version`. Supplied typed
values override corresponding raw build environment keys; clearing one removes
the override. `env update --build-env ''`, `--go-target ''`, and
`--buildpack ''` clear their lists; `--builder ''` resets the builder. TOML
definitions use `[source.buildpacks] order = [...]` via `--from`.
Normal `app export` refuses unsupported state; `app export <app> <env> --inspect`
provides non-secret metadata for inspection only and cannot be applied. It does
not reconstruct provider ownership or DNS automation.

Processes are `web` (domains), `worker` (no port), or `service` (private
port). Web and service processes answer on their process-name DNS alias and
declared container port on the environment network. Current deployment health
gates probe web replicas only and require a healthy web upstream; service and
worker health is not independently gated. The release command is separate and
runs once per release, not once per deployment or as a fourth process kind.
Use bare executable names such as `ssr`, `migrate` and `queue` when installed
on the effective launch-time PATH; explicit paths are used as given. A
commandless plain or CNB image uses its native default. Process and release
command text accepts quoted argv and backslash escapes, not expansion, globs,
substitution or implicit shell operators; `/bin/sh -c` is explicit if needed.

For supported submitting commands, terminal plain output follows by default;
JSON and piped plain output return acceptance only. Explicit NDJSON follows
unless detached. In every mode acceptance is not completion.

`eregion deploy <app> <env>` builds the current source or deploys an existing
release from the same environment; `rollback` restores an earlier release with
its recorded revision and secret snapshot. Both are durable operations: follow
the operation and read the serving state before claiming success.

`eregion env promote <app> <source-environment>` moves the source's current
serving release to the same app's fixed `production` environment without
rebuilding. Production keeps its own domain, plain config, secrets and release
command; the command never copies source domains, config, secrets or command.
Use `--dry-run --json` first. The source must be serving a ready, unpruned release
and both environments must be on the same node. A repeated submission with the
same `--idempotency-key` returns the original operation; an artifact already
serving or actively deploying to production is refused. `deploy --release` is
not a cross-environment promotion and refuses another environment's release.

### Domains

`eregion domain add <app> <environment> <hostname> [--primary]` saves the binding
and shows the exact A/AAAA record. It is desired state: the route, DNS,
certificate and public response are separate observations, and only an all-ok
`ready` is proof. Use `eregion domain list --json` to see each state and
`eregion domain remove` (plan-bound `--confirm`) to remove an alias. Promote
another hostname with `--primary` before removing the primary.

### Push to deploy

Explicit `env update <app> <env> --auto-deploy --github-webhook --json` uses
authenticated local `gh` to install the hook without showing the HMAC secret.
`--rotate-webhook-secret --github-webhook` deliberately replaces it. This is
different from `--github-auth`, which supplies a repository clone credential.
Repository hook administration must pass preflight before any secret write.
Without explicit gh assistance, enabling/rotating is a human-terminal flow;
never request the manually displayed secret from the human.

GitHub and the platform are not one transaction. If hook installation fails
after activation, read the safe partial-failure details: a new secret may already
be active. Do not claim rollback or print the value; fix gh and request explicit
rotation recovery with a **new** key. A same-key replay returns the original
acceptance; it does not retry or verify hook installation. This explicit recovery
is not permission to blindly retry an unknown outcome. A signed push's 202
response means admitted, not deployed;
follow the resulting operation and inspect serving state.

### Secrets

```sh
eregion secret set --app <app> --environment <env> --name <NAME> \
  --file /protected/value --expected-revision 0 --json
eregion secret list --app <app> --environment <env> --json
```

Changing a secret does not deploy by itself. The next deploy resolves the
environment's current secret revisions.

### Operations and upgrades

Use `eregion operations get|follow|cancel` for durable work.
`eregion upgrade --dry-run --json` plans a local CLI upgrade; it never upgrades a
node. Node upgrades belong to the host-convergence flow, not to an agent guess.

### Agent harness

`eregion guide agents` prints this guide. `eregion skill install` writes the
same text as `SKILL.md` into the supported agent harness directories. In a
terminal without `--harness` it asks which harnesses to use; JSON and no-TTY use
must pass `--harness`.

```sh
eregion skill install --dir /path/to/project --harness claude --json
```

It is idempotent: the same document is overwritten in place. Run it with
`--dry-run` to see the target paths first.

## Refusals

- Never print the secrets handoff, a transfer bundle, its passphrase, a token or
  a secret value.
- Never mint a token or lift a human factor to make an agent workflow work.
- Never pass a destructive `--confirm` or `--yes` without the human's explicit
  approval of that exact target.
- Never retry work whose outcome is unknown without the same idempotency key.
- Stop and ask the human when the catalog, the error hint or the plan is not
  clear.
