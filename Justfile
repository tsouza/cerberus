# Cerberus task runner. All commands go through `just`.
# Run `just` for the full recipe list.

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

GOLANGCI_LINT_VERSION := "v2.12.2"
GOFUMPT_VERSION := "v0.7.0"
GOIMPORTS_VERSION := "latest"
GREMLINS_VERSION := "v0.6.0"
MARKDOWNLINT_VERSION := "v0.18.1"
ACTIONLINT_VERSION := "v1.7.12"
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
    go install github.com/rhysd/actionlint/cmd/actionlint@{{ACTIONLINT_VERSION}}

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
# chclienttest package itself and the consumer-corpus replay lane
# (test/consumer-corpus — captured Grafana request shapes executed
# through chDB seeds). Same prerequisite as spec-chdb (libchdb at the
# default install path). Mirrors the `chdb` CI job.
test-chdb:
    go test -tags chdb -count=1 ./internal/chclienttest/... ./internal/api/... ./test/consumer-corpus/...

# Run the chDB-tagged property tests (rapid + from-scratch oracle).
# Requires libchdb.so (see `just chdb-install`). Local default is rapid's
# 100 iterations; the nightly `property` CI workflow overrides to 500.
property:
    go test -tags chdb -count=1 ./test/property/...

# Run the chDB-tagged perf regression guards (test/perf). These are
# deterministic ASSERTION pins — not wall-clock benchmarks — that bite a
# regression of the landed perf wins: the metrics-table MetricName-first
# ORDER BY granule prune (EXPLAIN indexes=1 ratio floor) and the /series
# fan-out round-trip baseline. Requires libchdb.so (see `just chdb-install`).
# Mirrors the `perf-guards` CI job in chdb.yml. Distinct from the
# informational `perf-benchmark.yml` lane, which only reports benchstat
# deltas and never gates.
perf-chdb:
    go test -tags chdb -count=1 ./test/perf/...

# Profile the WHOLE TXTAR corpus for compute fan-out (perf-assessment
# Component B). Walks every executable fixture under test/spec/** (those
# with `seed:` + `expected_rows:` + `sql:`) and, per fixture, runs
# EXPLAIN PLAN actions=1 (CROSS JOIN / ARRAY JOIN / recursive-CTE
# detection) + a per-subquery-level count() decomposition (peak
# intermediate cardinality vs leaf scan rows) in-process via chDB.
# Writes a JSON profile array and prints the top fan_factor fixtures.
# Requires libchdb.so (see `just chdb-install`). Drives the nightly
# perf-profile.yml lane — NOT a per-PR gate (corpus-wide breadth over
# ~640 fixtures is too heavy for every PR). Override OUT / TOP to taste.
perf-profile OUT="perf-profile.json" TOP="40":
    go run -tags chdb ./cmd/perf-profile -spec test/spec -out {{OUT}} -top {{TOP}}

# Run the chclient testcontainers integration tests against a real
# ClickHouse container. Requires Docker. Gated behind the `integration`
# build tag so regular `just test` doesn't pull in Docker.
chclient-integration:
    go test -race -tags=integration ./internal/chclient/...

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
# total + a per-package summary sorted by coverage.
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
# Two lanes: the default-tag pass rewrites `-- sql --` / `-- chplan --`
# text goldens, then a chdb-tagged pass (mirroring `just spec-chdb`)
# rewrites the `-- expected_rows --` round-trip cells, which only execute
# under that build tag. Requires libchdb.so (`just chdb-install`) — the
# recipe fails fast without it rather than leaving stale expected_rows
# behind (the PR #758 failure mode).
# Review `git diff test/spec/` before committing.
update-golden:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; without it the chdb-tagged -- expected_rows -- sections cannot regenerate and go stale" >&2; exit 1; }
    GOLDEN_UPDATE=1 go test ./...
    GOLDEN_UPDATE=1 go test -tags chdb -count=1 ./test/spec/...
    @echo
    @echo "Diff of regenerated fixtures:"
    @git --no-pager diff --stat test/spec/ || true

