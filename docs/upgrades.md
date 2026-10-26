# Signed releases and CLI upgrade

Reference for the current code. Product decisions are MASTER D14–D15: `master`
is the edge channel, `1.x` is the whole v1 line, and tags are
`v1.0.0-alpha.N`, then `v1.0.0`. This replaces the earlier Ed25519 manifests,
the one-host replacement journal and the Windows release matrix.

## Channels

| Channel | Published by | Pointer under the release origin | Installer |
| --- | --- | --- | --- |
| `edge` | every push to `master` | `/edge/manifest.json` | `/edge/install.sh` |
| `stable-<major>.x` | tags on the `<major>.x` line | `/<major>.x/manifest.json` | `/<major>.x/install.sh` |
| `stable` | the same stable tags, highest version only | `/manifest.json` | `/install.sh` |

A tag `v1.0.0-alpha.N` builds channel `stable-1.x`; its manifest says `stable`
because the stable workflow signs it, and its binary's `Channel` is `stable`.
With no `--channel`, `eregion upgrade` follows its own line (`stable-1.x` for a
1.x binary), so a final release does not move a pinned line by accident.
`--channel stable` follows the root pointer, and `--channel edge` follows
`master`. A `dev` build has no channel and must pass `--channel`.

Immutable artifacts live under `/releases/<version>/<os>/<arch>/eregion`, with
`.sha256` and `.sigstore.json` sidecars. Supported platforms are linux/amd64,
linux/arm64, darwin/amd64 and darwin/arm64; there are no Windows builds.

Each channel manifest is a small JSON file with `schemaVersion`, `channel`,
`version`, `releaseBaseUrl` and one URL/SHA256 pair per platform. It is strict:
unknown fields, an unexpected channel, version or base URL, a missing platform
or a malformed digest fail closed.

## Trust

Signing is cosign keyless through GitHub Actions OIDC. The pinned identity is
the workflow and ref, not a key file:

- stable: `github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v<version>`
- edge: `github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master`

`internal/release` verifies the channel manifest and its bundle against that
identity, then downloads the platform binary and checks the SHA256 bound by
that signed manifest. `upgrade` additionally runs the downloaded binary's
`version --output json` and refuses it unless `version` and `channel` match the
manifest. `scripts/install.sh` also verifies each binary's own sigstore bundle.
The digest check and smoke test catch a build or upload mismatch before
replacement.

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

`--dry-run` stops after the plan. A terminal prompts for confirmation unless
`--yes` is given; JSON and no-TTY use never prompt. A binary under Homebrew or
Nix is refused. A non-writable install directory is an error that points at the
channel installer, not a silent sudo. The result reports from/to, path,
previous path, changed, and `nodes: "not upgraded"`.

It never upgrades a node. Nodes are unchanged:
`eregion apply --auto-upgrade-nodes` is the node path. The verified download
half exists as `upgrade.FetchNodeBinary(ctx, channel, version, arch)`, which
returns the linux/amd64 or linux/arm64 binary from the channel manifest. The
`apply` command itself is work list row 3 and does not exist yet.

## Release scripts and workflows

| File | Purpose |
| --- | --- |
| `scripts/build-release.sh VERSION CHANNEL BASE_URL` | Four-platform build with `buildinfo` version/commit/channel ldflags, version manifest, per-platform SHA256, channel manifest and substituted `install.sh`. |
| `scripts/sign-release.sh RELEASE_ROOT` | `cosign sign-blob` and `verify-blob` for the channel manifest, version manifest and all four binaries, pinned to `GITHUB_WORKFLOW_REF`. |
| `scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT [all\|artifacts\|channel]` | Uploads immutable `/releases/<version>/` objects, then the channel manifest, bundle and installer last. |
| `scripts/install.sh` | Per-channel installer template; verifies cosign, manifest, binary digest and the smoke version/channel, then installs. |
| `internal/release/cmd/promote` | Prints `yes`/`no` for a stable channel move using the same monotonic prerelease policy; reads the current pointer fail-closed from the bucket. |
| `.github/workflows/publish-edge.yml` | Every push to `master`: verify, build `edge-<12-hex>`, sign, upload. |
| `.github/workflows/publish-stable.yml` | Tag `v*`: require the commit on `origin/<major>.x`, verify, build `stable-<major>.x`, sign, refuse an existing version, upload artifacts, promote the line and root pointers forward only. |

`just release-local VERSION CHANNEL BASE_URL` builds an unsigned tree under
`dist/release-<channel>` for inspection. It contacts no bucket. Signing is
only in the workflow.

The stable workflow publishes a pre-release only while that channel has no
final release, and never replaces a final with an alpha, beta or rc. Immutable
artifacts are never overwritten: an existing `/releases/<version>/` prefix
fails the run.

## Configuration placeholders

`internal/buildinfo.ReleaseBaseURL` is a documented placeholder
(`https://releases.eregion.example`) until the public release origin exists.
Both publish workflows refuse to run unless the `EREGION_RELEASE_BASE_URL`
repository variable exactly equals it. Before the first publish, set:

- repository variable `EREGION_RELEASE_BASE_URL` to the bucket's public HTTPS
  origin, and change `ReleaseBaseURL` in the same commit;
- repository variable `EREGION_RELEASE_BUCKET` to the R2 bucket name (for
  example the placeholder `eregion-releases`);
- environment `edge` and environment `stable` with secrets
  `EREGION_R2_ENDPOINT`, `EREGION_R2_ACCESS_KEY_ID` and
  `EREGION_R2_SECRET_ACCESS_KEY`.

No endpoint, account ID, bucket credential or base URL is recorded in this
repository. The stable environment must exist before the first tag. Tags and
the `1.x` branch are created only when the user authorizes them.

## Qualification

Local verification is `build-release.sh` plus inspection of the generated
manifest and installer; signing, upload and promotion are workflow-only. A
real release qualification installs from a channel on Linux and macOS, checks
that a tampered manifest or binary is refused, moves between two edge builds
with `upgrade`, verifies the previous-binary copy, and confirms that upgrade
left the node untouched. Record that separately from the binary build.
