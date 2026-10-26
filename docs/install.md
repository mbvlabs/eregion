# Installing eregion

**Status:** M7 install notes for the v0.1 experimental line. The commands below
describe the implementation in this tree. Check discovery before relying on a
command name:

```sh
eregion commands --json
eregion help upgrade --json
```

## Platforms

| Component | Supported platforms |
| --- | --- |
| `eregion` operator CLI / control plane | Linux (`amd64`, `arm64`), macOS (`amd64`, `arm64`), Windows (`amd64`, `arm64`) |
| `eregion-node` | Linux only (`amd64`, `arm64`) |
| Host bootstrap | Debian or Ubuntu on `amd64`/`arm64` |

The control plane needs Linux, an external PostgreSQL 17+ database, and the
release's `eregion` binary. The node binary does not talk to PostgreSQL and
holds no control-plane credential.

## Verify a signed release before installing

A release consists of archives, `manifest.json`, and the release public key.
The Ed25519 signature covers the manifest, including every artifact name, size,
and SHA256. `SHA256SUMS` is an unauthenticated convenience listing, not a
signature.

The release workflow uploads the archives, manifest, checksum listing, and
public key to the configured S3-compatible object store before publishing the
manifest. Download artifacts from the direct HTTPS object prefix recorded as
`EREGION_RELEASE_BASE_URL`; GitHub release assets are a convenience copy whose
redirecting URLs the upgrade downloader refuses.

Pin the public key out of band (configuration management, a previously
trusted install, or a published fingerprint you can compare). Then verify the
manifest:

```sh
export EREGION_UPGRADE_PUBLIC_KEY_FILE=/etc/eregion/release.pub
eregion upgrade verify --manifest /tmp/eregion-release/manifest.json --json
```

`upgrade verify` checks the signature and manifest structure without touching an
installation. It does not download artifacts. If the key file is missing or the
manifest is unsigned, it fails closed.

For a machine with no previous `eregion` binary, verify the public-key
fingerprint through a second channel and compare the archive SHA256 with the one
in the signed manifest. Do not run an archive whose manifest signature you have
not checked with a trusted verifier; extracting first defeats the purpose.

## Install the operator CLI

1. Download the archive for the target OS/architecture from the signed
   manifest, for example `eregion_1.1.0_linux_amd64.tar.gz`.
2. Verify its size and SHA256 against the **signed manifest** (not only
   `SHA256SUMS`).
3. Extract the `eregion` entry and install it atomically:

```sh
umask 022
tar -xzf eregion_1.1.0_linux_amd64.tar.gz -C /tmp/eregion-release eregion
sudo install -d -m 0755 /usr/local/bin
sudo install -m 0755 /tmp/eregion-release/eregion /usr/local/bin/eregion
/usr/local/bin/eregion version --json
```

The installed binary must be a regular file, not a symlink, and must not be
group- or world-writable. `eregion upgrade apply` refuses anything else.

The same archive layout is used for macOS and Windows (`eregion.exe` on
Windows). The operator CLI is a portable API client; control-plane-only commands
(`serve`, `worker`, `system migrate`) require Linux.

## Install the node binary

`eregion-node` is Linux-only. Install it from its own signed artifact, or let
`eregion bootstrap` install it on a node role with `--node-binary`.

```sh
tar -xzf eregion-node_1.1.0_linux_amd64.tar.gz -C /tmp/eregion-release eregion-node
sudo install -d -m 0755 /usr/local/bin
sudo install -m 0755 /tmp/eregion-release/eregion-node /usr/local/bin/eregion-node
sudo install -d -m 0700 -o eregion-node -g eregion-node /var/lib/eregion/node
sudo install -m 0600 -o root -g root node.json /etc/eregion/node.json
/usr/local/bin/eregion-node version
/usr/local/bin/eregion-node version --json
```

The JSON form is also the upgrade health probe's identity check: a signed node
artifact must report the exact version in the manifest. A release build that
still prints `dev` will not be accepted by `upgrade apply --target node`.

Minimal `/etc/eregion/node.json`:

```json
{"node": "app-1", "state_dir": "/var/lib/eregion/node"}
```

Bootstrap owns `node` and `state_dir` when it writes this file and preserves
operator keys already present, including an optional privileged WireGuard helper:

```json
{"node": "app-1", "state_dir": "/var/lib/eregion/node",
 "wireguard_apply": ["/usr/local/sbin/eregion-wg-apply"]}
```

