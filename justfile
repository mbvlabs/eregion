set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

commit := `git rev-parse --short=12 HEAD`
ldflags := "-s -w -X github.com/mbvlabs/eregion/internal/buildinfo.Version=dev -X github.com/mbvlabs/eregion/internal/buildinfo.Commit=" + commit + " -X github.com/mbvlabs/eregion/internal/buildinfo.Channel=dev"

default:
    @just --list

# Build the one eregion binary: operator CLI, control plane, and node agent.
build:
    mkdir -p bin
    CGO_ENABLED=0 go build -trimpath -ldflags "{{ldflags}}" -o bin/eregion ./cmd/eregion

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
    go build -trimpath -o testdata/ssh/eregion ./cmd/eregion
    docker build -t eregion-ssh-fixture:latest testdata/ssh
    rm -f testdata/ssh/eregion

# Run the transport tests, including the disposable SSH node fixture.
transport:
    go test -count=1 ./internal/transport -timeout 600s

# Run the WireGuard transport tests: the in-process tunnel and the disposable
# node behind kernel WireGuard. The container tests skip without Docker,
# NET_ADMIN, or the host's wireguard module.
wireguard:
    go test -count=1 ./internal/transport -run 'WireGuard' -v -timeout 600s

# Check formatting, static analysis, and tests.
check: fmt-check lint test

# Build an unsigned channel tree under dist/release-<channel>.
release-local VERSION CHANNEL BASE_URL:
    scripts/build-release.sh "{{VERSION}}" "{{CHANNEL}}" "{{BASE_URL}}"