# Regenerate the cardinality/fan-factor ratchet baseline (perf-assessment
# Component C) from the current corpus profile. Re-profiles every executable
# TXTAR fixture under test/spec/** in-process via chDB and rewrites
# test/perf/cardinality-baseline.json (deterministic, sorted by fixture).
# Requires libchdb.so (`just chdb-install`). Run this — and review the diff —
# whenever the ratchet test reports a NEW/REMOVED fixture or a deliberately
# intended fan_factor change; the diff is the built-in cost review (it shows
# each construct's absolute fan_factor). The gating assertion is
# TestCardinalityRatchet in the already-required `perf-guards` job.
update-cardinality-baseline:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; the ratchet baseline is generated by an in-process chDB profile pass" >&2; exit 1; }
    UPDATE_CARDINALITY_BASELINE=1 go test -tags chdb -count=1 -run TestCardinalityRatchet ./test/perf/
    @echo
    @echo "Diff of regenerated baseline:"
    @git --no-pager diff --stat test/perf/cardinality-baseline.json || true

# Regenerate the routing-DECISION ratchet baseline (perf-assessment
# Component D) from the current PromQL corpus. Parses every `-- query.promql --`
# fixture under test/spec/promql/**, lowers it on the fixed eval grid
# (end=2026-01-01T00:00:00Z, range=1h, step=15s), optimizes it, and records the
# solver Planner's routing decision {routed, K, reason} under Mode=auto into
# test/perf/solver-decision-baseline.json (deterministic, sorted by query).
# Pure Go — NO chDB — so it runs in the standard `check`/`just test` lane.
# Run this — and REVIEW THE DIFF — whenever the ratchet test reports drift or a
# NEW/REMOVED query. The diff classifies each moved row as ADVANCEMENT vs
# REGRESSION; a REGRESSION (route B->A, K down, or a routed query now rejected)
# MUST be justified in the PR with a real reason (a correctness fix that
# disqualifies the query), never accepted as a silent relaxation. The gating
# assertion is TestSolverDecisionRatchet in the already-required `check` job.
update-solver-decision-baseline:
    UPDATE_SOLVER_DECISION_BASELINE=1 go test -count=1 -run TestSolverDecisionRatchet ./test/perf/
    @echo
    @echo "Diff of regenerated baseline:"
    @git --no-pager diff --stat test/perf/solver-decision-baseline.json || true

# Regenerate the SCALE-WALL pin baseline — the perf guard for the wall /
# scan-amplification regression classes the cardinality ratchet is blind to
# (it pins fan_factor only, so #97's 6x CPU-bound wall regression and the
# anchor-grid sharding's 8x scan amplification both sailed through it). Seeds
# a counter table at scale, lowers `sum(rate(http_requests_total[5m]))` on a
# 1h/15s query_range grid through the real lower -> optimizer -> emit chain,
# and records two bounds into test/perf/scale-wall-baseline.json: the
# deterministic peak-intermediate/scan-rows amplification ceiling (PRONG 1)
# and the in-run query/yardstick wall ratio ceiling (PRONG 2). Both carry
# headroom over the measured floor (1.5x / 2.5x). Requires libchdb.so
# (`just chdb-install`). Run this — and REVIEW THE DIFF — only when a bound
# move is genuinely intended (a real, justified compute-cost increase); a
# silent loosen is exactly the regression the pin exists to catch. The gating
# assertion is TestScaleWallPin in the already-required `perf-guards` job.
update-scale-wall-baseline:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; the scale-wall bounds are measured by an in-process chDB run" >&2; exit 1; }
    UPDATE_SCALE_WALL_BASELINE=1 go test -tags chdb -count=1 -run TestScaleWallPin ./test/perf/
    @echo
    @echo "Diff of regenerated baseline:"
    @git --no-pager diff --stat test/perf/scale-wall-baseline.json || true

