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

# Run integration tests against a disposable PostgreSQL server.
test: integration

# Run integration tests with the race detector.
race: integration-race

# Run live PostgreSQL/River/API/process-crash tests against isolated test databases.
integration:
    @test -n "${EREGION_TEST_DATABASE_URL:-}" || { echo "Set EREGION_TEST_DATABASE_URL to a disposable PostgreSQL test server (role requires CREATEDB)." >&2; exit 1; }
    go test -count=1 -v ./internal/controlplane -run '^TestIntegration' -timeout 600s

# Run the same suite with the race detector.
integration-race:
    @test -n "${EREGION_TEST_DATABASE_URL:-}" || { echo "Set EREGION_TEST_DATABASE_URL to a disposable PostgreSQL test server (role requires CREATEDB)." >&2; exit 1; }
    go test -race -count=1 -v ./internal/controlplane -run '^TestIntegration' -timeout 600s

# Check formatting, static analysis, and build without executing tests.
check: fmt-check lint build

# Build an unsigned channel tree under dist/release-<channel>.
release-local VERSION CHANNEL BASE_URL:
    scripts/build-release.sh "{{VERSION}}" "{{CHANNEL}}" "{{BASE_URL}}"
