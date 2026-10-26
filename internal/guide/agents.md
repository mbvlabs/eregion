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
`remote`, `audience`, `platforms` and `error_codes`.

Use `eregion help <command...> --json` or
`eregion commands --path <command> --json` for one command. The catalog is
authoritative: a command that is not listed does not exist in this build. The
target command tree in `docs/MASTER.md` grows a row at a time, so this guide
describes the implemented surface rather than promising commands that are not
here yet.

## Confirmation

Destructive commands take a confirmation token that names the target:
`--confirm <target>`, where the target is `<app>`, `<app>/<env>` or `<server>`
(MASTER D12). The node checks the token against current state.

- Never invent a token, reuse one for another target, or pass `--yes` instead.
  `--yes` never satisfies a destructive confirmation.
- Ask the human before passing a destructive `--confirm`, and pass exactly the
  token the command reports.
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
eregion operations get <operation_id> --json
eregion operations events <operation_id> --after <cursor> --json
```

`follow --json` waits for a final snapshot. Its exit code does not by itself
mean success: read `data.state` and any error code. Use `events` with the
returned cursor to read a bounded page; sequences, not timestamps, order events.
Cancellation is explicit (`operations cancel`); detaching or a timeout does not
cancel accepted work.

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

- Never ask a human to paste a secret into a prompt, an operation input, an
  idempotency key or a log.
- `secret set` reads plaintext from a protected file (`--file`), not a flag.
  `secret export` writes only to an explicit protected destination
  (`--dest`), never prints the value, and needs a TOTP code at a human's
  terminal. `secret list` shows metadata only. `secrets export` rebuilds the
  whole server handoff behind the same factor.
- A command that prints a secret once is a human handoff: show it to the human
  and do not retain it.
- Token issuance needs a human factor at the human's terminal, such as a code
  from their authenticator app or a master password (MASTER D7). There is no
  flag, environment variable or stdin path that lets an agent mint its own
  token; do not look for a way around this.

## Workflows

### Server setup and login

`eregion server init` contacts the server, shows a plan and applies versioned
host steps, then shows or writes the secrets handoff. In JSON or no-TTY use,
pass every answer, `--accept-new-host-key` or `--host-key-fingerprint`,
`--secrets-out` and `--confirm <server>`; it never prompts. The external
database URL is a protected file, never an argument.

```sh
eregion server init --host root@203.0.113.10 --hostname app.example.com \
  --database-url-file /protected/database-url --context-name prod \
  --accept-new-host-key --secrets-out /protected/prod-secrets.txt \
  --confirm prod --json
```

An interrupted init is resumed with `--resume --context-name <server>` and a
first-contact credential; `--discard --confirm <server>` drops only the local
state. `eregion login` imports the transfer bundle from the handoff into a local
context. Use `--file` and `--passphrase-file` when no terminal is available.
`eregion context list|show|use` inspects and selects contexts without printing
credentials; `context remove <name> --confirm <name>` deletes one with its key
files.

### Domains

`eregion domain add <app> <environment> <hostname> [--primary]` saves the binding
and shows the exact A/AAAA record. It is desired state: the route, DNS,
certificate and public response are separate observations, and only an all-ok
`ready` is proof. Use `eregion domain list --json` to see each state and
`eregion domain remove` (plan-bound `--confirm`) to remove an alias. Promote
another hostname with `--primary` before removing the primary.

### Secrets

```sh
eregion secret set --app <app> --environment <env> --name <NAME> \
  --file /protected/value --expected-revision 0 --json
eregion secret list --app <app> --environment <env> --json
```

Changing a secret does not deploy by itself. The next deploy resolves the
environment's current secret revisions.

### Operations and upgrades

Use `eregion operations get|events|follow|cancel` for durable work.
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
