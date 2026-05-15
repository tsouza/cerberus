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

# Install lefthook + activate git hooks. Idempotent; run once after clone.
# Hooks defined in lefthook.yml run gofumpt / goimports / markdownlint-cli2 --fix
# on staged files at commit time, and commitlint on the commit message.
# Heavy validation (go test / golangci-lint / go build) is NOT in the hook —
# CI owns that. See CLAUDE.md § "No local validation; lefthook + CI own it."
hooks-install:
    go install github.com/evilmartians/lefthook@latest
    lefthook install

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

# Run the internal/schema/ddl integration tests against a real ClickHouse
# container (spun up via testcontainers-go). Requires Docker. Gated behind
# the `integration` build tag so regular `just test` doesn't pull in
# Docker.
schema-ddl-test:
    go test -race -tags=integration ./internal/schema/ddl/...

# Run the TXTAR spec suite with the chDB-backed round-trip assertion
# layer enabled. Requires libchdb.so (see `just chdb-install`). The
# default `just test` lane stays CGO_ENABLED=0 and never compiles the
# chdb-go driver. Only fixtures that declare both `seed:` and
# `expected_rows:` are executed against chDB; everything else still
# runs through the text-equality golden path.
spec-chdb:
    go test -tags chdb -count=1 ./test/spec/...

# Run the chDB-tagged handler tests under internal/api/... plus the
# chclienttest package itself. Same prerequisite as spec-chdb (libchdb
# at the default install path). Mirrors the `chdb` CI job.
test-chdb:
    go test -tags chdb -count=1 ./internal/chclienttest/... ./internal/api/...

# Run the FuzzParse target for one parser head for a bounded duration.
# Usage: `just fuzz QL=promql DURATION=60s` (defaults).
fuzz QL="promql" DURATION="60s":
    go test -run='^$' -fuzz=FuzzParse -fuzztime={{DURATION}} ./internal/{{QL}}/...

# Run all Go benchmarks (no tests). Short benchtime for local use.
bench:
    go test -bench=. -benchmem -benchtime=5x -run='^$' ./...

# Generate the GA-prep coverage baseline (default-tag + chdb-tagged
# lanes, merged via in-line awk because gocovmerge can't reconcile
# block-boundary drift between the two compilations). Writes
# cover.out, cover-chdb.out, and cover-merged.out, then prints the
# total + a per-package summary sorted by coverage. See
# docs/coverage-baseline.md for what the numbers mean and which
# packages are intentionally under-tested.
#
# Requires chDB for the second lane (`just chdb-install`). If
# libchdb.so isn't present, the recipe still emits cover.out and
# treats cover-merged.out as cover.out (default-tag baseline only).
coverage:
    @echo "==> default-tag coverage"
    # `|| true` tolerates partial failures (e.g. `main` packages that
    # require the `covdata` tool on toolchains that ship without it).
    # The cover.out profile is still written for every package that
    # compiled, which is all production code in internal/**.
    go test -coverprofile=cover.out ./... || true
    @test -s cover.out
    @if [ -e /usr/local/lib/libchdb.so ]; then \
        echo "==> chdb-tagged coverage"; \
        go test -tags chdb -coverprofile=cover-chdb.out ./... || true; \
        echo "==> merging profiles"; \
        { echo "mode: set"; \
          awk 'FNR==1{next} { k=$1" "$2; if (!(k in m) || $3>m[k]) m[k]=$3 } END { for (k in m) print k, m[k] }' cover.out cover-chdb.out | sort; \
        } > cover-merged.out; \
    else \
        echo "==> libchdb.so not found, skipping chdb lane"; \
        cp cover.out cover-merged.out; \
    fi
    @echo
    @echo "==> Total"
    @go tool cover -func=cover-merged.out | tail -1
    @echo
    @echo "==> Per-package (sorted by coverage)"
    @awk -F'[: ,]' 'NR > 1 { \
        n = split($0, w, " "); stmts = w[n-1]; hits = w[n]; \
        split($0, a, ":"); fp = a[1]; \
        sub(/^github\.com\/tsouza\/cerberus\//, "", fp); \
        k = fp; sub(/\/[^\/]+$/, "", k); \
        total[k] += stmts; \
        if (hits != 0) covered[k] += stmts; \
      } END { \
        for (p in total) { \
          pct = (total[p] > 0) ? 100.0*covered[p]/total[p] : 0; \
          printf "%6.2f%%  %5d / %-5d  %s\n", pct, covered[p], total[p], p; \
        } \
      }' cover-merged.out | sort -rn

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

