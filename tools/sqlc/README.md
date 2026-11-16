# Pinned sqlc tool

`sqlc` is not an application dependency. This directory pins the official
standalone release and installs it under the ignored `bin/` directory, so the
Go module remains unchanged.

- Version: `v1.31.1`
- Upstream: `https://github.com/sqlc-dev/sqlc/releases/tag/v1.31.1`
- Platforms: Linux amd64 and arm64
- Archive integrity: official release SHA-256 values in `SHA256SUMS`
- Executable integrity: expected extracted-binary SHA-256 values in
  `BINARY_SHA256SUMS`

The executable checksums were derived from the official archives after
verifying those archives against `SHA256SUMS`; they are platform-specific.

On every invocation `install.sh` validates an existing cached binary against its
expected executable checksum and its exact `v1.31.1` version output. A healthy
cache is used without downloading the archive again. A missing, corrupt,
replaced or wrongly executable cache triggers a fresh download into an isolated
`.install.*` directory under `bin/`, verifies both the archive and the extracted
executable, installs it, and removes the scratch directory.

The checksum files and cache live under the same user-owned directory. These
checks protect against stale/corrupt caches and accidental replacement; they do
not claim to defend against an actor who can rewrite the checksums and binary
together.

Install and print the binary path:

```sh
tools/sqlc/install.sh
```

Install if needed and run a sqlc command from the repository root:

```sh
tools/sqlc/install.sh generate -f sqlc.yaml
tools/sqlc/install.sh diff -f sqlc.yaml
just sqlc-generate
just sqlc-diff
```

The checked-in configuration generates `internal/entities/internal/query` from
the existing Goose migrations in `internal/dbschema/migrations` and the queries
in `internal/entities/queries`. The generated package is private to
`internal/entities`.

The `just` recipes cover all three private generation targets:

- `sqlc.yaml`: typed application persistence using pgx.
- `sqlc.river.yaml`: application-owned River metadata queries, against the
  pinned generation-only River schema.
- `sqlc.infrastructure.yaml`: catalog inspection on Goose's existing
  repeatable-read `database/sql` transaction, using pgx's stdlib driver and the
  same pool. This is not another domain persistence path.

`internal/entities/infrastructure/schema.sql` describes PostgreSQL's built-in
TLS view for generation only. Like the pinned River schema, it is not embedded
or applied as a migration. Goose and River continue to own their library SQL;
application-owned SQL lives under `internal/entities`.
