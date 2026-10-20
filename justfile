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

# Run bootstrap check/plan and journaled apply/resume/idempotency in disposable
# Debian/Ubuntu containers. This is a proxy: no systemd, Docker daemon, or apt.
bootstrap-matrix:
    python3 scripts/bootstrap_matrix.py

# Exercise the real interactive viewer in a resizable pseudo-terminal (requires dev-up).
terminal-smoke:
    python3 scripts/terminal_smoke.py

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