# Run gremlins on internal/optimizer/ + internal/chsql/ with the `chdb`
# build tag enabled so the chDB-backed property test (R8.3) and the
# TXTAR round-trip suite (R8.1) participate in the kill criterion.
#
# `-i` is the integration flag: per mutation, gremlins runs the
# complete `go test -tags chdb ./...` instead of just the mutated
# package's local test file. That brings test/spec/<head>/ round-trip
# tests into scope, so a mutation that changes SQL text but not the
# rendered row set is correctly NOT killed (semantically equivalent),
# which sharpens the score over the default lane.
#
# Slow: hundreds of mutants, each spinning up an ephemeral chDB
# session. Expect tens of minutes. Requires libchdb.so (see
# `just chdb-install`). Not on the PR critical path — informational.
mutate-chdb:
    gremlins unleash -t chdb -i ./internal/optimizer/... ./internal/chsql/...

# === Lint / format ===

# Run Go linters.
lint:
    golangci-lint run ./...

# Lint all Markdown files (run via npm exec; no global Node deps).
lint-md:
    npm exec --yes -- markdownlint-cli2@{{MARKDOWNLINT_VERSION}} "**/*.md" "!harness/prometheus-compliance/upstream/**" "!**/node_modules/**"

# Auto-fix Markdown lint issues where possible.
fmt-md:
    npm exec --yes -- markdownlint-cli2@{{MARKDOWNLINT_VERSION}} --fix "**/*.md" "!harness/prometheus-compliance/upstream/**" "!**/node_modules/**"

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

# === chDB (in-process ClickHouse engine probe) ===

CHDB_VERSION := "v4.0.2"
CHDB_INSTALL_PATH := "/usr/local/lib/libchdb.so"

# Install libchdb.so (the in-process ClickHouse engine shared library)
# used by the chdb-go database/sql driver. Required only for tests that
# carry the `chdb` build tag — currently the engine-probe test under
# `internal/chclient/`. Production builds never link against this; the
# release binary stays CGO_ENABLED=0.
#
# Pinned to v4.0.2 because that is the last upstream release that ships
# the standalone `<platform>-libchdb.tar.gz` assets the chdb-go driver
# expects; v4.1.x releases bundle libchdb inside Python wheels only.
# Mirror update_libchdb.sh shipped inside chdb-go.
#
# Idempotent: skips download if the install path already exists. Override
# CHDB_VERSION at the recipe call (`just chdb-install CHDB_VERSION=v4.0.2`).
chdb-install:
    @if [ -f "{{CHDB_INSTALL_PATH}}" ]; then \
        echo "==> libchdb already present at {{CHDB_INSTALL_PATH}} (delete to reinstall)"; \
        exit 0; \
    fi
    @os="$(uname -s)"; \
        arch="$(uname -m)"; \
        case "$os" in \
            Linux) \
                case "$arch" in \
                    aarch64|arm64) asset="linux-aarch64-libchdb.tar.gz" ;; \
                    *)             asset="linux-x86_64-libchdb.tar.gz" ;; \
                esac ;; \
            Darwin) \
                case "$arch" in \
                    arm64) asset="macos-arm64-libchdb.tar.gz" ;; \
                    *)     asset="macos-x86_64-libchdb.tar.gz" ;; \
                esac ;; \
            *) echo "unsupported platform: $os" >&2; exit 1 ;; \
        esac; \
        url="https://github.com/chdb-io/chdb/releases/download/{{CHDB_VERSION}}/$asset"; \
        echo "==> downloading $url"; \
        tmp="$(mktemp -d)"; \
        curl -fsSL -o "$tmp/libchdb.tar.gz" "$url"; \
        tar -C "$tmp" -xzf "$tmp/libchdb.tar.gz"; \
        echo "==> installing to {{CHDB_INSTALL_PATH}} (sudo may prompt)"; \
        sudo install -m 0755 "$tmp/libchdb.so" "{{CHDB_INSTALL_PATH}}"; \
        rm -rf "$tmp"; \
        echo "==> libchdb {{CHDB_VERSION}} installed"

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
    kubectl apply -k test/e2e/k3s/
    @echo "==> waiting for pods (up to 3 min)"
    kubectl -n cerberus wait --for=condition=Available deployment/clickhouse              --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/cerberus                --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/grafana                 --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/otel-collector-gateway  --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-traces       --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-metrics      --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-logs         --timeout=180s
    kubectl -n cerberus rollout status daemonset/otel-collector-agent                     --timeout=180s
    @echo "==> e2e-up done"
    @echo "    grafana:    http://localhost:3000 (admin/admin)"
    @echo "    cerberus:   http://localhost:8080/healthz"

