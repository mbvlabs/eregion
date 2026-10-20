# Bootstrap a control plane (M1)

`eregion bootstrap` converges one fresh Debian or Ubuntu host. It has two
surfaces with the same steps and the same node-local journal:

- **local**: the binary already runs on the target host as root;
- **remote first contact**: a workstation uploads the verified binary over
  pinned SSH, then runs the same local commands on the host.

The platform database is not required to record pre-database progress. The
journal lives under the bootstrap state directory, so a first contact that
fails before PostgreSQL is reachable can be resumed without the platform store.

## Supported matrix and placement

| Distribution | Releases | Architectures |
| --- | --- | --- |
| Debian | 12, 13 | amd64, arm64 |
| Ubuntu | 22.04, 24.04 | amd64, arm64 |

`internal/host/platform.go` is the single source of truth. Any other release or
architecture is rejected before a package, account, service, or file mutation.
The detection normalizes `x86_64`/`aarch64` fixture values to Go's `amd64`/
`arm64`.

| Role | Docker runtime | Buildpacks (`pack`) | OCI registry | Node binary/config | Control plane |
| --- | --- | --- | --- | --- | --- |
| `main` | yes | yes | yes, loopback `127.0.0.1:5000` by default | no | API + worker |
| `application` | yes | no | no | yes | no |
| `builder` | yes | yes | no | yes | no |

The main node therefore installs the application runtime, the Buildpacks build
stack, and the container registry. **No telemetry stack is installed by any
bootstrap role.** `telemetry` and `edge` are rejected explicitly; telemetry is
an M5 role that must never share the control plane.

The registry image is pinned by digest. `pack` is downloaded from the pinned
release artifact and its SHA-256 is verified. Package installation uses the
distribution repositories; there is no third-party apt repository.

## Local workflow

```sh
# The DSN is supplied through a private file or EREGION_DATABASE_URL, never a flag.
sudo eregion bootstrap plan \
  --role main --node main \
  --database-url-file /root/eregion-db.url \
  --operator-token-out /root/eregion-operator.token \
  --json

# Review the plan and diff, then apply with the exact token.
sudo eregion bootstrap apply \
  --role main --node main \
  --database-url-file /root/eregion-db.url \
  --operator-token-out /root/eregion-operator.token \
  --apply --confirm '<token from plan>' --json

sudo eregion bootstrap status --role main --node main --json
```

Useful facts:

- `plan`, `diff`, and `status` are read-only. They never write the journal,
  start a service, or install a package.
- `apply` requires both `--apply` and the plan-bound `--confirm` token. The token
  expires after 15 minutes and is bound to the canonical intent, generation, and
  the access-change list.
- The JSON output of a failed apply carries `operation_id`, `step`, `journal`,
  `resumable: true`, and the completed steps. The engine journal is written
  **before** the database step.
- A re-run with the same intent resumes the same generation and operation ID.
  Completed effects are acknowledged, not re-applied. A changed intent, or a
  completed generation with drift, starts the next generation.
- Bootstrap and node effects share the host lock (`host.lock`) when the state
  directories are separate, so a bootstrap drop cannot race a live node effect.

State layout (paths are configurable in `host.Layout`, production defaults):

```text
/var/lib/eregion/bootstrap/state.json        generation/operation index
/var/lib/eregion/bootstrap/journal.json      engine step journal
/var/lib/eregion/bootstrap/secrets/          owner-only cached credentials
/var/lib/eregion/bootstrap/commit/<op>/      rollback snapshot and watchdog
/var/lib/eregion/node/                       node execution state
/etc/eregion/                                auth, environment, TLS, node config
```

## Remote first contact over pinned SSH

```sh
eregion bootstrap remote \
  --ssh-address root@203.0.113.10:22 \
  --ssh-identity /root/eregion-bootstrap \
  --ssh-known-hosts /root/known_hosts \
  --role main --node main \
  --database-url-file /root/eregion-db.url \
  --apply --json
```

- The host key is pinned with the same `known_hosts` implementation the node
  transport uses. An unpinned host, a changed key, a revoked key, or a
  group-readable identity fails before upload. There is no trust-on-first-use.
- The command uploads the exact running `eregion` binary (and the node binary
  for node roles) plus private inputs as a tar stream over the SSH channel. It
  then runs `eregion bootstrap plan` and `eregion bootstrap apply` on the host.
