# Nodes: enrollment, placement, and private connectivity (M2)

**Status:** implemented and tested against disposable Debian 12 SSH nodes
(including a real kernel WireGuard tunnel) · 2026-10-20

This document is the operator and integrator reference for additional
application and builder nodes. The action contracts are in
[the platform contract](platform-contract.md); the transport is in
[the SSH plan](m0.3-ssh-plan.md) and [the WireGuard plan](m0.3-wireguard-plan.md).

## What a node is

| State | Meaning | Receives remote work |
| --- | --- | --- |
| `registered` | Declared roles and capabilities. Nothing verified. Used for the main node and development. | Only through the worker's own configured transport, and only if that transport authenticates as exactly this node. |
| `pending` | An enrollment was accepted and has not finished. | No |
| `enrolled` | Host key pinned, identity key pinned, host probed, role configuration applied. | Yes, over its own pinned SSH transport. |
| `failed` | Enrollment hit a permanent problem. `enrollment_error` says why. | No. Fix the cause and run `platform nodes retry`. |

A hostname or address is never authorization. A node is trusted because of three
independent facts: the SSH host key the operator pinned, the SSH key the control
plane holds (authorized only on nodes the operator provisioned), and an identity
key the node generated for itself and proves possession of.

## Setting up a node

Supported hosts: **Debian 12/13 and Ubuntu 22.04/24.04**, amd64 or arm64. The
probe refuses anything else (`unsupported node`).

The operator does this once per host (a bootstrap script or image can do it; the
control plane deliberately never needs root on a node):

1. Create an unprivileged account, for example `eregion`, with the control
   plane's public SSH key in `authorized_keys`. No sudo, no agent forwarding, no
   PTY (see the SSH plan).
2. Install `eregion-node` at `/usr/local/bin/eregion-node` and write
   `/etc/eregion/node.json` (owned by the account, mode 0600):

   ```json
   { "node": "builder-1", "state_dir": "/var/lib/eregion/node" }
   ```

   The `node` name must equal the name you enroll; the handshake enforces it.
   Optional `"wireguard_apply": ["/usr/local/sbin/eregion-wg-apply"]` names a
   privileged helper (see Private connectivity).
3. Install what the roles need: Docker for `application`, `builder`, and
   `telemetry` (the account must reach the daemon), `pack` for `builder`, `git`
   for builder-capable hosts, `wireguard-tools` for the private network, and
   Docker Compose for `telemetry`. Enrollment verifies Docker/`pack`/`wg`; it
   does not install them. `eregion bootstrap` installs the matching role
   packages (`git` for `main`/`builder`, `wireguard-tools` for node roles,
   `docker-compose` for `telemetry`) but never a telemetry stack.
4. Optional but recommended: print the node's identity fingerprint and verify it
   out of band: `eregion-node identity --config /etc/eregion/node.json` prints
   `SHA256:...` and creates the key if needed. Also verify the host key
   (`ssh-keyscan -t ed25519 host`, compare with the console/provider).