# Regenerate the publishable benchmark document (docs/benchmarks.md) from
# LIVE measurements: optimizer before/after wins, per-construct scaling
# curves, per-stage Go micro-benchmarks, and end-to-end query latency on a
# large synthetic dataset (millions of rows generated server-side via
# numbers(N)). The optimized SQL shapes are driven through the real
# cerberus lowering pipeline (internal/{promql,logql,traceql} -> chsql),
# so the measured SQL is the SQL cerberus emits.
#
# Requires libchdb.so (`just chdb-install`) — all measurements run
# in-process against an embedded chDB engine. This is a MANUALLY-run
# artifact, NOT a CI gate: structural metrics (fan_factor, granules,
# allocs/op) are deterministic and committed; timings are presented as
# speedup ratios + labelled indicative, so re-running yields a clean diff
# on the deterministic parts. Re-run whenever perf improves; review the
# diff before committing.
bench-report:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; the benchmark document is generated by an in-process chDB measurement pass" >&2; exit 1; }
    go run -tags chdb ./cmd/bench-report -out docs/benchmarks.md
    @echo
    @echo "Diff of regenerated benchmark document:"
    @git --no-pager diff --stat docs/benchmarks.md || true

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

# Validate GitHub Actions workflow files (expression contexts, action
# inputs, shellcheck of run blocks). Deliberately separate from `lint`
# (Go): workflow-file defects otherwise surface only as server-side
# zero-job "invalid workflow file" failure runs, which silently
# prevent required pull_request checks from ever being scheduled —
# the #749 secrets-in-step-if incident left the PR BLOCKED on four
# required contexts that could never report.
lint-actions:
    actionlint

# Lint all Markdown files (run via npm exec; no global Node deps).
lint-md:
    npm exec --yes -- markdownlint-cli2@{{MARKDOWNLINT_VERSION}} "**/*.md" "!compatibility/prometheus/upstream/**" "!**/node_modules/**"

# Auto-fix Markdown lint issues where possible.
fmt-md:
    npm exec --yes -- markdownlint-cli2@{{MARKDOWNLINT_VERSION}} --fix "**/*.md" "!compatibility/prometheus/upstream/**" "!**/node_modules/**"

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

# External images referenced by test/e2e/k3s/*.yaml. Kept in sync with the
# manifests by convention; a stale entry surfaces as a `Pending` /
# `ImagePullBackOff` pod once that image is no longer pre-loaded. When you
# bump a version in a manifest, bump it here too — both sides MUST agree.
#
# Why pre-pull: a fresh k3d cluster's containerd hits the registry directly
# (DockerHub + GHCR). DockerHub's anonymous-pull rate limit is shared across
# GHA-runner IP pools and intermittently fires at ~1/20 e2e runs, leaving
# ClickHouse stuck in `ImagePullBackOff` past the 180 s deployment wait. By
# pulling on the host docker daemon (which has its own auth + cache) and
# importing into k3d via the API, we never go through containerd's pull
# path. See run 26136032208 for the symptom.
# MUST stay in lock-step with the image pins in test/e2e/k3s/*.yaml —
# a stale entry here means the pod pulls straight from the registry at
# start-up (no pre-pull, no import, full Docker-Hub-flake exposure).
E2E_EXTERNAL_IMAGES := "clickhouse/clickhouse-server:25.8-alpine ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.116.0 grafana/grafana:12.2.9 otel/opentelemetry-collector-contrib:0.152.1"