# Ingest sample OTel data into ClickHouse. Runs the Go seed program at
# test/e2e/seed/cmd/seed/ which (a) applies the upstream OTel-CH DDL via
# internal/schema/ddl.Apply and (b) inserts the deterministic fixture rows.
# The DDL is the source of truth — the schema can no longer drift from the
# upstream exporter, unlike the previous hand-maintained *.sql scripts.
#
# Connects from the host via a transient kubectl port-forward; CH listens on
# port 9000 inside the cluster.
#
# Dual-data-source model (see test/e2e/k3s/README.md):
#   - `e2e-seed` inserts deterministic synthetic rows used by spec tests
#     that need exact values (e.g. `up` metric with known labels).
#   - The OTel collector DaemonSet+gateway+sample-app trio populates real
#     OTel data continuously for realistic Grafana smoke + dashboard tests.
# Both share the same `otel.*` tables (schema cannot drift — both write
# via the upstream sqltemplates).
e2e-seed:
    @echo "==> seeding OTel data via Go seeder"
    @kubectl -n cerberus port-forward svc/clickhouse 19000:9000 > /tmp/cerberus-e2e-seed-pf.log 2>&1 & \
        pf_pid=$!; \
        trap "kill $pf_pid 2>/dev/null || true" EXIT; \
        for i in 1 2 3 4 5 6 7 8 9 10; do \
            if nc -z 127.0.0.1 19000 2>/dev/null; then break; fi; \
            sleep 1; \
        done; \
        CH_ADDR=127.0.0.1:19000 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
            go run ./test/e2e/seed/cmd/seed
    @echo "==> seed done"

# Wait until the OTel collector has populated real data in every signal
# table (logs / traces / one of the metrics tables). Bootstraps the
# pipeline before tests rely on it — telemetrygen + kubeletstats take
# ~30-60s to flush a first batch through the gateway.
#
# Polls every 5s for up to 3 min; fails the recipe if any signal stays
# empty. Uses `kubectl exec` against the ClickHouse pod so it does not
# need a host-side port-forward.
e2e-wait-otel:
    @echo "==> waiting for real OTel data in ClickHouse"
    @deadline=$(($(date +%s) + 180)); \
        while [ $(date +%s) -lt $deadline ]; do \
            logs=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_logs" 2>/dev/null || echo 0); \
            traces=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_traces" 2>/dev/null || echo 0); \
            sum=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_sum" 2>/dev/null || echo 0); \
            gauge=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_gauge" 2>/dev/null || echo 0); \
            echo "    logs=$logs traces=$traces metrics_sum=$sum metrics_gauge=$gauge"; \
            if [ "$logs" -gt 0 ] && [ "$traces" -gt 0 ] && { [ "$sum" -gt 0 ] || [ "$gauge" -gt 0 ]; }; then \
                echo "==> OTel pipeline is live"; \
                exit 0; \
            fi; \
            sleep 5; \
        done; \
        echo "==> timeout waiting for OTel data"; \
        exit 1

# Run Go E2E HTTP tests against the deployed stack.
e2e-run:
    @echo "==> running Go E2E tests"
    go test -tags=e2e ./test/e2e/...

# Run the startup-speed benchmark: spawn cerberus and measure wall-clock
# time from process-start to first 200 OK on /healthz. Asserts < 2.5 s.
# Requires a reachable ClickHouse at $CH_ADDR (default 127.0.0.1:9000);
# the benchmark sets CERBERUS_AUTO_CREATE_SCHEMA=false so we measure
# pure HTTP-listener bootstrap and not DDL apply time.
#
# Override with CH_ADDR / CH_DATABASE / CH_USERNAME / CH_PASSWORD env
# vars; see test/e2e/startup_bench_test.go for the full list.
startup-bench:
    @echo "==> startup-speed benchmark (target < 2 s to /healthz)"
    go test -tags=startup_bench -v -count=1 -run TestStartupSpeed ./test/e2e/...

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