On the control plane set `EREGION_NODE_SSH_IDENTITY` (the private key authorized
on every node) and, for the private network, `EREGION_NODE_WG_KEY_DIR`,
`EREGION_NODE_WG_ADDRESS` (default `10.77.0.1`), and `EREGION_NODE_WG_SUBNET`
(default `10.77.0.0/24`). `serve` and `worker` read them; they are listed in
`eregion help serve --json`. `bootstrap` writes these keys (and
`EREGION_NODE_SSH_KNOWN_HOSTS`) into `/etc/eregion/control-plane.env` with the
paths shown in [bootstrap](bootstrap.md#local-workflow); the operator still
installs the private key and pins the host keys. Re-applying bootstrap preserves
operator-added environment lines and `node.json` keys such as
`wireguard_apply`.

## Enrolling

```
eregion platform nodes enroll builder-1 --role builder \
  --address 203.0.113.10:22 --host-key-file builder-1.pub \
  --identity-fingerprint SHA256:... --build-concurrency 2 \
  --idempotency-key ci:enroll-builder-1 --json
eregion platform nodes retry builder-1 --idempotency-key ci:retry-1 --json
eregion platform nodes list | status builder-1 | check builder-1
```

`enroll` is one durable operation (`platform.node.enroll`, one River job). It
runs, in order, as Check/Apply node steps over the pinned SSH transport:

1. **Identity.** `node-identity-01-v1`: the node signs
   `eregion-node-identity-v1 / node name / nonce` with an ed25519 key it
   generated (`<state_dir>/identity/node.ed25519`, 0600, never transmitted). The
   control plane verifies the signature. If the operator supplied
   `--identity-fingerprint`, the key must match it; otherwise the key is pinned
   on this first verification. Because the SSH host key is already pinned and
   only the control plane's key is authorized on the host, that first use is
   bounded, not blind. One identity key identifies exactly one node row: a
   cloned machine that carries the same key is refused.
2. **Probe.** `node-probe-01-v1`: reads `/etc/os-release`, the architecture, and
   runs `docker version`, `pack version`, `wg --version`. Capabilities are what
   the host proved. Declared roles must be backed: `application` needs `docker`,
   `builder` needs `docker` and `buildpacks`. Otherwise enrollment fails with
   `placement_violation` reason `capability_missing` naming the tool.
3. **Role configuration.** `node-hostcfg-01-v1`: application nodes get
   `<state_dir>/runtime`; builder nodes get the isolated
   `<state_dir>/build/{cache,tmp,home,docker-config}` (0700) and, if
   `--build-user` was given, the dedicated account must already exist.

Then the node row becomes `enrolled`. Everything is idempotent: re-submitting
the same facts keeps an enrolled node enrolled and re-verifies it; the same
idempotency key returns the same operation; `retry` re-runs the recorded
enrollment and completed steps are recovered from the node's effect record
instead of repeating. A different host key for an enrolled node is a `conflict`
(a pinned key is never replaced by re-submission), and `platform nodes register`
can never overwrite an enrolling or enrolled node.

`check` on an enrolled node repeats the identity challenge and probe, records
`last_check_at/state/detail`, and updates capabilities. `list`/`status` show
enrollment state, the host-key and identity fingerprints, WireGuard state, and
how fresh the last check is ("ok 3m0s ago", "(stale)" after 15 minutes, "never
checked"). A host that answers with a different identity key is **quarantined**
(`last_check_state=identity_mismatch`): it receives no work until a check
verifies the pinned identity again.

## Main node identity

The control plane's in-process transport identifies itself as a node. `local`
is the original development identity; `main` is the documented main-node name.
Both name the same local transport, so an environment created with
`--node main` works on a deployment that still has a `local` node row, and the
reverse also holds. The database row keeps its original name; new environments
reference the actual row instead of creating a duplicate.

`EREGION_NODE_NAME` selects the identity (default `local` for compatibility).
Fresh bootstrap writes the requested `--node` value into
`/etc/eregion/control-plane.env`. A remote or enrolled transport is never treated
as an alias: its authenticated node name must match exactly.

## Role combinations (the decision)

Roadmap question "which roles may combine beyond the main node's defaults":

| Role set on an enrolled node | Allowed | Why |
| --- | --- | --- |
| `application`, `edge`, `application`+`edge` | yes | runtime workloads and their ingress |
| `builder` | yes | source execution, build credentials, and caches |
| `registry`, `builder`+`registry` | yes | artifact store beside the node that pushes to it |
| `telemetry` | alone | never co-located with anything else |
| `builder` + `application` | **no** | build source and credentials must not share a host with runtime workloads |
| `control-plane` | **not enrollable** | belongs to the main node |

The **main node** keeps its development defaults (`application`,
`control-plane`, `builder` when `pack` exists). Builds there run under per-build
isolation (`<state_dir>/builds/<op>-<step>` with a scrubbed environment) and the
same per-node concurrency limit, but they share the host with runtime
workloads. A production deployment that wants real isolation enrolls a separate
builder node. The policy lives in `platform.ValidateEnrollRoles`.

## Placement is enforced before dispatch

`checkPlacement` runs when work is accepted and again in the worker. It refuses
with `precondition_failed`, a message starting `placement_violation:`,
`details.reason`, and a `placement_violation` operation event:

| `reason` | Refused work |
| --- | --- |
| `not_enrolled` | any work on a `pending` or `failed` node |
| `role_missing` | e.g. an environment on a builder-only node |
| `builder_required` | a build on a node that only has the application role |
| `capability_missing` | a role that the probe does not back (no `docker`/`pack`) |
| `identity_mismatch` | a quarantined node, or a registered node no transport authenticates as |
| `transport_unpinned` | a worker transport that does not report an identity |
| `registry_scope` | a push-scoped registry credential for a node without the builder role |
| `build_capacity` | all build slots of a node stayed busy for 10 minutes |

Build isolation: builds run only on a node with `builder`+`buildpacks` (today:
the environment's node, which for an enrolled fleet means the main node, because
an environment needs the `application` role and an enrolled node cannot be both).
Each node admits `build_concurrency` builds at a time through PostgreSQL session
advisory locks (a dead worker frees its slot); a queued build emits a
`build_queued` event. Registry credentials follow roles
(`platform.RegistryScopeFor`: builders push, everything else pulls) and
`checkRegistryCredential` refuses a mismatch. Registry credentials and moving
images from a builder to application nodes arrive with M3's registry work; the
scope rule and the slot limit are in place now.

## Per-node transport resolution

Workers no longer have one transport for everything. Extensions take the
resolver:

```go
// controlplane.NodeTransports
TransportFor(ctx context.Context, nodeName string) (node.Transport, error)
```

`Runtime.Nodes` is the shared instance (`*NodeResolver`). Contract:

- The node is read from PostgreSQL. `pending`/`failed` is refused with
  `placement_violation` `not_enrolled`.
- `enrolled` returns a cached pinned-SSH transport built from the record: the
  pinned host key, the account, `ExpectedNode=<name>` (the handshake must report
  it), and the control plane's key. Once the node's WireGuard peers are `applied`
  and `EREGION_NODE_WG_KEY_DIR` is set, SSH runs inside the userspace WireGuard
  tunnel to the node's tunnel address. The cache rebuilds when the record changes.
- `registered` returns the worker's own configured transport (local, or the
  single env-configured SSH transport of `EREGION_NODE_TRANSPORT=ssh`), only if
  it authenticates as exactly that node. That keeps development and the main
  node working unchanged.
- The returned transport is shared and concurrency-safe; the caller never closes
  it. Resolution does not check roles: call `Service` placement before
  dispatching role-bound work. `Direct(rec)` (uncached, closed by the caller) is
  used by enrollment and by `reconcile --direct`.

Telemetry and resources should take a `NodeTransports` instead of a transport.
The `TODO(telemetry)` in `extensions.go` can pass `run.Nodes`.

## Private connectivity

`--private-network-endpoint host:port` at enrollment allocates the node a tunnel
address (lowest free host of the subnet, never the control address). Enrollment
itself changes nothing about WireGuard. `platform nodes reconcile <node>`
(`platform.node.reconcile`) then:

1. converges the role directories;
2. has the node generate its WireGuard key pair (`node-wireguard-01-v1`;
   `<state_dir>/wireguard/private.key`, 0600; only the public key is returned);
3. creates the control plane's per-node key in `EREGION_NODE_WG_KEY_DIR`
   (`<node>.key`, 0600; never in PostgreSQL);
4. stores both public keys and the allowed IPs in PostgreSQL;
5. renders the node's peer set (`node-wireguard-02-v1`): the control plane
   (allowed `10.77.0.1/32`, no endpoint, it dials) plus every other enrolled
   node that has a key and an endpoint (allowed `<addr>/32`, endpoint,
   keepalive 25), sorted by key so the file is deterministic;
6. applies it if a privileged helper is configured.

The node account is unprivileged, so loading a configuration needs an
operator-installed helper named in `node.json` (`wireguard_apply`, an absolute
path, called with the rendered `wg0.conf` as its argument; typically a sudo rule
on that one script or a root path unit). Without it the state is `rendered` and
the operation says so: the configuration is on the node but not loaded, and SSH
stays on the direct address. With it the state is `applied` and the resolver
switches to the tunnel. Reconciling is idempotent: the file is rewritten only
when it differs and the helper runs only when the applied hash differs. After
enrolling a new private node, reconcile the existing ones so they learn the new
peer. If a tunnel breaks, `reconcile --direct` repairs it over the direct
address. Two control-plane processes on one host cannot hold the same node tunnel
(the WireGuard key lock); run API and workers in one process or accept a
retryable `unavailable` from the second.

Secrets: no private key (node identity, node WireGuard, control WireGuard) is in
an operation input, event, step result, River argument, or node row. The tests
read the private keys back from the node and the key directory and search every
durable surface for them.

## Tests

- Unit: `internal/platform` (role matrix, distro/arch matrix, enrollment input
  normalization), `internal/node` (attestation binding, pinned-key and impostor
  rejection, probe against fake os-release/tools, role directories and drift
  repair, WireGuard key generation, deterministic rendering, idempotent apply,
  payload injection refusal), `internal/controlplane` (placement rules,
  quarantine, registry scope, fallback identity, `local`/`main` aliasing),
  `internal/cli` (enroll needs a pinned host key, plain node table with
  freshness, remote bundle stability, once-only credential handoff),
  `internal/host` (status without apply inputs, env/node-config merge,
  action-grant credentials, the generated watchdog script).
- Integration (`EREGION_TEST_DATABASE_URL`, Docker):
  `go test -count=1 ./internal/controlplane -run TestIntegration -timeout 600s`.
  `integration_nodes_test.go` enrolls an application node and a builder node over
  SSH into the `sshfixture` containers and shows work landing on the right node
  and refused elsewhere; wrong host key, wrong node name, wrong identity,
  cloned identity, retry, check-time identity swap and quarantine; build slots;
  and WireGuard reconcile of two nodes followed by a node check that crosses the
  real userspace-to-kernel tunnel.

## Not tested, and honest limits

- **Real multi-distro VMs.** Every test node is the Debian 12 fixture container.
  Debian 13 and Ubuntu 22.04/24.04 are accepted by the release matrix
  (`platform.SupportedRelease`, unit-tested on os-release text) but no such host
  was enrolled. arm64 is accepted by the probe, untested.
- **Real Docker and `pack` on enrolled nodes.** The fixture's `docker` and `pack`
  are small stand-ins; they prove the probe, the capability gate, and dispatch,
  not a build or a deployment on a remote node. Remote builds and image movement
  are M3. `bootstrap remote` apply is exercised against the SSH fixture with fake
  package/daemon tools; it is not a fresh-VM proof.
- **The WireGuard apply helper.** The test's root loop stands in for the
  production helper (sudo rule or systemd path unit). No shipped helper or unit
  file exists yet; installing one is part of bootstrap.
- **The dedicated build user.** An unprivileged account cannot create users or
  `sudo` into one. Enrollment verifies a named build account exists and builds
  use the isolated per-node `build/` tree and a scrubbed environment, but
  switching the build to that OS user is not implemented.
- **Edge and registry roles** are enrollable and get no host configuration yet;
  their services arrive with M3/M5.
- **Cross-node placement of a build.** The environment's node is the build node;
  an application-only node refuses builds (`builder_required`) instead of
  building elsewhere and shipping the image.
- Failure modes not exercised: SSH key rotation on a live fleet, host-key
  rotation (deliberately a remove-and-re-enroll), subnet exhaustion, and two
  workers contending for one node tunnel.