# Extra args appended verbatim to `k3d cluster create` in `e2e-up`. Empty by
# default (CI uses none). Interpolated unquoted, so the value is shell-parsed:
# wrap any single arg containing shell metacharacters (`<`, `%`, `,`) in
# single quotes INSIDE the value.
#
# The motivating use case is dev hosts low on disk: k3s kubelet's default
# eviction thresholds (nodefs.available<10%, imagefs.available<15%) taint the
# node with disk-pressure and nothing schedules. For a throwaway local
# cluster, disable eviction:
#
#   K3D_EXTRA_ARGS="--k3s-arg '--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:0'" just e2e-up
K3D_EXTRA_ARGS := env_var_or_default("K3D_EXTRA_ARGS", "")

# Boot the k3d cluster, build cerberus image, import it, apply manifests, wait for pods.
# Host ports map via the k3d loadbalancer to NodePorts on the k3s nodes:
#   host:8080 -> LB -> NodePort 30080 (cerberus svc)
#   host:3000 -> LB -> NodePort 30030 (grafana svc)
# The k3d loadbalancer publishes on 0.0.0.0, so both ports are reachable on
# every host interface (LAN IP included), not just localhost.
e2e-up: e2e-down
    @echo "==> creating k3d cluster {{K3D_CLUSTER}}"
    k3d cluster create {{K3D_CLUSTER}} \
        --port "3000:30030@loadbalancer" \
        --port "8080:30080@loadbalancer" \
        --no-lb=false \
        --k3s-arg "--disable=traefik@server:0" \
        {{K3D_EXTRA_ARGS}} \
        --wait
    @echo "==> building cerberus image"
    docker build -t {{CERBERUS_IMAGE}} -f Dockerfile.local .
    @echo "==> pre-pulling external images on host docker"
    @for img in {{E2E_EXTERNAL_IMAGES}}; do \
        echo "    docker pull $img"; \
        docker pull "$img" >/dev/null || { echo "ERROR: docker pull $img failed" >&2; exit 1; }; \
    done
    @echo "==> importing images into k3d ({{K3D_CLUSTER}})"
    k3d image import {{CERBERUS_IMAGE}} {{E2E_EXTERNAL_IMAGES}} -c {{K3D_CLUSTER}}
    @echo "==> verifying images landed in the k3d node's containerd"
    @# `k3d image import` exits 0 on partial/silent failures (observed:
    @# run 27274975563 — cerberus pods in ImagePullBackOff trying
    @# docker.io/library/cerberus:e2e because the local-only image never
    @# reached the node). Verify every image is actually present;
    @# normalise short names the way containerd stores them
    @# (docker.io/ + library/ prefixes).
    @for img in {{CERBERUS_IMAGE}} {{E2E_EXTERNAL_IMAGES}}; do \
        ref="$img"; \
        case "$ref" in \
            *.*/*|*:*/*) ;; \
            */*) ref="docker.io/$ref" ;; \
            *)   ref="docker.io/library/$ref" ;; \
        esac; \
        if ! docker exec k3d-{{K3D_CLUSTER}}-server-0 ctr -n k8s.io images ls -q | grep -qF "$ref"; then \
            echo "ERROR: $ref missing from k3d node containerd after import" >&2; \
            exit 1; \
        fi; \
        echo "    ok $ref"; \
    done
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

# One-shot RE-seed through a fresh, self-contained port-forward — used by the
# chaos lane's heal step after a CH-destructive scenario recreates ClickHouse
# EMPTY (test/e2e/k3s/clickhouse.yaml: `strategy: Recreate`, no volumes ⇒
# container-ephemeral storage). The rolling seeder's long-lived port-forward
# (`e2e-seed-rolling`) is bound to a single backing pod, so `ch-pod-kill`
# breaks that tunnel and it never reconnects — the rolling seeder then writes
# into a dead socket for the rest of the run and CH stays empty, so every
# downstream scenario fails with `code:60 Unknown table … otel_*`. This recipe
# stands up its OWN throwaway forward on a DISTINCT local port (so it never
# races the rolling forward's 19000 socket), re-applies the idempotent
# OTel-CH DDL, re-inserts every fixture, verifies the rowcounts, and tears the
# forward down — exactly the one-shot the seeder already performs on boot, so
# a freshly-recreated empty CH is repopulated before the next scenario asserts.
# Idempotent + re-runnable: the seeder's DDL is CREATE … IF NOT EXISTS and the
# INSERTs re-anchor on now64(9), so running it against either an empty or an
# already-populated CH is safe.
#
# One-shot re-seed of a recreated (empty) ClickHouse for the chaos heal step.
e2e-reseed:
    @echo "==> re-seeding OTel data (one-shot, fresh port-forward) after CH recreation"
    @kubectl -n cerberus port-forward svc/clickhouse 19001:9000 > /tmp/cerberus-e2e-reseed-pf.log 2>&1 & \
        pf_pid=$!; \
        trap "kill $pf_pid 2>/dev/null || true" EXIT; \
        for i in 1 2 3 4 5 6 7 8 9 10; do \
            if nc -z 127.0.0.1 19001 2>/dev/null; then break; fi; \
            sleep 1; \
        done; \
        CH_ADDR=127.0.0.1:19001 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
            go run ./test/e2e/seed/cmd/seed
    @echo "==> re-seed done"

# Rolling seeder. Performs the same INSERTs as `e2e-seed` and then stays
# alive, re-anchoring the metric/log windows on now64(9) every 30 s until
# stopped via `just e2e-seed-stop` (or SIGTERM). Replaces the static-window
# arms-race that widened the seed envelope to ±15 min in PRs #590 / #615 /
# #617 / #693 just to survive the ~12 min Playwright suite drift — with
# fresh data arriving continuously the static window only has to cover
# the 30 s gap between two ticks plus the 5 m Prom/Loki staleness lookback.
#
# Background pattern: a single bash command builds + launches the seeder
# under nohup, dissociated from this just-recipe shell (so the recipe
# returns and the next CI step can run while the seeder keeps reseeding).
# PID files at /tmp/cerberus-e2e-seed-*.pid let `e2e-seed-stop` find the
# processes for clean teardown. Logs land at /tmp/cerberus-e2e-seed-rolling.log
# so a failing tick is visible in CI artefacts.
e2e-seed-rolling:
    @echo "==> launching rolling seeder (30s tick) in background"
    @# 1) start a long-lived port-forward and stash its PID.
    @kubectl -n cerberus port-forward svc/clickhouse 19000:9000 > /tmp/cerberus-e2e-seed-pf.log 2>&1 & \
        echo $! > /tmp/cerberus-e2e-seed-pf.pid
    @# 2) wait for the forward to come up.
    @for i in 1 2 3 4 5 6 7 8 9 10; do \
        if nc -z 127.0.0.1 19000 2>/dev/null; then break; fi; \
        sleep 1; \
    done
    @# 3) build the seeder once so `nohup` launches a real binary
    @#    (a stray `go run` keeps the toolchain attached to the shell that
    @#    spawned it; harder to detach cleanly across CI step boundaries).
    @go build -o /tmp/cerberus-e2e-seeder ./test/e2e/seed/cmd/seed
    @# 4) launch the seeder under nohup with the rolling flag.
    @CH_ADDR=127.0.0.1:19000 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
        nohup /tmp/cerberus-e2e-seeder --re-seed-interval=30s \
            > /tmp/cerberus-e2e-seed-rolling.log 2>&1 & \
        echo $! > /tmp/cerberus-e2e-seed-rolling.pid
    @echo "==> rolling seeder pid=$(cat /tmp/cerberus-e2e-seed-rolling.pid) pf-pid=$(cat /tmp/cerberus-e2e-seed-pf.pid)"
    @echo "    initial seed runs synchronously inside the seeder before the loop starts —"
    @echo "    tail /tmp/cerberus-e2e-seed-rolling.log to confirm 'seed: done' lands."
    @# 5) wait for the initial seed to land before returning, so the next
    @#    CI step (e2e-wait-otel / e2e-run) sees a populated database.
    @for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
        if grep -q '^.*seed: done' /tmp/cerberus-e2e-seed-rolling.log 2>/dev/null; then \
            echo "==> initial seed landed"; \
            exit 0; \
        fi; \
        sleep 2; \
    done; \
        echo "==> ERROR: initial seed did not complete within 30s. log:"; \
        cat /tmp/cerberus-e2e-seed-rolling.log; \
        exit 1

# Stop the rolling seeder + its port-forward (idempotent). Called from
# CI teardown so the dashboard job tears down cleanly even when the
# Playwright step failed before reaching `e2e-down`. SIGTERM gives the
# seeder a chance to log the exit reason; the port-forward never has any
# state to flush.
e2e-seed-stop:
    @echo "==> stopping rolling seeder"
    @if [ -f /tmp/cerberus-e2e-seed-rolling.pid ]; then \
        pid=$(cat /tmp/cerberus-e2e-seed-rolling.pid); \
        kill -TERM "$pid" 2>/dev/null || true; \
        rm -f /tmp/cerberus-e2e-seed-rolling.pid; \
    fi
    @if [ -f /tmp/cerberus-e2e-seed-pf.pid ]; then \
        pid=$(cat /tmp/cerberus-e2e-seed-pf.pid); \
        kill -TERM "$pid" 2>/dev/null || true; \
        rm -f /tmp/cerberus-e2e-seed-pf.pid; \
    fi

# Wait until the OTel collector has populated real data in every signal
# table (logs / traces / one of the metrics tables) AND the metric table
# carries ≥60 s of history. Bootstraps the pipeline before tests rely on
# it — telemetrygen + kubeletstats take ~30-60s to flush a first batch
# through the gateway, and 1m-windowed queries (rate(x[1m]), up[1m:30s])
# need a 60s span of TimeUnix values before they return a vector.
#
# Polls every 5s for up to 3 min; fails the recipe if any signal stays
# empty or the metric stream never reaches 60 s of spread. Uses
# `kubectl exec` against the ClickHouse pod so it does not need a
# host-side port-forward. Spread is asserted on whichever metric table
# (sum or gauge) carries non-zero rows first.
e2e-wait-otel:
    @echo "==> waiting for real OTel data (incl. clickhouse query_log stream) + ≥60s metric history in ClickHouse"
    @deadline=$(($(date +%s) + 180)); \
        while [ $(date +%s) -lt $deadline ]; do \
            logs=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_logs" 2>/dev/null || echo 0); \
            chlogs=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_logs WHERE ServiceName = 'clickhouse'" 2>/dev/null || echo 0); \
            traces=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_traces" 2>/dev/null || echo 0); \
            sum=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_sum" 2>/dev/null || echo 0); \
            gauge=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_gauge" 2>/dev/null || echo 0); \
            histogram=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_histogram WHERE MetricName = 'http_server_request_duration'" 2>/dev/null || echo 0); \
            hist_spread=0; \
            if [ "$histogram" -gt 0 ]; then \
                hist_spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_histogram WHERE MetricName = 'http_server_request_duration'" 2>/dev/null || echo 0); \
            fi; \
            spread=0; \
            if [ "$sum" -gt 0 ]; then \
                spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_sum" 2>/dev/null || echo 0); \
            elif [ "$gauge" -gt 0 ]; then \
                spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_gauge" 2>/dev/null || echo 0); \
            fi; \
            echo "    logs=$logs chlogs=$chlogs traces=$traces metrics_sum=$sum metrics_gauge=$gauge metrics_histogram=$histogram spread=${spread}s hist_spread=${hist_spread}s"; \
            if [ "$logs" -gt 0 ] && [ "$chlogs" -gt 0 ] && [ "$traces" -gt 0 ] && { [ "$sum" -gt 0 ] || [ "$gauge" -gt 0 ]; } && [ "$spread" -ge 60 ] && [ "$histogram" -gt 0 ] && [ "$hist_spread" -ge 60 ]; then \
                echo "==> OTel pipeline is live with ≥60s of metric history (incl. histogram companion + clickhouse query_log stream)"; \
                exit 0; \
            fi; \
            sleep 5; \
        done; \
        echo "==> timeout waiting for OTel data / metric history span"; \
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

# Apply the chaos overlay onto the running cerberus Deployment: the
# resilience knobs in test/e2e/chaos/manifests/chaos-overlay.env (low
# breaker threshold + small CERBERUS_QUERY_TIMEOUT + small admit/pool
# caps) so the live-stack chaos faults trip FAST + DETERMINISTICALLY
# within budget. Patches the Deployment's pod env via `kubectl set env`
# (one rollout), then waits for it to roll out so every cerberus pod
# carries the overlay before fault injection. Idempotent — re-applying
# the same env values is a no-op rollout.
e2e-chaos-overlay:
    @echo "==> applying chaos overlay (resilience knobs) to deploy/cerberus"
    @env_args=""; \
        while IFS= read -r line; do \
            case "$line" in ''|\#*) continue ;; esac; \
            env_args="$env_args $line"; \
        done < test/e2e/chaos/manifests/chaos-overlay.env; \
        echo "    kubectl set env deploy/cerberus$env_args"; \
        kubectl -n cerberus set env deployment/cerberus $env_args
    @echo "==> waiting for the overlay rollout"
    kubectl -n cerberus rollout status deployment/cerberus --timeout=120s

# Run the live-stack chaos lane: fault-inject against the running k3d
# stack and assert the gateway's resilience contracts (circuit breaker,
# per-query wall-clock timeout, admission control, replica resilience)
# hold under REAL faults. Drives .github/scripts/chaos-run.mjs (node ESM,
# kubectl + fetch). Assumes `e2e-up` + `e2e-seed-rolling` + `e2e-wait-otel`
# already ran AND the chaos overlay is applied (`e2e-chaos-overlay`).
# CHAOS_PHASE=phase-1 by default (ch-pod-kill, ch-slow/query-timeout,
# cerberus-pod-kill); CHAOS_PHASE=all adds the phase-2 scenarios.
# Mirrors the `e2e-run` / `e2e-playwright` shape — locally reproducible.
e2e-chaos:
    @echo "==> running live-stack chaos lane (chaos-run.mjs)"
    CERBERUS_URL=http://localhost:8080 \
        CHAOS_PHASE="${CHAOS_PHASE:-phase-1}" \
        node .github/scripts/chaos-run.mjs

# Tear down the cluster. Also stops the rolling seeder + port-forward
# if either was started by `e2e-seed-rolling` — idempotent and silent
# when the PID files don't exist.
e2e-down:
    @if [ -f /tmp/cerberus-e2e-seed-rolling.pid ] || [ -f /tmp/cerberus-e2e-seed-pf.pid ]; then \
        just e2e-seed-stop; \
    fi
    @if k3d cluster list | grep -q "^{{K3D_CLUSTER}} "; then \
        echo "==> deleting k3d cluster {{K3D_CLUSTER}}"; \
        k3d cluster delete {{K3D_CLUSTER}}; \
    fi



# Full lifecycle. Seed first (deterministic rows, rolling so the window
# slides with wall-clock now), then wait for the collector to populate
# real OTel data, then run the test matrix. `e2e-down` stops the rolling
# seeder on teardown.
e2e: e2e-up e2e-seed-rolling e2e-wait-otel e2e-run e2e-playwright e2e-down

# Run the compose-stack Grafana catch-net spec locally. Assumes the
# quickstart compose stack is already up (`docker compose up --wait`).
# Drives Grafana through every provisioned dashboard and asserts every
# /api/ds/query + /api/dashboards/* response is 2xx with no tunneled
# per-target error. Mirrors the Playwright step the compose-smoke CI
# job runs.
compose-grafana-smoke:
    @echo "==> compose-grafana-smoke playwright catch-net"
    cd test/e2e/playwright && \
        ( [ -f package-lock.json ] && npm ci || npm install --no-audit --no-fund ) && \
        npx playwright install --with-deps chromium && \
        GRAFANA_BASE_URL=http://localhost:3000 \
        GRAFANA_URL=http://localhost:3000 \
        CERBERUS_URL=http://localhost:8080 \
        npx playwright test compose_grafana_smoke.spec.ts --reporter=list

# === Compatibility (prometheus/compliance differential harness) ===

# Run the PromQL compatibility suite end-to-end. Slow; expect minutes.
# Sets up the Docker Compose stack (reference Prom + cerberus + CH + seeder),
# runs the upstream tester, writes compatibility/prometheus/report.json.
compat-promql:
    ./compatibility/prometheus/scripts/run-compatibility.sh

# Keep the compatibility stack running after the tester finishes (for debugging).
compat-promql-keep:
    COMPOSE_KEEP=1 ./compatibility/prometheus/scripts/run-compatibility.sh

# Tear down the compatibility stack manually.
compat-promql-down:
    cd compatibility/prometheus && docker compose down -v

# === Compatibility (LogQL — Loki compatibility harness) ===

# Run the LogQL compatibility harness end-to-end. Brings up reference
# Loki + cerberus + ClickHouse, seeds both, builds the diff driver from
# the vendored upstream/loki-bench corpus, runs TestRemoteStorageEquality
# against both endpoints, writes compatibility/loki/reports/diff.json.
# See compatibility/loki/README.md for the harness layout.
compat-logql:
    ./compatibility/loki/scripts/run-loki-compatibility.sh

# Run the smoke (compose + seed + /labels assertion) without the diff
# driver. Useful when the seeder is the bisect target.
compat-logql-smoke:
    DRIVER_SKIP=1 ./compatibility/loki/scripts/run-loki-compatibility.sh

# Keep the Loki compatibility stack running after the run finishes
# (for debugging /loki/api/v1/* + ClickHouse manually).
compat-logql-keep:
    COMPOSE_KEEP=1 ./compatibility/loki/scripts/run-loki-compatibility.sh

# Tear down the Loki compatibility stack manually.
compat-logql-down:
    cd compatibility/loki && docker compose down -v
# === Tempo / TraceQL compatibility harness ===

# Run the Tempo / TraceQL compatibility harness end-to-end. Slow; expect
# minutes. Sets up the Docker Compose stack (reference Tempo +
# cerberus + CH + seeder driver), runs the seeder which pushes a
# deterministic OTLP batch to Tempo and an equivalent INSERT into CH
# for cerberus, then runs the differ over the TXTAR corpus.
compat-traceql:
    ./compatibility/tempo/scripts/run-tempo-compatibility.sh

# Keep the tempo-compatibility stack running after the driver finishes (for debugging).
compat-traceql-keep:
    COMPOSE_KEEP=1 ./compatibility/tempo/scripts/run-tempo-compatibility.sh

# Tear down the tempo-compatibility stack manually.
compat-traceql-down:
    cd compatibility/tempo && docker compose down -v

# === Compatibility — all three heads ===

# Run all three compatibility harnesses sequentially: PromQL
# (prometheus/compliance), LogQL (vendored grafana/loki:pkg/logql/bench
# + cerberus-owned tester), TraceQL (TXTAR corpus + cerberus-owned
# differ over /api/search + tags + metrics endpoints). Each sub-recipe
# tears its own compose stack down on every exit path.
# Slow — expect tens of minutes. Use the per-head recipes for iteration.
# Exit semantics: fails fast on the first non-zero recipe; the report
# files for each head land under compatibility/*/reports/ regardless.
compat-all: compat-promql compat-logql compat-traceql
