# Cerberus task runner. All commands go through `just`.
# Run `just` for the full recipe list.

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

GOLANGCI_LINT_VERSION := "v2.12.2"
GOFUMPT_VERSION := "v0.7.0"
GOIMPORTS_VERSION := "latest"
MODULE := "github.com/tsouza/cerberus"

# Default: list recipes.
default:
    @just --list

# === Tools ===

# Install dev tools into $GOBIN (one-time).
install-tools:
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{GOLANGCI_LINT_VERSION}}
    go install mvdan.cc/gofumpt@{{GOFUMPT_VERSION}}
    go install golang.org/x/tools/cmd/goimports@{{GOIMPORTS_VERSION}}

# === Build ===

# Build cerberus into ./bin.
build:
    go build -trimpath -o bin/cerberus ./cmd/cerberus

# Install cerberus into $GOBIN.
install:
    go install -trimpath ./cmd/cerberus

# Remove build outputs.
clean:
    rm -rf bin/ dist/ coverage.out

# === Test ===

# Run unit + spec tests with race detector + coverage.
test:
    go test -race -coverprofile=coverage.out ./...

# Regenerate TXTAR golden sections in test/spec/**/*.txtar from current output.
# Review `git diff test/spec/` before committing.
update-golden:
    GOLDEN_UPDATE=1 go test ./...
    @echo
    @echo "Diff of regenerated fixtures:"
    @git --no-pager diff --stat test/spec/ || true

# === Lint / format ===

# Run all linters.
lint:
    golangci-lint run ./...

# Format Go code.
fmt:
    gofumpt -l -w .
    goimports -l -w -local {{MODULE}} .

# === CI entry point ===

# Lint + test + build. Used by ci.yml.
ci: lint test build

# === Dependencies ===

# go mod tidy.
deps-tidy:
    go mod tidy
