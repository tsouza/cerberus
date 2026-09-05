# === E2E BWC (bundled ClickHouse on object storage, MinIO-backed) ===
#
# Additive sibling of the `e2e-up` lane that proves the Helm chart's
# `clickhouse.bundled.enabled=true` data tier on REAL object storage:
# cerberus + a chart-deployed ClickHouse StatefulSet whose MergeTree storage
# policy targets an in-cluster MinIO bucket. Same k3d cluster name, same
# cerberus:e2e image, same otel.* tables on clickhouse:9000 — so the seed +
# Go-test recipes (`e2e-seed-rolling`, `e2e-run`) are REUSED UNCHANGED.
#
# Ordering is load-bearing:
#   1. MinIO + the bucket-create Job come up FIRST — a ClickHouse `s3` disk does
#      not create its bucket, so it must exist before CH starts (gotcha #3).
#   2. `helm install` then brings up the bundled CH + cerberus. cerberus runs
#      its auto-create DDL stamping storage_policy=bwc_object_store BEFORE
#      serving, and `--wait` blocks until that is done.
#   3. ONLY THEN do the otel collector / grafana / sample-app fixtures land — so
#      the collector's clickhouseexporter (create_schema=true) can never win a
#      race to create the otel tables UNSTAMPED on the local disk; it finds
#      cerberus's stamped tables already present and its CREATE … IF NOT EXISTS
#      is a no-op.

# `scenario` selects the storage-tiering overlay layered on top of the base
# `cerberus-values-bwc.yaml` (cerberus issue #3075):
#   object-storage — the chart's PRE-#3075 mode (hotVolume.enabled=false +
#                    objectStorage.enabled=true), unchanged.
#   hot-only       — clickhouse.bundled.hotVolume.enabled=true +
#                    objectStorage.enabled=false + an explicit schema.ttl.
#   hot-cold       — the chart's DEFAULT mode: hotVolume.enabled=true,
#                    objectStorage stays on.
# MinIO + the bucket-create Job still come up in every scenario (shared setup,
# per the issue's "extend the SAME lane" design) even though hot-only never
# touches them.