# Full lifecycle. Seed first (deterministic rows), then wait for the
# collector to populate real OTel data, then run the test matrix.
e2e: e2e-up e2e-seed e2e-wait-otel e2e-run e2e-playwright e2e-down

# === Compatibility (prometheus/compliance differential harness) ===

# Run the PromQL compatibility suite end-to-end. Slow; expect minutes.
# Sets up the Docker Compose stack (reference Prom + cerberus + CH + seeder),
# runs the upstream tester, writes harness/prometheus-compliance/report.json.
compatibility:
    ./harness/prometheus-compliance/scripts/run-compatibility.sh

# Keep the compatibility stack running after the tester finishes (for debugging).
compatibility-keep:
    COMPOSE_KEEP=1 ./harness/prometheus-compliance/scripts/run-compatibility.sh

# Tear down the compatibility stack manually.
compatibility-down:
    cd harness/prometheus-compliance && docker compose down -v

# === Compatibility (LogQL — Loki compatibility harness) ===

# Run the LogQL compatibility harness end-to-end. Brings up reference
# Loki + cerberus + ClickHouse, seeds both, builds the diff driver from
# the vendored upstream/loki-bench corpus, runs TestRemoteStorageEquality
# against both endpoints, writes harness/loki-compatibility/reports/diff.json.
# See harness/loki-compatibility/README.md + docs/loki-compliance-plan.md.
loki-compatibility:
    ./harness/loki-compatibility/scripts/run-loki-compatibility.sh

# Run the smoke (compose + seed + /labels assertion) without the diff
# driver. Useful when the seeder is the bisect target.
loki-compatibility-smoke:
    DRIVER_SKIP=1 ./harness/loki-compatibility/scripts/run-loki-compatibility.sh

# Keep the Loki compatibility stack running after the run finishes
# (for debugging /loki/api/v1/* + ClickHouse manually).
loki-compatibility-keep:
    COMPOSE_KEEP=1 ./harness/loki-compatibility/scripts/run-loki-compatibility.sh

# Tear down the Loki compatibility stack manually.
loki-compatibility-down:
    cd harness/loki-compatibility && docker compose down -v
# === Tempo / TraceQL compatibility harness ===

# Run the Tempo / TraceQL compatibility harness end-to-end. Slow; expect
# minutes. Sets up the Docker Compose stack (reference Tempo +
# cerberus + CH + seeder driver), runs the seeder which pushes a
# deterministic OTLP batch to Tempo and an equivalent INSERT into CH
# for cerberus, then smokes /api/traces/<id> on both backends.
# PR 3 of docs/tempo-compliance-plan.md — diff driver lands in PR 4.
tempo-compatibility:
    ./harness/tempo-compatibility/scripts/run-tempo-compatibility.sh

# Keep the tempo-compatibility stack running after the driver finishes (for debugging).
tempo-compatibility-keep:
    COMPOSE_KEEP=1 ./harness/tempo-compatibility/scripts/run-tempo-compatibility.sh

# Tear down the tempo-compatibility stack manually.
tempo-compatibility-down:
    cd harness/tempo-compatibility && docker compose down -v

# === Shadow-mode differential testing (RC3 R3.9) ===

# Build + run the shadow-mode harness against a corpus.
# Expects a running cerberus reachable at $CERBERUS_URL (default
# http://localhost:9090). The oracle is wired to internal/promshim/local
# and evaluates against a seeded in-memory dataset; native errors are
# expected when CERBERUS_URL points at nothing.
# See harness/prometheus-compliance/shadow/README.md.
shadow-mode CORPUS="harness/prometheus-compliance/shadow/corpus/smoke.txt" STRATEGY="prefer-native":
    @echo "==> building shadow-mode harness"
    go build -trimpath -o bin/shadow ./harness/prometheus-compliance/shadow/cmd/shadow
    @echo "==> running shadow-mode (strategy={{STRATEGY}})"
    ./bin/shadow \
        --corpus {{CORPUS}} \
        --strategy {{STRATEGY}} \
        --cerberus-url "${CERBERUS_URL:-http://localhost:9090}" \
        --report shadow-report.json
