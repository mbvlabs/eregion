# Signed releases and CLI upgrade

Reference for the current code. Product decisions are MASTER D14–D15: `master`
is the edge channel, `1.x` is the whole v1 line, and tags are
`v1.0.0-alpha.N`, `v1.0.0-beta.N`, `v1.0.0-rc.N`, then `v1.0.0`. This replaces
the earlier Ed25519 manifests, the one-host replacement journal and the Windows
release matrix.

## Channels and manifest metadata

| Channel | Published by | Pointer under the release origin | Installer |
| --- | --- | --- | --- |
| `edge` | every push to `master` | `/edge/manifest.json` | `/edge/install.sh` |
| `rc` | `vMAJOR.MINOR.PATCH-rc.N` tags | `/rc/manifest.json` | `/rc/install.sh` |
| `stable-<major>.x` | final tags on the `<major>.x` line | `/<major>.x/manifest.json` | `/<major>.x/install.sh` |
| `stable` | final tags, highest version only | `/manifest.json` | `/install.sh` |

A `v1.0.0-alpha.N` or `v1.0.0-beta.N` tag publishes only an immutable
snapshot, never a channel pointer. An rc tag moves only the opt-in rc pointer;
final tags move stable pointers forward. The versioned installer is pinned to
its own version and is available before any pointer exists, for example
`curl -fsSL https://eregion.mbvlabs.com/releases/1.0.0-alpha.3/install.sh | bash`.

**Manifest `channel` is the signing family, not the promotion channel.**
Every tagged artifact—final, alpha/beta snapshot and rc—is signed by
`publish-stable.yml@refs/tags/v<version>` and its manifest says `stable`.
Edge manifests say `edge`. An alpha/beta/rc manifest therefore says `stable`
even though it is never promoted to a stable pointer. This is the existing
schema-1 protocol and remains understood by already-published alpha.2 nodes;
do not read the field as proof of stable-channel membership. Stable and
major-line pointer guards inspect the **version** and reject every prerelease;
the rc pointer accepts only an rc version.

The versioned snapshot installer is the supported exact-prerelease path.
Channel installers also accept `--version`; the requested version determines
the signing family, independently of the installer channel. Without an explicit
`--version`, stable and major-line channel installers reject alpha/beta/rc and
the rc installer accepts only an rc release; an explicit version is the
snapshot pin path.

### Beta.1 fresh-install boundary

Beta.1 is a fresh-install test prerelease, not an alpha-to-beta upgrade promise.
Its operator store is `~/.config/eregion/workspaces.json`; it does not import the
old filename or private schema, identityless handoffs, or old unfinished setup.
Keep existing alpha access files, host and database intact. A reused credential
directory is refused for a new init rather than silently adopting its keys.
Use a separately authorized fresh server and control database for qualification.

The immutable installer path is
`https://eregion.mbvlabs.com/releases/1.0.0-beta.1/install.sh` once publication
has succeeded. Publication and signature verification do not establish a
fresh-install pass; MANUAL-TESTING records the remaining acceptance evidence.

Immutable artifacts live under `/releases/<version>/<os>/<arch>/eregion`, with
`.sha256` and `.sigstore.json` sidecars. Supported platforms are linux/amd64,
linux/arm64, darwin/amd64 and darwin/arm64; there are no Windows builds.

Each manifest is schema 1 with `channel`, `version`, `releaseBaseUrl` and one
URL/SHA256 pair per platform. It is strict: unknown fields, an unexpected
channel, version or base URL, a missing platform or a malformed digest fail
closed.

## Release safety guards

### Linear branch promotion

Squash feature PRs into `master`. For a release, review the exact candidate and
wait for its checks, then advance `1.x` to that same commit with a normal
fast-forward push before tagging it. Verify that the current `1.x` tip is an
ancestor of the candidate; if not, stop for explicit resolution.

Do not create a second squashed release commit or merge the release branch back
into `master`. Both branches share the released commit, and later development
continues on `master`. Tags never move. The tagged workflow's existing major-line
ancestry check supports this model without changing signing or artifact paths.
The operator-approved November 2026 history cleanup was a one-time guarded
`master` realignment, not an automatic force-push release step.

### Artifact and channel guards

These do not change the manifest schema or make alpha.2 artifacts invalid:

- `build-release.sh` accepts `snapshot` only for alpha/beta and creates no
  channel pointer; `rc` only for rc; `stable`/`stable-<major>.x` only for a
  final version. A stable/line build for a prerelease fails before building.