# Boot the k3d stack with the chart's bundled ClickHouse for the given scenario.
e2e-bwc-up scenario="object-storage": e2e-down
    @echo "==> [bwc] pre-pulling k3s node image (retry — Docker Hub flaky from CI)"
    @just _pull-retry {{K3S_IMAGE}}
    @echo "==> [bwc] creating k3d cluster {{K3D_CLUSTER}}"
    k3d cluster create {{K3D_CLUSTER}} \
        --image {{K3S_IMAGE}} \
        --port "3000:30030@loadbalancer" \
        --port "8080:30080@loadbalancer" \
        --no-lb=false \
        --k3s-arg "--disable=traefik@server:0" \
        {{K3D_EXTRA_ARGS}} \
        --wait
    @echo "==> [bwc] building cerberus image (build tags: '{{CERBERUS_BUILD_TAGS}}')"
    node .github/scripts/build-with-registry-retry.mjs \
        docker build -t {{CERBERUS_IMAGE}} --build-arg GO_BUILD_TAGS="{{CERBERUS_BUILD_TAGS}}" --build-arg GO_IMAGE -f Dockerfile.local .
    @echo "==> [bwc] pre-pulling external + bwc images on host docker"
    @# The standalone-CH image (the *-alpine tag in E2E_EXTERNAL_IMAGES) backs
    @# test/e2e/k3s/clickhouse.yaml, which the bwc kustomization EXCLUDES — this
    @# lane runs the chart's BUNDLED ClickHouse (the non-alpine tag in
    @# E2E_BWC_IMAGES) instead, so skip importing the unused standalone image.
    @for img in {{E2E_EXTERNAL_IMAGES}} {{E2E_BWC_IMAGES}}; do \
        case "$img" in clickhouse/clickhouse-server:*-alpine) continue ;; esac; \
        just _pull-retry "$img"; \
    done
    @# Import each image individually + VERIFY it landed in the node's
    @# containerd (k3d image import can print success while the node import
    @# silently failed -> ImagePullBackOff with pullPolicy:Never). Same robust
    @# loop as `e2e-up`, extended with the bwc image set.
    @for img in {{CERBERUS_IMAGE}} {{E2E_EXTERNAL_IMAGES}} {{E2E_BWC_IMAGES}}; do \
        case "$img" in clickhouse/clickhouse-server:*-alpine) continue ;; esac; \
        ref="$img"; \
        case "$ref" in \
            *.*/*|*:*/*) ;; \
            */*) ref="docker.io/$ref" ;; \
            *) ref="docker.io/library/$ref" ;; \
        esac; \
        landed=0; \
        for attempt in 1 2 3 4 5; do \
            k3d image import "$img" -c {{K3D_CLUSTER}} || true; \
            if docker exec k3d-{{K3D_CLUSTER}}-server-0 ctr -n k8s.io images ls -q | grep -qF "$ref"; then \
                landed=1; break; \
            fi; \
            echo "  import attempt $attempt: $ref not in containerd yet, retrying with backoff" >&2; \
            sleep $((attempt * 2)); \
        done; \
        if [ "$landed" != "1" ]; then \
            echo "ERROR: $ref missing from k3d node containerd after 5 import attempts" >&2; \
            exit 1; \
        fi; \
    done
    @echo "==> [bwc] phase 1: namespace + MinIO + bucket-create Job (before ClickHouse)"
    kubectl apply -f test/e2e/k3s/namespace.yaml
    kubectl apply -f test/e2e/k3s-bwc/minio.yaml -f test/e2e/k3s-bwc/bucket-job.yaml
    @echo "==> [bwc] waiting for MinIO"
    kubectl -n cerberus rollout status deployment/minio --timeout=120s
    @echo "==> [bwc] waiting for the bucket-create Job to complete"
    kubectl -n cerberus wait --for=condition=complete job/minio-create-bucket --timeout=120s
    @echo "==> [bwc] phase 2: installing cerberus + bundled ClickHouse via Helm (scenario: {{scenario}})"
    @case "{{scenario}}" in \
        object-storage) overlay="" ;; \
        hot-only) overlay="--values test/e2e/k3s/cerberus-values-bwc-hot-only.yaml" ;; \
        hot-cold) overlay="--values test/e2e/k3s/cerberus-values-bwc-hot-cold.yaml" ;; \
        *) echo "ERROR: unknown scenario '{{scenario}}' (expected object-storage|hot-only|hot-cold)" >&2; exit 1 ;; \
    esac; \
    helm upgrade --install cerberus deploy/helm/cerberus \
        --namespace cerberus \
        --values test/e2e/k3s/cerberus-values.yaml \
        --values test/e2e/k3s/cerberus-values-bwc.yaml \
        $overlay \
        --wait --timeout 360s
    @echo "==> [bwc] waiting for bundled ClickHouse StatefulSet + cerberus"
    kubectl -n cerberus rollout status statefulset/cerberus-clickhouse --timeout=300s
    kubectl -n cerberus rollout status deployment/cerberus --timeout=180s
    @echo "==> [bwc] phase 3: applying grafana / collector / sample-app fixtures + clickhouse alias"
    kubectl kustomize --load-restrictor=LoadRestrictionsNone test/e2e/k3s-bwc | kubectl apply -f -
    @echo "==> [bwc] waiting for grafana"
    kubectl -n cerberus rollout status deployment/grafana --timeout=180s
    @echo "==> e2e-bwc-up done (bundled ClickHouse on MinIO object storage)"

# Assert the data tier actually lives on the storage tier `scenario` claims
# (MUST match the one passed to `e2e-bwc-up`): storage_policy stamped on every
# MergeTree table, active parts on the expected disk(s) (never the local
# `default` disk), and — for object-storage / hot-cold — the MinIO bucket
# non-empty after the seed. Run AFTER `just e2e-seed-rolling`. Logic lives in
# the env-driven Node module (per the CLAUDE.md "non-trivial step logic in
# .github/scripts/*.mjs" rule), invoked with the pinned mc image so the
# in-cluster bucket-ls pod matches the lane.

# Assert the bwc data tier really lives on the storage tier the scenario claims.
e2e-bwc-verify scenario="object-storage":
    @echo "==> [bwc] verifying {{scenario}} placement"
    SCENARIO="{{scenario}}" \
    MC_IMAGE="minio/mc:RELEASE.2025-08-13T08-35-41Z" \
        node .github/scripts/e2e-bwc-verify-placement.mjs

# Mode-toggle migration-safety leg (cerberus issue #3082): helm-upgrade an
# ALREADY-POPULATED bwc cluster — brought up as the plain object-storage
# scenario (`just e2e-bwc-up object-storage` + `just e2e-seed-rolling`), so
# every table's metadata already says `storage_policy='bwc_object_store'` —
# into hot-cold mode WITHOUT pinning `hotVolume.enabled: false`. This
# reproduces docs/helm-clickhouse.md's "Upgrading into the hot/cold default"
# hazard exactly: the new storage XML only defines `bwc_hot_cold`, a policy
# name the already-populated ClickHouse has never seen, so its own startup
# validation must refuse to start — the object-store -> hot-cold direction of
# cerberus issue #3075/#3076's per-mode storage-policy names — rather than
# silently reusing or renaming the policy.
#
# Deliberately NO --wait: the upgrade is EXPECTED to leave the ClickHouse pod
# permanently crash-looping, so blocking on rollout would just burn the whole
# --timeout doing nothing `e2e-bwc-verify-mode-toggle` doesn't already do far
# more precisely with its own targeted log-content poll. Leading `-`: a
# non-zero/timeout exit here is the EXPECTED shape, not something this recipe
# itself needs to interpret — the verify recipe is what proves WHICH failure
# this actually was.

# Helm-upgrade the already-populated bwc cluster into an incompatible mode.
e2e-bwc-toggle-mode:
    @echo "==> [bwc] mode-toggle: helm upgrade into hot-cold mode against the ALREADY-POPULATED bwc_object_store cluster (must fail loudly, never silently)"
    -helm upgrade --install cerberus deploy/helm/cerberus \
        --namespace cerberus \
        --values test/e2e/k3s/cerberus-values.yaml \
        --values test/e2e/k3s/cerberus-values-bwc.yaml \
        --values test/e2e/k3s/cerberus-values-bwc-hot-cold.yaml \
        --values test/e2e/k3s/cerberus-values-bwc-mode-toggle.yaml \
        --timeout 120s
    @echo "==> [bwc] mode-toggle upgrade command returned — see 'just e2e-bwc-verify-mode-toggle' for the actual pass/fail verdict"

# Assert the mode-toggling upgrade above failed the SPECIFIC way cerberus
# issue #3075/#3076 promises: ClickHouse's own UNKNOWN_POLICY startup
# exception, never merely a generic non-ready pod, a plain non-zero helm
# exit, a timeout, or (the one failure mode this whole mechanism exists to
# prevent) a silent success. Logic lives in the env-driven Node module per
# the CLAUDE.md "non-trivial step logic in .github/scripts/*.mjs" rule.

# Assert the mode-toggle upgrade failed loudly with ClickHouse's UNKNOWN_POLICY error.
e2e-bwc-verify-mode-toggle:
    @echo "==> [bwc] verifying the mode-toggle upgrade failed loudly (UNKNOWN_POLICY)"
    node .github/scripts/e2e-bwc-verify-mode-toggle.mjs

# Tear down the bwc lane. Same cluster name + rolling seeder as the standard
# lane, so the standard teardown covers it exactly.

# Tear the bundled-ClickHouse (bwc) lane down.
e2e-bwc-down: e2e-down

# Run the compose-stack Grafana catch-net spec locally. Assumes the
# quickstart compose stack is already up (`docker compose up --wait`).
# Drives Grafana through every provisioned dashboard and asserts every
# /api/ds/query + /api/dashboards/* response is 2xx with no tunneled
# per-target error. Mirrors the Playwright step the compose-smoke CI
# job runs.

# Run the compose-stack Grafana catch-net spec. Assumes the compose stack is already up.
compose-grafana-smoke:
    @echo "==> compose-grafana-smoke playwright catch-net"
    cd test/e2e/playwright && \
        ( [ -f package-lock.json ] && npm ci || npm install --no-audit --no-fund ) && \
        npx playwright install --with-deps chromium && \
        GRAFANA_BASE_URL=http://localhost:3000 \
        GRAFANA_URL=http://localhost:3000 \
        CERBERUS_URL=http://localhost:8080 \
        npx playwright test compose_grafana_smoke.spec.ts --reporter=list

