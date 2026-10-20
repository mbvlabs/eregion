default:
    @just --list

# Build the operator CLI.
build:
    go build -trimpath -o bin/eregion ./cmd/eregion

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
    go test -race -count=1 ./internal/controlplane -run TestIntegration -timeout 240s

# Check formatting, static analysis, and tests.
check: fmt-check lint test