- `sign-release.sh` verifies that a version manifest's `channel` matches its
  signing family (`stable` for every tagged version, `edge` for edge). An
  immutable alpha/beta tree has no channel manifest; stable and rc trees must
  carry one.
- `upload-release.sh` independently refuses to write a stable or major-line
  pointer whose version is not final, and refuses an rc pointer whose version
  is not rc. This is a defense-in-depth guard even if a workflow step is wrong.
- Every tagged workflow, final and prerelease, fetches `origin/<major>.x` and
  requires the tag commit to be an ancestor of that major line. The stable job
  additionally accepts only a final tag, while alpha/beta/rc tags are handled by
  the prerelease job in the `edge` environment.
- `promote` accepts only a final candidate on a stable pointer, moves only
  forward, and fails closed if the current pointer is not a final release; RC
  promotion accepts only newer rc versions.

## Trust

Signing is cosign keyless through GitHub Actions OIDC. The pinned identity is
the workflow and ref, not a key file:

- tagged versions, including alpha/beta and rc:
  `github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v<version>`
- edge: `github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master`

`internal/release` verifies the channel or version manifest and its bundle
against that identity, then downloads the platform binary and checks the SHA256
bound by that signed manifest. `upgrade` additionally runs the downloaded
binary's `version --output json` and refuses it unless `version` and `channel`
match the manifest. `scripts/install.sh` also verifies each binary's own
sigstore bundle. The digest check and smoke test catch a build or upload
mismatch before replacement.

`scripts/install.sh` performs the same manifest and binary verification with
the pinned cosign version and checksum, then installs atomically. No public key
or signing secret is needed by an installer or by `upgrade`. GitHub asset URLs
redirect and are not used; the object-store origin must serve direct HTTPS.

## `eregion upgrade`

`eregion upgrade [--version V] [--channel C] [--dry-run] [--yes]` replaces only
the local CLI binary:

1. resolve the channel (or exact version) manifest and verify its signature;
2. plan the from/to version and channel, warning on a downgrade, a major-line
   change or a channel change;
3. download and verify this platform's binary into the release cache;
4. preflight the staged binary's version and channel;
5. copy it beside the target, fsync, and rename atomically, keeping the
   previous binary at `<path>.prev` when a hard link is possible.

With no `--channel`, a final `stable` binary follows its own major line
(`stable-1.x` for a 1.x binary), an rc binary follows the rc pointer, and an
edge binary follows edge. `--channel stable` follows the root pointer and
`--channel edge` follows `master`. An rc binary also checks its stable major
line; a newer final supersedes the rc without moving the rc pointer.
`--channel rc` is explicit consent to move a stable installation to an RC;
stable and major-line defaults never do so. An exact `--version` resolves that
signed snapshot or release directly without requiring a channel flag. A `dev`
build has no channel and must pass `--channel` or an exact `--version`.

Without `--version` or `--channel`, a tagged default never moves backward even
if a pointer resolves lower; an explicit version or channel may downgrade and
emits the `downgrade` warning.

`--dry-run` stops after the plan. A terminal prompts for confirmation unless
`--yes` is given; JSON and no-TTY use never prompt. A binary under Homebrew or
Nix is refused. A non-writable install directory is an error that points at the
channel installer, not a silent sudo. The result reports from/to, path,
previous path, changed, and `nodes: "not upgraded"`.

It never upgrades a node. Nodes are unchanged:
`eregion apply --auto-upgrade-nodes` is the node path. The verified download
half exists as `upgrade.FetchNodeBinary(ctx, channel, version, arch)`, which
returns the linux/amd64 or linux/arm64 binary from the channel or version
manifest. `apply --auto-upgrade-nodes` uses this verified path on a version
mismatch. The alpha.2 node can still verify a schema-1 alpha.3 artifact with its
existing stable signing-family path.

## Release scripts and workflows