- Remote plan and apply use the **same** uploaded bundle path, so the plan token
  binds to the same binary path and content. The bundle is removed after the
  action; artifacts that failed to converge remain in the host journal.
- Step progress is streamed from the remote stderr while the final envelope stays
  on stdout. An interrupted connection is a lost progress stream, not a lost
  journal: re-running picks up the same generation on the host.
- `eregion bootstrap remote --dry-run` returns only the remote plan. Nothing is
  applied.

### Credential handoff

The initial operator token is generated **on the host**. It is written once to
the protected remote path (`--operator-token-out`, or
`/var/lib/eregion/bootstrap/operator.handoff` by default) with mode 0600. The
remote command reads it once over the SSH channel, validates its token shape, and
stores it in the local context file with mode 0600. The token is never printed in
plain or JSON output. Use `--token-file-out /private/path` only when another
protected file is explicitly required.

`--context-name` names the saved context (default: the node name).
`--context-url` sets its API origin. If it is omitted and the API listens on
loopback, the context records `http://127.0.0.1:<port>`: the operator must
reach it through the intended tunnel or reverse proxy. Bootstrap does not
pretend a loopback listener is publicly reachable.

## Contexts and credential precedence

`eregion context login/list/show/use/export` stores credentials in a private
file (`$XDG_CONFIG_HOME/eregion/contexts.json` or `EREGION_CONTEXT_FILE`). The
file is rejected if it is readable by group or others. Views expose only a
12-character token fingerprint.

Client resolution is explicit:

1. `--api-url` (flag);
2. `EREGION_API_URL` / `EREGION_API_TOKEN` (environment);
3. the active saved context.

An explicit origin never receives a saved token for a different origin. If the
origin is explicit but no token is available, the command fails with
`input_required` instead of silently reusing another context's credential. The
same origin may reuse the saved token. `EREGION_API_URL`/`EREGION_API_TOKEN`
continue to work exactly as before for `scripts/dev.py`, `scripts/smoke.py`, and
the existing tests.

`context export` writes a token only to an explicit absolute owner-only file; it
never writes a credential to stdout.

## Scoped automation credentials

```sh
eregion context credential add \
  --auth-file /etc/eregion/auth.json \
  --subject ci-deploy \
  --scope operations:submit \
  --token-out /root/eregion-ci.token \
  --dry-run --json
# Then repeat with --confirm '<token from the plan>'.
```

The command adds a `ci` actor to the existing server auth file. Dry-run returns
the auth-file revision and a plan-bound confirmation token. Apply verifies that
the revision is unchanged, writes the token once to the owner-only output, and
stores only the SHA-256 principal in the auth file. Neither the plan nor the
result contains the token. The available scopes are `operations:submit`,
`operations:read`, `operations:cancel`, and `system:read`; a credential cannot
widen its own scope.

## Commit-confirm for access-changing apply

A change that can break the operator's path to the host is not treated as a
normal convergence. Examples:

- replacing or adding node `authorized_keys`;
- moving the API from loopback to a non-loopback address, or installing TLS;
- replacing a live API/worker unit or the installed binary;
- changing node identity/configuration;
- changing the live auth/environment/TLS files.

Before applying such a change, bootstrap:

1. snapshots the affected files (content, mode, numeric owner) under
   `/var/lib/eregion/bootstrap/commit/<operation_id>/`;
2. starts a detached watchdog (`setsid /bin/sh watchdog.sh`) with a deadline
   (`--commit-confirm-seconds`, default 90, range 30–900);
3. applies the steps;
4. returns `confirmation_required` with `operation_id`, `deadline`,
   `access_changes`, and completed steps. Exit code is 5.

The operator verifies that the host is reachable through the new path **from a
fresh connection**, then runs:

```sh
eregion bootstrap confirm <operation_id> --json
# remote: add --ssh-address/--ssh-identity/--ssh-known-hosts
```

Confirmation writes the `confirmed` marker, clears the pending window in the
node-local state, and marks the generation complete. If the deadline passes
first, the detached watchdog restores the snapshot, reapplies the original
modes/owners, runs `systemctl daemon-reload`, restarts units that existed, and
disables units that did not. It writes a `rolled-back` marker. A confirmation
after the deadline is rejected; the operator must inspect the host and plan
again.

The revert path is tested directly (`rollbackCommit`), including the pending
state and confirmation markers. The watchdog process itself is detached so a
dropped SSH connection or a CLI exit does not cancel the deadline. A normal
fresh bootstrap with a loopback API and no key replacement has no access changes
and completes without a rollback window.

