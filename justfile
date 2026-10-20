default:
    @just --list

# Build the operator CLI.
build:
    go build -trimpath -o bin/eregion ./cmd/eregion

# Build the node binary that answers node requests on a node.
build-node:
    go build -trimpath -o bin/eregion-node ./cmd/eregion-node

# Format Go sources.
fmt:
    gofmt -w cmd internal schema

# Fail if Go sources need formatting.
fmt-check:
    @test -z "$(gofmt -l cmd internal schema)" || { gofmt -l cmd internal schema; exit 1; }

# Run static analysis.
lint:
    go vet ./...

# Run unit, contract, schema, and executable tests.
test:
    go test ./...

# Run tests with the race detector.
race:
    go test -race ./...

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

# Build the disposable SSH node fixture image used by the transport tests.
ssh-fixture:
    go build -trimpath -o testdata/ssh/eregion-node ./cmd/eregion-node
    docker build -t eregion-ssh-fixture:latest testdata/ssh
    rm -f testdata/ssh/eregion-node

# Run the transport tests, including the disposable SSH node fixture.
transport:
    go test -count=1 ./internal/transport -timeout 600s

# Check formatting, static analysis, and tests.
check: fmt-check lint test