| File | Purpose |
| --- | --- |
| `scripts/build-release.sh VERSION CHANNEL BASE_URL` | Four-platform build with `buildinfo` version/commit/channel ldflags, schema-1 version manifest, per-platform SHA256, channel manifest and substituted installers. `CHANNEL` is `stable`, `snapshot`, `rc`, `edge` or `stable-<major>.x`; `snapshot` is the immutable alpha/beta tree with no pointer. |
| `scripts/sign-release.sh RELEASE_ROOT` | Verifies signing-family metadata, then `cosign sign-blob`/`verify-blob` for the version manifest, the channel manifest when one exists, and all four binaries, pinned to `GITHUB_WORKFLOW_REF`. |
| `scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT [all\|artifacts\|channel]` | Uploads immutable `/releases/<version>/` objects, then the channel manifest, bundle and installer last. Pointer uploads pass the stable/line final-version and rc-version guard. |
| `scripts/install.sh` | Per-channel or pinned installer template; verifies cosign, manifest, binary digest and the smoke version/channel, then installs. |
| `internal/release/cmd/promote` | Prints `yes`/`no` for stable or rc pointer movement; stable accepts only a newer final and fails closed on a prerelease current pointer. Reads the current pointer from the bucket. |
| `.github/workflows/publish-edge.yml` | Every push to `master`: verify, build `edge-<12-hex>`, sign, upload. |
| `.github/workflows/publish-stable.yml` | Tag `v*`: require the commit on `origin/<major>.x`, verify and sign one artifact set, upload it immutably, then promote only the eligible rc or stable pointers forward. Alpha/beta use the `snapshot` build channel; rc uses `rc`; final tags use their stable line. |

`just release-local VERSION CHANNEL BASE_URL` builds an unsigned tree under
`dist/release-<channel>` for inspection. It contacts no bucket. Signing is
only in the workflow.

Immutable tagged artifacts have a prefix-existence guard: an existing
`/releases/<version>/` prefix fails the run. An interrupted tagged upload can
leave a partial prefix that blocks retry and requires operator review. The
check is not an atomic lock against another writer. Edge retains its existing
upload semantics, including overwrites on a rerun.

Channel manifests and their signature bundles are separate objects. Publishing
the bundle and manifest is not atomic: a failure or read between writes can
temporarily leave a channel unverifiable. Clients fail closed; this release
does not claim atomic publication or retry safety. A future single-object
pointer requires a deliberate transition for installed clients.

The original `v1.0.0-alpha.1` publication failed. With explicit authorization,
the tag was moved to the reviewed release-channel commit and its snapshot was
published on 2026-11-06; this workflow has no manual-dispatch signing exception.

## Release configuration

Same setup as vaire. `internal/buildinfo.ReleaseBaseURL` is
`https://eregion.mbvlabs.com`; both publish workflows refuse to run unless the
`RELEASE_BASE_URL` variable equals it exactly. The GitHub environments `edge`
and `stable` each carry their own configuration:

- variables `RELEASE_BASE_URL` = `https://eregion.mbvlabs.com` and
  `RELEASE_BUCKET` = `eregion`;
- secrets `R2_ENDPOINT`, `R2_ACCESS_KEY_ID` and `R2_SECRET_ACCESS_KEY`.

No endpoint, account ID or bucket credential is recorded in this repository.
Alpha, beta and rc tag jobs use the `edge` environment and bucket, never
`stable`; the edge environment must allow the `master` branch and `v*` tags.
Canonical final tags use `stable` only. The edge pointer is updated only by the
master workflow, not by prerelease tag publication. The workflow classifies the
full tag before either environment job is eligible; invalid tags fail closed.
GitHub environment tag patterns use `File.fnmatch`: `v*` also matches
prereleases, so a selected-tag allowlist of exact final tag names or a custom
deployment protection rule is required for an independent environment-level
boundary. Review every workflow that can reference `stable`. Tags and the `1.x`
branch are created only when the user authorizes them.

## Alpha.3 status

Alpha.3 source adds:

- the shared read-only container resolver (plain images directly; Buildpacks
  after its launcher) for bare process/release commands, with no PATH rewrite;
- the release safety guards above.

The source is implemented and offline-tested. It is **not tagged, published or
host-qualified** in this worktree; the orchestrator owns commit, push, PR, tag
and release. A real `v1.0.0-alpha.3` qualification still needs a published
schema-1 snapshot, installer checks, a host node upgrade, and a real
Docker/Buildpacks run for the resolver. No migration or compatibility shim is
required for schema-1 alpha.2 nodes.

## Qualification

Local verification is `build-release.sh` plus inspection of the generated
manifest and installer; signing, upload and promotion are workflow-only. A
real release qualification installs from a channel on Linux and macOS, checks
that a tampered manifest or binary is refused, moves between two edge builds
with `upgrade`, verifies the previous-binary copy, and confirms that upgrade
left the node untouched. Record that separately from the binary build.
