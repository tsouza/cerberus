# Cerberus task runner. All commands go through `just`.
# Run `just` for the full recipe list.

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

GOLANGCI_LINT_VERSION := "v2.12.2"
GOFUMPT_VERSION := "v0.7.0"
GOIMPORTS_VERSION := "latest"
GREMLINS_VERSION := "v0.6.0"
MARKDOWNLINT_VERSION := "v0.18.1"
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
    go install github.com/go-gremlins/gremlins/cmd/gremlins@{{GREMLINS_VERSION}}

# === Build ===

# Build cerberus into ./bin.
build:
    go build -trimpath -o bin/cerberus ./cmd/cerberus

# Install cerberus into $GOBIN.
install:
    go install -trimpath ./cmd/cerberus

# Remove build outputs.
clean:
    rm -rf bin/ dist/

# === Test ===

# Run unit + spec tests with race detector.
test:
    go test -race ./...

# Regenerate TXTAR golden sections in test/spec/**/*.txtar from current output.
# Review `git diff test/spec/` before committing.
update-golden:
    GOLDEN_UPDATE=1 go test ./...
    @echo
    @echo "Diff of regenerated fixtures:"
    @git --no-pager diff --stat test/spec/ || true

# === Mutation testing ===

# Run gremlins across internal/. Slow; expect minutes. Honors .gremlins.yaml.
mutate:
    gremlins unleash ./internal/...

# Quick mutation pass on a single package: `just mutate-pkg internal/chsql`.
mutate-pkg PATH:
    gremlins unleash ./{{PATH}}

# === Lint / format ===

# Run Go linters.
lint:
    golangci-lint run ./...

# Lint all Markdown files (run via npm exec; no global Node deps).
lint-md:
    npm exec --yes -- markdownlint-cli2@{{MARKDOWNLINT_VERSION}} "**/*.md" "!harness/compatibility/upstream/**" "!**/node_modules/**"

# Auto-fix Markdown lint issues where possible.
fmt-md:
    npm exec --yes -- markdownlint-cli2@{{MARKDOWNLINT_VERSION}} --fix "**/*.md" "!harness/compatibility/upstream/**" "!**/node_modules/**"

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

# === E2E (k3d + ClickHouse + Grafana + cerberus) ===

K3D_CLUSTER := "cerberus-e2e"
CERBERUS_IMAGE := "cerberus:e2e"

# Boot the k3d cluster, build cerberus image, import it, apply manifests, wait for pods.
# Host ports map via the k3d loadbalancer to NodePorts on the k3s nodes:
#   host:8080 -> LB -> NodePort 30080 (cerberus svc)
#   host:3000 -> LB -> NodePort 30030 (grafana svc)
e2e-up: e2e-down
    @echo "==> creating k3d cluster {{K3D_CLUSTER}}"
    k3d cluster create {{K3D_CLUSTER}} \
        --port "3000:30030@loadbalancer" \
        --port "8080:30080@loadbalancer" \
        --no-lb=false \
        --k3s-arg "--disable=traefik@server:0" \
        --wait
    @echo "==> building cerberus image"
    docker build -t {{CERBERUS_IMAGE}} -f Dockerfile.local .
    @echo "==> importing image into k3d"
    k3d image import {{CERBERUS_IMAGE}} -c {{K3D_CLUSTER}}
    @echo "==> applying manifests"
    kubectl apply -k deploy/k3s/
    @echo "==> waiting for pods (up to 3 min)"
    kubectl -n cerberus wait --for=condition=Available deployment/clickhouse --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/cerberus   --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/grafana    --timeout=180s
    @echo "==> e2e-up done"
    @echo "    grafana:    http://localhost:3000 (admin/admin)"
    @echo "    cerberus:   http://localhost:8080/healthz"

# Ingest sample OTel data into ClickHouse.
# kubectl exec needs -i to forward stdin into the remote container; without
# it, the `< file` redirect goes only to local stdin and the seed never runs.
e2e-seed:
    @echo "==> seeding OTel metrics"
    kubectl -n cerberus exec -i deploy/clickhouse -- \
        clickhouse-client \
            --user cerberus --password cerberus \
            --database otel --multiquery \
            < test/e2e/seed/otel_metrics.sql
    @echo "==> verifying table rowcounts"
    kubectl -n cerberus exec deploy/clickhouse -- \
        clickhouse-client --user cerberus --password cerberus --database otel --query "\
            SELECT MetricName, count() AS rows FROM ( \
                SELECT MetricName FROM otel_metrics_gauge \
                UNION ALL \
                SELECT MetricName FROM otel_metrics_sum \
            ) GROUP BY MetricName ORDER BY MetricName FORMAT PrettyCompact"
    @echo "==> seed done"

# Run Go E2E HTTP tests against the deployed stack.
e2e-run:
    @echo "==> running Go E2E tests"
    go test -tags=e2e ./test/e2e/...

# Run the Grafana playwright smoke (lands in M0.2).
e2e-playwright:
    @echo "==> playwright smoke (lands in M0.2)"
    @if [ -d test/e2e/playwright ]; then \
        cd test/e2e/playwright && npm ci && npx playwright test; \
    else \
        echo "    (no playwright suite yet — landing in M0.2)"; \
    fi

# Tear down the cluster.
e2e-down:
    @if k3d cluster list | grep -q "^{{K3D_CLUSTER}} "; then \
        echo "==> deleting k3d cluster {{K3D_CLUSTER}}"; \
        k3d cluster delete {{K3D_CLUSTER}}; \
    fi



# Full lifecycle.
e2e: e2e-up e2e-seed e2e-run e2e-playwright e2e-down

# === Compatibility (prometheus/compliance differential harness) ===

# Run the PromQL compatibility suite end-to-end. Slow; expect minutes.
# Sets up the Docker Compose stack (reference Prom + cerberus + CH + seeder),
# runs the upstream tester, writes harness/compatibility/report.json.
compatibility:
    ./harness/compatibility/scripts/run-compatibility.sh

# Keep the compatibility stack running after the tester finishes (for debugging).
compatibility-keep:
    COMPOSE_KEEP=1 ./harness/compatibility/scripts/run-compatibility.sh

# Tear down the compatibility stack manually.
compatibility-down:
    cd harness/compatibility && docker compose down -v
