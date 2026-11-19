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
	golangci-lint run ./...

# Run integration tests against a disposable PostgreSQL server.
test: integration

# Run integration tests with the race detector.
race: integration-race

# Run integration tests with a disposable PostgreSQL server and isolated Docker
# daemon. Real Pack tests also require Pack on PATH, a local Paketo builder and
# EREGION_TEST_REAL_PACK_REGISTRY pointing to the pinned eregion-registry on
# loopback; they create images, volumes and networks on that daemon.
integration:
    @test -n "${EREGION_TEST_DATABASE_URL:-}" || { echo "Set EREGION_TEST_DATABASE_URL to a disposable PostgreSQL test server (role requires CREATEDB)." >&2; exit 1; }
    @test -n "${EREGION_TEST_REAL_PACK_REGISTRY:-}" && command -v pack >/dev/null || { echo "Install Pack and set EREGION_TEST_REAL_PACK_REGISTRY to the registry on an isolated test Docker daemon." >&2; exit 1; }
    go test -count=1 -v ./internal/controlplane -run '^TestIntegration' -timeout 20m
    go test -count=1 -v ./internal/cli -run '^TestIntegrationCLIRegression' -timeout 600s
    go test -count=1 -v ./internal/cli -run '^TestIntegrationBuildSlotsRPC$' -timeout 120s
    go test -count=1 -v ./internal/host -run '^TestIntegration' -timeout 600s
    go test -count=1 -v ./internal/noded -run '^TestIntegration' -timeout 120s
    go test -count=1 -v ./internal/platform -run '^TestIntegrationPack' -timeout 120s

# Run the same suite with the race detector.
integration-race:
    @test -n "${EREGION_TEST_DATABASE_URL:-}" || { echo "Set EREGION_TEST_DATABASE_URL to a disposable PostgreSQL test server (role requires CREATEDB)." >&2; exit 1; }
    @test -n "${EREGION_TEST_REAL_PACK_REGISTRY:-}" && command -v pack >/dev/null || { echo "Install Pack and set EREGION_TEST_REAL_PACK_REGISTRY to the registry on an isolated test Docker daemon." >&2; exit 1; }
    go test -race -count=1 -v ./internal/controlplane -run '^TestIntegration' -timeout 20m
    go test -race -count=1 -v ./internal/cli -run '^TestIntegrationCLIRegression' -timeout 600s
    go test -race -count=1 -v ./internal/cli -run '^TestIntegrationBuildSlotsRPC$' -timeout 120s
    go test -race -count=1 -v ./internal/host -run '^TestIntegration' -timeout 600s
    go test -race -count=1 -v ./internal/noded -run '^TestIntegration' -timeout 120s
    go test -race -count=1 -v ./internal/platform -run '^TestIntegrationPack' -timeout 120s

# Check formatting, static analysis, and build without executing tests.
check: fmt-check lint build

# Build an unsigned channel tree under dist/release-<channel>.
release-local VERSION CHANNEL BASE_URL:
    scripts/build-release.sh "{{VERSION}}" "{{CHANNEL}}" "{{BASE_URL}}"

# Generate the private query package with the pinned sqlc binary.
sqlc-generate:
    tools/sqlc/install.sh generate -f sqlc.yaml

# Fail if the private query package differs from the pinned generator output.
sqlc-diff:
    tools/sqlc/install.sh diff -f sqlc.yaml