## Guided terminal setup

```sh
eregion bootstrap --interactive
```

The Bubble Tea flow collects role, node, database URL file, token outputs,
automation inputs, binaries, listeners/TLS, node keys, worker concurrency, and
the commit-confirm window. It shows every step and diff, highlights access
changes and the rollback window, and requires typing `apply` before mutation.

Every guided field has the same flag on `eregion bootstrap`; the parity test
fails if a field lacks a flag. Machine output (`--json`) and non-TTY invocation
never start the interface. The guided flow uses the same `host.Runner` and
`Apply` path as the flag form; it is not a second implementation.

## Fresh-host matrix

```sh
just bootstrap-matrix
# or: python3 scripts/bootstrap_matrix.py --json
```

The script builds the binaries and runs one disposable container per supported
image: `debian:12`, `debian:13`, `ubuntu:22.04`, `ubuntu:24.04`. For each image
it runs:

- main-role `plan` (which must detect the release, include Docker, Buildpacks,
  registry, and database, and include no telemetry);
- application-role `plan`;
- an interrupted `apply` at the Docker step, proving the journal is written
  before PostgreSQL and the failure leaves a resumable generation;
- a resumed `apply` (`resumed: true`, same operation);
- a completed re-plan and re-apply with no step reporting `executed: true`.

Current result on this workstation (amd64): all four images pass. Containers are
uniquely named, run with `--network none` and `--rm`, and never touch the host's
systemd, Docker daemon, or existing containers.

**Honest limits of the proxy.** Containers have no systemd, no Docker daemon, and
no package network. The script installs local stubs for `systemctl`, `docker`,
`dpkg-query`, and `apt-get`. That exercises the real CLI, os-release detection,
role selection, journal, resume, idempotency, account/directory/file steps, and
the real distro userland, but it does **not** prove service supervision, a live
container runtime, real package installation, registry pulls, Buildpacks
execution, PostgreSQL migrations, or kernel/network behaviour. Those need a
fresh-host VM matrix; the remote SSH path additionally needs a real SSH endpoint.

## Database outage and competing migrations

### Bootstrap behaviour

- Pre-database steps (platform, packages, accounts, directories, Docker,
  Buildpacks, registry, binaries, services) are journaled under the bootstrap
  state directory. They do not read or write PostgreSQL.
- The database step validates TLS and privileges, then applies migrations. If
  PostgreSQL is unreachable, it returns `unavailable`; the apply fails with the
  journal intact and `resumable: true`.
- A re-run with the same intent resumes the same generation. Completed
  pre-database steps are acknowledged; the database step is retried.
- `readinessStep` requires both services to be active **and** an authenticated
  `GET /readyz` to succeed. A database outage after the services start is visible
  as an unready API; bootstrap does not report success from process liveness
  alone.
- Database TLS is mandatory except for a literal loopback development host.
  `sslmode=verify-full` is the tested external configuration; host overrides in
  the URL are rejected.

### Competing migrations

`internal/controlplane/database.go` serializes migration runs with a dedicated
PostgreSQL session advisory lock (`pg_advisory_lock`, key `eregmigr`). A second
migrator waits for the first; it does not race DDL. River versions commit
individually because PostgreSQL enum additions cannot share the transaction that
adds them; domain migrations are transactional. Services remain unready until
every domain and River version is current.

`PlanMigrations` fails closed on:

- a database schema newer than the binary;
- a gap in `eregion_schema_migrations`;
- an applied migration whose checksum differs from the embedded SQL;
- a River schema newer than the binary.

The PostgreSQL integration suite covers concurrent migration calls, an
unmigrated database not being ready, edited migration detection, a history gap,
and upgrading a version-1 database. A migration process that dies releases the
session lock with its connection; the next run resumes from the applied versions.
No service migrates automatically on startup.

## Security notes

- The database URL, operator token, automation token, and TLS private key are
  registered for redaction before any output.
- Secrets never enter events, River arguments, step inputs, or logs. Step inputs
  contain hashes only.
- Credential files are owner-only; the context store and auth file reject
  group/other-readable modes.
- Step scripts run with a fixed `PATH`, no inherited environment, and a separate
  process group. Request and bootstrap values are never interpolated into a
  shell by the node transport or the remote bootstrap command.
- The registry is loopback-only and unauthenticated in this increment. Expose it
  only through an authenticated proxy before changing the bind address.
