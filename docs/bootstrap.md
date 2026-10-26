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
| Debian | 13 | amd64, arm64 |
| Ubuntu | 26.04 | amd64, arm64 |

`internal/host/platform.go` is the single source of truth. Any other release or
architecture is rejected before a package, account, service, or file mutation.
The detection normalizes `x86_64`/`aarch64` fixture values to Go's `amd64`/
`arm64`.

| Role | Docker runtime | Buildpacks (`pack`) | OCI registry | Node binary/config | Control plane |
| --- | --- | --- | --- | --- | --- |
| `main` | yes | yes | yes, loopback `127.0.0.1:5000` by default | no | API + worker |
| `application` | yes | no | no | yes | no |
| `builder` | yes | yes | no | yes | no |
| `telemetry` | yes (+ Compose) | no | no | yes | no |

The main node therefore installs the application runtime, the Buildpacks build
stack, and the container registry. The `builder` and `main` roles install `git`,
which builds use to fetch source. Node roles install `wireguard-tools` because
enrollment probes `wg` and reconciliation renders a WireGuard configuration.

The `telemetry` role prepares a dedicated Debian/Ubuntu host with Docker,
Compose, and the node execution account. **Bootstrap does not install a
telemetry stack, collector, database, or storage, and telemetry is never
co-located with the control plane.** `edge` is still rejected explicitly.

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
- `status` reads the node-local state and engine journal only. It needs no
  database URL, operator-token output, or binary, so it works from a fresh
  shell after an interrupted first contact. It reports the generation, active
  and completed state, pending commit deadline, and every persisted step. It
  does **not** evaluate drift; run `plan` when the apply inputs are available.
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

The control-plane environment is **merged**, not rewritten byte-for-byte. Bootstrap
owns `EREGION_DATABASE_URL`, the auth/state paths, `EREGION_NODE_NAME`, and the
node transport settings (`EREGION_NODE_SSH_IDENTITY`,
`EREGION_NODE_SSH_KNOWN_HOSTS`, `EREGION_NODE_WG_KEY_DIR`,
`EREGION_NODE_WG_ADDRESS`, `EREGION_NODE_WG_SUBNET`). It replaces those keys and
preserves every operator-added line and comment. Plan/check output names the
preserved keys instead of hiding the merge. It records the identity and pin-file
paths for enrolled nodes; it never invents or writes the private key.

`node.json` is merged the same way: bootstrap owns `node` and `state_dir` and
preserves operator keys such as `wireguard_apply` and `task_command`.

The in-process local node identity defaults to `local` for compatibility and
accepts `main` as the documented main-node name. `EREGION_NODE_NAME` selects it;
bootstrap writes the requested `--node` value (for example `main`) into
`control-plane.env`. A deployment that already has a `local` node row keeps
working through the alias instead of being silently duplicated.

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
- Remote plan and apply use one **stable** uploaded bundle directory derived from
  the host, node, role, and binary content, under
  `/var/lib/eregion/bootstrap/remote/`. The remote configuration hash includes the
  uploaded binary path, so a timestamped directory would make every re-run a new
  generation. Re-running the same intent now resumes the same generation and
  operation and acknowledges completed steps.
- The bundle is kept after a dry-run, an interrupted apply, or a pending
  commit-confirm, because the next invocation must use the same binary path and
  content. A fully successful apply (or confirm) removes it explicitly. A failed
  cleanup is reported on stderr but does not turn a completed apply into a
  failure.
- Step progress is streamed from the remote stderr while the final envelope stays
  on stdout. An interrupted connection is a lost progress stream, not a lost
  journal: re-running picks up the same generation on the host.
- `eregion bootstrap remote --dry-run` returns only the remote plan. Nothing is
  applied, and the bundle remains for the subsequent apply.

### Credential handoff

The initial operator token is generated **on the host**. It is written once to
the protected remote path (`--operator-token-out`, or
`/var/lib/eregion/bootstrap/operator.handoff` by default) with mode 0600. The
remote command reads it once over the SSH channel, validates its token shape,
stores it in the local context file with mode 0600, and **removes the host file**.
The token is never printed in plain or JSON output. A read failure keeps the file
so the operator can recover it from a fresh connection; a successful read always
consumes it, even if the content is malformed. Node-only roles generate no
operator token and do not attempt a handoff. Use `--token-file-out /private/path`
only when another protected file is explicitly required.

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
continue to work exactly as before for scripts and the existing tests.

`context export` writes a token only to an explicit absolute owner-only file; it
never writes a credential to stdout.

## Scoped automation credentials

```sh
eregion context credential add \
  --auth-file /etc/eregion/auth.json \
  --subject ci-deploy \
  --scope operations:submit \
  --action-grant platform.deploy:env/api/production \
  --action-grant platform.release.create:env/api/* \
  --token-out /root/eregion-ci.token \
  --dry-run --json
# Then repeat with --confirm '<token from the plan>'.
```

The command adds a `ci` actor to the existing server auth file. Dry-run returns
the auth-file revision, requested grants, and a plan-bound confirmation token.
Apply verifies that the revision is unchanged, writes the token once to the
owner-only output, and stores only the SHA-256 principal in the auth file.
Neither the plan nor the result contains the token. `--action-grant` is
repeatable and uses the same `kind:target` semantics as the server
(`internal/api/authorization.go`): an exact action kind and canonical target,
with only `env/<app>/*` permitted as a wildcard. Scopes alone never grant
platform actions. Invalid, ambiguous, or duplicate grants fail before apply.

The available scopes are `operations:submit`, `operations:read`,
`operations:cancel`, and `system:read`; a credential cannot widen its own scope.
The bootstrap `--automation-scope` path creates the older diagnostic-only CI
credential; use `context credential add --action-grant` when CI must submit
platform actions.

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
after the deadline is rejected **and reconciles the snapshot first**; the
operator must inspect the host and plan again.

The generated watchdog honors `confirmed` and `rolled-back` before and after the
deadline and takes an atomic `rollback.lock` claim. An old watchdog therefore
cannot overwrite a newer confirmed configuration. An explicit rollback writes
`rolled-back`, which disarms any watchdog still alive. `apply` reconciles
unconfirmed snapshots under the apply lock: an expired snapshot is restored
before new mutation, and an unconfirmed future window from another operation
blocks a second convergence. This covers a reboot that killed the detached
watchdog. The generated shell script is exercised directly in tests with fake
`systemctl` and temporary paths, in addition to the Go `rollbackCommit` tests.

A normal fresh bootstrap with a loopback API and no key replacement has no access
changes and completes without a rollback window.

## Guided terminal setup

A human sets up a main node with `eregion server init`: preflight, questions
with defaults, plan, confirm, live progress, hardening and the WireGuard
lockdown, then the secrets handoff. Every question has a flag for agents.

## Fresh-host evidence

Bootstrap has no simulated-host test. Containers or SSH fixtures with stubbed
`systemctl`, `docker`, `dpkg-query`, or `apt-get` were removed because they
passed without proving package installation, service supervision, a live
container runtime, registry pulls, Buildpacks execution, or migrations.

Unit tests in `internal/host` cover only logic that runs for real on temporary
files: release detection, confirmation binding, credential and context files,
artifact checksums, archive extraction, and the commit-confirm watchdog script.
Whether `bootstrap apply` converges a host is **unverified** until it runs on a
clean Debian/Ubuntu VM; see G8 in [release criteria](release-criteria.md).

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
- Database TLS is mandatory except for a literal loopback address or a tailnet host
  (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`, or a `*.ts.net` MagicDNS name), where
  WireGuard already encrypts the path.
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