The node account owns only its state directory. The control plane reaches the
node over SSH with a pinned host key; see
[control-plane.md](control-plane.md#remote-node-execution-over-pinned-ssh).
An unpinned host key or a node-protocol mismatch fails closed.

## External PostgreSQL

- PostgreSQL **17 or newer**, on a dedicated database. Do not point migrations
  at an unrelated application database.
- Use TLS: `sslmode=verify-full` for any non-loopback host. `sslmode=disable`
  is accepted only for a literal loopback address or a tailnet host (`100.64.0.0/10`,
  `fd7a:115c:a1e0::/48`, or a `*.ts.net` MagicDNS name), not `localhost`.
- The migration role needs permission to create the eregion and River schema
  objects. Grant that deliberately; a runtime role with fewer privileges needs
  migrations applied by an authorized operator first.
- Keep the URL out of shell history and process arguments. Supply it through
  `EREGION_DATABASE_URL` or, for bootstrap, `--database-url-file` pointing at an
  owner-only (`0600`) regular file.
- Migrations are explicit and forward-only. API and worker startup fail closed
  until `eregion system migrate` has applied every required version and River
  migrations are current.

Example (replace every value; do not commit it):

```sh
export EREGION_DATABASE_URL='postgres://eregion:REDACTED@db.example.net:5432/eregion?sslmode=verify-full'
```

## Bootstrap the main node

The bootstrap surface is implemented by `internal/cli/bootstrap.go`. It is
noninteractive and role-aware; `plan`, `diff`, and `status` never mutate.
`apply` converges the host after an exact plan-bound confirmation.

Run as root on a supported Debian/Ubuntu host:

```sh
umask 077
eregion bootstrap plan \
  --role main \
  --node main \
  --database-url-file /root/eregion-db.url \
  --operator-token-out /root/eregion-operator.token \
  --api-listen 127.0.0.1:8080 \
  --worker-concurrency 2 \
  --binary /usr/local/bin/eregion \
  --json
```

Review the plan and diff, then apply with the printed token:

```sh
eregion bootstrap diff --role main --node main \
  --database-url-file /root/eregion-db.url \
  --operator-token-out /root/eregion-operator.token --json

eregion bootstrap apply --role main --node main \
  --database-url-file /root/eregion-db.url \
  --operator-token-out /root/eregion-operator.token \
  --apply --confirm TOKEN --json

eregion bootstrap status --role main --node main --json
```

`status` reads the node-local state and journal only. It requires neither the
database URL nor `--operator-token-out`; use `plan` when you want a drift check.
The installed `/etc/eregion/control-plane.env` includes the node transport paths
for enrolled nodes (`EREGION_NODE_SSH_IDENTITY`, `EREGION_NODE_SSH_KNOWN_HOSTS`,
`EREGION_NODE_WG_KEY_DIR`, `EREGION_NODE_WG_ADDRESS`, `EREGION_NODE_WG_SUBNET`)
and `EREGION_NODE_NAME`. Re-applying preserves operator-added lines; install the
SSH private key and pinned `known_hosts` at the reported paths.

Flags implemented today:

| Flag | Meaning |
| --- | --- |
| `--role main\|application\|builder\|telemetry` | Placement role. `telemetry` prepares Docker/Compose only; it does not install a telemetry stack. |
| `--node NAME` | Node name. `local` is the default; `local` and `main` both name the in-process control-plane identity. |
| `--database-url-file PATH` | Owner-only external PostgreSQL URL file; otherwise `EREGION_DATABASE_URL`. |
| `--operator-token-out PATH` | Required absolute output for the first operator token (mode `0600`). |
| `--operator-token-file PATH` | Optional existing owner-only operator token input. |
| `--automation-subject`, `--automation-scope`, `--automation-token-out` | Optional CI credential and its protected output file. |
| `--binary PATH`, `--node-binary PATH` | Control-plane binary and the node binary for application/builder roles. |
| `--api-listen ADDR` | Literal `host:port`; non-loopback listeners require TLS. |
| `--api-tls-cert`, `--api-tls-key` | TLS certificate and key sources. |
| `--node-authorized-keys-file PATH` | SSH public keys for the node account (access-changing). |
| `--worker-concurrency N` | Worker concurrency, 1–16. |
| `--dry-run` | Read-only plan/diff; never applies. |
| `--apply`, `--confirm TOKEN` | Explicit apply authorization and exact plan-bound token. |

Role notes:

- `main` requires the external database URL and `--operator-token-out`; it
  installs the control-plane service units, applies migrations, and starts
  workers/API only after readiness. It also installs `git` for builds.
- `application` and `builder` require `--node-binary` (and a builder-capable
  host for Buildpacks builds). They install the node service, node account,
  `wireguard-tools`, and `git` on a builder.
- `telemetry` requires `--node-binary` and installs Docker, Compose, and the node
  account on a dedicated host. It does not install a collector, storage, or any
  telemetry stack, and it is never co-located with the control plane.
- Access-changing plans (non-loopback API, node authorized keys, TLS material)
  require an explicit confirmation; read the reported risks before applying.
- The bootstrap command group may continue to gain flags. `eregion commands
  --json` is authoritative for the binary you are running; do not guess a flag
  that is not listed.

If a build exposes the same local bootstrap surface under the older `host`
group, substitute `eregion host` for `eregion bootstrap`; the flags are the same.

## First context login

Bootstrap writes the first operator token to `--operator-token-out`. Log in
without putting the token on the command line:

```sh
eregion context login \
  --name production \
  --api-url https://api.example.com \
  --token-file /root/eregion-operator.token \
  --json

eregion context show --name production --json
eregion version --json
```

`context login` stores the endpoint and token in a private context file and
never prints the token. `EREGION_API_URL` and `EREGION_API_TOKEN` override the
saved context for a single process; an explicit origin and a saved token for a
different origin are refused. Keep the token out of CI logs and terminal
recordings. If the binary you are running does not list `context`, export
`EREGION_API_URL` and read the token from a protected file into
`EREGION_API_TOKEN` for the single command instead.

For CI, issue a scoped credential instead of copying the operator token:

```sh
eregion context credential add \
  --auth-file /etc/eregion/auth.json \
  --subject ci-deploy \
  --scope operations:submit \
  --action-grant platform.deploy:env/api/production \
  --token-out /root/eregion-ci.token \
  --dry-run --json
# Repeat with --confirm <token from the plan>.
```

`--action-grant` is repeatable and uses the server's exact `kind:target` rules;
scopes alone do not authorize platform actions.

## First deploy of `examples/hello`

The hello fixture is a tiny HTTP service with `GET /healthz` and `GET /`.
Place its source under a configured build source root on the target node
(default `/srv/eregion/sources`) or build and publish an image. For a
directory build on the main node:

```sh
sudo install -d -m 0755 /srv/eregion/sources
sudo cp -a examples/hello /srv/eregion/sources/hello
```

Then submit the platform actions. Every mutating submission needs a stable
idempotency key; reuse the same key after a timeout, never a fresh one.

```sh
eregion platform apps create hello \
  --idempotency-key bootstrap:app-hello --json

eregion platform environments create hello production \
  --node main \
  --container-port 8080 \
  --health-type http --health-path /healthz --health-timeout 30 \
  --idempotency-key bootstrap:env-hello-production --json

# Capture the release ID from the accepted operation's result, then:
eregion platform releases create hello production \
  --directory /srv/eregion/sources/hello \
  --builder paketobuildpacks/builder-jammy-base \
  --idempotency-key bootstrap:release-hello-1 --json

eregion platform deployments plan hello production --release-id rel_... --json
eregion platform deployments deploy hello production \
  --release-id rel_... --idempotency-key bootstrap:deploy-hello-1 --json
```

Follow the operation rather than assuming success:

```sh
eregion operations follow op_... --output ndjson
eregion operations get op_... --json
```

The serving release changes only after the candidate passes its health check.
A failed candidate leaves the previous serving release in place. Exact inputs
and target rules are in [platform-contract.md](platform-contract.md).

## Upgrade and recovery pointers

- Read [upgrades.md](upgrades.md) for the signed manifest format, trust model,
  compatibility checks, lock/journal behavior, and recovery drills.
- Before a coordinated rollout, run `eregion upgrade plan` and review the
  documented order: **migrate → workers → API → nodes**. The plan is dry-run
  documentation; the CLI does not drive multiple hosts or drain traffic.
- `eregion upgrade apply` health-checks the newly installed binary
  (`version` plus a read-only migration readiness probe when
  `EREGION_DATABASE_URL` is set) and restores the verified previous binary
  automatically when that probe fails. `--health-check=false` is an explicit
  opt-out.
- `eregion upgrade rollback --dry-run` prints the restore plan and token;
  `--confirm TOKEN` restores one verified recorded backup. It never restores an
  arbitrary path.
- Database migrations are forward-only. Restoring an older schema requires an
  explicit database restore outside the CLI.
- Do not publish HA or failover claims unless the corresponding drill was run.
  See [release-criteria.md](release-criteria.md).
