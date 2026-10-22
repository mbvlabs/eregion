default:
    @just --list

# Start a disposable loopback PostgreSQL + API + worker; never touches host services.
dev-up:
    python3 scripts/dev.py up

# Stop the development API and discard only this checkout's disposable database.
dev-down:
    python3 scripts/dev.py down

# Check local development readiness without printing credentials.
dev-status:
    python3 scripts/dev.py status

# Rebuild/restart API+workers and apply new migrations, retaining the dev database.
dev-restart:
    python3 scripts/dev.py restart

# Check development launcher permissions, identity guards, and atomic writes.
dev-test:
    python3 -m unittest discover -s scripts -p 'test_*.py'

# Install checksum-pinned pack into .local/dev/tools, never system-wide.
dev-buildpacks:
    python3 scripts/buildpacks.py

# Build real sample images, deploy, reject a bad candidate, and roll back (requires dev-up).
smoke:
    python3 scripts/smoke.py

# Build the sample app from a directory with Buildpacks, deploy, and serve it (requires dev-up and dev-buildpacks).
buildpacks-smoke:
    python3 scripts/buildpacks_smoke.py

# Exercise the real interactive viewer in a resizable pseudo-terminal (requires dev-up).
terminal-smoke:
    python3 scripts/terminal_smoke.py

# Run the disposable PostgreSQL backup/restore drill. Prerequisites: Docker is
# installed and reachable, and the image below is available locally. Without
# them this target prints a skip and exits successfully.
recovery-drill:
    @if ! command -v docker >/dev/null 2>&1; then echo "skip: Docker is not installed"; elif ! docker info >/dev/null 2>&1; then echo "skip: Docker daemon is not reachable"; elif ! docker image inspect postgres:17 >/dev/null 2>&1; then echo "skip: postgres:17 is not available locally; pull it explicitly to run this drill"; else python3 scripts/recovery_drill.py; fi

# Exercise the separate external control-plane database recovery procedure:
# dump a migrated source, restore into two generated temporary databases on the
# same server, verify migration/River readiness and key tables, then compare the
# restores. Prerequisites: PostgreSQL client tools, a built bin/eregion, and
# EREGION_TEST_DATABASE_URL or EREGION_DATABASE_URL pointing at an already-migrated
# control-plane database (or a running just dev-up environment). It never drops or
# writes the source database and refuses names outside its generated prefix.
controlplane-recovery-drill: build
    python3 scripts/controlplane_recovery_drill.py

# Run the signed upgrade drill: build disposable 1.0.0/1.1.0 binaries and a
# signed manifest, then exercise verify/check/apply/rollback/status and the
# automatic restore after a failed health probe.
upgrade-drill:
    python3 scripts/upgrade_drill.py

# Run every release drill. Container prerequisites skip clearly and never
# silently pass a failed drill.
release-drill:
    @just recovery-drill
    @just upgrade-drill

# Run the interactive TERM matrix when a development environment is ready;
# otherwise print a clear skip.
release-terminal:
    @if python3 scripts/dev.py status >/dev/null 2>&1; then python3 scripts/terminal_smoke.py; else echo "skip: development environment is not ready; run just dev-up"; fi

# Build the operator CLI.
build:
    go build -trimpath -o bin/eregion ./cmd/eregion

# Build the node binary that answers node requests on a node.
build-node:
    go build -trimpath -o bin/eregion-node ./cmd/eregion-node

# Format Go sources.
fmt:
    gofmt -w cmd internal schema examples

# Fail if Go sources need formatting.
fmt-check:
    @test -z "$(gofmt -l cmd internal schema examples)" || { gofmt -l cmd internal schema examples; exit 1; }

# Run static analysis.
lint:
    go vet ./...
    go -C examples/hello vet ./...

# Run unit, contract, schema, and executable tests.
test:
    go test ./...
    go -C examples/hello test ./...

# Run tests with the race detector.
race:
    go test -race ./...
    go -C examples/hello test -race ./...

# Run live PostgreSQL/River/API/process-crash tests against isolated test databases.
integration:
    @test -n "${EREGION_TEST_DATABASE_URL:-}" || { echo "Set EREGION_TEST_DATABASE_URL to a disposable PostgreSQL test server (role requires CREATEDB)." >&2; exit 1; }
    go test -race -count=1 ./internal/controlplane -run TestIntegration -timeout 600s

# Run the integration suite without the race detector, for hosts without a C compiler.
integration-norace:
    @test -n "${EREGION_TEST_DATABASE_URL:-}" || { echo "Set EREGION_TEST_DATABASE_URL to a disposable PostgreSQL test server (role requires CREATEDB)." >&2; exit 1; }
    go test -count=1 ./internal/controlplane -run TestIntegration -timeout 600s

# Run a throwaway PostgreSQL 17 for the integration suite.
postgres:
    docker run -d --rm --name eregion-postgres -e POSTGRES_PASSWORD=eregion -e POSTGRES_USER=eregion -e POSTGRES_DB=eregion -p 127.0.0.1:55432:5432 postgres:17

# Remove the throwaway PostgreSQL container.
postgres-stop:
    docker rm -f eregion-postgres

# Build the disposable SSH node fixture image used by the transport tests. The
# same image runs the node behind kernel WireGuard for the WireGuard tests.
ssh-fixture:
    go build -trimpath -o testdata/ssh/eregion-node ./cmd/eregion-node
    docker build -t eregion-ssh-fixture:latest testdata/ssh
    rm -f testdata/ssh/eregion-node

# Run the transport tests, including the disposable SSH node fixture.
transport:
    go test -count=1 ./internal/transport -timeout 600s

# Run the WireGuard transport tests: the in-process tunnel and the disposable
# node behind kernel WireGuard. The container tests skip without Docker,
# NET_ADMIN, or the host's wireguard module.
wireguard:
    go test -count=1 ./internal/transport -run 'WireGuard' -v -timeout 600s

# Check formatting, static analysis, and tests.
check: fmt-check lint test dev-test

# Run the v0.1 release gate. Drills and the terminal matrix skip explicitly when
# Docker or a dev environment is unavailable; skipped evidence is not a pass.
release-check:
    just fmt-check
    go vet ./...
    go test ./...
    go -C examples/hello test ./...
    python3 -m unittest discover -s scripts -p 'test_*.py'
    @just release-drill
    @just release-terminal

# Run the interactive terminal TERM matrix (requires just dev-up).
terminal-matrix:
    python3 scripts/terminal_smoke.py
