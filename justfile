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

# Check formatting, static analysis, and tests.
check: fmt-check lint test
